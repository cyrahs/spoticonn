// Package audio provides a bounded, paced PCM boundary around librespot's
// unpaced FIFO output. It must never drain a playing track at disk speed.
package audio

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type Source struct {
	mu      sync.Mutex
	fd      int
	closed  bool
	path    string
	forward func([]byte) error
	rate    int
}

func NewSource(path string) (*Source, error) {
	if err := unix.Mkfifo(path, 0600); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeNamedPipe == 0 {
		return nil, errors.New("audio path is not a FIFO")
	}
	// RDWR keeps the read end alive between tracks and makes cancellation nonblocking.
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	return &Source{fd: fd, path: path, rate: 44100}, nil
}

// Route serializes changes against writes. Once it returns no write can reach
// the previous sink. The callback must have a finite write deadline.
func (s *Source) Route(rate int, forward func([]byte) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rate == 44100 || rate == 48000 {
		s.rate = rate
	}
	s.forward = forward
}

func (s *Source) Drain() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	b := make([]byte, 8192)
	// Bound work even if a faulty producer ignores pause.
	for n := 0; n < 128; n++ {
		count, err := unix.Read(s.fd, b)
		if count <= 0 || err != nil {
			break
		}
	}
}

func (s *Source) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return unix.Close(s.fd)
}

func (s *Source) Run(ctx context.Context, onError func(error)) {
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	buf := make([]byte, 9600) // at most 50 ms, including scheduler catch-up
	last := time.Now()
	credit := 0.0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return
		}
		// A bounded token bucket catches up after short scheduling delays,
		// without ever draining a whole track after a long stall or pause.
		now := time.Now()
		credit = min(credit+now.Sub(last).Seconds()*float64(s.rate*4), float64(s.rate*4)/20)
		last = now
		budget := int(credit) / 4 * 4
		if budget < 4 {
			s.mu.Unlock()
			continue
		}
		n, err := unix.Read(s.fd, buf[:budget])
		if n > 0 {
			credit -= float64(n)
		} else {
			credit = 0
		}
		if n > 0 && s.forward != nil {
			err = s.forward(buf[:n])
		}
		s.mu.Unlock()
		if err != nil && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, io.EOF) {
			onError(err)
			return
		}
	}
}
