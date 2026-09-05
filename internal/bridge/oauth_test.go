package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spoticonn/internal/model"
	"spoticonn/internal/spotify"
	"spoticonn/internal/store"
)

func authorizationCallback(t *testing.T, a model.AccountView) string {
	t.Helper()
	if a.Authorization == nil {
		t.Fatal("missing authorization URL")
	}
	u, err := url.Parse(a.Authorization.URL)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query().Get("redirect_uri") + "?" + url.Values{"code": {"SECRET-CODE"}, "state": {u.Query().Get("state")}}.Encode()
}

func newOAuthManager(t *testing.T) *Manager {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := New(context.Background(), s, Config{DisableDiscovery: true})
	m.cfg.StartPlayer = func(context.Context, spotify.Config, func(spotify.Event), func(error)) (Player, error) {
		t.Error("unexpected engine start")
		return nil, errors.New("unexpected engine start")
	}
	m.cfg.ExchangeAuthorization = func(context.Context, *spotify.Authorization, string) (*spotify.Login, error) {
		return &spotify.Login{Username: "alice", AccessToken: "SECRET-TOKEN", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	t.Cleanup(m.cancel)
	return m
}

func TestOAuthEnrollmentPersistsAndRestartsWithDeviceCredentials(t *testing.T) {
	m := newOAuthManager(t)
	a, err := m.AddAccount("A")
	if err != nil {
		t.Fatal(err)
	}
	view := m.Snapshot().Accounts[0]
	if view.Status != "waiting_oauth" || m.accounts[a.ID].player != nil {
		t.Fatal("waiting for OAuth started a pairing worker")
	}
	starts := 0
	m.cfg.StartPlayer = func(_ context.Context, cfg spotify.Config, _ func(spotify.Event), _ func(error)) (Player, error) {
		starts++
		if cfg.Account.DeviceID != a.DeviceID || cfg.Name != "Spoticonn" {
			t.Fatal("login changed the device identity/name")
		}
		if starts == 1 {
			if cfg.Login == nil || cfg.Login.AccessToken != "SECRET-TOKEN" {
				t.Fatal("missing bootstrap token")
			}
			if err := store.AtomicWrite(filepath.Join(cfg.Dir, "state.json"), []byte(`{"credentials":{"username":"alice","data":"AQ=="}}`)); err != nil {
				t.Fatal(err)
			}
		} else if !cfg.Account.Bound || cfg.Login != nil {
			t.Fatal("saved session still requires OAuth")
		}
		return &fakePlayer{}, nil
	}
	callback := authorizationCallback(t, view)
	if err := m.CompleteAuthorization(context.Background(), a.ID, callback); err != nil {
		t.Fatal(err)
	}
	if err := m.CompleteAuthorization(context.Background(), a.ID, callback); err == nil {
		t.Fatal("callback replay accepted")
	}
	serialized, _ := json.Marshal(m.Snapshot())
	if strings.Contains(string(serialized), "SECRET") {
		t.Fatal("OAuth secret leaked in snapshot")
	}
	m.reconcile()
	v := m.Snapshot().Accounts[0]
	if !v.Bound || v.Username != "alice" || v.Authorization != nil || starts != 2 {
		t.Fatal("credentials did not become a persistent session")
	}
	m.stopAccount(a.ID)
	reopened, err := store.Open(m.store.Dir())
	if err != nil {
		t.Fatal(err)
	}
	restarted := New(context.Background(), reopened, Config{DisableDiscovery: true, StartPlayer: m.cfg.StartPlayer})
	defer restarted.cancel()
	restarted.reconcile()
	if starts != 3 || restarted.Snapshot().Accounts[0].DeviceID != a.DeviceID {
		t.Fatal("restart lost the authenticated device")
	}
}

func TestOAuthRestartInvalidatesPendingLinksAndRecoversSavedCredentials(t *testing.T) {
	m := newOAuthManager(t)
	a, _ := m.AddAccount("A")
	old := authorizationCallback(t, m.Snapshot().Accounts[0])
	restarted := New(context.Background(), m.store, m.cfg)
	defer restarted.cancel()
	restarted.reconcile()
	if restarted.CompleteAuthorization(context.Background(), a.ID, old) == nil {
		t.Fatal("pre-restart callback accepted")
	}
	_ = m.store.Update(func(s *store.State) error { s.Accounts[0].AddedAt = time.Now().Add(-time.Hour); return nil })
	if err := store.AtomicWrite(filepath.Join(m.store.AccountDir(a.ID), "state.json"), []byte(`{"credentials":{"username":"alice","data":"AQ=="}}`)); err != nil {
		t.Fatal(err)
	}
	started := false
	restarted.cfg.StartPlayer = func(_ context.Context, cfg spotify.Config, _ func(spotify.Event), _ func(error)) (Player, error) {
		if !cfg.Account.Bound || cfg.Login != nil {
			t.Fatal("crash recovery requested OAuth")
		}
		started = true
		return &fakePlayer{}, nil
	}
	restarted.reconcile()
	if !started || !restarted.Snapshot().Accounts[0].Bound {
		t.Fatal("saved credentials were discarded after authorization timeout")
	}
}

func TestOAuthPendingDeleteRebindAndReplayDoNotRaceTokenExchange(t *testing.T) {
	for _, operation := range []string{"delete", "rebind", "expire"} {
		t.Run(operation, func(t *testing.T) {
			m := newOAuthManager(t)
			a, _ := m.AddAccount("A")
			callback := authorizationCallback(t, m.Snapshot().Accounts[0])
			entered, release := make(chan struct{}), make(chan struct{})
			m.cfg.ExchangeAuthorization = func(ctx context.Context, _ *spotify.Authorization, _ string) (*spotify.Login, error) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return nil, ctx.Err()
				}
				return &spotify.Login{Username: "alice", AccessToken: "SECRET", ExpiresAt: time.Now().Add(time.Hour)}, nil
			}
			done := make(chan error, 1)
			go func() { done <- m.CompleteAuthorization(context.Background(), a.ID, callback) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("exchange did not start")
			}
			if err := m.CompleteAuthorization(context.Background(), a.ID, callback); err == nil {
				t.Fatal("concurrent callback accepted")
			}
			mutation := make(chan error, 1)
			go func() {
				switch operation {
				case "delete":
					mutation <- m.DeleteAccount(a.ID)
				case "rebind":
					mutation <- m.Rebind(a.ID)
				case "expire":
					m.op.Lock()
					_ = m.store.Update(func(s *store.State) error { s.Accounts[0].AddedAt = time.Now().Add(-time.Hour); return nil })
					m.reconcile()
					m.op.Unlock()
					mutation <- nil
				}
			}()
			select {
			case err := <-mutation:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("exchange held lifecycle lock")
			}
			close(release)
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("stale exchange started an engine")
				}
			case <-time.After(time.Second):
				t.Fatal("exchange did not finish")
			}
			if operation == "delete" {
				if len(m.Snapshot().Accounts) != 0 {
					t.Fatal("deleted account revived")
				}
				if _, err := os.Stat(m.store.AccountDir(a.ID)); !os.IsNotExist(err) {
					t.Fatal("deleted account directory recreated")
				}
			} else if operation == "rebind" {
				if m.Snapshot().Accounts[0].DeviceID != a.DeviceID || m.CompleteAuthorization(context.Background(), a.ID, callback) == nil {
					t.Fatal("re-login retained old authorization")
				}
			} else if m.Snapshot().Accounts[0].Status != "expired" {
				t.Fatal("expired authorization revived")
			}
		})
	}
}

