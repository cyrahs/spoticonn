package airplay

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"spoticonn/internal/model"
)

func groupLogs(g *groupOutput) <-chan string {
	logs := make(chan string, 100)
	g.diagnostic = func(s string) { logs <- s }
	return logs
}

func waitLog(t *testing.T, logs <-chan string, parts ...string) string {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line := <-logs:
			matches := true
			for _, p := range parts {
				matches = matches && strings.Contains(line, p)
			}
			if matches {
				return line
			}
		case <-timer.C:
			t.Fatalf("missing diagnostic %v", parts)
		}
	}
}

func TestGroupFailureEventsPreserveMemberAndPhase(t *testing.T) {
	for _, role := range []string{"homepod", "apple_tv"} {
		for _, phase := range []string{"connect", "join", "prime", "stream"} {
			for _, reason := range []string{"timeline_changed", "disconnected", "clock_stalled", "error"} {
				t.Run(role+"/"+phase+"/"+reason, func(t *testing.T) {
					g, p, events := testGroup(t)
					logs := groupLogs(g)
					tv := &testMember{anchor: p.anchor + 10}
					if phase == "join" {
						tv.joinGate = make(chan struct{})
					}
					if phase == "prime" {
						tv.writeGate = make(chan struct{})
					}
					inject := func() {
						if role == "homepod" {
							g.primaryStatus(reason)
						} else {
							tv.callback(reason)
						}
					}
					g.open = func(ctx context.Context, _ Config, _ model.Device, _ model.PairingSecret, _, _ int, cb func(string)) (timedOutput, error) {
						tv.ctx, tv.callback = ctx, cb
						if phase == "connect" {
							inject()
						}
						return tv, nil
					}
					close(p.ack)
					g.Begin()
					_ = g.Write(make([]byte, 3528))
					if phase == "stream" {
						waitGroup(t, events, "group_joined")
					}
					if phase != "connect" {
						eventually(t, func() bool {
							g.mu.Lock()
							defer g.mu.Unlock()
							return g.cycle.attempt != nil && g.cycle.attempt.phase == phase
						})
						inject()
					}
					state := "group_degraded_" + reason
					if role == "homepod" {
						state = "group_degraded_homepod_" + reason
					}
					waitGroup(t, events, state)
					waitLog(t, logs, "role="+role, "phase="+phase, "reason="+reason, "retry=false")
					g.mu.Lock()
					done := g.cycle.done
					g.mu.Unlock()
					<-done
					tv.mu.Lock()
					closes := tv.closes
					tv.mu.Unlock()
					if closes != 1 {
						t.Fatal("failed physical session not closed")
					}
					p.mu.Lock()
					defer p.mu.Unlock()
					if p.starts != 1 || p.flushes != 0 || p.closes != 0 {
						t.Fatal("member event interrupted primary")
					}
				})
			}
		}
	}
}

