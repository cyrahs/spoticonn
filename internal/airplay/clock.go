package airplay

import (
	"context"
	"errors"
	"io"
	"net"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A shared daemon owns UDP 319/320 once for all members. The v0.5.3 control
// reply is sent after the first shared-memory sample is published. Merely
// spawning the daemon (or seeing its startup log) is not readiness.
type sharedClock struct {
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func clockAlive() bool {
	return probeClock("127.0.0.1:9010")
}

func probeClock(address string) bool {
	c, err := net.DialTimeout("udp4", address, 100*time.Millisecond)
	if err != nil {
		return false
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err = c.Write([]byte("?")); err != nil {
		return false
	}
	b := make([]byte, 128)
	n, err := c.Read(b)
	return err == nil && strings.HasPrefix(string(b[:n]), "OK peers=") && strings.Contains(string(b[:n]), " gm=")
}

func startSharedClock(ctx context.Context, cfg Config) (*sharedClock, error) {
	return startClock(ctx, cfg, clockAlive)
}

func startClock(ctx context.Context, cfg Config, probe func() bool) (*sharedClock, error) {
	if probe() {
		return &sharedClock{}, nil
	} // Do not stop a daemon owned by another service.
	child, cancel := context.WithCancel(ctx)
	args := []string{"--ptp-daemon"}
	if cfg.InterfaceIP != "" {
		args = append(args, "--if", cfg.InterfaceIP)
	}
	cmd := exec.CommandContext(child, cfg.Binary, args...)
	configureCapabilities(cmd)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.Cancel = func() error {
		err := cmd.Process.Signal(syscall.SIGTERM)
		time.AfterFunc(2*time.Second, func() { _ = cmd.Process.Kill() })
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, errors.New("共享 AirPlay 时钟启动失败")
	}
	clock := &sharedClock{cancel: cancel, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(clock.done) }()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-clock.done:
			clock.Close()
			return nil, errors.New("共享 AirPlay 时钟不可用，请检查 UDP 319/320")
		case <-ctx.Done():
			clock.Close()
			return nil, ctx.Err()
		case <-deadline.C:
			clock.Close()
			return nil, errors.New("共享 AirPlay 时钟就绪超时")
		case <-tick.C:
			if probe() {
				return clock, nil
			}
		}
	}
}

func (c *sharedClock) Close() {
	c.once.Do(func() {
		if c.cancel != nil {
			c.cancel()
			<-c.done
		}
	})
}
