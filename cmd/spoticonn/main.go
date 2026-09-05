package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/sys/unix"
	"spoticonn/internal/bridge"
	"spoticonn/internal/httpapi"
	"spoticonn/internal/store"
	"spoticonn/internal/webui"
)

func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
func main() {
	if err := run(); err != nil {
		slog.Error("Spoticonn stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) > 1 && os.Args[1] == "hash-password" {
		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			return errors.New("read password from stdin")
		}
		p := scanner.Text()
		if len(p) < 12 || len(p) > 72 {
			return errors.New("password must be 12–72 bytes")
		}
		h, err := bcrypt.GenerateFromPassword([]byte(p), 12)
		if err != nil {
			return err
		}
		fmt.Println(string(h))
		return nil
	}
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		c := http.Client{Timeout: 3 * time.Second}
		r, err := c.Get("http://" + env("SPOTICONN_LISTEN_ADDR", "127.0.0.1:8080") + "/healthz")
		if err != nil {
			return err
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			return errors.New("unhealthy")
		}
		return nil
	}
	if len(os.Args) > 1 && os.Args[1] != "serve" {
		return errors.New("usage: spoticonn [serve|hash-password|healthcheck]")
	}
	unix.Umask(0077)
	dir := env("SPOTICONN_DATA_DIR", "./data")
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	dir = abs
	state, err := store.Open(dir)
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("another Spoticonn process owns this data directory")
	}
	hash := os.Getenv("SPOTICONN_ADMIN_PASSWORD_HASH")
	if file := os.Getenv("SPOTICONN_ADMIN_PASSWORD_HASH_FILE"); file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		hash = strings.TrimSpace(string(b))
	}
	if hash == "" {
		p := os.Getenv("SPOTICONN_ADMIN_PASSWORD")
		if len(p) < 12 || len(p) > 72 {
			return errors.New("set SPOTICONN_ADMIN_PASSWORD_HASH_FILE or a 12–72 byte SPOTICONN_ADMIN_PASSWORD")
		}
		b, err := bcrypt.GenerateFromPassword([]byte(p), 12)
		if err != nil {
			return err
		}
		hash = string(b)
	}
	// Child audio engines should never inherit the management password.
	_ = os.Unsetenv("SPOTICONN_ADMIN_PASSWORD")
	_ = os.Unsetenv("SPOTICONN_ADMIN_PASSWORD_HASH")
	runtimeDir, err := os.MkdirTemp(env("SPOTICONN_RUNTIME_DIR", os.TempDir()), "spoticonn-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(runtimeDir)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	m := bridge.New(ctx, state, bridge.Config{SpotifyBinary: env("SPOTICONN_SPOTIFY_BINARY", "go-librespot"), AirPlayBinary: env("SPOTICONN_AIRPLAY_BINARY", "cliairplay"), RuntimeDir: runtimeDir, Interface: os.Getenv("SPOTICONN_INTERFACE")})
	handler, err := httpapi.New(m, hash, os.Getenv("SPOTICONN_SECURE_COOKIES") == "true", webui.Files())
	if err != nil {
		return err
	}
	m.Run()
	defer m.Close()
	server := &http.Server{Addr: env("SPOTICONN_LISTEN_ADDR", "127.0.0.1:8080"), Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
		BaseContext: func(net.Listener) context.Context { return ctx }, // close SSE streams on shutdown
	}
	errCh := make(chan error, 1)
	go func() { slog.Info("Spoticonn listening", "address", server.Addr); errCh <- server.ListenAndServe() }()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}
