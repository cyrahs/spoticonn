package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"spoticonn/internal/airplay"
	"spoticonn/internal/model"
	"spoticonn/internal/store"
)

func TestPasswordPreservesMemberCredentialsAndSurvivesRestart(t *testing.T) {
	m, _, _, _ := setup(t)
	setupGroup(t, m)
	m.devices["pod"].TXT["pw"] = "true"
	_ = m.store.Update(func(s *store.State) error {
		s.Pairings["tv"] = model.PairingSecret{DACP: "tv-dacp", Credentials: "tv-secret"}
		return nil
	})
	before := m.store.Snapshot()
	if err := m.SaveDevicePassword("tv", "pod", "a password; $with symbols"); err != nil {
		t.Fatal(err)
	}
	saved := m.store.Snapshot()
	if saved.Pairings["tv"] != before.Pairings["tv"] || saved.Pairings["pod"].Credentials != before.Pairings["pod"].Credentials {
		t.Fatal("saving password overwrote existing pairing credentials")
	}
	reopened, err := store.Open(m.store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	restarted := New(context.Background(), reopened, Config{DisableDiscovery: true})
	defer restarted.cancel()
	view := restarted.Snapshot()
	b, _ := json.Marshal(view)
	if strings.Contains(string(b), "a password") || strings.Contains(string(b), "pod-secret") || strings.Contains(string(b), "tv-secret") {
		t.Fatal("snapshot leaked authentication secrets")
	}
	if !view.Devices[0].Authentication.PasswordSaved || !view.Devices[0].Paired {
		t.Fatal("restart lost saved authentication status")
	}
	called := false
	m.cfg.OpenGroupOutput = func(_ context.Context, _ airplay.Config, _ airplay.Target, pairs map[string]model.PairingSecret, _, _ int, _, _ func(string)) (Output, error) {
		called = true
		if pairs["pod"].Password != "a password; $with symbols" || pairs["tv"].Password != "" {
			t.Fatal("password was not passed to its physical member")
		}
		return &fakeOutput{}, nil
	}
	m.handle(playing("a", 1))
	if !called {
		t.Fatal("password-protected output was not opened")
	}
}

func TestPasswordAndPINValidateMemberPolicyWithoutChangingCredentials(t *testing.T) {
	m, _, _, _ := setup(t)
	setupGroup(t, m)
	before, _ := json.Marshal(m.store.Snapshot().Pairings)
	for _, password := range []string{"", "a\nb", "a\x00b", strings.Repeat("a", 257)} {
		if m.SaveDevicePassword("tv", "pod", password) == nil {
			t.Fatal("invalid password accepted")
		}
	}
	if m.SaveDevicePassword("tv", "foreign", "password") == nil {
		t.Fatal("foreign member accepted")
	}
	m.devices["pod"].TXT["pw"] = "true"
	if _, err := m.StartMemberPairing("tv", "pod"); err == nil || !strings.Contains(err.Error(), "设备密码") {
		t.Fatal("password-protected HomePod was sent to the PIN flow")
	}
	m.devices["pod"].TXT["acl"] = "1"
	if m.SaveDevicePassword("tv", "pod", "password") == nil {
		t.Fatal("home access restriction was treated as password authentication")
	}
	if _, err := m.StartMemberPairing("tv", "pod"); err == nil || !strings.Contains(err.Error(), "访问") {
		t.Fatal("restricted device was sent to the PIN flow")
	}
	pod := m.devices["pod"]
	pod.Online = false
	m.devices["pod"] = pod
	if m.SaveDevicePassword("tv", "pod", "password") == nil {
		t.Fatal("offline member accepted")
	}
	after, _ := json.Marshal(m.store.Snapshot().Pairings)
	if string(before) != string(after) {
		t.Fatal("failed authentication request altered credentials")
	}
}
