package audio

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestPacingAndRouteIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audio")
	s, err := NewSource(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var a, b atomic.Int64
	s.Route(44100, func(v []byte) error { a.Add(int64(len(v))); return nil })
	go s.Run(ctx, func(error) {})
	fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	go func() {
		chunk := make([]byte, 1764)
		for ctx.Err() == nil {
			_, _ = unix.Write(fd, chunk)
			time.Sleep(time.Millisecond)
		}
	}()
	time.Sleep(120 * time.Millisecond)
	before := a.Load()
	if before < 1000 || before > 35280 {
		t.Fatalf("FIFO was not paced: %d bytes", before)
	}
	s.Route(44100, func(v []byte) error { b.Add(int64(len(v))); return nil })
	atSwitch := a.Load()
	time.Sleep(60 * time.Millisecond)
	if a.Load() != atSwitch || b.Load() == 0 {
		t.Fatal("old source still received audio after route change")
	}
}
func TestRejectRegularFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "not-a-pipe")
	_ = os.WriteFile(p, []byte("data"), 0600)
	if s, err := NewSource(p); err == nil {
		s.Close()
		t.Fatal("regular file accepted as PCM pipe")
	}
}
