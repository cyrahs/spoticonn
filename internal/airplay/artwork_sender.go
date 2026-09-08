package airplay

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"spoticonn/internal/model"
)

type artworkKey struct{ item, cover string }

// All control state belongs to Sender.mu; one worker owns the temporary files.
type senderArtwork struct {
	track               model.Track
	key                 artworkKey
	epoch               uint64
	sent, suspended     bool
	cancel              context.CancelFunc
	changed, stop, done chan struct{}
	stopped             bool
}

func trackArtworkKey(t model.Track) artworkKey {
	item := t.URI
	if item == "" {
		item = fmt.Sprintf("%q/%q/%q", t.Name, t.Artists, t.Album)
	}
	return artworkKey{item: item, cover: t.Cover}
}

func metadataCommand(t model.Track, artworkPath string) string {
	clean := func(v string) string { return strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(v) }
	return fmt.Sprintf("TITLE=%s\nARTIST=%s\nALBUM=%s\nITEMID=%s\nDURATION=%d\nPROGRESS=%d\nARTWORKFILE=%s\nACTION=SENDMETA\n", clean(t.Name), clean(strings.Join(t.Artists, ", ")), clean(t.Album), clean(t.URI), t.Duration/1000, t.Position/1000, artworkPath)
}

func (s *Sender) Metadata(t *model.Track) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var track model.Track
	if t != nil {
		track = *t
		track.Artists = append([]string(nil), t.Artists...)
	}
	key := trackArtworkKey(track)
	a := s.artwork
	command := metadataCommand(track, "")
	// Empty ARTWORK clears retained MRP artwork in v0.5.3, including when the
	// same item's cover is removed/replaced. ARTWORKFILE= alone only unstages.
	if a != nil && a.key.cover != "" && a.key.cover != key.cover {
		command += "ARTWORK=\n"
	}
	// Invalidate the previous download even if this control write fails.
	if a != nil {
		if a.key != key {
			a.epoch++
			a.sent = false
			if a.cancel != nil {
				a.cancel()
			}
		}
		a.track, a.key = track, key
	}
	if err := s.commandLocked(command); err != nil {
		return err
	}
	if a == nil {
		a = &senderArtwork{track: track, key: key, changed: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
		s.artwork = a
		go s.runArtwork(a)
	}
	s.wakeArtworkLocked()
	return nil
}

func (s *Sender) wakeArtworkLocked() {
	if a := s.artwork; a != nil && !a.stopped {
		select {
		case a.changed <- struct{}{}:
		default:
		}
	}
}

func (s *Sender) suspendArtworkLocked() {
	if a := s.artwork; a != nil {
		a.epoch++
		a.sent, a.suspended = false, true
		if a.cancel != nil {
			a.cancel()
		}
	}
}

func (s *Sender) resumeArtworkLocked() {
	if a := s.artwork; a != nil && a.suspended {
		a.suspended = false
		s.wakeArtworkLocked()
	}
}

func (s *Sender) stopArtworkLocked() {
	if a := s.artwork; a != nil && !a.stopped {
		a.stopped = true
		if a.cancel != nil {
			a.cancel()
		}
		close(a.stop)
	}
}

func (s *Sender) artworkDiagnostic(message string) {
	if s.diagnostic != nil {
		s.diagnostic("AirPlay 封面：" + message)
	}
}

func (s *Sender) runArtwork(a *senderArtwork) {
	var dir string
	var files []string
	defer func() {
		// FIFO writes don't acknowledge file consumption. Keep immutable files
		// until the child exits; retain only the latest eight during playback.
		<-s.done
		if dir != "" {
			_ = os.RemoveAll(dir)
		}
		close(a.done)
	}()
	cache := s.artworkCache
	if cache == nil {
		cache = defaultArtworkCache
	}
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-a.stop:
			return
		case <-a.changed:
		}
		s.mu.Lock()
		if s.closed || a.stopped {
			s.mu.Unlock()
			return
		}
		if a.sent || a.suspended {
			s.mu.Unlock()
			continue
		}
		if a.key.cover == "" {
			a.sent = true
			s.mu.Unlock()
			s.artworkDiagnostic("当前曲目未提供封面")
			continue
		}
		epoch, source := a.epoch, a.key.cover
		ctx, cancel := context.WithCancel(s.ctx)
		a.cancel = cancel
		s.mu.Unlock()

		data, err := cache.get(ctx, source)
		var path string
		if err == nil && ctx.Err() == nil {
			if dir == "" {
				dir, err = os.MkdirTemp(s.runtimeDir, "airplay-artwork-")
				if err == nil {
					dir, err = filepath.Abs(dir)
				}
			}
			if err == nil {
				var file *os.File
				file, err = os.CreateTemp(dir, "cover-*.jpg")
				if err == nil {
					path = file.Name()
					if strings.ContainsAny(path, "\r\n\x00") {
						err = errors.New("unsafe artwork path")
					} else {
						_, err = file.Write(data)
					}
					if closeErr := file.Close(); err == nil {
						err = closeErr
					}
				}
			}
			if err != nil {
				err = errors.New("封面临时文件不可用")
			}
		}
		cancel()
		s.mu.Lock()
		current := !s.closed && !a.stopped && !a.suspended && a.epoch == epoch && s.ctx.Err() == nil
		if current {
			// One attempt per item/cover/transport epoch, even after failure.
			// Progress and volume updates must not cause retries or resends.
			a.sent = true
			if err == nil && path != "" {
				// Use the latest text/position, never the pre-download snapshot.
				err = s.commandLocked(metadataCommand(a.track, path))
			}
		}
		s.mu.Unlock()
		if path != "" {
			if current && err == nil {
				files = append(files, path)
			} else {
				_ = os.Remove(path)
			}
		}
		for len(files) > artworkCacheEntries {
			// A lagging child may miss obsolete artwork, but can never read a
			// new track's bytes through an old track's path.
			_ = os.Remove(files[0])
			files = files[1:]
		}
		if current && err != nil {
			s.artworkDiagnostic(err.Error())
		}
	}
}
