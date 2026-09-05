package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"spoticonn/internal/airplay"
	"spoticonn/internal/model"
	"spoticonn/internal/spotify"
	"spoticonn/internal/store"
)

type fakePlayer struct {
	mu        sync.Mutex
	state     model.PlayerStatus
	commands  []string
	forward   func([]byte) error
	closed    bool
	drains    int
	statusErr error
}

func (f *fakePlayer) Status(context.Context) (model.PlayerStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v := f.state
	if v.Track != nil {
		c := *v.Track
		v.Track = &c
	}
	return v, f.statusErr
}
func (f *fakePlayer) Command(_ context.Context, a string, b any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, a)
	switch a {
	case "pause":
		f.state.Paused = true
	case "resume":
		f.state.Paused = false
	case "seek":
		f.state.Track.Position = b.(map[string]any)["position"].(int64)
	}
	return nil
}
func (f *fakePlayer) Route(_ int, w func([]byte) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forward = w
}
func (f *fakePlayer) Drain() { f.mu.Lock(); defer f.mu.Unlock(); f.drains++ }
func (f *fakePlayer) Close() { f.mu.Lock(); defer f.mu.Unlock(); f.closed = true }
func (f *fakePlayer) write(b string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forward != nil {
		_ = f.forward([]byte(b))
	}
}

type fakeOutput struct {
	mu              sync.Mutex
	data            string
	closed          bool
	flushes, starts int
	volume          int
}

func (f *fakeOutput) Begin() { f.starts++ }
func (f *fakeOutput) Write(b []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("closed")
	}
	f.data += string(b)
	return nil
}
func (f *fakeOutput) Flush(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = ""
	f.flushes++
	return nil
}
func (f *fakeOutput) Standby() error              { return nil }
func (f *fakeOutput) Volume(v int) error          { f.volume = v; return nil }
func (f *fakeOutput) Metadata(*model.Track) error { return nil }
func (f *fakeOutput) Close()                      { f.mu.Lock(); defer f.mu.Unlock(); f.closed = true }

func setup(t *testing.T) (*Manager, *fakePlayer, *fakePlayer, *[]*fakeOutput) {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Update(func(v *store.State) error {
		v.Accounts = []model.Account{{ID: "a", Label: "A", Bound: true, Username: "alice", DeviceID: "device-a"}, {ID: "b", Label: "B", Bound: true, Username: "bob", DeviceID: "device-b"}}
		v.Settings.TargetID = "tv"
		v.Devices["tv"] = model.Device{ID: "tv", Name: "Living room", Address: "192.168.1.2", Port: 7000, Online: true, LastSeen: time.Now()}
		return nil
	})
	outputs := []*fakeOutput{}
	m := New(context.Background(), s, Config{DisableDiscovery: true, RuntimeDir: t.TempDir(), OpenOutput: func(context.Context, airplay.Config, model.Device, model.PairingSecret, int, int, func(string)) (Output, error) {
		f := &fakeOutput{}
		outputs = append(outputs, f)
		return f, nil
	}})
	a := &fakePlayer{state: model.PlayerStatus{Track: &model.Track{URI: "spotify:track:a", Position: 5000, SampleRate: 44100}}}
	b := &fakePlayer{state: model.PlayerStatus{Track: &model.Track{URI: "spotify:track:b", Position: 9000, SampleRate: 44100}}}
	m.accounts["a"] = &runtimeAccount{player: a, generation: 1, status: "online"}
	m.accounts["b"] = &runtimeAccount{player: b, generation: 1, status: "online"}
	t.Cleanup(func() { m.cancel(); m.detach(); m.closeOutput() })
	return m, a, b, &outputs
}
func playing(id string, generation uint64) event {
	return event{kind: "spotify", id: id, generation: generation, spotify: spotify.Event{Type: "playing"}}
}

