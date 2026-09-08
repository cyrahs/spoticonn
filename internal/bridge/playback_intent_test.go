package bridge

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"spoticonn/internal/airplay"
	"spoticonn/internal/model"
	"spoticonn/internal/spotify"
)

func externalPause(m *Manager, p *fakePlayer, id, kind string, generation uint64) {
	p.mu.Lock()
	p.state.Paused = true
	p.mu.Unlock()
	m.emit(event{kind: "spotify", id: id, generation: generation, spotify: spotify.Event{Type: kind}})
}

func drainEvents(m *Manager) {
	for {
		events := m.takeEvents()
		if len(events) == 0 {
			return
		}
		for _, e := range events {
			m.handle(e)
		}
	}
}

func assertCommands(t *testing.T, p *fakePlayer, want ...string) {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if !slices.Equal(p.commands, want) {
		t.Fatalf("commands = %v, want %v", p.commands, want)
	}
}

func assertPaused(t *testing.T, m *Manager, p *fakePlayer) {
	t.Helper()
	status, _ := p.Status(context.Background())
	v := m.Snapshot().Playback
	if !status.Paused || v.Status != "paused" || v.Recovering || m.outputRetryAccount != "" || p.forward != nil {
		t.Fatalf("pause did not persist: player=%+v playback=%+v", status, v)
	}
}

func TestSpotifyPauseCancelsRecoveryBeforeMailboxIsHandled(t *testing.T) {
	for _, kind := range []string{"paused", "inactive", "stopped", "not_playing"} {
		t.Run(kind, func(t *testing.T) {
			m, a, _, outputs := setup(t)
			m.handle(playing("a", 1))
			m.failOutput("test failure")
			externalPause(m, a, "a", kind, 1)
			// The ticker may win the select before the queued user event.
			m.outputRetryAt = time.Now().Add(-time.Second)
			m.reconcile()
			drainEvents(m)
			assertPaused(t, m, a)
			assertCommands(t, a, "pause", "volume", "seek", "resume", "pause")
			if len(*outputs) != 1 {
				t.Fatal("cancelled recovery reopened output")
			}
		})
	}
}

func TestUserPauseRemainsPausedWhenOutputFails(t *testing.T) {
	for _, source := range []string{"spotify", "web"} {
		for _, failure := range []string{"none", "flush", "standby", "disconnect"} {
			t.Run(source+"/"+failure, func(t *testing.T) {
				m, a, _, outputs := setup(t)
				m.handle(playing("a", 1))
				o := (*outputs)[0]
				if failure == "flush" {
					o.flushErr = errors.New("test flush failure")
				}
				if failure == "standby" {
					o.standbyErr = errors.New("test standby failure")
				}
				a.commands = nil
				if source == "web" {
					_ = m.Playback(context.Background(), "pause", 0)
					// The web action must silence audio without an event-loop turn.
					assertPaused(t, m, a)
				} else {
					externalPause(m, a, "a", "paused", 1)
				}
				drainEvents(m)
				if failure == "disconnect" {
					m.handle(event{kind: "output", generation: m.outputGeneration, status: "disconnected"})
				}
				for i := 0; i < 3; i++ {
					m.outputRetryAt = time.Now().Add(-time.Minute)
					m.reconcile()
				}
				a.write("must not play")
				assertPaused(t, m, a)
				if source == "web" {
					assertCommands(t, a, "pause")
				} else {
					assertCommands(t, a)
				}
				if len(*outputs) != 1 || o.data != "" {
					t.Fatal("pause restarted output or retained PCM")
				}
			})
		}
	}
}

func TestInternalPauseEchoKeepsLegitimateRecovery(t *testing.T) {
	m, a, _, outputs := setup(t)
	m.handle(playing("a", 1))
	m.failOutput("test failure")
	drainEvents(m)
	if !m.Snapshot().Playback.Recovering {
		t.Fatal("internal pause acknowledgement cancelled recovery")
	}
	m.outputRetryAt = time.Now().Add(-time.Second)
	m.reconcile()
	drainEvents(m)
	assertCommands(t, a, "pause", "volume", "seek", "resume", "pause", "volume", "seek", "resume")
	a.write("recovered")
	if len(*outputs) != 2 || (*outputs)[1].data != "recovered" || a.state.Paused {
		t.Fatal("legitimate recovery lost playback")
	}
}

