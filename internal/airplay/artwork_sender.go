package airplay

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"image/jpeg"
	"os"
	"path/filepath"
	"strings"
	"time"

	"spoticonn/internal/model"
)

const artworkBundleBudget = 200 * time.Millisecond

type artworkKey struct{ item, cover string }

// Control state belongs to Sender.mu; one worker owns the immutable files.
// sent means the preparation attempt finished, not receiver/display success.
type senderArtwork struct {
	track                model.Track
	key                  artworkKey
	epoch                uint64
	sent, suspended      bool
	cancel               context.CancelFunc
	changed, stop, done  chan struct{}
	stopped              bool
	preparedCover, path  string
	text, item, cover    string // identities successfully written to the FIFO
	textSet, progressSet bool
	duration, position   int64
	release              chan struct{}
	released             bool
	deliveryErr          error
}

func trackArtworkKey(t model.Track) artworkKey {
	item := t.URI
	if item == "" {
		item = fmt.Sprintf("%q/%q/%q", t.Name, t.Artists, t.Album)
	}
	return artworkKey{item: item, cover: t.Cover}
}

func metadataText(t model.Track) string {
	clean := func(v string) string { return strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(v) }
	return fmt.Sprintf("TITLE=%s\nARTIST=%s\nALBUM=%s\nITEMID=%s\n", clean(t.Name), clean(strings.Join(t.Artists, ", ")), clean(t.Album), clean(t.URI))
}

func metadataCommand(t model.Track, artworkPath string) string {
	command := metadataText(t) + fmt.Sprintf("DURATION=%d\n", t.Duration/1000)
	if artworkPath != "" {
		command += "ARTWORKFILE=" + artworkPath + "\n"
	}
	// v0.5.3 resets elapsed time when ITEMID changes. Anchor progress AFTER
	// SENDMETA; PROGRESS before it applies to the previous item and gets reset.
	return command + "ACTION=SENDMETA\n" + progressCommand(t)
}

func progressCommand(t model.Track) string {
	return fmt.Sprintf("DURATION=%d\nPROGRESS=%d\n", t.Duration/1000, t.Position/1000)
}

func (a *senderArtwork) releaseWait() {
	if !a.released {
		close(a.release)
		a.released = true
	}
}

func (s *Sender) Metadata(t *model.Track) error {
	s.mu.Lock()
	if err := s.commandLocked(""); err != nil {
		s.mu.Unlock()
		return err
	}
	var track model.Track
	if t != nil {
		track = *t
		track.Artists = append([]string(nil), t.Artists...)
	}
	key := trackArtworkKey(track)
	a := s.artwork
	if a == nil {
		a = &senderArtwork{changed: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}), release: make(chan struct{})}
		s.artwork = a
		go s.runArtwork(a)
	}
	if a.key != key {
		a.releaseWait()
		a.release, a.released = make(chan struct{}), false
		a.epoch++
		a.sent, a.deliveryErr = false, nil
		if a.cancel != nil {
			a.cancel()
		}
	}
	a.track, a.key = track, key
	// Retain a prepared file across same-album tracks and warm starts. A new
	// ITEMID still needs the image bundled: v0.5.3 drops it on an unbundled item.
	if key.cover == "" || (a.path != "" && a.preparedCover == key.cover) {
		a.sent = true
	}
	if a.suspended {
		s.mu.Unlock()
		return nil
	}
	s.wakeArtworkLocked()
	if a.sent || a.released {
		err := s.flushMetadataLocked()
		s.mu.Unlock()
		return err
	}
	epoch, release := a.epoch, a.release
	s.mu.Unlock()

	// Downloads never hold the control lock. START/Join, stop or a newer item
	// can release this wait early; slow images continue as ARTWORK-only updates.
	timer := time.NewTimer(artworkBundleBudget)
	defer timer.Stop()
	select {
	case <-release:
	case <-timer.C:
	case <-s.ctx.Done():
	case <-s.done:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if a.epoch != epoch || a.suspended || a.stopped {
		return nil
	}
	if a.deliveryErr != nil {
		return a.deliveryErr
	}
	return s.flushMetadataLocked()
}

