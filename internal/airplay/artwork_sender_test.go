package airplay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"spoticonn/internal/model"
)

func artworkPaths(trace string) []string {
	var paths []string
	for _, line := range strings.Split(trace, "\n") {
		if path, ok := strings.CutPrefix(line, "ARTWORKFILE="); ok && path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func waitArtwork(t *testing.T, s *Sender) {
	t.Helper()
	eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.artwork != nil && s.artwork.sent })
}

func TestSenderArtworkUsesImmutableFilesAndAvoidsRepeatedWork(t *testing.T) {
	data := testArtwork(t, "jpeg")
	var calls atomic.Int32
	cache := newArtworkCache(func(context.Context, string) ([]byte, error) { calls.Add(1); return data, nil })
	s, commands, _ := controlSender(t, 30)
	s.artworkCache, s.runtimeDir = cache, t.TempDir()
	track := &model.Track{URI: "track", Name: "title\nACTION=STOP", Artists: []string{"artist"}, Album: "album", Cover: "https://cover.example/image?token=private", Position: 1000}
	if err := s.Metadata(track); err != nil {
		t.Fatal(err)
	}
	waitArtwork(t, s)
	paths := artworkPaths(commands())
	if len(paths) != 1 {
		t.Fatal("missing artwork file", commands())
	}
	if got, err := os.ReadFile(paths[0]); err != nil || !strings.HasSuffix(paths[0], ".jpg") || !strings.HasPrefix(paths[0], s.runtimeDir) || !bytes.Equal(got, data) {
		t.Fatal("incorrect local artwork", paths, err)
	}
	file, _ := os.Stat(paths[0])
	dir, _ := os.Stat(filepath.Dir(paths[0]))
	if file.Mode().Perm() != 0600 || dir.Mode().Perm() != 0700 {
		t.Fatal("artwork files are not private")
	}
	for i := 0; i < 10; i++ {
		track.Position++
		_ = s.Metadata(track)
		_ = s.Volume(i)
	}
	if calls.Load() != 1 || len(artworkPaths(commands())) != 1 {
		t.Fatal("progress or volume resent artwork")
	}
	if strings.Contains(commands(), "token=private") || strings.Contains(commands(), "\nACTION=STOP\n") {
		t.Fatal("URL or command injection reached the FIFO")
	}
	// A new item on the same album gets the cached bytes, in a different,
	// immutable file. The old command can never read the new item's file.
	track.URI = "next-track"
	_ = s.Metadata(track)
	waitArtwork(t, s)
	paths = artworkPaths(commands())
	if len(paths) != 2 || paths[0] == paths[1] || calls.Load() != 1 {
		t.Fatal("track update did not reuse cache safely", paths)
	}
	s.Close()
	if _, err := os.Stat(filepath.Dir(paths[0])); !os.IsNotExist(err) {
		t.Fatal("artwork survived process shutdown")
	}
}

func TestSenderArtworkDropsSupersededDownloadsAndUsesLatestPosition(t *testing.T) {
	oldStarted, oldRelease, oldDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	newStarted, newRelease := make(chan struct{}), make(chan struct{})
	data := testArtwork(t, "jpeg")
	cache := newArtworkCache(func(ctx context.Context, source string) ([]byte, error) {
		if source == "old" {
			close(oldStarted)
			<-oldRelease
			close(oldDone)
			return []byte("old-image"), nil
		}
		close(newStarted)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-newRelease:
			return data, nil
		}
	})
	s, commands, _ := controlSender(t, 30)
	s.artworkCache, s.runtimeDir = cache, t.TempDir()
	_ = s.Metadata(&model.Track{URI: "old-track", Cover: "old"})
	<-oldStarted
	_ = s.Metadata(&model.Track{URI: "new-track", Cover: "new", Position: 1000})
	<-newStarted
	// Mutating the caller's artists after Metadata must not race with the worker.
	track := &model.Track{URI: "new-track", Cover: "new", Name: "latest", Artists: []string{"artist"}, Position: 9000}
	_ = s.Metadata(track)
	track.Artists[0] = "mutated"
	close(newRelease)
	waitArtwork(t, s)
	before := commands()
	close(oldRelease)
	<-oldDone
	eventually(t, func() bool { cache.mu.Lock(); defer cache.mu.Unlock(); return len(cache.pending) == 0 })
	if commands() != before {
		t.Fatal("old download rewrote new metadata")
	}
	paths := artworkPaths(before)
	if len(paths) != 1 {
		t.Fatal("stale download sent artwork", before)
	}
	got, err := os.ReadFile(paths[0])
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal("old image replaced current image", err)
	}
	if !strings.HasSuffix(before, "TITLE=latest\nARTIST=artist\nALBUM=\nITEMID=new-track\nDURATION=0\nPROGRESS=9\nARTWORKFILE="+paths[0]+"\nACTION=SENDMETA\n") {
		t.Fatal("download reverted refined metadata", before)
	}
}

