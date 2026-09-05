package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestServeHelper(t *testing.T) {
	if os.Getenv("SPOTICONN_TEST_SERVER") != "1" {
		return
	}
	os.Args = []string{os.Args[0], "serve"}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestShutdownClosesActiveEventStreams(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	hash, err := bcrypt.GenerateFromPassword([]byte("smoke-test-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestServeHelper$")
	cmd.Env = append(os.Environ(), "SPOTICONN_TEST_SERVER=1", "SPOTICONN_LISTEN_ADDR="+address,
		"SPOTICONN_DATA_DIR="+t.TempDir(), "SPOTICONN_RUNTIME_DIR="+t.TempDir(),
		"SPOTICONN_ADMIN_PASSWORD_HASH_FILE=", "SPOTICONN_ADMIN_PASSWORD_HASH="+string(hash),
		"SPOTICONN_INTERFACE=spoticonn-test-no-interface")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	base := "http://" + address
	client := &http.Client{Timeout: time.Second}
	ready := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		resp, err := client.Get(base + "/healthz")
		if err == nil {
			resp.Body.Close()
			ready = resp.StatusCode == 200
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ready {
		t.Fatal("test server did not start")
	}
	request, _ := http.NewRequest("POST", base+"/api/auth/login", strings.NewReader(`{"password":"smoke-test-password"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Spoticonn-Request", "1")
	login, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	login.Body.Close()
	if login.StatusCode != 200 || len(login.Cookies()) == 0 {
		t.Fatalf("login: %d", login.StatusCode)
	}
	request, _ = http.NewRequest("GET", base+"/api/events", nil)
	request.AddCookie(login.Cookies()[0])
	stream, err := (&http.Client{}).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	if stream.StatusCode != 200 || stream.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal("event stream not established")
	}
	if err = cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("shutdown: %v\n%s", err, output.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("active SSE stream blocked shutdown")
	}
}