// Exercise the full worker/feed path, including a blocked prime with a full
// queue. A replacement receives only audio at its newly acknowledged position.
func TestGroupRecoverableFailuresRejoinWithoutRestartingHomePod(t *testing.T) {
	for _, reason := range []string{"disconnected", "write_timeout", "pipe_closed", "backpressure"} {
		t.Run(reason, func(t *testing.T) {
			g, p, events := testGroup(t)
			g.retryDelays = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
			logs := groupLogs(g)
			var active, opens atomic.Int32
			members := make(chan *countedMember, 3)
			g.open = func(ctx context.Context, _ Config, _ model.Device, _ model.PairingSecret, _, _ int, cb func(string)) (timedOutput, error) {
				if active.Add(1) != 1 {
					t.Error("overlapping member sessions")
				}
				n := opens.Add(1)
				tv := &countedMember{testMember: testMember{ctx: ctx, callback: cb, anchor: p.anchor + 10}, active: &active}
				if n == 1 {
					switch reason {
					case "write_timeout":
						tv.writeErr = os.ErrDeadlineExceeded
					case "pipe_closed":
						tv.writeErr = os.ErrClosed
					case "backpressure":
						tv.writeGate = make(chan struct{})
					}
				} else {
					g.mu.Lock()
					tv.anchor = p.anchor + (g.total*1000+int64(g.rate*4)-1)/int64(g.rate*4)
					g.mu.Unlock()
				}
				members <- tv
				return tv, nil
			}
			close(p.ack)
			g.Begin()
			_ = g.Write(bytes.Repeat([]byte{1}, 3528))
			first := <-members
			if reason == "disconnected" {
				waitGroup(t, events, "group_joined")
				first.callback("disconnected")
			}
			if reason == "backpressure" {
				eventually(t, func() bool {
					g.mu.Lock()
					defer g.mu.Unlock()
					return g.cycle.feeding && g.cycle.attempt.phase == "prime"
				})
				for i := 0; i < 140; i++ {
					if err := g.Write(bytes.Repeat([]byte{2}, 1764)); err != nil {
						t.Fatal(err)
					}
				}
			}
			waitGroup(t, events, "group_retrying_"+reason)
			line := waitLog(t, logs, "reason="+reason, "retry=true", "attempt=1")
			if reason == "backpressure" && (!strings.Contains(line, "queue=128") || !strings.Contains(line, "phase=prime")) {
				t.Fatal(line)
			}
			// Even after backoff, no replacement is opened until new PCM arrives.
			time.Sleep(30 * time.Millisecond)
			if opens.Load() != 1 || active.Load() != 0 {
				t.Fatal("retry did not wait for new audio/old Close")
			}
			eventually(t, func() bool { _ = g.Write(bytes.Repeat([]byte{9}, 1764)); return opens.Load() == 2 })
			second := <-members
			waitGroup(t, events, "group_joined")
			eventually(t, func() bool {
				_ = g.Write(bytes.Repeat([]byte{9}, 1764))
				second.mu.Lock()
				defer second.mu.Unlock()
				return len(second.data) > 0
			})
			// Late events from the retired process must not cancel the new attempt.
			first.callback("timeline_changed")
			first.callback("disconnected")
			second.mu.Lock()
			if !bytes.Equal(second.data, bytes.Repeat([]byte{9}, len(second.data))) || second.requested <= first.requested {
				t.Error("replayed stale PCM or join request")
			}
			second.mu.Unlock()
			if second.ctx.Err() != nil {
				t.Fatal("retired callback cancelled replacement")
			}
			p.mu.Lock()
			if p.starts != 1 || p.flushes != 0 || p.closes != 0 {
				t.Error("retry interrupted HomePod")
			}
			p.mu.Unlock()
		})
	}
}

func TestGroupRetryBudgetDoesNotResetAfterSuccessfulJoin(t *testing.T) {
	g, p, events := testGroup(t)
	g.retryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	var opens atomic.Int32
	members := make(chan *testMember, 4)
	g.open = func(ctx context.Context, _ Config, _ model.Device, _ model.PairingSecret, _, _ int, cb func(string)) (timedOutput, error) {
		opens.Add(1)
		tv := &testMember{ctx: ctx, anchor: p.anchor + 10, callback: cb}
		members <- tv
		return tv, nil
	}
	close(p.ack)
	g.Begin()
	_ = g.Write(make([]byte, 3528))
	for i := 1; i <= 3; i++ {
		tv := <-members
		waitGroup(t, events, "group_joined")
		tv.callback("disconnected")
		if i < 3 {
			waitGroup(t, events, "group_retrying_disconnected")
			eventually(t, func() bool { _ = g.Write(make([]byte, 1764)); return opens.Load() == int32(i+1) })
		} else {
			waitGroup(t, events, "group_degraded_disconnected")
		}
	}
	g.mu.Lock()
	done := g.cycle.done
	g.mu.Unlock()
	<-done
	for i := 0; i < 10; i++ {
		g.Begin()
		_ = g.Write(make([]byte, 1764))
	}
	if opens.Load() != 3 {
		t.Fatal("exhausted retry budget restarted")
	}
}

