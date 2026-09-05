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

func TestLegacyMemberSelectionUsesLeaderAndItsOwnCredentials(t *testing.T) {
	m, _, _, _ := setup(t)
	setupGroup(t, m)
	view := m.Snapshot()
	if len(view.Devices) != 1 || view.Settings.TargetID != "tv" || view.Devices[0].Paired {
		t.Fatalf("legacy selection or paired status incorrect: %+v", view.Devices)
	}
	for _, credentials := range []string{"", "tv-secret"} {
		if err := m.store.Update(func(s *store.State) error {
			if credentials != "" {
				s.Pairings["tv"] = model.PairingSecret{Credentials: credentials}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		called := false
		m.cfg.OpenOutput = func(_ context.Context, _ airplay.Config, d model.Device, p model.PairingSecret, _, _ int, _ func(string)) (Output, error) {
			called = true
			if d.ID != "tv" || d.Address != "192.0.2.10" || p.Credentials != credentials || d.TXT["igl"] != "1" {
				t.Fatal("output used a member endpoint or another device's credentials")
			}
			return &fakeOutput{}, nil
		}
		m.handle(playing("a", 1))
		if !called {
			t.Fatal("output did not open")
		}
		m.closeOutput()
	}
}

func TestSettingsCanonicalizeAndPersistGroupWithoutInterruptingPlayback(t *testing.T) {
	m, _, _, outputs := setup(t)
	setupGroup(t, m)
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
	m, _, _, _ := setup(t)
	setupGroup(t, m)
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
	view, err := m.StartPairing("pod")
	if err != nil {
		t.Fatal(err)
	}
	defer m.pairing.Close()
	if view.DeviceID != "tv" {
		t.Fatal("pairing did not resolve the group leader")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p := m.Snapshot().Pairing; p != nil && p.Status == "paired" {
			saved := m.store.Snapshot()
			if saved.Pairings["tv"].Credentials != strings.Repeat("a", 192) || saved.Pairings["pod"].Credentials != "pod-secret" {
				t.Fatal("pairing credentials were saved to the wrong physical device")
			}
			args, err := os.ReadFile(argsFile)
			if err != nil || !strings.HasSuffix(string(args), "192.0.2.10\n") || strings.Contains(string(args), "192.0.2.11") {
				t.Fatal("pairing subprocess did not receive the Apple TV address")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("pairing completion was not persisted")
}