func TestSenderArtworkFailureAndMissingCoverKeepControlsUsable(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			var calls atomic.Int32
			cache := newArtworkCache(func(context.Context, string) ([]byte, error) {
				calls.Add(1)
				return nil, errors.New("封面下载失败或超时")
			})
			s, commands, _ := controlSender(t, 30)
			s.artworkCache, s.runtimeDir = cache, t.TempDir()
			diagnostics := make(chan string, 8)
			s.diagnostic = func(v string) { diagnostics <- v }
			track := &model.Track{URI: "track", Name: "title", Cover: "https://cover.example/secret"}
			if missing {
				track.Cover = ""
			}
			if err := s.Metadata(track); err != nil {
				t.Fatal(err)
			}
			if !missing {
				waitArtwork(t, s)
				select {
				case message := <-diagnostics:
					if strings.Contains(message, "secret") {
						t.Fatal(message)
					}
				case <-time.After(time.Second):
					t.Fatal("missing diagnostic")
				}
			}
			for i := 0; i < 10; i++ {
				_ = s.Metadata(track)
				_ = s.Volume(12)
			}
			if len(artworkPaths(commands())) != 0 || !strings.Contains(commands(), "TITLE=title\n") || !strings.HasSuffix(commands(), "VOLUME=12\n") {
				t.Fatal("artwork failure affected text/controls")
			}
			if (!missing && calls.Load() != 1) || (missing && calls.Load() != 0) {
				t.Fatal("unexpected repeated downloads", calls.Load())
			}
			track.Cover = ""
			_ = s.Metadata(track)
			if !missing && !strings.HasSuffix(commands(), "ARTWORK=\n") {
				t.Fatal("same-item missing cover did not clear retained MRP artwork")
			}
		})
	}
}

func TestSenderNoCoverAndShutdownCancelPendingArtwork(t *testing.T) {
	for _, action := range []string{"nil", "no_cover", "close", "standby"} {
		t.Run(action, func(t *testing.T) {
			started, cancelled := make(chan struct{}), make(chan struct{})
			cache := newArtworkCache(func(ctx context.Context, _ string) ([]byte, error) {
				close(started)
				<-ctx.Done()
				close(cancelled)
				return nil, ctx.Err()
			})
			s, commands, _ := controlSender(t, 30)
			s.artworkCache, s.runtimeDir = cache, t.TempDir()
			_ = s.Metadata(&model.Track{URI: "track", Cover: "old"})
			<-started
			switch action {
			case "nil":
				_ = s.Metadata(nil)
			case "no_cover":
				_ = s.Metadata(&model.Track{URI: "track"})
			case "close":
				s.Close()
			case "standby":
				_ = s.Standby()
			}
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("download survived invalidation")
			}
			if len(artworkPaths(commands())) != 0 {
				t.Fatal("cancelled download published artwork")
			}
			if (action == "nil" || action == "no_cover") && !strings.Contains(commands(), "ARTWORK=\n") {
				t.Fatal("cover was not cleared")
			}
		})
	}
}

