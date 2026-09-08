package bridge

import (
	"context"
	"strings"
	"testing"

	"spoticonn/internal/airplay"
	"spoticonn/internal/model"
	"spoticonn/internal/spotify"
)

type intentGroup struct {
	cancellableOutput
	intent context.Context
}

func (o *intentGroup) BeginWithContext(ctx context.Context) { o.intent = ctx; o.Begin() }

func setupIntentGroup(t *testing.T) (*Manager, *fakePlayer, *intentGroup, airplay.Config) {
	t.Helper()
	m, a, _, _ := setup(t)
	setupGroup(t, m)
	o := &intentGroup{}
	var cfg airplay.Config
	m.cfg.OpenGroupOutput = func(_ context.Context, c airplay.Config, _ airplay.Target, _ map[string]model.PairingSecret, _, _ int, _, _ func(string)) (Output, error) {
		cfg = c
		return o, nil
	}
	m.handle(playing("a", 1))
	drainEvents(m)
	if o.intent == nil || o.intent.Err() != nil {
		t.Fatal("missing active member intent")
	}
	return m, a, o, cfg
}

func TestTransportIntentCancelsMemberBeforeLifecycleLock(t *testing.T) {
	for _, action := range []string{"pause", "stop", "seek", "next", "prev", "target"} {
		t.Run(action, func(t *testing.T) {
			m, a, o, _ := setupIntentGroup(t)
			m.op.Lock()
			done := make(chan error, 1)
			go func() {
				if action == "target" {
					s := m.Snapshot().Settings
					s.TargetID = ""
					done <- m.UpdateSettings(context.Background(), s)
				} else {
					done <- m.Playback(context.Background(), action, 100)
				}
			}()
			awaitSignal(t, o.intent.Done())
			// Lifecycle lock is still held: cancellation cannot depend on sending a
			// Spotify command or waiting for its asynchronous event.
			assertCommands(t, a, "pause", "volume", "seek", "resume")
			m.op.Unlock()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSpotifyTransportCancelsMemberBeforeEventHandling(t *testing.T) {
	for _, kind := range []string{"paused", "stopped", "inactive", "not_playing", "will_play", "seek"} {
		t.Run(kind, func(t *testing.T) {
			m, a, o, _ := setupIntentGroup(t)
			m.emit(event{kind: "spotify", id: "a", generation: 1, spotify: spotify.Event{Type: kind}})
			awaitSignal(t, o.intent.Done())
			assertCommands(t, a, "pause", "volume", "seek", "resume")
		})
	}
}

func TestOldMemberStatusCannotOverwriteResumedPlayback(t *testing.T) {
	m, a, o, cfg := setupIntentGroup(t)
	old := o.intent
	cfg.GroupStatus(old, "group_retrying_disconnected")
	if err := m.Playback(context.Background(), "pause", 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Playback(context.Background(), "resume", 0); err != nil {
		t.Fatal(err)
	}
	m.handle(playing("a", 1))
	if o.intent == old || o.intent.Err() != nil {
		t.Fatal("resume reused cancelled member intent")
	}
	cfg.GroupStatus(o.intent, "group_joined")
	cfg.GroupStatus(old, "group_joined") // cannot coalesce away the new event
	cfg.GroupStatus(old, "group_degraded_timeline_changed")
	drainEvents(m)
	if view := m.Snapshot().Playback; view.GroupStatus != "group_joined" || view.GroupReason != "" {
		t.Fatalf("stale member status survived: %+v", view)
	}
	// A queued callback before cancellation also carries its old intent.
	m.handle(event{kind: "output", generation: m.outputGeneration, status: "group_retrying_disconnected", memberIntent: old})
	if m.Snapshot().Playback.GroupStatus != "group_joined" {
		t.Fatal("queued old retry survived")
	}
	assertCommands(t, a, "pause", "volume", "seek", "resume", "pause", "resume")
}

func TestAccountTakeoverCancelsPreviousMemberIntent(t *testing.T) {
	m, _, o, _ := setupIntentGroup(t)
	old := o.intent
	m.handle(playing("b", 1))
	if old.Err() == nil {
		t.Fatal("account takeover retained old member retries")
	}
	if o.intent == old || o.intent.Err() != nil {
		t.Fatal("new account lacks its own member intent")
	}
}

func TestMemberReasonsDistinguishFailureAndKeepPrimaryPlaying(t *testing.T) {
	m, a, o, cfg := setupIntentGroup(t)
	m.handle(event{kind: "output", generation: m.outputGeneration, status: "playing"})
	reasons := map[string]string{
		"timeline_changed": "Apple TV 时间线", "homepod_timeline_changed": "HomePod 时间线",
		"write_timeout": "写入超时", "pipe_closed": "管道关闭", "backpressure": "队列已满",
		"disconnected": "连接中断", "clock_stalled": "时钟停滞", "error": "引擎报告错误",
	}
	for reason, want := range reasons {
		cfg.GroupStatus(o.intent, "group_degraded_"+reason)
		drainEvents(m)
		view := m.Snapshot().Playback
		if !strings.Contains(view.GroupReason, want) || view.Recovering || view.Status != "playing" || m.output != o {
			t.Fatalf("incorrect member failure: %+v", view)
		}
	}
	for _, reason := range []string{"disconnected", "write_timeout", "pipe_closed", "backpressure"} {
		cfg.GroupStatus(o.intent, "group_retrying_"+reason)
		drainEvents(m)
		if !strings.Contains(m.Snapshot().Playback.GroupReason, "正在重试 Apple TV") {
			t.Fatal("retry reason missing")
		}
	}
	assertCommands(t, a, "pause", "volume", "seek", "resume")
}

func TestSupersededOperationCannotCreateAnotherMemberIntent(t *testing.T) {
	m, _, o, _ := setupIntentGroup(t)
	q := m.playbackRequest("a")
	old := o.intent
	queued := m.observePlaybackEvent(playing("a", 1))
	m.mu.Lock()
	invalidateJoin(m.accounts["a"])
	m.mu.Unlock()
	m.beginOutput(q) // an in-flight operation reaches Begin after user next/seek
	m.handle(queued) // a playing notification was already queued before that change
	if o.intent != old || o.intent.Err() == nil {
		t.Fatal("superseded operation recreated a member intent")
	}
}

func TestRepeatedPlayingKeepsCurrentMemberIntent(t *testing.T) {
	m, _, o, _ := setupIntentGroup(t)
	old := o.intent
	m.handle(playing("a", 1))
	if o.intent != old || old.Err() != nil {
		t.Fatal("duplicate playing cancelled the active member")
	}
}
