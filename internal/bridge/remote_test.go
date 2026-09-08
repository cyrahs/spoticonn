package bridge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"spoticonn/internal/airplay"
	"spoticonn/internal/model"
	"spoticonn/internal/spotify"
)

func captureRemote(t *testing.T, m *Manager) func(airplay.RemoteCommand) {
	t.Helper()
	open := m.cfg.OpenOutput
	var callback func(airplay.RemoteCommand)
	m.cfg.OpenOutput = func(ctx context.Context, cfg airplay.Config, d model.Device, pair model.PairingSecret, vol, rate int, cb func(string)) (Output, error) {
		callback = cfg.RemoteControl
		return open(ctx, cfg, d, pair, vol, rate, cb)
	}
	m.handle(playing("a", 1))
	if callback == nil {
		t.Fatal("output was not given a remote callback")
	}
	return callback
}

func dispatchRemote(m *Manager, cb func(airplay.RemoteCommand), ctx context.Context, device, action string, at time.Time) {
	cb(airplay.RemoteCommand{Context: ctx, DeviceID: device, Action: action, ReceivedAt: at})
	for _, e := range m.takeEvents() {
		m.handle(e)
	}
}

func TestRemotePauseStopsSpotifyAndAudioBeforeAnySpotifyEvent(t *testing.T) {
	for _, member := range []string{"tv", "pod"} {
		t.Run(member, func(t *testing.T) {
			m, a, b, _ := setup(t)
			setupGroup(t, m)
			var cb func(airplay.RemoteCommand)
			out := &cancellableOutput{}
			m.cfg.OpenGroupOutput = func(_ context.Context, cfg airplay.Config, _ airplay.Target, _ map[string]model.PairingSecret, _, _ int, _ func(string), _ func(string)) (Output, error) {
				cb = cfg.RemoteControl
				return out, nil
			}
			m.handle(playing("a", 1))
			a.commands = nil
			a.write("queued audio")
			dispatchRemote(m, cb, context.Background(), member, "pause", time.Now())
			a.write("must not reach output")
			v := m.Snapshot().Playback
			if !a.state.Paused || a.forward != nil || out.data != "" || out.flushes != 1 || out.cancellations != 1 || v.Status != "paused" || v.Recovering {
				t.Fatal("remote pause did not synchronously park the source and group")
			}
			if len(a.commands) != 1 || a.commands[0] != "pause" || len(b.commands) != 0 {
				t.Fatal("pause targeted the wrong Spotify session or was repeated")
			}
			m.handle(event{kind: "spotify", id: "a", generation: 1, spotify: spotify.Event{Type: "paused"}})
			m.handle(playing("a", 1))
			m.handle(event{kind: "output", generation: m.outputGeneration, status: "playing"})
			if out.flushes != 1 || m.Snapshot().Playback.Status != "paused" {
				t.Fatal("late acknowledgements repeated pause or restarted playback")
			}
			m.handle(event{kind: "output", generation: m.outputGeneration, status: "disconnected"})
			m.handle(event{kind: "spotify", id: "a", generation: 1, spotify: spotify.Event{Type: "audio_error"}})
			m.reconcile()
			if m.Snapshot().Playback.Recovering || !a.state.Paused {
				t.Fatal("losing the parked output scheduled automatic resume")
			}
		})
	}
}

func TestRemoteControlsRejectObsoleteOwners(t *testing.T) {
	for _, scenario := range []string{"takeover", "player_restart", "member_cancel", "output_reopen", "ui_pause"} {
		t.Run(scenario, func(t *testing.T) {
			m, a, b, _ := setup(t)
			cb := captureRemote(t, m)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			at := time.Now()
			switch scenario {
			case "takeover":
				m.handle(playing("b", 1))
			case "player_restart":
				m.accounts["a"].generation++
			case "member_cancel":
				cancel()
			case "output_reopen":
				m.closeOutput()
				m.handle(playing("a", 1))
			case "ui_pause":
				if err := m.Playback(context.Background(), "pause", 0); err != nil {
					t.Fatal(err)
				}
			}
			a.commands, b.commands = nil, nil
			dispatchRemote(m, cb, ctx, "tv", "play_pause", at)
			if len(a.commands) != 0 || len(b.commands) != 0 {
				t.Fatal("obsolete receiver request controlled a current session")
			}
		})
	}
}

