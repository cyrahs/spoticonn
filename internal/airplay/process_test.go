package airplay

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grandcat/zeroconf"
	"net"
	"spoticonn/internal/model"
)

func fakeBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "cliairplay")
	_ = os.WriteFile(path, []byte("#!/bin/sh\nexec '"+strings.ReplaceAll(exe, "'", "'\"'\"'")+"' -test.run=TestAirPlayEngineProcess -- \"$@\"\n"), 0700)
	t.Setenv("SPOTICONN_TEST_AIRPLAY", "1")
	return path
}
func TestRealSubprocessPairingProtocol(t *testing.T) {
	binary := fakeBinary(t)
	waiting := make(chan struct{}, 1)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, err := Pair(ctx, Config{Binary: binary}, model.Device{Address: "127.0.0.1", Port: 7000}, "test-id", func(s string) {
		if s == "waiting_pin" {
			waiting <- struct{}{}
		}
	}, func(c string, err error) {
		if err == nil && c != strings.Repeat("a", 192) {
			err = fmt.Errorf("bad credentials")
		}
		done <- err
	})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	select {
	case <-waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("PIN prompt not detected")
	}
	if p.PIN("1\n23") == nil {
		t.Fatal("unsafe PIN accepted")
	}
	if err := p.PIN("1234"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("pairing stuck")
	}
}
func TestSenderPCMStartFlushAndPersistentInput(t *testing.T) {
	binary := fakeBinary(t)
	commandLog := filepath.Join(t.TempDir(), "commands")
	t.Setenv("SPOTICONN_TEST_AIRPLAY_COMMANDS", commandLog)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan string, 8)
	s, err := Open(ctx, Config{Binary: binary, RuntimeDir: t.TempDir()}, model.Device{Address: "127.0.0.1", Port: 7000}, model.PairingSecret{}, 30, 44100, func(v string) {
		select {
		case events <- v:
		default:
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for i := 0; i < 2; i++ {
		if i > 0 {
			if err := s.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			if err := s.Standby(); err != nil {
				t.Fatal(err)
			}
		}
		s.Begin()
		if err := s.Write(make([]byte, 1764)); err != nil {
			t.Fatal(err)
		}
		select {
		case e := <-events:
			if e != "playing" {
				t.Fatal(e)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("did not start after first PCM frame")
		}
	}
	if err := s.Metadata(&model.Track{Name: "title\nACTION=STOP", URI: "id", Duration: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := s.Volume(50); err != nil {
		t.Fatal(err)
	}
	trace := waitForEngineCommands(t, commandLog, "VOLUME=50\n", 1)
	if strings.Count(trace, "VOLUME=30\n") != 3 || strings.Count(trace, "STATUS started") != 2 {
		t.Fatalf("initial, cold-start and resume volumes missing: %s", trace)
	}
}

func waitForEngineCommands(t *testing.T, path, command string, count int) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, _ := os.ReadFile(path)
		if strings.Count(string(b), command) >= count {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("engine did not consume %q %d times: %s", command, count, b)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSenderInitialAndPostStartVolumeAcrossReconnect(t *testing.T) {
	for _, volume := range []int{0, 30} {
		t.Run(strconv.Itoa(volume), func(t *testing.T) {
			binary := fakeBinary(t)
			commandLog := filepath.Join(t.TempDir(), "commands")
			t.Setenv("SPOTICONN_TEST_AIRPLAY_COMMANDS", commandLog)
			for attempt := 0; attempt < 2; attempt++ {
				events := make(chan string, 8)
				s, err := Open(t.Context(), Config{Binary: binary, RuntimeDir: t.TempDir()}, model.Device{Address: "127.0.0.1", Port: 7000}, model.PairingSecret{}, volume, 44100, func(v string) { events <- v })
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(s.Close)
				s.Begin()
				if err := s.Write(make([]byte, 1764)); err != nil {
					t.Fatal(err)
				}
				select {
				case event := <-events:
					if event != "playing" {
						t.Fatal(event)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("sender never started")
				}
				trace := waitForEngineCommands(t, commandLog, fmt.Sprintf("VOLUME=%d\n", volume), 2*(attempt+1))
				lines := strings.Split(strings.TrimSpace(trace), "\n")
				cycle := lines[attempt*5:]
				if len(cycle) != 5 || cycle[0] != fmt.Sprintf("VOLUME=%d", volume) || cycle[1] != "START_UNIX_MS=0" || cycle[2] != "ACTION=START" || !strings.HasPrefix(cycle[3], "STATUS started ") || cycle[4] != cycle[0] {
					t.Fatalf("incorrect initial/post-start volume order: %s", trace)
				}
				s.Close()
			}
		})
	}
}
func TestDiscoveryIdentitySurvivesAddressChange(t *testing.T) {
	e := &zeroconf.ServiceEntry{ServiceRecord: zeroconf.ServiceRecord{Instance: "Living room"}, HostName: "tv.local.", AddrIPv4: []net.IP{net.ParseIP("192.168.1.2")}, Port: 7000, TTL: 120, Text: []string{"deviceid=AA:BB:CC:DD:EE:FF", "model=AppleTV6,2"}}
	a, ok := DeviceFromEntry(e)
	if !ok {
		t.Fatal("valid device rejected")
	}
	e.AddrIPv4 = []net.IP{net.ParseIP("192.168.1.3")}
	b, _ := DeviceFromEntry(e)
	if a.ID != b.ID || a.ID != "aabbccddeeff" || a.Address == b.Address {
		t.Fatal("identity coupled to IP address")
	}
}
func TestAirPlayEngineProcess(t *testing.T) {
	if os.Getenv("SPOTICONN_TEST_AIRPLAY") != "1" {
		return
	}
	if path := os.Getenv("SPOTICONN_TEST_AIRPLAY_ARGS"); path != "" {
		_ = os.WriteFile(path, []byte(strings.Join(os.Args, "\n")), 0600)
	}
	if code := os.Getenv("SPOTICONN_TEST_AIRPLAY_ERROR"); code != "" {
		fmt.Printf("[STATUS] error code=%s http=401 detail=\"secret-from-engine\"\n", code)
		fmt.Println("[STATUS] error: generic failure")
		os.Exit(1)
	}
	var pipe string
	pair := false
	for i, v := range os.Args {
		if v == "--ptp-daemon" {
			for {
				time.Sleep(time.Second)
			}
		}
		if v == "--pair-setup" {
			pair = true
		}
		if v == "--cmdpipe" && i+1 < len(os.Args) {
			pipe = os.Args[i+1]
		}
	}
	if pair {
		fmt.Fprintln(os.Stderr, "Enter the PIN shown on the device:")
		sc := bufio.NewScanner(os.Stdin)
		if sc.Scan() && sc.Text() == "1234" {
			fmt.Println("CREDENTIALS: " + strings.Repeat("a", 192))
			os.Exit(0)
		}
		os.Exit(1)
	}
	f, err := os.OpenFile(pipe, os.O_RDWR, 0600)
	if err != nil {
		os.Exit(2)
	}
	var commandLog *os.File
	if path := os.Getenv("SPOTICONN_TEST_AIRPLAY_COMMANDS"); path != "" {
		commandLog, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			os.Exit(3)
		}
	}
	fmt.Fprintln(os.Stderr, "[STATUS] connected")
	fmt.Fprintln(os.Stdout, "[STATUS] clock_ready mode=ntp state=ready")
	var buffered atomic.Bool
	go func() {
		b := make([]byte, 1764)
		for {
			n, err := os.Stdin.Read(b)
			if n > 0 && buffered.CompareAndSwap(false, true) {
				fmt.Fprintln(os.Stderr, "[STATUS] audio buffered_ms=10")
			}
			if err == io.EOF || err != nil {
				os.Exit(0)
			}
		}
	}()
	sc := bufio.NewScanner(f)
	var requested int64
	join := false
	for sc.Scan() {
		if value, ok := strings.CutPrefix(sc.Text(), "START_UNIX_MS="); ok {
			requested, _ = strconv.ParseInt(value, 10, 64)
		}
		if commandLog != nil {
			fmt.Fprintln(commandLog, sc.Text())
		}
		switch sc.Text() {
		case "START_JOIN=1":
			join = true
		case "ACTION=START":
			at := time.Now().Add(25 * time.Millisecond).UnixMilli()
			if join {
				at = requested + 17
			}
			if commandLog != nil {
				fmt.Fprintf(commandLog, "STATUS started at_unix_ms=%d\n", at)
			}
			fmt.Fprintf(os.Stderr, "[STATUS] started requested_unix_ms=%d at_unix_ms=%d\n", requested, at)
			join = false
		case "ACTION=FLUSH":
			buffered.Store(false)
			fmt.Fprintln(os.Stderr, "[STATUS] flushed")
		case "ACTION=STOP":
			os.Exit(5)
		}
	}
	os.Exit(0)
}

func TestSenderJoinAcknowledgementAndDuplicateStart(t *testing.T) {
	binary := fakeBinary(t)
	events := make(chan string, 10)
	s, err := Open(context.Background(), Config{Binary: binary, RuntimeDir: t.TempDir()}, model.Device{Address: "127.0.0.1", Port: 7000}, model.PairingSecret{}, 23, 48000, func(state string) { events <- state })
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	at, err := s.Join(ctx, 12345)
	if err != nil || at != 12362 {
		t.Fatal("did not use sender acknowledgement", at, err)
	}
	if _, err = s.Join(ctx, 23456); err == nil {
		t.Fatal("duplicate join accepted")
	}
	s.Begin()
	if err = s.Write(make([]byte, 1920)); err != nil {
		t.Fatal(err)
	}
	<-events
	select {
	case <-events:
		t.Fatal("duplicate Begin sent another START")
	case <-time.After(30 * time.Millisecond):
	}
}

func TestSharedPTPRejectsImplicitNTPTimingFallback(t *testing.T) {
	binary := fakeBinary(t)
	s, err := Open(context.Background(), Config{Binary: binary, RuntimeDir: t.TempDir(), SharedPTP: true}, model.Device{Address: "127.0.0.1", Port: 7000}, model.PairingSecret{}, 23, 44100, func(string) {})
	if err == nil {
		s.Close()
		t.Fatal("mixed PTP and NTP timing accepted in a group")
	}
}

func TestMalformedAndUnsolicitedStartsCannotConfirmPlayback(t *testing.T) {
	for _, line := range []string{"[STATUS] started at_unix_ms=oops", "[STATUS] started at_unix_ms=0", "[STATUS] started requested_unix_ms=123"} {
		s := &Sender{startConfirmed: make(chan struct{}), startSent: true, startAcks: []uint64{0}, status: func(state string) {
			if state != "error" {
				t.Error("invalid start published")
			}
		}}
		s.scan(strings.NewReader(line + "\n"))
		if s.anchor != 0 {
			t.Fatal("invalid anchor accepted")
		}
	}
	s := &Sender{startConfirmed: make(chan struct{}), status: func(string) { t.Error("unsolicited start published") }}
	s.scan(strings.NewReader("[STATUS] started at_unix_ms=123\n"))
	if s.anchor != 0 {
		t.Fatal("unsolicited anchor accepted")
	}
}

func TestSenderPassesPasswordWithoutLosingPairedIdentity(t *testing.T) {
	binary := fakeBinary(t)
	argsPath := filepath.Join(t.TempDir(), "args")
	t.Setenv("SPOTICONN_TEST_AIRPLAY_ARGS", argsPath)
	secret := model.PairingSecret{DACP: "saved-id", Credentials: strings.Repeat("b", 192), Password: "sp ace;$literal"}
	s, err := Open(t.Context(), Config{Binary: binary, RuntimeDir: t.TempDir()}, model.Device{Address: "127.0.0.1", Port: 7000, TXT: map[string]string{"sf": "80"}}, secret, 30, 44100, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--password\n" + secret.Password, "--auth\n" + secret.Credentials, "--dacp\n" + secret.DACP, "--pw\ntrue"} {
		if !strings.Contains(string(args), want) {
			t.Fatal("missing literal authentication argument")
		}
	}
}

func TestSenderReturnsSafeAuthenticationErrors(t *testing.T) {
	for code, want := range map[string]error{"auth_required": ErrAuthRequired, "auth_failed": ErrAuthFailed, "connect_failed": nil} {
		t.Run(code, func(t *testing.T) {
			binary := fakeBinary(t)
			t.Setenv("SPOTICONN_TEST_AIRPLAY_ERROR", code)
			s, err := Open(t.Context(), Config{Binary: binary, RuntimeDir: t.TempDir()}, model.Device{Address: "127.0.0.1", Port: 7000}, model.PairingSecret{}, 30, 44100, func(string) {})
			if err == nil {
				s.Close()
				t.Fatal("authentication failure accepted")
			}
			if strings.Contains(err.Error(), "secret-from-engine") {
				t.Fatal("engine detail leaked")
			}
			if want != nil && !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}
			if want == nil && (errors.Is(err, ErrAuthRequired) || errors.Is(err, ErrAuthFailed)) {
				t.Fatal("network failure mislabeled as authentication")
			}
		})
	}
}