func TestSenderArtworkSendFailureIsDiagnosticOnly(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	data := testArtwork(t, "jpeg")
	cache := newArtworkCache(func(context.Context, string) ([]byte, error) { close(started); <-release; return data, nil })
	s, _, events := controlSender(t, 30)
	s.artworkCache, s.runtimeDir = cache, t.TempDir()
	diagnostics := make(chan string, 8)
	s.diagnostic = func(v string) { diagnostics <- v }
	_ = s.Metadata(&model.Track{URI: "track", Cover: "cover"})
	<-started
	s.mu.Lock()
	s.cmdFD = -1
	s.mu.Unlock()
	close(release)
	select {
	case <-diagnostics:
	case <-time.After(time.Second):
		t.Fatal("missing send failure diagnostic")
	}
	if len(events) != 0 {
		t.Fatal("artwork error interrupted playback")
	}
	waitArtwork(t, s)
}

func TestSenderArtworkFilesStayBounded(t *testing.T) {
	data := testArtwork(t, "jpeg")
	s, commands, _ := controlSender(t, 30)
	s.artworkCache, s.runtimeDir = newArtworkCache(func(context.Context, string) ([]byte, error) { return data, nil }), t.TempDir()
	for i := 0; i < artworkCacheEntries+3; i++ {
		_ = s.Metadata(&model.Track{URI: fmt.Sprint(i), Cover: "cover"})
		waitArtwork(t, s)
	}
	paths := artworkPaths(commands())
	eventually(t, func() bool { files, _ := os.ReadDir(filepath.Dir(paths[0])); return len(files) == artworkCacheEntries })
	if _, err := os.Stat(paths[len(paths)-1]); err != nil {
		t.Fatal("removed current artwork", err)
	}
}

func TestArtworkRuntimePathCannotInjectCommands(t *testing.T) {
	data := testArtwork(t, "jpeg")
	s, commands, _ := controlSender(t, 30)
	s.artworkCache = newArtworkCache(func(context.Context, string) ([]byte, error) { return data, nil })
	s.runtimeDir = filepath.Join(t.TempDir(), "runtime\nACTION=STOP\n")
	if err := os.Mkdir(s.runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	diagnostics := make(chan string, 1)
	s.diagnostic = func(v string) { diagnostics <- v }
	_ = s.Metadata(&model.Track{URI: "track", Cover: "cover"})
	select {
	case <-diagnostics:
	case <-time.After(time.Second):
		t.Fatal("missing unsafe path diagnostic")
	}
	if len(artworkPaths(commands())) != 0 || strings.Contains(commands(), "ACTION=STOP") {
		t.Fatal("runtime path injected commands")
	}
}

func TestArtworkEngineDiagnosticsNeverExposeEngineDetails(t *testing.T) {
	s, _, events := controlSender(t, 30)
	var messages []string
	s.diagnostic = func(v string) { messages = append(messages, v) }
	senderStatus(s, "mrp artwork=rejected reason=secret path=/private/cover?token=secret")
	senderStatus(s, "mrp artwork=posted status=500 bytes=123 url=secret")
	if len(messages) != 2 || len(events) != 0 {
		t.Fatal("artwork status affected playback")
	}
	for _, v := range messages {
		if strings.Contains(v, "secret") || strings.Contains(v, "private") {
			t.Fatal("engine details leaked", v)
		}
	}
}

func TestArtworkSubprocessPCMResumeAndReconnect(t *testing.T) {
	binary := fakeBinary(t)
	commandLog := filepath.Join(t.TempDir(), "commands")
	t.Setenv("SPOTICONN_TEST_AIRPLAY_COMMANDS", commandLog)
	started, release := make(chan struct{}), make(chan struct{})
	data := testArtwork(t, "jpeg")
	var calls atomic.Int32
	cache := newArtworkCache(func(ctx context.Context, _ string) ([]byte, error) {
		calls.Add(1)
		close(started)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
			return data, nil
		}
	})
	track := &model.Track{URI: "track", Name: "title", Cover: "https://cover.example/image"}
	loaded := fmt.Sprintf("ARTWORK_LOADED item=track sha256=%x\n", sha256.Sum256(data))
	for attempt := 0; attempt < 2; attempt++ {
		s, err := Open(t.Context(), Config{Binary: binary, RuntimeDir: t.TempDir(), Artwork: cache}, model.Device{Address: "127.0.0.1", Port: 7000}, model.PairingSecret{}, 30, 44100, func(string) {})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		if err = s.Metadata(track); err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			<-started
		}
		s.Begin()
		if err = s.Write(make([]byte, 1764)); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		_, err = s.Started(ctx)
		cancel()
		if err != nil {
			t.Fatal("artwork download blocked PCM/start", err)
		}
		if attempt == 0 {
			close(release)
		}
		waitForEngineCommands(t, commandLog, loaded, attempt*2+1)
		ctx, cancel = context.WithTimeout(t.Context(), time.Second)
		err = s.Flush(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Standby(); err != nil {
			t.Fatal(err)
		}
		s.Begin()
		_ = s.Metadata(track)
		if err = s.Write(make([]byte, 1764)); err != nil {
			t.Fatal(err)
		}
		trace := waitForEngineCommands(t, commandLog, loaded, attempt*2+2)
		if strings.Contains(trace, "ARTWORK_LOAD_FAILED") {
			t.Fatal("child could not read artwork", trace)
		}
		s.Close()
	}
	if calls.Load() != 1 {
		t.Fatal("resume/reconnect downloaded artwork again", calls.Load())
	}
}