func TestGroupIntentCancelsRetryAndManualResumeRejoins(t *testing.T) {
	for _, phase := range []string{"backoff", "opening", "joining", "prime", "joined"} {
		t.Run(phase, func(t *testing.T) {
			g, p, events := testGroup(t)
			g.retryDelays = []time.Duration{20 * time.Millisecond}
			var opens, active atomic.Int32
			members := make(chan *countedMember, 5)
			g.open = func(ctx context.Context, _ Config, _ model.Device, _ model.PairingSecret, _, _ int, cb func(string)) (timedOutput, error) {
				n := opens.Add(1)
				if active.Add(1) != 1 {
					t.Error("overlapping sessions")
				}
				tv := &countedMember{testMember: testMember{ctx: ctx, callback: cb, anchor: p.anchor + 10}, active: &active}
				if n == 2 {
					if phase == "joining" {
						tv.joinGate = make(chan struct{})
					}
					if phase == "prime" {
						tv.writeGate = make(chan struct{})
					}
				}
				members <- tv
				if n == 2 && phase == "opening" {
					<-ctx.Done()
					active.Add(-1)
					return nil, ctx.Err()
				}
				return tv, nil
			}
			close(p.ack)
			intent, cancel := context.WithCancel(context.Background())
			defer cancel()
			g.BeginWithContext(intent)
			_ = g.Write(make([]byte, 3528))
			first := <-members
			waitGroup(t, events, "group_joined")
			first.callback("disconnected")
			waitGroup(t, events, "group_retrying_disconnected")
			if phase != "backoff" {
				eventually(t, func() bool { _ = g.Write(make([]byte, 1764)); return opens.Load() == 2 })
				<-members
				want := map[string]string{"opening": "connect", "joining": "join", "prime": "prime", "joined": "stream"}[phase]
				eventually(t, func() bool {
					g.mu.Lock()
					defer g.mu.Unlock()
					return g.cycle.attempt != nil && g.cycle.attempt.phase == want
				})
			}
			cancel() // no Spotify command, Flush or lifecycle lock is required to cancel
			g.mu.Lock()
			done := g.cycle.done
			g.mu.Unlock()
			<-done
			if active.Load() != 0 {
				t.Fatal("cancelled member survived")
			}
			before := opens.Load()
			time.Sleep(30 * time.Millisecond)
			g.BeginWithContext(intent)
			_ = g.Write(make([]byte, 3528))
			if opens.Load() != before {
				t.Fatal("cancelled intent reopened a member")
			}
			for len(events) > 0 {
				<-events
			}
			// Existing pause/continue recovery creates a fresh cycle and timeline.
			if err := g.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			// The phase-specific blocker only belongs to the retired operation.
			opens.Store(10)
			g.BeginWithContext(context.Background())
			_ = g.Write(make([]byte, 3528))
			waitGroup(t, events, "group_joined")
			if active.Load() != 1 {
				t.Fatal("manual resume failed to join")
			}
		})
	}
}

func TestHomePodTimelineChangeDuringBackoffPreventsRetry(t *testing.T) {
	g, p, events := testGroup(t)
	g.retryDelays = []time.Duration{10 * time.Millisecond}
	logs := groupLogs(g)
	var opens atomic.Int32
	var tv *testMember
	g.open = func(ctx context.Context, _ Config, _ model.Device, _ model.PairingSecret, _, _ int, cb func(string)) (timedOutput, error) {
		opens.Add(1)
		tv = &testMember{ctx: ctx, anchor: p.anchor + 10, callback: cb}
		return tv, nil
	}
	close(p.ack)
	g.Begin()
	_ = g.Write(make([]byte, 3528))
	waitGroup(t, events, "group_joined")
	tv.callback("disconnected")
	waitGroup(t, events, "group_retrying_disconnected")
	g.primaryStatus("timeline_changed")
	waitGroup(t, events, "group_degraded_homepod_timeline_changed")
	waitLog(t, logs, "role=homepod", "reason=timeline_changed", "retry=false")
	if opens.Load() != 1 {
		t.Fatal("reused invalid primary timeline")
	}
}