func TestOAuthFailureDenialAndDuplicateNeverStartWorker(t *testing.T) {
	for _, kind := range []string{"failure", "denial", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			m := newOAuthManager(t)
			a, _ := m.AddAccount("A")
			callback := authorizationCallback(t, m.Snapshot().Accounts[0])
			switch kind {
			case "failure":
				m.cfg.ExchangeAuthorization = func(context.Context, *spotify.Authorization, string) (*spotify.Login, error) {
					return nil, errors.New("SECRET-UPSTREAM-BODY")
				}
			case "denial":
				u, _ := url.Parse(callback)
				q := u.Query()
				q.Del("code")
				q.Set("error", "access_denied")
				u.RawQuery = q.Encode()
				callback = u.String()
			case "duplicate":
				_ = m.store.Update(func(s *store.State) error {
					s.Accounts = append(s.Accounts, model.Account{ID: "existing", Username: "alice", Bound: true})
					return nil
				})
			}
			err := m.CompleteAuthorization(context.Background(), a.ID, callback)
			if err == nil || strings.Contains(err.Error(), "SECRET") {
				t.Fatal("failed authorization accepted or leaked")
			}
			if m.Snapshot().Accounts[0].Authorization != nil {
				t.Fatal("consumed authorization still offered")
			}
			if m.CompleteAuthorization(context.Background(), a.ID, callback) == nil {
				t.Fatal("failed callback replay accepted")
			}
		})
	}
}
