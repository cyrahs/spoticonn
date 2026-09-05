package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"spoticonn/internal/model"
)

type State struct {
	Version  int                            `json:"version"`
	Settings model.Settings                 `json:"settings"`
	Accounts []model.Account                `json:"accounts"`
	Devices  map[string]model.Device        `json:"devices"`
	Pairings map[string]model.PairingSecret `json:"pairings"`
}

type Store struct {
	mu    sync.Mutex
	dir   string
	state State
}

func ID(bytes int) string {
	b := make([]byte, bytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, state: State{Version: 1, Settings: model.Settings{Name: "Spoticonn", Volume: 30}, Accounts: []model.Account{}, Devices: map[string]model.Device{}, Pairings: map[string]model.PairingSecret{}}}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err == nil {
		if err = json.Unmarshal(b, &s.state); err != nil {
			return nil, fmt.Errorf("invalid state: %w", err)
		}
		if s.state.Version != 1 {
			return nil, errors.New("unsupported state version")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if s.state.Devices == nil {
		s.state.Devices = map[string]model.Device{}
	}
	if s.state.Pairings == nil {
		s.state.Pairings = map[string]model.PairingSecret{}
	}
	return s, nil
}

func (s *Store) Dir() string                 { return s.dir }
func (s *Store) AccountDir(id string) string { return filepath.Join(s.dir, "accounts", id) }

func clone(v State) State        { b, _ := json.Marshal(v); var c State; _ = json.Unmarshal(b, &c); return c }
func (s *Store) Snapshot() State { s.mu.Lock(); defer s.mu.Unlock(); return clone(s.state) }

// Update commits an atomic replacement before publishing new in-memory state.
func (s *Store) Update(fn func(*State) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := clone(s.state)
	if err := fn(&next); err != nil {
		return err
	}
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	if err := AtomicWrite(filepath.Join(s.dir, "state.json"), b); err != nil {
		return err
	}
	s.state = next
	return nil
}

func AtomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
