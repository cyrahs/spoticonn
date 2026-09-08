package airplay

import (
	"context"
	"strings"
	"testing"
	"time"

	"spoticonn/internal/model"
)

func TestRemoteEventContract(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var commands []RemoteCommand
	s := &Sender{ctx: ctx, deviceID: "tv", remoteControl: func(c RemoteCommand) { commands = append(commands, c) }}
	s.scan(strings.NewReader(strings.Join([]string{
		"[EVENT] remote command=play", "[EVENT] remote command=pause",
		"[EVENT] remote command=play_pause", "[EVENT] remote command=next",
		"[EVENT] remote command=previous",
		"[EVENT] remote command=stop", "[EVENT] remote command=volume",
		"[EVENT] remote command=pause secret", "[EVENT] remote command=",
		"debug [EVENT] remote command=pause", "TITLE=[EVENT] remote command=pause",
		"[STATUS] remote command=pause", "[EVENT] remote command=PAUSE",
	}, "\n")))
	if len(commands) != 5 {
		t.Fatalf("expected only the five upstream commands, got %d", len(commands))
	}
	for _, c := range commands {
		if c.DeviceID != "tv" || c.Context != ctx || c.ReceivedAt.IsZero() {
			t.Fatal("lost command origin or lifetime")
		}
	}
	cancel()
	s.scan(strings.NewReader("[EVENT] remote command=pause\n"))
	if len(commands) != 5 {
		t.Fatal("cancelled sender forwarded a command")
	}
}

func TestSenderRemoteControlFromSubprocessStdout(t *testing.T) {
	binary := fakeBinary(t)
	t.Setenv("SPOTICONN_TEST_AIRPLAY_REMOTE", "[EVENT] remote command=pause")
	commands := make(chan RemoteCommand, 1)
	s, err := Open(context.Background(), Config{
		Binary: binary, RuntimeDir: t.TempDir(),
		RemoteControl: func(c RemoteCommand) { commands <- c },
	}, model.Device{ID: "tv", Address: "127.0.0.1", Port: 7000}, model.PairingSecret{}, 30, 44100, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Metadata(&model.Track{Name: "test"}); err != nil {
		t.Fatal(err)
	}
	select {
	case c := <-commands:
		if c.Action != "pause" || c.DeviceID != "tv" || c.Context.Err() != nil {
			t.Fatal("incorrect callback")
		}
		s.Close()
		if c.Context.Err() == nil {
			t.Fatal("closing the sender left queued controls valid")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stdout remote event was discarded")
	}
}

func TestGroupMemberControlCarriesJoinLifetime(t *testing.T) {
	g, p, events := testGroup(t)
	commands := make(chan RemoteCommand, 1)
	g.cfg.RemoteControl = func(c RemoteCommand) { commands <- c }
	g.open = func(ctx context.Context, cfg Config, d model.Device, _ model.PairingSecret, _, _ int, _ func(string)) (timedOutput, error) {
		cfg.RemoteControl(RemoteCommand{Context: ctx, DeviceID: d.ID, Action: "pause", ReceivedAt: time.Now()})
		return &testMember{ctx: ctx, anchor: p.anchor + 10}, nil
	}
	close(p.ack)
	g.Begin()
	if err := g.Write(make([]byte, 44100*4)); err != nil {
		t.Fatal(err)
	}
	waitGroup(t, events, "group_joined")
	c := <-commands
	if c.Context.Err() != nil || c.DeviceID != "tv" {
		t.Fatal("optional member lost its control callback")
	}
	g.CancelJoin()
	if c.Context.Err() == nil {
		t.Fatal("cancelled join left queued member controls valid")
	}
}