func TestTakeoverIsolatesAudioAndKeepsSessions(t *testing.T) {
	m, a, b, outputs := setup(t)
	m.handle(playing("a", 1))
	a.write("A")
	m.handle(playing("b", 1))
	a.write("OLD-A")
	b.write("B")
	if m.Snapshot().Playback.AccountID != "b" {
		t.Fatal("B did not take over")
	}
	if len(*outputs) != 2 || !(*outputs)[0].closed || (*outputs)[1].data != "B" {
		t.Fatalf("output contamination: %+v", outputs)
	}
	if !a.state.Paused || a.closed || b.closed {
		t.Fatal("takeover must pause A without disconnecting either session")
	}
	if b.state.Track.Position != 9000 {
		t.Fatal("startup dropped playback position")
	}
	if len(m.store.Snapshot().Accounts) != 2 {
		t.Fatal("lost accounts")
	}
}
func TestStalePauseResumeAndProcessEventsCannotTakeOver(t *testing.T) {
	m, a, b, outputs := setup(t)
	m.handle(playing("a", 1))
	m.handle(event{kind: "spotify", id: "a", generation: 1, spotify: spotify.Event{Type: "paused"}})
	a.write("A")
	if (*outputs)[0].data != "A" {
		t.Fatal("delayed internal pause detached current audio")
	}
	m.handle(playing("b", 1))
	m.handle(playing("a", 1)) // A has actually been paused by the B takeover.
	m.handle(playing("a", 0)) // a previous process generation
	m.handle(event{kind: "output", generation: m.outputGeneration - 1, status: "error"})
	b.write("B")
	if m.Snapshot().Playback.AccountID != "b" || (*outputs)[1].data != "B" {
		t.Fatal("stale event disrupted B")
	}
}
func TestSeekClearsOutputAndResumesOnlyActiveSource(t *testing.T) {
	m, a, b, outputs := setup(t)
	m.handle(playing("a", 1))
	a.write("stale")
	m.handle(event{kind: "spotify", id: "a", generation: 1, spotify: spotify.Event{Type: "seek"}})
	a.write("new")
	b.write("other")
	if (*outputs)[0].data != "new" || (*outputs)[0].flushes != 1 {
		t.Fatal("seek retained stale frames")
	}
}
func TestOutputFailurePausesSpotifyAndPreservesAccounts(t *testing.T) {
	m, a, _, _ := setup(t)
	m.handle(playing("a", 1))
	m.handle(event{kind: "output", generation: m.outputGeneration, status: "error"})
	if !a.state.Paused || m.output != nil || m.Snapshot().Playback.OutputStatus != "error" {
		t.Fatal("output failure did not stop audio")
	}
	if a.closed || len(m.store.Snapshot().Accounts) != 2 {
		t.Fatal("output failure removed Spotify sessions")
	}
}
func TestPersistenceAndRebindPreserveDeviceID(t *testing.T) {
	m, _, _, _ := setup(t)
	m.cfg.StartPlayer = func(context.Context, spotify.Config, func(spotify.Event), func(error)) (Player, error) {
		return &fakePlayer{}, nil
	}
	dir := m.store.AccountDir("a")
	_ = os.MkdirAll(dir, 0700)
	_ = os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"credentials":{"username":"alice","data":"AQ=="}}`), 0600)
	if err := m.Rebind("a"); err != nil {
		t.Fatal(err)
	}
	state, err := store.Open(m.store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	account := state.Snapshot().Accounts[0]
	if account.DeviceID != "device-a" || account.Bound {
		t.Fatal("rebind changed identity or retained binding")
	}
	if _, err := os.Stat(filepath.Join(dir, "state.json")); !os.IsNotExist(err) {
		t.Fatal("rebind did not erase upstream credentials")
	}
}
func TestDuplicateAccountNeverCreatesSecondCloudSession(t *testing.T) {
	m, _, _, _ := setup(t)
	a := model.Account{ID: "duplicate", Label: "Duplicate", AddedAt: time.Now()}
	_ = m.store.Update(func(s *store.State) error { s.Accounts = append(s.Accounts, a); return nil })
	f := &fakePlayer{}
	m.accounts[a.ID] = &runtimeAccount{player: f, generation: 1}
	m.bind(a, "alice")
	if !f.closed || m.accounts[a.ID].status != "duplicate" || m.store.Snapshot().Accounts[2].Bound {
		t.Fatal("duplicate was allowed to stay online")
	}
}
func TestSnapshotDoesNotExposePairingSecrets(t *testing.T) {
	m, _, _, _ := setup(t)
	_ = m.store.Update(func(s *store.State) error {
		s.Pairings["tv"] = model.PairingSecret{Credentials: "private-value", DACP: "private-id"}
		return nil
	})
	v := m.Snapshot()
	if len(v.Devices) != 1 || !v.Devices[0].Paired {
		t.Fatal("pairing status missing")
	}
	if v.Devices[0].TXT != nil {
		t.Fatal("raw discovery data exposed")
	}
}

func TestOutputRecoveryResumesAfterBackoff(t *testing.T) {
	m, a, _, outputs := setup(t)
	m.handle(playing("a", 1))
	m.handle(event{kind: "output", generation: m.outputGeneration, status: "disconnected"})
	if !m.Snapshot().Playback.Recovering || !a.state.Paused {
		t.Fatal("output failure must pause the source and expose recovery")
	}
	m.outputRetryAt = time.Now().Add(-time.Second)
	m.reconcile()
	a.write("recovered")
	if len(*outputs) != 2 || (*outputs)[1].data != "recovered" || a.state.Paused || m.Snapshot().Playback.Recovering {
		t.Fatal("recovery did not establish a fresh output and resume")
	}
}

func TestExplicitPauseCancelsRecovery(t *testing.T) {
	m, a, _, outputs := setup(t)
	m.handle(playing("a", 1))
	m.handle(event{kind: "output", generation: m.outputGeneration, status: "error"})
	if err := m.Playback(context.Background(), "pause", 0); err != nil {
		t.Fatal(err)
	}
	m.outputRetryAt = time.Now().Add(-time.Second)
	m.reconcile()
	if !a.state.Paused || len(*outputs) != 1 || m.Snapshot().Playback.Recovering {
		t.Fatal("explicit pause allowed automatic playback")
	}
}

func TestUnresponsivePlayerRestartsWithoutLosingIdentity(t *testing.T) {
	m, a, b, _ := setup(t)
	a.statusErr = errors.New("control socket unavailable")
	for i := 0; i < 5; i++ {
		m.reconcile()
	}
	if !a.closed || b.closed || m.accounts["a"].player != nil {
		t.Fatal("failed control connection did not isolate the affected account")
	}
	if m.store.Snapshot().Accounts[0].DeviceID != "device-a" {
		t.Fatal("recovery lost the account identity")
	}
}

func TestEngineNotificationsNeverBlockProcessShutdown(t *testing.T) {
	m, _, _, _ := setup(t)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			m.emit(playing("a", 1))
			m.emit(event{kind: "spotify", id: "a", generation: 1, spotify: spotify.Event{Type: "paused"}})
		}
		m.emit(playing("a", 1))
		m.emit(event{kind: "exit", id: "b", generation: 1})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("engine callback blocked while the manager was busy")
	}
	events := m.takeEvents()
	if len(events) != 3 || events[1].spotify.Type != "playing" || events[2].kind != "exit" {
		t.Fatalf("mailbox lost final state or exit notification: %+v", events)
	}
}

func TestLateOutputStartCannotUndoPause(t *testing.T) {
	m, a, _, _ := setup(t)
	m.handle(playing("a", 1))
	a.state.Paused = true
	m.handle(event{kind: "spotify", id: "a", generation: 1, spotify: spotify.Event{Type: "paused"}})
	m.handle(event{kind: "output", generation: m.outputGeneration, status: "playing"})
	if m.Snapshot().Playback.Status != "paused" {
		t.Fatal("late sender notification undid pause")
	}
}

func TestChangingTargetWhilePausedDoesNotAutoplay(t *testing.T) {
	m, a, _, outputs := setup(t)
	m.handle(playing("a", 1))
	m.handle(event{kind: "output", generation: m.outputGeneration, status: "playing"})
	a.state.Paused = true
	m.devices["other"] = model.Device{ID: "other", Name: "Other speaker", Address: "192.168.1.3", Port: 7000}
	settings := m.store.Snapshot().Settings
	settings.TargetID = "other"
	if err := m.UpdateSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	v := m.Snapshot()
	if !a.state.Paused || len(*outputs) != 1 || !(*outputs)[0].closed || v.Playback.OutputStatus != "disconnected" {
		t.Fatal("changing the output while paused resumed or misreported playback")
	}
	if v.Settings.TargetID != "other" {
		t.Fatal("new output selection was not persisted")
	}
}
