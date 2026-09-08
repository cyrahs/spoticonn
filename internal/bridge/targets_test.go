package bridge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spoticonn/internal/airplay"
	"spoticonn/internal/model"
	"spoticonn/internal/store"
)

func setupGroup(t *testing.T, m *Manager) {
	t.Helper()
	m.devices = map[string]model.Device{
		"tv":  {ID: "tv", Name: `Living\ Room`, Model: "AppleTV14,1", Address: "192.0.2.10", Port: 7000, Online: true, LastSeen: time.Now(), TXT: map[string]string{"gid": "living-room-group", "gpn": "Living Room", "igl": "1", "gcgl": "1"}},
		"pod": {ID: "pod", Name: `Living\ Room\ \(2\)`, Model: "AudioAccessory6,1", Address: "192.0.2.11", Port: 7000, Online: true, LastSeen: time.Now(), TXT: map[string]string{"gid": "living-room-group", "gpn": "Living Room", "igl": "0", "gcgl": "1"}},
	}
	if err := m.store.Update(func(s *store.State) error {
		s.Settings.TargetID = "pod" // legacy selection from before groups existed
		s.Devices = m.devices
		s.Pairings["pod"] = model.PairingSecret{Credentials: "pod-secret"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyMemberSelectionRetainsEveryEndpointAndCredential(t *testing.T) {
	m, _, _, _ := setup(t)
	setupGroup(t, m)
	view := m.Snapshot()
	if len(view.Devices) != 1 || view.Settings.TargetID != "tv" || !view.Devices[0].Paired {
		t.Fatal("legacy selection or HomePod paired status incorrect")
	}
	_ = m.store.Update(func(s *store.State) error {
		s.Pairings["tv"] = model.PairingSecret{Credentials: "tv-secret"}
		return nil
	})
	called := false
	m.cfg.OpenGroupOutput = func(_ context.Context, _ airplay.Config, target airplay.Target, pairs map[string]model.PairingSecret, _, _ int, _, _ func(string)) (Output, error) {
		called = true
		pod, tv, ok := target.HomeTheater()
		if !ok || pod.Address != "192.0.2.11" || tv.Address != "192.0.2.10" || pairs[pod.ID].Credentials != "pod-secret" || pairs[tv.ID].Credentials != "tv-secret" {
			t.Fatal("group lost physical addresses or per-member credentials")
		}
		return &fakeOutput{}, nil
	}
	m.handle(playing("a", 1))
	if !called {
		t.Fatal("group output did not open")
	}
}

func TestSettingsCanonicalizeAndPersistGroupWithoutInterruptingPlayback(t *testing.T) {
	m, _, _, outputs := setup(t)
	setupGroup(t, m)
	m.cfg.OpenGroupOutput = func(_ context.Context, _ airplay.Config, _ airplay.Target, _ map[string]model.PairingSecret, _, _ int, _, _ func(string)) (Output, error) {
		f := &fakeOutput{}
		*outputs = append(*outputs, f)
		return f, nil
	}
	m.handle(playing("a", 1))
	settings := m.Snapshot().Settings
	settings.Volume = 42
	if err := m.UpdateSettings(context.Background(), settings); err != nil {
		t.Fatal(err)
	}
	saved := m.store.Snapshot()
	if saved.Settings.TargetID != "tv" || len(saved.Devices) != 2 || saved.Pairings["pod"].Credentials != "pod-secret" {
		t.Fatal("canonical selection or member records were not preserved")
	}
	if len(*outputs) != 1 || (*outputs)[0].closed || (*outputs)[0].volume != 42 {
		t.Fatal("normalizing a legacy selection interrupted playback")
	}
	s, err := store.Open(m.store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	restarted := New(context.Background(), s, Config{DisableDiscovery: true})
	defer restarted.cancel()
	if view := restarted.Snapshot(); len(view.Devices) != 1 || view.Settings.TargetID != view.Devices[0].ID {
		t.Fatal("saved group was not restored after restart")
	}
}

func TestMissingGroupLeaderBlocksConnectionAndPairing(t *testing.T) {
	m, a, _, outputs := setup(t)
	setupGroup(t, m)
	delete(m.devices, "tv")
	view := m.Snapshot()
	if view.Settings.TargetID != view.Devices[0].ID || !view.Devices[0].WaitingForLeader {
		t.Fatal("missing leader lost the existing group selection")
	}
	m.handle(playing("a", 1))
	if len(*outputs) != 0 || !a.state.Paused {
		t.Fatal("connected to HomePod as a fallback")
	}
	if _, err := m.StartPairing("pod"); err == nil {
		t.Fatal("paired with a follower while the leader was missing")
	}
	settings := view.Settings
	settings.Volume = 24
	if err := m.UpdateSettings(context.Background(), settings); err != nil {
		t.Fatal("an offline group prevented updating settings:", err)
	}
	if m.store.Snapshot().Settings.TargetID != "pod" {
		t.Fatal("a temporary group placeholder replaced the saved hardware ID")
	}
}

func TestGroupPairingStoresCredentialsUnderActualEndpoint(t *testing.T) {
	for _, member := range []string{"pod", "tv"} {
		t.Run(member, func(t *testing.T) {
			m, _, _, _ := setup(t)
			setupGroup(t, m)
			_ = m.store.Update(func(s *store.State) error {
				secret := s.Pairings[member]
				secret.Password = "existing-password"
				s.Pairings[member] = secret
				return nil
			})
			argsFile := filepath.Join(t.TempDir(), "pair-args")
			t.Setenv("SPOTICONN_TEST_PAIR_ARGS", argsFile)
			binary := filepath.Join(t.TempDir(), "fake-pair")
			// Exercise the real subprocess and completion callback without contacting
			// any AirPlay device. Only the fixed test credential is emitted.
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$SPOTICONN_TEST_PAIR_ARGS\"\nprintf 'CREDENTIALS: " + strings.Repeat("a", 192) + "\\n'\n"
			if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			m.cfg.AirPlayBinary = binary
			view, err := m.StartMemberPairing("tv", member)
			if err != nil {
				t.Fatal(err)
			}
			defer m.pairing.Close()
			if view.DeviceID != member {
				t.Fatal("group pairing did not default to its initial audio member")
			}
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if p := m.Snapshot().Pairing; p != nil && p.Status == "paired" {
					saved := m.store.Snapshot()
					if saved.Pairings[member].Credentials != strings.Repeat("a", 192) || saved.Pairings[member].Password != "existing-password" {
						t.Fatal("pairing credentials were saved to the wrong physical device")
					}
					args, err := os.ReadFile(argsFile)
					if err != nil || !strings.HasSuffix(string(args), m.devices[member].Address+"\n") {
						t.Fatal("pairing subprocess did not receive the HomePod address")
					}
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatal("pairing completion was not persisted")
		})
	}
}

func TestGroupDegradationDoesNotPauseOrReconnectPrimary(t *testing.T) {
	m, a, _, _ := setup(t)
	setupGroup(t, m)
	m.cfg.OpenGroupOutput = func(context.Context, airplay.Config, airplay.Target, map[string]model.PairingSecret, int, int, func(string), func(string)) (Output, error) {
		return &fakeOutput{}, nil
	}
	m.handle(playing("a", 1))
	m.handle(event{kind: "output", generation: m.outputGeneration, status: "playing"})
	for _, state := range []string{"group_joining", "group_degraded_offline", "group_degraded_auth", "group_degraded_connect", "group_degraded_start"} {
		m.handle(event{kind: "output", generation: m.outputGeneration, status: state})
		view := m.Snapshot().Playback
		if a.state.Paused || view.Status != "playing" || view.Recovering || m.output == nil || view.GroupStatus != state {
			t.Fatal("member degradation triggered output recovery")
		}
	}
	if m.Snapshot().Playback.GroupReason == "" {
		t.Fatal("degradation reason missing")
	}
	generation := m.outputGeneration
	m.closeOutput()
	m.handle(event{kind: "output", generation: generation, status: "group_joined"})
	if m.Snapshot().Playback.GroupStatus != "" {
		t.Fatal("stale member state survived output close")
	}
}

type cancellableOutput struct {
	fakeOutput
	cancellations int
}

func (o *cancellableOutput) CancelJoin() { o.cancellations++ }

func TestTransportCommandsCancelJoinBeforeSpotifyEvents(t *testing.T) {
	for _, action := range []string{"pause", "stop", "next", "prev", "seek"} {
		t.Run(action, func(t *testing.T) {
			m, _, _, _ := setup(t)
			m.handle(playing("a", 1))
			output := &cancellableOutput{}
			m.output.Close()
			m.output = output
			if err := m.Playback(context.Background(), action, 100); err != nil {
				t.Fatal(err)
			}
			if output.cancellations != 1 {
				t.Fatal("transport command left pending join active")
			}
		})
	}
}

func TestGroupMemberPairingRejectsUnknownOrOfflineMember(t *testing.T) {
	m, _, _, _ := setup(t)
	setupGroup(t, m)
	if _, err := m.StartMemberPairing("tv", "foreign"); err == nil {
		t.Fatal("paired foreign member")
	}
	tv := m.devices["tv"]
	tv.Online = false
	m.devices[tv.ID] = tv
	if _, err := m.StartMemberPairing("tv", "tv"); err == nil {
		t.Fatal("paired offline member")
	}
}

func TestSampleRateChangeReopensOutput(t *testing.T) {
	m, a, _, outputs := setup(t)
	m.handle(playing("a", 1))
	a.state.Track.SampleRate = 48000
	m.handle(playing("a", 1))
	if len(*outputs) != 2 || !(*outputs)[0].closed || m.outputRate != 48000 {
		t.Fatal("new format reused an incompatible output")
	}
}
