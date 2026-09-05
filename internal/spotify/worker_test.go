package spotify

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"spoticonn/internal/model"
	"spoticonn/internal/store"
)

func TestCredentialsPrefersCurrentStateAndAcceptsLegacy(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"username":"old","data":"AQ=="}`), 0600)
	if u, err := Credentials(dir); err != nil || u != "old" {
		t.Fatal("legacy credentials rejected")
	}
	_ = os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"credentials":{"username":"new","data":"Ag=="}}`), 0600)
	if u, err := Credentials(dir); err != nil || u != "new" {
		t.Fatal("preferred stale credential file")
	}
}

func helperBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "go-librespot")
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(exe, "'", "'\"'\"'") + "' -test.run=TestSpotifyEngineProcess -- \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SPOTICONN_TEST_SPOTIFY", "1")
	return path
}

func TestWorkerUsesPrivatePersistentIdentityAndLocalAPI(t *testing.T) {
	binary := helperBinary(t)
	dir := t.TempDir()
	runtime := t.TempDir()
	_ = store.AtomicWrite(filepath.Join(dir, "state.json"), []byte(`{"credentials":{"username":"test-user","data":"AQ=="}}`))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan Event, 8)
	w, err := Start(ctx, Config{Binary: binary, Dir: dir, RuntimeDir: runtime, Name: "Home", Account: model.Account{ID: "test-worker", DeviceID: strings.Repeat("a", 40), Bound: true}, Volume: 30}, func(e Event) {
		select {
		case events <- e:
		default:
		}
	}, func(error) {})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	select {
	case e := <-events:
		if e.Type != "connected" {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker API did not connect")
	}
	status, err := w.Status(ctx)
	if err != nil || status.Username != "test-user" {
		t.Fatalf("status: %v %#v", err, status)
	}
	if err := w.Command(ctx, "pause", nil); err != nil {
		t.Fatal(err)
	}
	status, _ = w.Status(ctx)
	if !status.Paused {
		t.Fatal("pause was not forwarded")
	}
	if err := w.Command(ctx, "stop", nil); err == nil {
		t.Fatal("disconnecting account is not an allowed transport action")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
	var c map[string]any
	_ = json.Unmarshal(b, &c)
	if c["zeroconf_enabled"] != false || c["device_id"] != strings.Repeat("a", 40) || c["external_volume"] != true {
		t.Fatalf("incorrect persistent worker config: %s", b)
	}
	server := c["server"].(map[string]any)
	if server["address"] != "127.0.0.1" {
		t.Fatal("worker API exposed")
	}
}

func TestSpotifyEngineProcess(t *testing.T) {
	if os.Getenv("SPOTICONN_TEST_SPOTIFY") != "1" {
		return
	}
	var dir string
	for i, a := range os.Args {
		if a == "--config_dir" && i+1 < len(os.Args) {
			dir = os.Args[i+1]
		}
	}
	if dir == "" {
		os.Exit(2)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "config.yml"))
	var cfg struct {
		Server struct {
			Address string `json:"address"`
			Port    int    `json:"port"`
		} `json:"server"`
	}
	_ = json.Unmarshal(b, &cfg)
	var mu sync.Mutex
	paused := false
	mux := http.NewServeMux()
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = json.NewEncoder(w).Encode(model.PlayerStatus{Username: "test-user", Paused: paused})
	})
	mux.HandleFunc("/player/pause", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paused = true
		mu.Unlock()
		w.WriteHeader(200)
	})
	mux.HandleFunc("/events", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		for {
			if _, _, err := c.Read(r.Context()); err != nil {
				return
			}
		}
	})
	_ = http.ListenAndServe(fmt.Sprintf("%s:%d", cfg.Server.Address, cfg.Server.Port), mux)
	os.Exit(0)
}
