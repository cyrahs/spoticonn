package airplay

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Exercise the real status parser and control writes with a virtual clock.
// A file records the exact wire order without requiring a live audio process.
func controlSender(t *testing.T, volume int) (*Sender, func() string, chan string) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "commands")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	events := make(chan string, 16)
	s := &Sender{input: f, cmdFD: int(f.Fd()), ctx: t.Context(), done: done, volume: volume,
		cancel: func() { close(done) }, status: func(v string) { events <- v }}
	t.Cleanup(s.Close)
	return s, func() string {
		t.Helper()
		b, err := os.ReadFile(f.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}, events
}

func senderStatus(s *Sender, line string) { s.scan(strings.NewReader("[STATUS] " + line + "\n")) }

func startSender(s *Sender) {
	s.Begin()
	senderStatus(s, "audio buffered_ms=10")
}

func startAck(at time.Time) string {
	return fmt.Sprintf("started requested_unix_ms=0 at_unix_ms=%d", at.UnixMilli())
}

func expectSenderEvent(t *testing.T, events chan string, want string) {
	t.Helper()
	select {
	case got := <-events:
		if got != want {
			t.Fatalf("status = %q, want %q", got, want)
		}
	default:
		t.Fatalf("missing status %q", want)
	}
}

func TestStartedVolumeUsesLatestValueAtAudibleTime(t *testing.T) {
	for _, volume := range []int{0, 72} {
		t.Run(fmt.Sprint(volume), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, commands, events := controlSender(t, 30)
				startSender(s)
				ack := startAck(time.Now().Add(time.Second))
				senderStatus(s, ack)
				synctest.Wait()
				if strings.Contains(commands(), "VOLUME") || len(events) != 0 {
					t.Fatal("volume or playing status preceded the scheduled start")
				}
				if err := s.Volume(volume); err != nil {
					t.Fatal(err)
				}
				// Repeated Spotify playing notifications must not cancel an
				// already scheduled volume update when no new PCM start occurs.
				s.Begin()
				time.Sleep(time.Second - time.Millisecond)
				synctest.Wait()
				if strings.Count(commands(), "VOLUME=") != 1 || len(events) != 0 {
					t.Fatal("post-start update ran early")
				}
				time.Sleep(time.Millisecond)
				synctest.Wait()
				want := fmt.Sprintf("START_UNIX_MS=0\nACTION=START\nVOLUME=%d\nVOLUME=%d\n", volume, volume)
				if got := commands(); got != want {
					t.Fatalf("commands = %q, want %q", got, want)
				}
				expectSenderEvent(t, events, "playing")
				senderStatus(s, ack)
				time.Sleep(time.Second)
				synctest.Wait()
				if commands() != want || len(events) != 0 {
					t.Fatal("duplicate started notification reapplied volume")
				}
			})
		})
	}
}

func TestStartedVolumeCannotOverwriteConcurrentMute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, commands, events := controlSender(t, 30)
		startSender(s)
		go func() {
			if err := s.Volume(0); err != nil {
				t.Error(err)
			}
		}()
		senderStatus(s, startAck(time.Now()))
		synctest.Wait()
		if !strings.HasSuffix(commands(), "VOLUME=0\n") {
			t.Fatalf("start overwrote mute: %q", commands())
		}
		expectSenderEvent(t, events, "playing")
	})
}

func flushSender(t *testing.T, s *Sender) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.Flush(context.Background()) }()
	synctest.Wait()
	senderStatus(s, "flushed")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPendingStartVolumeIsCancelled(t *testing.T) {
	for _, action := range []string{"flush", "standby", "close", "cancel", "error", "disconnected", "stopped"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, commands, events := controlSender(t, 30)
				ctx, cancel := context.WithCancel(s.ctx)
				defer cancel()
				s.ctx = ctx
				startSender(s)
				ack := startAck(time.Now().Add(time.Second))
				senderStatus(s, ack)
				switch action {
				case "flush":
					flushSender(t, s)
				case "standby":
					if err := s.Standby(); err != nil {
						t.Fatal(err)
					}
				case "close":
					s.Close()
				case "cancel":
					cancel()
				default:
					senderStatus(s, action)
					want := action
					if action == "stopped" {
						want = "disconnected"
					}
					expectSenderEvent(t, events, want)
				}
				before := commands()
				senderStatus(s, ack)
				time.Sleep(2 * time.Second)
				synctest.Wait()
				if commands() != before || len(events) != 0 {
					t.Fatalf("%s allowed a late volume update or playing notification", action)
				}
			})
		})
	}
}

func TestLateStartAckCannotApplyVolumeToNewStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, commands, events := controlSender(t, 30)
		startSender(s)
		oldAck := startAck(time.Now().Add(time.Second))
		flushSender(t, s)
		startSender(s)
		senderStatus(s, oldAck)
		senderStatus(s, oldAck) // duplicate must not consume the new start's ack
		senderStatus(s, startAck(time.Now().Add(2*time.Second)))
		time.Sleep(time.Second)
		synctest.Wait()
		if strings.Contains(commands(), "VOLUME") || len(events) != 0 {
			t.Fatal("superseded ack applied volume to the new start")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if strings.Count(commands(), "VOLUME=30\n") != 1 {
			t.Fatalf("new start did not reapply volume once: %q", commands())
		}
		expectSenderEvent(t, events, "playing")
	})
}

func TestNewStartCancelsPreviousVolumeTimer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s, commands, events := controlSender(t, 30)
		startSender(s)
		senderStatus(s, startAck(time.Now().Add(time.Second)))
		startSender(s)
		senderStatus(s, startAck(time.Now().Add(2*time.Second)))
		time.Sleep(time.Second)
		synctest.Wait()
		if strings.Contains(commands(), "VOLUME") || len(events) != 0 {
			t.Fatal("old timer survived a new start")
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if strings.Count(commands(), "VOLUME=30\n") != 1 {
			t.Fatalf("new start did not reapply volume once: %q", commands())
		}
		expectSenderEvent(t, events, "playing")
	})
}

func TestStartedVolumeFailureDoesNotReportPlaying(t *testing.T) {
	for _, failure := range []string{"pipe", "missing_time", "invalid_time", "overflow_time"} {
		t.Run(failure, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, commands, events := controlSender(t, 30)
				startSender(s)
				switch failure {
				case "pipe":
					s.mu.Lock()
					s.cmdFD = -1
					s.mu.Unlock()
					senderStatus(s, startAck(time.Now().Add(time.Second)))
				case "missing_time":
					senderStatus(s, "started requested_unix_ms=0")
				case "invalid_time":
					senderStatus(s, "started at_unix_ms=invalid")
				case "overflow_time":
					senderStatus(s, "started at_unix_ms=9223372036854775808")
				}
				time.Sleep(2 * time.Second)
				synctest.Wait()
				expectSenderEvent(t, events, "error")
				if strings.Contains(commands(), "VOLUME") || len(events) != 0 {
					t.Fatal("failed start retried volume or reported playing")
				}
			})
		})
	}
}