func TestPauseDuringOutputOpenRejectsLateCompletion(t *testing.T) {
	for _, group := range []bool{false, true} {
		for _, source := range []string{"spotify", "web"} {
			t.Run(source+"/group="+map[bool]string{false: "false", true: "true"}[group], func(t *testing.T) {
				m, a, _, _ := setup(t)
				m.handle(playing("a", 1))
				m.failOutput("test failure")
				if group {
					setupGroup(t, m)
					m.outputRetryRequest = m.playbackRequest("a")
				}
				started, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
				o := &fakeOutput{}
				open := func(ctx context.Context) (Output, error) {
					close(started)
					<-ctx.Done()
					close(cancelled)
					<-release // simulate an opener returning success after cancellation
					return o, nil
				}
				m.cfg.OpenOutput = func(ctx context.Context, _ airplay.Config, _ model.Device, _ model.PairingSecret, _, _ int, _ func(string)) (Output, error) {
					return open(ctx)
				}
				m.cfg.OpenGroupOutput = func(ctx context.Context, _ airplay.Config, _ airplay.Target, _ map[string]model.PairingSecret, _, _ int, _, _ func(string)) (Output, error) {
					return open(ctx)
				}
				done := make(chan struct{})
				go func() {
					m.op.Lock()
					defer m.op.Unlock()
					defer close(done)
					m.outputRetryAt = time.Now().Add(-time.Second)
					m.reconcile()
				}()
				awaitSignal(t, started)
				webDone := make(chan error, 1)
				if source == "spotify" {
					externalPause(m, a, "a", "paused", 1)
				} else {
					go func() { webDone <- m.Playback(context.Background(), "pause", 0) }()
				}
				awaitSignal(t, cancelled)
				close(release)
				awaitSignal(t, done)
				if source == "web" {
					if err := <-webDone; err != nil {
						t.Fatal(err)
					}
				}
				drainEvents(m)
				assertPaused(t, m, a)
				want := []string{"pause", "volume", "seek", "resume", "pause"}
				if source == "web" {
					want = append(want, "pause")
				}
				assertCommands(t, a, want...)
				if !o.closed || o.starts != 0 {
					t.Fatal("late output completion started a cancelled session")
				}
			})
		}
	}
}

func awaitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not reach expected boundary")
	}
}

func TestPauseBeforeResumeCommandInvalidatesConnection(t *testing.T) {
	m, a, _, outputs := setup(t)
	onCommand := a.onCommand
	a.onCommand = func(action string) {
		onCommand(action)
		if action == "seek" {
			externalPause(m, a, "a", "paused", 1)
		}
	}
	m.handle(playing("a", 1))
	drainEvents(m)
	assertPaused(t, m, a)
	assertCommands(t, a, "pause", "volume", "seek")
	if !(*outputs)[0].closed || (*outputs)[0].starts != 0 {
		t.Fatal("output began after user pause during setup")
	}
}

func TestPauseCancelsGroupJoinAndIgnoresLateCallbacks(t *testing.T) {
	m, a, _, _ := setup(t)
	setupGroup(t, m)
	o := &cancellableOutput{}
	var callback func(string)
	m.cfg.OpenGroupOutput = func(_ context.Context, _ airplay.Config, _ airplay.Target, _ map[string]model.PairingSecret, _, _ int, cb, _ func(string)) (Output, error) {
		callback = cb
		return o, nil
	}
	m.handle(playing("a", 1))
	callback("group_joining")
	externalPause(m, a, "a", "paused", 1)
	drainEvents(m)
	callback("group_joined")
	callback("playing")
	callback("disconnected")
	drainEvents(m)
	m.outputRetryAt = time.Now().Add(-time.Minute)
	m.reconcile()
	assertPaused(t, m, a)
	assertCommands(t, a, "pause", "volume", "seek", "resume")
	if o.cancellations != 1 {
		t.Fatal("Spotify pause did not cancel the group join")
	}
}

func TestRapidPauseResumeDoesNotRunOldRecovery(t *testing.T) {
	m, a, _, outputs := setup(t)
	m.handle(playing("a", 1))
	m.failOutput("test failure")
	oldRetry := m.outputRetryRequest
	externalPause(m, a, "a", "paused", 1)
	if err := m.Playback(context.Background(), "resume", 0); err != nil {
		t.Fatal(err)
	}
	m.emit(playing("a", 1))
	drainEvents(m)
	if m.requestCurrent(oldRetry) || len(*outputs) != 2 || a.state.Paused {
		t.Fatal("explicit resume reused the old recovery or was lost")
	}
	assertCommands(t, a, "pause", "volume", "seek", "resume", "pause", "resume", "pause", "volume", "seek", "resume")
}