func TestRemoteDuplicateToggleCannotResumePause(t *testing.T) {
	m, a, _, _ := setup(t)
	cb := captureRemote(t, m)
	a.commands = nil
	at := time.Now()
	dispatchRemote(m, cb, context.Background(), "tv", "play_pause", at)
	for i := 1; i <= 30; i++ {
		action := "play_pause"
		if i%2 == 0 {
			action = "play" // echoes and other member reports share the transport guard
		}
		dispatchRemote(m, cb, context.Background(), "pod", action, at.Add(time.Duration(i)*100*time.Millisecond))
	}
	if !a.state.Paused || len(a.commands) != 1 || a.commands[0] != "pause" {
		t.Fatal("duplicate/echo storm toggled pause into resume")
	}
	dispatchRemote(m, cb, context.Background(), "pod", "play", at.Add(4*time.Second))
	if a.state.Paused || len(a.commands) != 2 || a.commands[1] != "resume" {
		t.Fatal("deliberate resume after quiet period was lost")
	}
	// A receiver confirming the successful resume must not issue another resume.
	dispatchRemote(m, cb, context.Background(), "pod", "play", at.Add(5*time.Second))
	if len(a.commands) != 2 {
		t.Fatal("play feedback was sent back to Spotify")
	}
}

func TestRemoteSkipStormAndUnknownCommands(t *testing.T) {
	for action, want := range map[string]string{"next": "next", "previous": "prev"} {
		t.Run(action, func(t *testing.T) {
			m, a, _, _ := setup(t)
			cb := captureRemote(t, m)
			a.commands = nil
			at := time.Now()
			for i := 0; i < 60; i++ {
				dispatchRemote(m, cb, context.Background(), "tv", action, at.Add(time.Duration(i)*100*time.Millisecond))
			}
			if len(a.commands) != 1 || a.commands[0] != want {
				t.Fatal("receiver storm repeatedly skipped tracks")
			}
			for _, unknown := range []string{"stop", "volume", "pause secret"} {
				dispatchRemote(m, cb, context.Background(), "tv", unknown, at.Add(10*time.Second))
			}
			if len(a.commands) != 1 {
				t.Fatal("unsupported command reached Spotify")
			}
			for _, d := range m.Snapshot().Diagnostics {
				if strings.Contains(d.Message, "secret") {
					t.Fatal("raw event leaked into diagnostics")
				}
			}
		})
	}
}

func TestRemoteToggleRequiresLiveStateButPauseStillWorks(t *testing.T) {
	m, a, _, _ := setup(t)
	cb := captureRemote(t, m)
	a.commands = nil
	a.statusErr = errors.New("unavailable")
	at := time.Now()
	dispatchRemote(m, cb, context.Background(), "tv", "play_pause", at)
	if len(a.commands) != 0 {
		t.Fatal("guessed toggle direction without player state")
	}
	dispatchRemote(m, cb, context.Background(), "tv", "pause", at.Add(time.Second))
	if !a.state.Paused || len(a.commands) != 1 {
		t.Fatal("explicit pause depended on status availability")
	}
}