func TestGroupStreamWriteFailureKeepsErrorClass(t *testing.T) {
	for _, reason := range []string{"write_timeout", "pipe_closed"} {
		t.Run(reason, func(t *testing.T) {
			g, p, events := testGroup(t)
			logs := groupLogs(g)
			tv := &testMember{anchor: p.anchor + 10}
			g.open = func(ctx context.Context, _ Config, _ model.Device, _ model.PairingSecret, _, _ int, cb func(string)) (timedOutput, error) {
				tv.ctx, tv.callback = ctx, cb
				return tv, nil
			}
			close(p.ack)
			g.Begin()
			_ = g.Write(make([]byte, 3528))
			waitGroup(t, events, "group_joined")
			tv.mu.Lock()
			tv.writeErr = &audioWriteError{reason: reason}
			tv.mu.Unlock()
			_ = g.Write(make([]byte, 1764))
			waitGroup(t, events, "group_degraded_"+reason)
			waitLog(t, logs, "phase=stream", "reason="+reason, "prime_bytes=1764", "elapsed_ms=")
		})
	}
}

func TestGroupDiagnosticsDoNotLeakUnknownEvents(t *testing.T) {
	g, p, events := testGroup(t)
	logs := groupLogs(g)
	tv := &testMember{anchor: p.anchor + 10}
	g.open = func(ctx context.Context, _ Config, _ model.Device, _ model.PairingSecret, _, _ int, cb func(string)) (timedOutput, error) {
		tv.ctx, tv.callback = ctx, cb
		return tv, nil
	}
	close(p.ack)
	g.Begin()
	_ = g.Write(make([]byte, 3528))
	waitGroup(t, events, "group_joined")
	tv.callback("--auth tv-secret RAW_AUDIO")
	tv.mu.Lock()
	tv.writeErr = fmt.Errorf("private path --auth tv-secret RAW_AUDIO")
	tv.mu.Unlock()
	_ = g.Write(make([]byte, 1764))
	waitGroup(t, events, "group_degraded_audio")
	g.CancelJoin()
	for len(logs) > 0 {
		line := <-logs
		for _, private := range []string{"--auth", "tv-secret", "RAW_AUDIO", "private path"} {
			if strings.Contains(line, private) {
				t.Fatal("private detail leaked")
			}
		}
	}
}

func TestHomePodTimelineChangeBeforeStartDoesNotWaitOrRestart(t *testing.T) {
	g, _, events := testGroup(t)
	logs := groupLogs(g)
	g.open = func(context.Context, Config, model.Device, model.PairingSecret, int, int, func(string)) (timedOutput, error) {
		t.Error("invalid timeline opened TV")
		return nil, fmt.Errorf("unexpected open")
	}
	g.Begin()
	g.primaryStatus("timeline_changed")
	waitGroup(t, events, "group_degraded_homepod_timeline_changed")
	waitLog(t, logs, "role=homepod", "phase=start", "reason=timeline_changed")
	for len(events) > 0 {
		if <-events == "error" {
			t.Fatal("timeline change interrupted primary")
		}
	}
}

func TestTypedAuthenticationErrorOnlyRefinesAppleTVFailure(t *testing.T) {
	for _, role := range []string{"homepod", "apple_tv"} {
		t.Run(role, func(t *testing.T) {
			g, p, events := testGroup(t)
			logs := groupLogs(g)
			g.open = func(_ context.Context, _ Config, _ model.Device, _ model.PairingSecret, _, _ int, cb func(string)) (timedOutput, error) {
				if role == "homepod" {
					g.primaryStatus("error")
				} else {
					cb("error")
				}
				return nil, ErrAuthFailed
			}
			close(p.ack)
			g.Begin()
			_ = g.Write(make([]byte, 3528))
			state, reason := "group_degraded_auth", "auth"
			if role == "homepod" {
				state, reason = "group_degraded_homepod_error", "error"
			}
			waitGroup(t, events, state)
			waitLog(t, logs, "role="+role, "reason="+reason, "phase=connect")
		})
	}
}