func TestRecoveryCannotControlReplacementSessionOrTarget(t *testing.T) {
	for _, change := range []string{"account", "generation", "target"} {
		t.Run(change, func(t *testing.T) {
			m, a, b, outputs := setup(t)
			m.handle(playing("a", 1))
			m.failOutput("test failure")
			oldRetry := m.outputRetryRequest
			oldOutputGeneration := m.outputGeneration - 1
			switch change {
			case "account":
				m.handle(playing("b", 1))
			case "generation":
				m.stopAccount("a")
				m.accounts["a"] = &runtimeAccount{player: b, generation: 3}
				b.onCommand = func(action string) {
					if action == "pause" {
						m.emit(event{kind: "spotify", id: "a", generation: 3, spotify: spotify.Event{Type: "paused"}})
					}
				}
				m.handle(playing("a", 3))
			case "target":
				m.devices["other"] = model.Device{ID: "other", Address: "192.0.2.2", Port: 7000}
				s := m.store.Snapshot().Settings
				s.TargetID = "other"
				if err := m.UpdateSettings(context.Background(), s); err != nil {
					t.Fatal(err)
				}
			}
			a.commands, b.commands = nil, nil
			count := len(*outputs)
			m.outputRetryAccount, m.outputRetryRequest = "a", oldRetry
			m.outputRetryAt = time.Now().Add(-time.Second)
			m.handle(event{kind: "output", generation: oldOutputGeneration, status: "disconnected"})
			m.reconcile()
			drainEvents(m)
			assertCommands(t, a)
			assertCommands(t, b)
			if len(*outputs) != count || m.Snapshot().Playback.Recovering {
				t.Fatal("old recovery affected replacement session")
			}
		})
	}
}

func TestUnconfirmedInternalPauseDoesNotConsumeFutureUserPause(t *testing.T) {
	m, a, _, _ := setup(t)
	m.handle(playing("a", 1))
	a.onCommand = nil // disconnected event stream
	m.failOutput("test failure")
	if m.Snapshot().Playback.Recovering || m.accounts["a"].intent.pauseAck != nil {
		t.Fatal("unconfirmed internal pause left automatic resume enabled")
	}
	externalPause(m, a, "a", "paused", 1)
	drainEvents(m)
	assertPaused(t, m, a)
	assertCommands(t, a, "pause", "volume", "seek", "resume", "pause")
}

func TestUserPauseBesideInternalConfirmationIsNotCoalescedAway(t *testing.T) {
	for _, userFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "user_first", false: "echo_first"}[userFirst], func(t *testing.T) {
			m, a, _, outputs := setup(t)
			m.handle(playing("a", 1))
			original := a.onCommand
			a.onCommand = func(action string) {
				if action != "pause" {
					return
				}
				if userFirst {
					externalPause(m, a, "a", "paused", 1)
				}
				original(action)
				if !userFirst {
					externalPause(m, a, "a", "paused", 1)
				}
			}
			m.failOutput("test failure")
			drainEvents(m)
			m.outputRetryAt = time.Now().Add(-time.Minute)
			m.reconcile()
			assertPaused(t, m, a)
			assertCommands(t, a, "pause", "volume", "seek", "resume", "pause")
			if len(*outputs) != 1 {
				t.Fatal("user pause was consumed as the internal confirmation")
			}
		})
	}
}

func TestFailedWebPauseStillIsolatesOutputAndCancelsRecovery(t *testing.T) {
	m, a, _, outputs := setup(t)
	m.handle(playing("a", 1))
	a.commandErr = map[string]error{"pause": errors.New("test command failure")}
	if err := m.Playback(context.Background(), "pause", 0); err == nil {
		t.Fatal("web pause hid command failure")
	}
	a.write("must not reach receiver")
	m.outputRetryAt = time.Now().Add(-time.Minute)
	m.reconcile()
	assertCommands(t, a, "pause", "volume", "seek", "resume", "pause")
	if a.forward != nil || !(*outputs)[0].closed || m.Snapshot().Playback.Recovering || len(*outputs) != 1 {
		t.Fatal("failed web pause left audio or recovery active")
	}
}

func TestPlaybackDiagnosticsContainControlTimelineWithoutTrackOrCredentials(t *testing.T) {
	m, a, _, _ := setup(t)
	a.state.Track.URI, a.state.Username = "PRIVATE-TRACK", "PRIVATE-USER"
	m.handle(playing("a", 1))
	m.failOutput("test failure")
	externalPause(m, a, "a", "paused", 1)
	drainEvents(m)
	var lines []string
	for _, d := range m.Snapshot().Diagnostics {
		lines = append(lines, d.Message)
	}
	text := strings.Join(lines, "\n")
	for _, want := range []string{"内部 resume", "安排输出恢复", "取消输出恢复", "Spotify paused", "paused=true"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing timeline entry %q: %s", want, text)
		}
	}
	if strings.Contains(text, "PRIVATE-") {
		t.Fatal("diagnostics exposed private playback data")
	}
}
