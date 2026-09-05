package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"spoticonn/internal/audio"
	"spoticonn/internal/model"
	"spoticonn/internal/store"
)

type Event struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}
type Config struct {
	Binary, Dir, RuntimeDir, Name string
	Account                       model.Account
	Login                         *Login
	Volume                        int
}
type Worker struct {
	Source *audio.Source
	base   string
	client *http.Client
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

func (w *Worker) Route(rate int, forward func([]byte) error) { w.Source.Route(rate, forward) }
func (w *Worker) Drain()                                     { w.Source.Drain() }

func Start(parent context.Context, cfg Config, emit func(Event), exited func(error)) (*Worker, error) {
	// Never start an interactive listener as a fallback for missing credentials.
	if cfg.Account.Bound {
		if _, err := Credentials(cfg.Dir); err != nil {
			return nil, fmt.Errorf("saved credentials unavailable: %w", err)
		}
	} else if cfg.Login == nil || cfg.Login.Username == "" || cfg.Login.AccessToken == "" || !time.Now().Before(cfg.Login.ExpiresAt) {
		return nil, errors.New("Spotify OAuth login required")
	}
	if err := os.MkdirAll(cfg.Dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(cfg.Dir, 0700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.RuntimeDir, 0700); err != nil {
		return nil, err
	}
	fifo := filepath.Join(cfg.RuntimeDir, "audio-"+cfg.Account.ID)
	source, err := audio.NewSource(fifo)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Worker, error) { _ = source.Close(); return nil, err }
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fail(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	config := map[string]any{
		"device_id": cfg.Account.DeviceID, "device_name": cfg.Name, "device_type": "speaker",
		"audio_backend": "pipe", "audio_output_pipe": fifo, "audio_output_pipe_format": "s16le",
		"audio_output_pipe_wait_for_reader": true, "bitrate": 320, "flac_enabled": false,
		"external_volume": true, "initial_volume": cfg.Volume, "volume_steps": 100, "ignore_last_volume": true,
		"zeroconf_enabled": false, "log_level": "warn", "mpris_enabled": false,
		"prefer_firewall_friendly_ports": true,
		"server":                         map[string]any{"enabled": true, "address": "127.0.0.1", "port": port},
		"credentials":                    map[string]any{"type": "spotify_token"},
	}
	cleanConfig, _ := json.MarshalIndent(config, "", "  ")
	if !cfg.Account.Bound {
		config["credentials"] = map[string]any{"type": "spotify_token", "spotify_token": map[string]any{
			"username": cfg.Login.Username, "access_token": cfg.Login.AccessToken,
		}}
	}
	b, _ := json.MarshalIndent(config, "", "  ") // JSON is valid YAML; avoids string interpolation.
	if err := store.AtomicWrite(filepath.Join(cfg.Dir, "config.yml"), b); err != nil {
		return fail(err)
	}
	// The bootstrap token is written only to the private engine config, never
	// argv or logs. Erase it once the process has finished reading/using it.
	scrub := func() { _ = store.AtomicWrite(filepath.Join(cfg.Dir, "config.yml"), cleanConfig) }
	ctx, cancel := context.WithCancel(parent)
	cmd := exec.CommandContext(ctx, cfg.Binary, "--config_dir", cfg.Dir)
	cmd.Env = append(os.Environ(), "XDG_CACHE_HOME="+filepath.Join(cfg.RuntimeDir, "cache"))
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard // upstream output may contain credentials
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 3 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		scrub()
		return fail(err)
	}
	w := &Worker{Source: source, base: fmt.Sprintf("http://127.0.0.1:%d", port), client: &http.Client{Timeout: 4 * time.Second}, cancel: cancel, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		scrub()
		cancel()
		_ = source.Close()
		close(w.done)
		if parent.Err() == nil {
			exited(err)
		}
	}()
	go source.Run(ctx, func(error) { emit(Event{Type: "audio_error"}); cancel() })
	go w.events(ctx, emit)
	return w, nil
}

func (w *Worker) Close() {
	w.once.Do(func() { w.cancel(); <-w.done })
}

func (w *Worker) events(ctx context.Context, emit func(Event)) {
	for ctx.Err() == nil {
		conn, _, err := websocket.Dial(ctx, strings.Replace(w.base, "http:", "ws:", 1)+"/events", nil)
		if err == nil {
			conn.SetReadLimit(256 << 10)
			emit(Event{Type: "connected"})
			for {
				_, b, err := conn.Read(ctx)
				if err != nil {
					break
				}
				var ev Event
				if json.Unmarshal(b, &ev) == nil {
					emit(ev)
				}
			}
			_ = conn.CloseNow()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (w *Worker) Status(ctx context.Context) (model.PlayerStatus, error) {
	var v model.PlayerStatus
	err := w.request(ctx, "GET", "/status", nil, &v)
	return v, err
}

func (w *Worker) Command(ctx context.Context, action string, value any) error {
	allowed := map[string]bool{"pause": true, "resume": true, "next": true, "prev": true, "seek": true, "volume": true}
	if !allowed[action] {
		return errors.New("unsupported player action")
	}
	return w.request(ctx, "POST", "/player/"+action, value, nil)
}

func (w *Worker) request(ctx context.Context, method, path string, value, out any) error {
	var body io.Reader
	if value != nil {
		b, err := json.Marshal(value)
		if err != nil {
			return err
		}
		body = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, w.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.client.Do(req)
	if err != nil {
		return errors.New("Spotify 控制连接不可用")
	}
	defer resp.Body.Close()
	if resp.StatusCode == 204 {
		return errors.New("Spotify 尚未登录")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Spotify 控制失败（HTTP %d）", resp.StatusCode)
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(out)
	}
	return nil
}

// v0.9.0 writes credentials into state.json; older separate credential files
// remain supported by upstream. Read only the username and verify data exists.
func Credentials(dir string) (string, error) {
	for _, name := range []string{"state.json", "credentials.json"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var c struct {
			Username    string `json:"username"`
			Data        []byte `json:"data"`
			Credentials struct {
				Username string `json:"username"`
				Data     []byte `json:"data"`
			} `json:"credentials"`
		}
		if json.Unmarshal(b, &c) != nil {
			continue
		}
		if c.Username != "" && len(c.Data) > 0 {
			return c.Username, nil
		}
		if c.Credentials.Username != "" && len(c.Credentials.Data) > 0 {
			return c.Credentials.Username, nil
		}
	}
	return "", errors.New("credentials not saved")
}