func TestRemoteMailboxKeepsDifferentActionsAndBoundsStorm(t *testing.T) {
	m, _, _, _ := setup(t)
	cb := captureRemote(t, m)
	for i := 0; i < 10000; i++ {
		cb(airplay.RemoteCommand{Context: context.Background(), DeviceID: "tv", Action: "pause", ReceivedAt: time.Now()})
	}
	cb(airplay.RemoteCommand{Context: context.Background(), DeviceID: "tv", Action: "play", ReceivedAt: time.Now()})
	events := m.takeEvents()
	if len(events) != 2 || events[0].remote.Action != "pause" || events[1].remote.Action != "play" {
		t.Fatal("mailbox grew unbounded or merged different remote actions")
	}
	cb(airplay.RemoteCommand{Context: context.Background(), DeviceID: "tv", Action: "pause", ReceivedAt: time.Now()})
	old, cancel := context.WithCancel(context.Background())
	cancel()
	cb(airplay.RemoteCommand{Context: old, DeviceID: "tv", Action: "pause", ReceivedAt: time.Now()})
	events = m.takeEvents()
	if len(events) != 1 || events[0].remote.Context.Err() != nil {
		t.Fatal("cancelled member replaced a valid request from its replacement")
	}
}

type failingParkOutput struct {
	fakeOutput
	stage string
}

func (f *failingParkOutput) Flush(ctx context.Context) error {
	if f.stage == "flush" {
		return errors.New("flush failed")
	}
	return f.fakeOutput.Flush(ctx)
}
func (f *failingParkOutput) Standby() error { return errors.New("standby failed") }

func TestRemotePauseOutputFailuresStayPaused(t *testing.T) {
	for _, stage := range []string{"flush", "standby"} {
		t.Run(stage, func(t *testing.T) {
			m, a, _, _ := setup(t)
			out := &failingParkOutput{stage: stage}
			m.cfg.OpenOutput = func(context.Context, airplay.Config, model.Device, model.PairingSecret, int, int, func(string)) (Output, error) {
				return out, nil
			}
			cb := captureRemote(t, m)
			dispatchRemote(m, cb, context.Background(), "tv", "pause", time.Now())
			v := m.Snapshot().Playback
			if !a.state.Paused || !out.closed || m.output != nil || v.Recovering || v.Error == "" {
				t.Fatal("failed pause cleanup left audio running or scheduled recovery")
			}
		})
	}
}

type rejectPausePlayer struct{ *fakePlayer }

func (p *rejectPausePlayer) Command(ctx context.Context, action string, value any) error {
	if action == "pause" {
		return errors.New("Spotify unavailable")
	}
	return p.fakePlayer.Command(ctx, action, value)
}

func TestFailedSpotifyPauseDoesNotLetPlayingEventsReopenOutput(t *testing.T) {
	m, a, _, outputs := setup(t)
	cb := captureRemote(t, m)
	m.accounts["a"].player = &rejectPausePlayer{a}
	dispatchRemote(m, cb, context.Background(), "tv", "pause", time.Now())
	m.handle(playing("a", 1))
	m.reconcile()
	if m.output != nil || a.forward != nil || len(*outputs) != 1 || m.Snapshot().Playback.Recovering {
		t.Fatal("failed source pause silently reopened the audio path")
	}
	// Once Spotify itself confirms a pause, a later Connect play is fresh intent.
	a.state.Paused = true
	m.handle(event{kind: "spotify", id: "a", generation: 1, spotify: spotify.Event{Type: "paused"}})
	m.accounts["a"].player = a
	a.state.Paused = false
	m.handle(playing("a", 1))
	if m.output == nil || len(*outputs) != 2 {
		t.Fatal("confirmed source pause permanently disabled Connect playback")
	}
}

func TestPolledPauseStillClearsUnparkedAudio(t *testing.T) {
	m, a, _, outputs := setup(t)
	m.handle(playing("a", 1))
	m.handle(event{kind: "output", generation: m.outputGeneration, status: "playing"})
	a.write("queued")
	a.state.Paused = true
	m.reconcile() // polling updates the UI before the asynchronous event arrives
	m.handle(event{kind: "spotify", id: "a", generation: 1, spotify: spotify.Event{Type: "paused"}})
	if (*outputs)[0].data != "" || (*outputs)[0].flushes != 1 || a.forward != nil {
		t.Fatal("polling a paused status incorrectly skipped audio cleanup")
	}
}