type artworkTestMember struct {
	*testMember
	sender *Sender
}

func (m *artworkTestMember) Metadata(t *model.Track) error { return m.sender.Metadata(t) }
func (m *artworkTestMember) Close()                        { m.testMember.Close(); m.sender.Close() }

func TestGroupLateJoinReceivesCachedCurrentArtwork(t *testing.T) {
	for _, changeDuringJoin := range []bool{false, true} {
		t.Run(fmt.Sprint(changeDuringJoin), func(t *testing.T) {
			data := testArtwork(t, "jpeg")
			var calls atomic.Int32
			cache := newArtworkCache(func(context.Context, string) ([]byte, error) { calls.Add(1); return data, nil })
			g, p, events := testGroup(t)
			primary, _, _ := controlSender(t, 30)
			primary.artworkCache, primary.runtimeDir = cache, t.TempDir()
			g.primary = &artworkTestMember{testMember: p, sender: primary}
			tvSender, tvCommands, _ := controlSender(t, 30)
			tvSender.artworkCache, tvSender.runtimeDir = cache, t.TempDir()
			tv := &artworkTestMember{testMember: &testMember{anchor: p.anchor + 10, joinGate: make(chan struct{})}, sender: tvSender}
			opened := make(chan struct{})
			g.open = func(ctx context.Context, _ Config, _ model.Device, _ model.PairingSecret, _, _ int, _ func(string)) (timedOutput, error) {
				tv.ctx = ctx
				close(opened)
				return tv, nil
			}
			track := &model.Track{URI: "current", Cover: "cover", Artists: []string{"artist"}}
			_ = g.Metadata(track)
			waitArtwork(t, primary)
			g.Begin()
			_ = g.Write(make([]byte, 3528))
			close(p.ack)
			<-opened
			waitArtwork(t, tvSender)
			if changeDuringJoin {
				track = &model.Track{URI: "new", Cover: "new-cover", Artists: []string{"new-artist"}}
				_ = g.Metadata(track)
				track.Artists[0] = "caller-mutation"
				waitArtwork(t, primary)
			}
			close(tv.joinGate)
			waitGroup(t, events, "group_joined")
			want := 1
			if changeDuringJoin {
				want = 2
			}
			eventually(t, func() bool { return len(artworkPaths(tvCommands())) == want })
			if calls.Load() != int32(want) {
				t.Fatal("late join repeated a download", calls.Load())
			}
			if changeDuringJoin && (!strings.Contains(tvCommands(), "ITEMID=new\n") || strings.Contains(tvCommands(), "caller-mutation")) {
				t.Fatal("join did not apply latest copied track", tvCommands())
			}
			_ = g.Volume(18)
			_ = g.Metadata(track)
			if err := g.Write(make([]byte, 1764)); err != nil {
				t.Fatal("metadata interrupted HomePod", err)
			}
			g.Close()
			if len(artworkPaths(tvCommands())) != want {
				t.Fatal("group volume/progress resent artwork")
			}
		})
	}
}