// Also called immediately before START, so a time-critical start cannot wait
// behind image preparation or trigger cliairplay's placeholder metadata.
func (s *Sender) flushMetadataLocked() error {
	a := s.artwork
	if a == nil || a.suspended || a.stopped {
		return nil
	}
	defer a.releaseWait()
	text := metadataText(a.track)
	replace := !a.textSet || a.text != text
	newItem := !a.textSet || a.item != a.key.item
	path := ""
	if a.preparedCover == a.key.cover && a.key.cover != "" {
		path = a.path
	}
	attach := path != "" && (newItem || a.cover != a.key.cover)
	// An unbundled new item clears retained MRP artwork itself. Only a
	// same-item removal/replacement needs explicit clearing while unavailable.
	clear := !newItem && a.cover != "" && a.cover != a.key.cover && path == ""
	var command string
	if replace {
		bundle := ""
		if attach {
			bundle = path
		}
		command = metadataCommand(a.track, bundle)
	} else if attach {
		command = "ARTWORK=" + path + "\n"
	}
	if clear {
		command += "ARTWORK=\n"
	}
	progress := replace || !a.progressSet || a.duration != a.track.Duration/1000 || a.position != a.track.Position/1000
	if progress && !replace {
		command += progressCommand(a.track)
	}
	if command == "" {
		return nil
	}
	if err := s.commandLocked(command); err != nil {
		a.deliveryErr = err
		return err
	}
	a.deliveryErr = nil
	if newItem || clear {
		a.cover = ""
	}
	if attach {
		a.cover = a.key.cover
	}
	a.text, a.item, a.textSet = text, a.key.item, true
	a.duration, a.position, a.progressSet = a.track.Duration/1000, a.track.Position/1000, true
	if replace || attach || clear {
		kind := "SENDMETA"
		if !replace {
			kind = "ARTWORK"
		}
		s.artworkDiagnostic(fmt.Sprintf("command=%s generation=%d item_changed=%t cover_present=%t attached=%t clear=%t fifo=written", kind, a.epoch, newItem, a.key.cover != "", attach, clear))
	}
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
		a.suspended, a.progressSet = true, false
		a.releaseWait()
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
		a.releaseWait()
		if a.cancel != nil {
			a.cancel()
		}
		close(a.stop)
	}
}

func (s *Sender) artworkDiagnostic(message string) {
	if s.diagnostic != nil {
		s.diagnostic(fmt.Sprintf("AirPlay 封面：member=%s session=%s %s", s.deviceKind, s.id, message))
	}
}

func (s *Sender) runArtwork(a *senderArtwork) {
	var dir string
	files := make(map[[32]byte]string)
	defer func() {
		// FIFO writes don't acknowledge file consumption (UNCHANGED artwork
		// emits no status either). Never evict a handed-off path while the
		// child may still open it. Deduplicate by bytes for this process lifetime.
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
		epoch, source := a.epoch, a.key.cover
		ctx, cancel := context.WithCancel(s.ctx)
		a.cancel = cancel
		s.mu.Unlock()

		data, err := cache.get(ctx, source)
		var path string
		var created bool
		checksum := sha256.Sum256(data)
		if err == nil && ctx.Err() == nil {
			if dir == "" {
				dir, err = os.MkdirTemp(s.runtimeDir, "airplay-artwork-")
				if err == nil {
					dir, err = filepath.Abs(dir)
				}
			}
			if err == nil {
				path = files[checksum]
				if path == "" {
					var file *os.File
					file, err = os.CreateTemp(dir, "cover-*.jpg")
					if err == nil {
						path, created = file.Name(), true
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
			}
			if err != nil {
				err = errors.New("封面临时文件不可用")
			}
		}
		prepared := err == nil && path != ""
		cancel()
		s.mu.Lock()
		current := !s.closed && !a.stopped && !a.suspended && a.epoch == epoch && s.ctx.Err() == nil
		if current {
			// Do not retry downloads on every progress event. FIFO identities
			// are committed separately, only after a successful command write.
			a.sent = true
			if prepared {
				a.path, a.preparedCover = path, source
				size, _ := jpeg.DecodeConfig(bytes.NewReader(data))
				s.artworkDiagnostic(fmt.Sprintf("prepared generation=%d format=jpeg bytes=%d width=%d height=%d file=closed", epoch, len(data), size.Width, size.Height))
			}
			if sendErr := s.flushMetadataLocked(); sendErr != nil && err == nil {
				err = sendErr
			}
		}
		s.mu.Unlock()
		if path != "" {
			if current && prepared {
				// Retain even on FIFO failure: a later control update may retry.
				files[checksum] = path
			} else if created {
				_ = os.Remove(path)
			}
		}
		if current && err != nil {
			s.artworkDiagnostic(err.Error())
		}
	}
}
