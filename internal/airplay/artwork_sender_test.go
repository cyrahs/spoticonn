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
		path, ok := strings.CutPrefix(line, "ARTWORKFILE=")
		if !ok {
			path, ok = strings.CutPrefix(line, "ARTWORK=")
		}
		if ok && path != "" {
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
	// A new item on the same album bundles the same immutable image.
	// No file rewrite or download is necessary.
	track.URI = "next-track"
	_ = s.Metadata(track)
	waitArtwork(t, s)
	paths = artworkPaths(commands())
	if len(paths) != 2 || paths[0] != paths[1] || calls.Load() != 1 {
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
	if !strings.Contains(before, "TITLE=latest\nARTIST=artist\nALBUM=\nITEMID=new-track\nDURATION=0\nARTWORKFILE=\nACTION=SENDMETA\nDURATION=0\nPROGRESS=9\n") || !strings.HasSuffix(before, "ARTWORK="+paths[0]+"\n") {
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
			if strings.Contains(commands(), "ARTWORK=\n") {
				t.Fatal("cleared artwork that was never delivered")
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
			if strings.Contains(commands(), "ARTWORK=\n") {
				t.Fatal("cleared artwork that was never delivered")
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

func TestSenderArtworkFilesSurviveUntilChildExit(t *testing.T) {
	data := testArtwork(t, "jpeg")
	s, commands, _ := controlSender(t, 30)
	s.artworkCache, s.runtimeDir = newArtworkCache(func(_ context.Context, source string) ([]byte, error) {
		// Distinct valid JPEGs with a small COM marker; alias-0 has the same
		// bytes as 0 even after the in-memory download cache evicts it.
		comment := strings.TrimPrefix(source, "alias-")
		result := append([]byte{}, data[:2]...)
		result = append(result, 0xff, 0xfe, 0, byte(len(comment)+2))
		result = append(result, comment...)
		return append(result, data[2:]...), nil
	}), t.TempDir()
	for i := 0; i < artworkCacheEntries+3; i++ {
		_ = s.Metadata(&model.Track{URI: fmt.Sprint(i), Cover: fmt.Sprint(i)})
		waitArtwork(t, s)
	}
	_ = s.Metadata(&model.Track{URI: "alias", Cover: "alias-0"})
	paths := artworkPaths(commands())
	files, _ := os.ReadDir(filepath.Dir(paths[0]))
	if len(files) != artworkCacheEntries+3 || paths[0] != paths[len(paths)-1] {
		t.Fatal("identical image bytes were not deduplicated", len(files))
	}
	for _, path := range paths {
		if _, err := os.ReadFile(path); err != nil {
			t.Fatal("deleted a path before the child consumed it", err)
		}
	}
	if _, err := os.Stat(paths[len(paths)-1]); err != nil {
		t.Fatal("removed current artwork", err)
	}
	s.Close()
	if _, err := os.Stat(filepath.Dir(paths[0])); !os.IsNotExist(err) {
		t.Fatal("process artwork files survived shutdown", err)
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
	diagnostics := make(chan string, 8)
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
		waitForEngineCommands(t, commandLog, loaded, attempt+1)
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
		_ = s.Volume(42)
		trace := waitForEngineCommands(t, commandLog, "VOLUME=42\n", attempt+1)
		if strings.Count(trace, loaded) != attempt+1 || strings.Count(trace, "ACTION=SENDMETA\n") != attempt+1 {
			t.Fatal("warm resume resent metadata/artwork", trace)
		}
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

func TestArtworkBundlesBeforeStartAndProgressNeverReplaces(t *testing.T) {
	data := testArtwork(t, "jpeg")
	cache := newArtworkCache(func(context.Context, string) ([]byte, error) { return data, nil })
	for _, warm := range []bool{false, true} {
		t.Run(fmt.Sprint(warm), func(t *testing.T) {
			if warm {
				_, _ = cache.get(t.Context(), "cover")
			}
			s, commands, _ := controlSender(t, 30)
			s.artworkCache, s.runtimeDir = cache, t.TempDir()
			track := &model.Track{URI: "track", Cover: "cover", Name: "title", Position: 9000, Duration: 120000}
			if err := s.Metadata(track); err != nil {
				t.Fatal(err)
			}
			startSender(s)
			trace := commands()
			paths := artworkPaths(trace)
			if len(paths) != 1 || strings.Count(trace, "ACTION=SENDMETA\n") != 1 || strings.Contains(trace, "ARTWORK=\n") {
				t.Fatal("initial metadata was not bundled", trace)
			}
			if !strings.Contains(trace, "ARTWORKFILE="+paths[0]+"\nACTION=SENDMETA\nDURATION=120\nPROGRESS=9\nSTART_UNIX_MS=0\nACTION=START\n") {
				t.Fatal("incorrect bundle/progress/start order", trace)
			}
			for i := 0; i < 20; i++ {
				track.Position += 1000
				_ = s.Metadata(track)
			}
			track.Duration += 1000
			_ = s.Metadata(track)
			if strings.Count(commands(), "ACTION=SENDMETA\n") != 1 || len(artworkPaths(commands())) != 1 {
				t.Fatal("progress/duration rebuilt the media card", commands())
			}
			before := commands()
			_ = s.Metadata(track)
			if commands() != before {
				t.Fatal("identical progress was not deduplicated")
			}
		})
	}
}

func TestArtworkStartInterruptsBundleWait(t *testing.T) {
	for _, join := range []bool{false, true} {
		t.Run(fmt.Sprint(join), func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			data := testArtwork(t, "jpeg")
			cache := newArtworkCache(func(ctx context.Context, _ string) ([]byte, error) {
				close(started)
				select {
				case <-release:
					return data, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			})
			s, commands, _ := controlSender(t, 30)
			s.artworkCache, s.runtimeDir = cache, t.TempDir()
			metadataDone := make(chan error, 1)
			go func() { metadataDone <- s.Metadata(&model.Track{URI: "track", Cover: "cover"}) }()
			<-started
			if join {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				go func() { _, _ = s.Join(ctx, 12345) }()
			} else {
				startSender(s)
			}
			select {
			case err := <-metadataDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(100 * time.Millisecond):
				t.Fatal("START did not interrupt the bundle wait")
			}
			trace := commands()
			if strings.Index(trace, "ACTION=SENDMETA\n") < 0 || strings.Index(trace, "ACTION=SENDMETA\n") > strings.Index(trace, "ACTION=START\n") {
				t.Fatal("START preceded text metadata", trace)
			}
			close(release)
			waitArtwork(t, s)
			trace = commands()
			if strings.Count(trace, "ACTION=SENDMETA\n") != 1 || strings.Count(trace, "ARTWORKFILE=\n") != 1 || len(artworkPaths(trace)) != 1 || !strings.Contains(trace, "\nARTWORK=") {
				t.Fatal("slow artwork resent the full metadata", trace)
			}
		})
	}
}

func TestArtworkReplacementAndFailureClearOnlyRetainedImage(t *testing.T) {
	for _, next := range []string{"new-cover", "failed", ""} {
		t.Run(next, func(t *testing.T) {
			data := testArtwork(t, "jpeg")
			cache := newArtworkCache(func(_ context.Context, source string) ([]byte, error) {
				if source == "failed" {
					return nil, errors.New("封面下载失败或超时")
				}
				return data, nil
			})
			s, commands, _ := controlSender(t, 30)
			s.artworkCache, s.runtimeDir = cache, t.TempDir()
			track := &model.Track{URI: "track", Cover: "old-cover"}
			_ = s.Metadata(track)
			before := commands()
			track.Cover = next
			_ = s.Metadata(track)
			waitArtwork(t, s)
			delta := strings.TrimPrefix(commands(), before)
			if strings.Contains(delta, "SENDMETA") {
				t.Fatal("same-item cover change resent text", delta)
			}
			if next == "new-cover" {
				if len(artworkPaths(delta)) != 1 || strings.Contains(delta, "ARTWORK=\n") {
					t.Fatal("replacement cleared the ready image", delta)
				}
			} else if delta != "ARTWORK=\n" {
				t.Fatal("missing/failed cover did not clear old artwork exactly once", delta)
			}
			before = commands()
			_ = s.Metadata(track)
			if commands() != before {
				t.Fatal("unchanged cover repeated commands")
			}
		})
	}
}

func TestArtworkFIFOFailureDoesNotCommitDeliveryIdentity(t *testing.T) {
	data := testArtwork(t, "jpeg")
	s, commands, _ := controlSender(t, 30)
	s.artworkCache, s.runtimeDir = newArtworkCache(func(context.Context, string) ([]byte, error) { return data, nil }), t.TempDir()
	fd := s.cmdFD
	s.cmdFD = -1
	track := &model.Track{URI: "track", Cover: "cover"}
	if s.Metadata(track) == nil {
		t.Fatal("failed metadata write reported success")
	}
	s.mu.Lock()
	if s.artwork.textSet || s.artwork.cover != "" {
		t.Error("failed write committed delivery state")
	}
	s.cmdFD = fd
	s.mu.Unlock()
	if err := s.Metadata(track); err != nil {
		t.Fatal(err)
	}
	if len(artworkPaths(commands())) != 1 || strings.Count(commands(), "ACTION=SENDMETA\n") != 1 {
		t.Fatal("failed delivery did not retry the prepared bundle", commands())
	}
}

func TestArtworkStopInterruptsBundleWait(t *testing.T) {
	started := make(chan struct{})
	s, commands, _ := controlSender(t, 30)
	s.artworkCache = newArtworkCache(func(ctx context.Context, _ string) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	done := make(chan error, 1)
	go func() { done <- s.Metadata(&model.Track{URI: "track", Cover: "cover"}) }()
	<-started
	_ = s.Standby()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("standby did not interrupt metadata wait")
	}
	if commands() != "ACTION=STANDBY\n" {
		t.Fatal("stopped metadata escaped its generation", commands())
	}
}

func TestMetadataDiscardsArtworkStagedByInterruptedWrite(t *testing.T) {
	binary := fakeBinary(t)
	commandLog := filepath.Join(t.TempDir(), "commands")
	t.Setenv("SPOTICONN_TEST_AIRPLAY_COMMANDS", commandLog)
	s, err := Open(t.Context(), Config{Binary: binary, RuntimeDir: t.TempDir()}, model.Device{Address: "127.0.0.1", Port: 7000}, model.PairingSecret{}, 30, 44100, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	oldPath := filepath.Join(t.TempDir(), "old.jpg")
	if err := os.WriteFile(oldPath, testArtwork(t, "jpeg"), 0600); err != nil {
		t.Fatal(err)
	}
	// Simulate the successfully written prefix of an interrupted bundle.
	if err := s.command("ARTWORKFILE=" + oldPath + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := s.Metadata(&model.Track{URI: "new-track"}); err != nil {
		t.Fatal(err)
	}
	_ = s.Volume(42)
	trace := waitForEngineCommands(t, commandLog, "VOLUME=42\n", 1)
	if strings.Contains(trace, "ARTWORK_LOADED") || strings.Contains(trace, "ARTWORK_LOAD_FAILED") {
		t.Fatal("new item consumed another item's staged artwork", trace)
	}
}
