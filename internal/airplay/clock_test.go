package airplay

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestClockProbeRequiresProtocolReply(t *testing.T) {
	for _, reply := range []string{"OK peers=0 gm=0123456789abcdef role=grandmaster", "unrelated UDP service"} {
		t.Run(reply, func(t *testing.T) {
			conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				b := make([]byte, 10)
				n, addr, err := conn.ReadFrom(b)
				if err == nil && string(b[:n]) == "?" {
					_, _ = conn.WriteTo([]byte(reply), addr)
				}
			}()
			want := reply != "unrelated UDP service"
			if got := probeClock(conn.LocalAddr().String()); got != want {
				t.Fatal("incorrect daemon readiness", got)
			}
			<-done
		})
	}
}

func TestExistingClockIsReusedWithoutSpawningOrStoppingIt(t *testing.T) {
	c, err := startClock(context.Background(), Config{Binary: "/does-not-exist"}, func() bool { return true })
	if err != nil || c.cancel != nil || c.done != nil {
		t.Fatal("external daemon was not reused")
	}
	c.Close()
	c.Close()
}

func TestOwnedClockWaitsForReadinessAndExitsOnClose(t *testing.T) {
	binary := fakeBinary(t)
	var probes atomic.Int32
	c, err := startClock(context.Background(), Config{Binary: binary}, func() bool { return probes.Add(1) >= 3 })
	if err != nil {
		t.Fatal(err)
	}
	if probes.Load() < 3 {
		t.Fatal("assumed process spawn meant clock readiness")
	}
	c.Close()
	c.Close()
	select {
	case <-c.done:
	default:
		t.Fatal("owned daemon survived Close")
	}
}

func TestClockStartupCancellationAndFailure(t *testing.T) {
	binary := fakeBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if c, err := startClock(ctx, Config{Binary: binary}, func() bool { return false }); err == nil {
		c.Close()
		t.Fatal("cancelled startup succeeded")
	}
	if c, err := startClock(context.Background(), Config{Binary: "/does-not-exist"}, func() bool { return false }); err == nil {
		c.Close()
		t.Fatal("failed binary was accepted")
	}
}
