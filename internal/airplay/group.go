package airplay

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"spoticonn/internal/model"
)

type Output interface {
	Begin()
	Write([]byte) error
	Flush(context.Context) error
	Standby() error
	Volume(int) error
	Metadata(*model.Track) error
	Close()
}

type timedOutput interface {
	Output
	Started(context.Context) (int64, error)
	Join(context.Context, int64) (int64, error)
}
type openMember func(context.Context, Config, model.Device, model.PairingSecret, int, int, func(string)) (timedOutput, error)

// groupOutput keeps optional-member I/O off the HomePod's feed. Its single
// cycle worker owns Open/Join/Close for the TV. Cancelling and joining that
// worker before another Begin guarantees no overlapping physical sessions.
type groupOutput struct {
	lifecycle          sync.Mutex
	mu                 sync.Mutex
	ctx                context.Context
	cancel             context.CancelFunc
	cfg                Config
	primary            timedOutput
	tv                 model.Device
	pair               model.PairingSecret
	open               openMember
	clock              *sharedClock
	rate, volume       int
	track              *model.Track
	status, diagnostic func(string)
	cycle              *groupCycle
	closed             bool
	ring               []byte
	total              int64
	retryDelays        []time.Duration // nil uses the bounded production policy
}
type groupCycle struct {
	ctx         context.Context
	intent      context.Context
	cancel      context.CancelFunc
	done        chan struct{}
	changed     chan struct{}
	queue       chan []byte
	feeding     bool
	skip        int64
	epoch       uint64
	attempt     *memberAttempt
	blocked     *memberFailure // a HomePod failure invalidates every member attempt
	startCancel context.CancelFunc
}

var groupEpoch atomic.Uint64

type memberFailure struct {
	role, reason, phase string
	queue, primeBytes   int
	elapsedMS, deltaMS  int64
}

type memberAttempt struct {
	ctx          context.Context
	cancel       context.CancelFunc
	phase        string
	failure      *memberFailure
	primeBytes   int
	deltaMS      int64
	writeStarted time.Time
}

func OpenHomeTheater(ctx context.Context, cfg Config, target Target, pairings map[string]model.PairingSecret, volume, rate int, status, diagnostic func(string)) (Output, error) {
	pod, tv, ok := target.HomeTheater()
	if !ok {
		return nil, errors.New("组合成员不明确")
	}
	if err := target.Ready(); err != nil {
		return nil, err
	}
	child, cancel := context.WithCancel(ctx)
	clock, clockErr := startSharedClock(child, cfg)
	cfg.SharedPTP = clockErr == nil
	g := &groupOutput{ctx: child, cancel: cancel, cfg: cfg, tv: tv, pair: pairings[tv.ID], clock: clock, rate: rate, volume: volume, status: status, diagnostic: diagnostic}
	g.open = func(ctx context.Context, cfg Config, d model.Device, p model.PairingSecret, volume, rate int, cb func(string)) (timedOutput, error) {
		if cfg.SharedPTP && !clockAlive() {
			return nil, errors.New("共享 AirPlay 时钟已断开")
		}
		return Open(ctx, cfg, d, p, volume, rate, cb)
	}
	diagnostic("组合角色：HomePod 接收初始音频，Apple TV 为待加入成员；两成员的远程控制回传至当前 Spotify 会话")
	primary, err := g.open(child, cfg, pod, pairings[pod.ID], volume, rate, g.primaryStatus)
	if err != nil {
		cancel()
		if clock != nil {
			clock.Close()
		}
		return nil, fmt.Errorf("HomePod（%s）：%w", displayName(pod.Name), err)
	}
	g.primary = primary
	if clock != nil && clock.done != nil {
		go func() {
			select {
			case <-child.Done():
			case <-clock.done:
				if child.Err() == nil {
					g.status("error")
				}
			}
		}()
	}
	if clockErr != nil {
		diagnostic("共享时钟不可用，本次仅尝试 HomePod 播放")
	}
	return g, nil
}

func (g *groupOutput) primaryStatus(s string) {
	switch s {
	case "timeline_changed", "error", "disconnected", "clock_stalled":
		g.mu.Lock()
		if c := g.cycle; c != nil && c.ctx.Err() == nil {
			if c.blocked == nil {
				c.blocked = &memberFailure{role: "homepod", reason: s, phase: "start"}
				if a := c.attempt; a != nil {
					c.blocked.phase = a.phase
					g.failMemberLocked(c, a, "homepod", s)
				}
				if c.startCancel != nil {
					c.startCancel()
				}
			}
			select {
			case c.changed <- struct{}{}:
			default:
			}
		}
		g.mu.Unlock()
	}
	if s != "timeline_changed" {
		g.status(s)
	}
}

func (g *groupOutput) report(state string) {
	g.status(state)
	g.diagnostic("组合状态：" + state)
}

func (g *groupOutput) reportCycle(c *groupCycle, state string) {
	if c.ctx.Err() != nil {
		return
	}
	if g.cfg.GroupStatus != nil {
		g.cfg.GroupStatus(c.intent, state)
	} else {
		g.status(state)
	}
	g.diagnostic("组合状态：" + state)
}

func (g *groupOutput) Begin() {
	g.BeginWithContext(g.ctx)
}

// The bridge supplies an independent playback-intent lifetime. Cancelling it
// stops only the optional member, including while a transport command waits.
func (g *groupOutput) BeginWithContext(intent context.Context) {
	g.lifecycle.Lock()
	defer g.lifecycle.Unlock()
	g.mu.Lock()
	if g.closed || g.ctx.Err() != nil || intent.Err() != nil || g.cycle != nil {
		g.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(intent)
	stop := context.AfterFunc(g.ctx, cancel)
	c := &groupCycle{ctx: ctx, intent: intent, cancel: cancel, done: make(chan struct{}), changed: make(chan struct{}, 1), epoch: groupEpoch.Add(1)}
	g.cycle = c
	g.mu.Unlock()
	g.reportCycle(c, "group_starting")
	g.primary.Begin()
	go func() { defer stop(); defer cancel(); g.run(c) }()
}

// Only known engine events and local error classes enter this record. The first
// failure wins, before cancelling I/O can produce secondary disconnect errors.
func (g *groupOutput) failMemberLocked(c *groupCycle, a *memberAttempt, role, reason string) {
	if a == nil || c.attempt != a || c.ctx.Err() != nil || a.ctx.Err() != nil || a.failure != nil {
		return
	}
	f := &memberFailure{role: role, reason: reason, phase: a.phase, queue: len(c.queue), primeBytes: a.primeBytes, deltaMS: a.deltaMS}
	if !a.writeStarted.IsZero() {
		f.elapsedMS = time.Since(a.writeStarted).Milliseconds()
	}
	a.failure = f
	c.feeding = false
	a.cancel()
}

func (g *groupOutput) failMember(c *groupCycle, a *memberAttempt, reason string) {
	g.mu.Lock()
	g.failMemberLocked(c, a, "apple_tv", reason)
	g.mu.Unlock()
}

func (g *groupOutput) reportFailure(c *groupCycle, attempt int, f *memberFailure, retry bool, delay time.Duration) {
	if c.ctx.Err() != nil {
		return
	}
	g.diagnostic(fmt.Sprintf("组合故障：cycle=%d attempt=%d role=%s phase=%s reason=%s queue=%d prime_bytes=%d elapsed_ms=%d start_delta_ms=%d retry=%t backoff_ms=%d",
		c.epoch, attempt, f.role, f.phase, f.reason, f.queue, f.primeBytes, f.elapsedMS, f.deltaMS, retry, delay.Milliseconds()))
	state := "group_degraded_" + f.reason
	if f.role == "homepod" {
		state = "group_degraded_homepod_" + f.reason
	}
	if retry {
		state = "group_retrying_" + f.reason
	}
	g.reportCycle(c, state)
}

// Waiting for new PCM after each failure prevents a quiet/paused source from
// triggering a new member session solely because a backoff timer elapsed.
func (g *groupOutput) waitAudio(c *groupCycle, anchor, after int64) *memberFailure {
	ctx, cancel := context.WithTimeout(c.ctx, 20*time.Second)
	defer cancel()
	for {
		if c.ctx.Err() != nil {
			return nil
		}
		g.mu.Lock()
		blocked, total := c.blocked, g.total
		g.mu.Unlock()
		if blocked != nil {
			return blocked
		}
		if total > after && time.Now().UnixMilli() >= anchor {
			return nil
		}
		select {
		case <-ctx.Done():
			return &memberFailure{role: "homepod", phase: "start", reason: "unstable"}
		case <-c.changed:
		}
	}
}

func (g *groupOutput) run(c *groupCycle) {
	defer close(c.done)
	defer func() { g.mu.Lock(); c.feeding = false; g.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(c.ctx, 20*time.Second)
	g.mu.Lock()
	c.startCancel = cancel
	if c.blocked != nil {
		cancel()
	}
	g.mu.Unlock()
	anchor, err := g.primary.Started(ctx)
	cancel()
	g.mu.Lock()
	c.startCancel = nil
	blocked := c.blocked
	g.mu.Unlock()
	if err != nil {
		if c.ctx.Err() == nil {
			if blocked != nil {
				g.reportFailure(c, 0, blocked, false, 0)
				return
			}
			g.reportFailure(c, 0, &memberFailure{role: "homepod", phase: "start", reason: "start"}, false, 0)
			g.status("error")
		}
		return
	}
	if f := g.waitAudio(c, anchor, 0); f != nil {
		g.reportFailure(c, 0, f, false, 0)
		return
	}
	if c.ctx.Err() != nil {
		return
	}
	if !g.tv.Online {
		g.reportFailure(c, 0, &memberFailure{role: "apple_tv", phase: "connect", reason: "offline"}, false, 0)
		return
	}
	if !g.cfg.SharedPTP {
		g.reportFailure(c, 0, &memberFailure{role: "apple_tv", phase: "connect", reason: "clock"}, false, 0)
		return
	}
	delays := g.retryDelays
	if delays == nil {
		delays = []time.Duration{time.Second, 3 * time.Second}
	}
	for attempt := 1; ; attempt++ {
		if c.ctx.Err() != nil {
			return
		}
		a := g.runMember(c, anchor, attempt)
		if c.ctx.Err() != nil {
			return
		}
		g.mu.Lock()
		f, blocked := a.failure, c.blocked
		g.mu.Unlock()
		if f == nil {
			return
		}
		retry := blocked == nil && f.recoverable() && attempt <= len(delays)
		var delay time.Duration
		if retry {
			delay = delays[attempt-1]
		}
		g.reportFailure(c, attempt, f, retry, delay)
		if blocked != nil && f.role != "homepod" {
			g.reportFailure(c, attempt, blocked, false, 0)
		}
		if !retry {
			return
		}
		timer := time.NewTimer(delay)
		select {
		case <-c.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		g.mu.Lock()
		after := g.total
		g.mu.Unlock()
		if f := g.waitAudio(c, anchor, after); f != nil {
			g.reportFailure(c, attempt, f, false, 0)
			return
		}
	}
}

func (f *memberFailure) recoverable() bool {
	if f.role != "apple_tv" {
		return false
	}
	switch f.reason {
	case "disconnected", "write_timeout", "pipe_closed", "backpressure":
		return true
	}
	return false
}

// Each attempt has its own cancellation and queue. Close completes before the
// next Open, and callbacks from a retired attempt cannot affect its successor.
func (g *groupOutput) runMember(c *groupCycle, anchor int64, number int) *memberAttempt {
	ctx, cancel := context.WithCancel(c.ctx)
	a := &memberAttempt{ctx: ctx, cancel: cancel, phase: "connect"}
	g.mu.Lock()
	c.attempt = a
	c.queue = make(chan []byte, 128)
	volume, blocked := g.volume, c.blocked
	if blocked != nil {
		a.failure = blocked
		cancel()
	}
	g.mu.Unlock()
	var tv timedOutput
	defer func() {
		g.mu.Lock()
		c.feeding = false
		c.attempt = nil
		c.queue = nil
		cancel()
		g.mu.Unlock()
		if tv != nil {
			tv.Close()
		}
	}()
	if ctx.Err() != nil {
		return a
	}
	if number == 1 {
		g.reportCycle(c, "group_joining")
	}
	var err error
	tv, err = g.open(ctx, g.cfg, g.tv, g.pair, volume, g.rate, func(s string) {
		switch s {
		case "error", "disconnected", "clock_stalled", "timeline_changed":
			g.failMember(c, a, s)
		}
	})
	if err != nil {
		reason := "connect"
		if errors.Is(err, ErrAuthRequired) || errors.Is(err, ErrAuthFailed) {
			reason = "auth"
			// A generic engine error may have arrived just before Open returns
			// its more specific, typed authentication failure.
			g.mu.Lock()
			if a.failure != nil && a.failure.role == "apple_tv" && a.failure.reason == "error" {
				a.failure.reason = reason
			}
			g.mu.Unlock()
		}
		g.failMember(c, a, reason)
		return a
	}
	if ctx.Err() != nil {
		return a
	}
	g.mu.Lock()
	a.phase = "control"
	volume, track := g.volume, g.track
	g.mu.Unlock()
	if err := tv.Volume(volume); err != nil {
		g.failMember(c, a, "control")
		return a
	}
	if err := tv.Metadata(track); err != nil {
		g.failMember(c, a, "control")
		return a
	}
	g.mu.Lock()
	a.phase = "join"
	g.mu.Unlock()
	setup, finishSetup := context.WithTimeout(ctx, 20*time.Second)
	requested := time.Now().Add(500 * time.Millisecond).UnixMilli()
	actual, err := tv.Join(setup, requested)
	finishSetup()
	if err != nil {
		g.failMember(c, a, "start")
		return a
	}
	if ctx.Err() != nil {
		return a
	}
	g.mu.Lock()
	prime, skip, mapErr := joinPCM(g.ring, g.total, anchor, actual, g.rate)
	a.deltaMS, a.primeBytes = actual-requested, len(prime)
	if mapErr == nil && ctx.Err() == nil && c.blocked == nil {
		c.skip, c.feeding = skip, true
	}
	g.mu.Unlock()
	if mapErr != nil {
		g.failMember(c, a, "timeline")
		return a
	}
	if ctx.Err() != nil {
		return a
	}
	g.diagnostic(fmt.Sprintf("组合加入：cycle=%d attempt=%d role=apple_tv phase=join prime_bytes=%d start_delta_ms=%d skip_frames=%d", c.epoch, number, len(prime), actual-requested, skip/4))
	if len(prime) > 0 && !g.writeMember(c, a, tv, prime, "prime") {
		return a
	}
	if ctx.Err() != nil {
		return a
	}
	g.mu.Lock()
	a.phase = "stream"
	g.mu.Unlock()
	g.reportCycle(c, "group_joined")
	// Apply controls again: they may have changed during Open/Join/prime.
	select {
	case c.changed <- struct{}{}:
	default:
	}
	for {
		if ctx.Err() != nil {
			return a
		}
		select {
		case <-ctx.Done():
			return a
		case data := <-c.queue:
			if !g.writeMember(c, a, tv, data, "stream") {
				return a
			}
		case <-c.changed:
			g.mu.Lock()
			a.phase = "control"
			volume, track := g.volume, g.track
			g.mu.Unlock()
			if err := tv.Volume(volume); err != nil {
				g.failMember(c, a, "control")
				return a
			}
			if err := tv.Metadata(track); err != nil {
				g.failMember(c, a, "control")
				return a
			}
			g.mu.Lock()
			a.phase = "stream"
			g.mu.Unlock()
		}
	}
}

func (g *groupOutput) writeMember(c *groupCycle, a *memberAttempt, tv timedOutput, data []byte, phase string) bool {
	if a.ctx.Err() != nil {
		return false
	}
	g.mu.Lock()
	a.phase, a.writeStarted = phase, time.Now()
	g.mu.Unlock()
	err := tv.Write(data)
	if err != nil {
		g.failMember(c, a, audioWriteReason(err))
	}
	g.mu.Lock()
	a.writeStarted = time.Time{}
	g.mu.Unlock()
	return err == nil && a.ctx.Err() == nil
}

// joinPCM maps the acknowledged instant to absolute s16le stereo frame
// coordinates. The ring may start/end mid-frame; only the first byte is rounded
// forward. Out-of-window acknowledgements degrade rather than inventing audio.
func joinPCM(ring []byte, total, origin, at int64, rate int) ([]byte, int64, error) {
	if at < origin || at-origin > 24*60*60*1000 || (rate != 44100 && rate != 48000) {
		return nil, 0, errors.New("invalid join timeline")
	}
	first := ((at-origin)*int64(rate) + 999) / 1000 * 4
	head := total - int64(len(ring))
	if first < head {
		return nil, 0, errors.New("join precedes retained audio")
	}
	if first >= total {
		return nil, first - total, nil
	}
	return append([]byte(nil), ring[first-head:]...), 0, nil
}

func (g *groupOutput) Write(b []byte) error {
	// Source.Route serializes this against lifecycle mutations. The mutex also
	// makes the PCM position and member attachment one atomic boundary.
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return errors.New("AirPlay 已断开")
	}
	if err := g.primary.Write(b); err != nil {
		return err
	}
	g.total += int64(len(b))
	g.ring = append(g.ring, b...)
	limit := g.rate * 4 * 10 // bounded ten-second history, no disk persistence
	if len(g.ring) > limit {
		g.ring = g.ring[len(g.ring)-limit:]
	}
	if c := g.cycle; c != nil && c.ctx.Err() == nil {
		// Signal only while starting; control updates use changed after joining.
		if !c.feeding {
			select {
			case c.changed <- struct{}{}:
			default:
			}
		}
		if c.feeding {
			n := min(int64(len(b)), c.skip)
			c.skip -= n
			b = b[n:]
			if len(b) > 0 {
				select {
				case c.queue <- append([]byte(nil), b...):
				default:
					c.feeding = false
					g.failMemberLocked(c, c.attempt, "apple_tv", "backpressure")
				}
			}
		}
	}
	return nil
}

func (g *groupOutput) cancelCycle(reason string) {
	g.mu.Lock()
	c := g.cycle
	if c != nil {
		c.cancel()
	}
	g.mu.Unlock()
	if c != nil {
		<-c.done
		g.diagnostic(fmt.Sprintf("组合取消：cycle=%d reason=%s", c.epoch, reason))
	}
	g.mu.Lock()
	g.cycle = nil
	g.ring = nil
	g.total = 0
	g.mu.Unlock()
}

// CancelJoin stops a pending or joined optional member as soon as the user
// requests a transport change, before Spotify's asynchronous event arrives.
func (g *groupOutput) CancelJoin() {
	g.lifecycle.Lock()
	defer g.lifecycle.Unlock()
	g.mu.Lock()
	c := g.cycle
	if c != nil {
		c.cancel()
	}
	g.mu.Unlock()
	if c != nil {
		<-c.done
		g.diagnostic(fmt.Sprintf("组合取消：cycle=%d reason=transport", c.epoch))
	}
	g.report("group_waiting")
}

func (g *groupOutput) Flush(ctx context.Context) error {
	g.lifecycle.Lock()
	defer g.lifecycle.Unlock()
	g.cancelCycle("flush")
	g.report("group_waiting")
	return g.primary.Flush(ctx)
}
func (g *groupOutput) Standby() error {
	g.lifecycle.Lock()
	defer g.lifecycle.Unlock()
	g.cancelCycle("standby")
	g.report("group_waiting")
	return g.primary.Standby()
}
func (g *groupOutput) Volume(v int) error {
	g.mu.Lock()
	g.volume = v
	if c := g.cycle; c != nil && c.feeding {
		select {
		case c.changed <- struct{}{}:
		default:
		}
	}
	g.mu.Unlock()
	g.diagnostic(fmt.Sprintf("HomePod 音量命令：%d", v))
	return g.primary.Volume(v)
}
func (g *groupOutput) Metadata(t *model.Track) error {
	g.mu.Lock()
	if t != nil {
		copied := *t
		copied.Artists = append([]string(nil), t.Artists...)
		g.track = &copied
	} else {
		g.track = nil
	}
	if c := g.cycle; c != nil && c.feeding {
		select {
		case c.changed <- struct{}{}:
		default:
		}
	}
	g.mu.Unlock()
	return g.primary.Metadata(t)
}
func (g *groupOutput) Close() {
	g.lifecycle.Lock()
	defer g.lifecycle.Unlock()
	g.mu.Lock()
	g.closed = true
	g.mu.Unlock()
	g.cancel()
	g.cancelCycle("close")
	g.primary.Close()
	if g.clock != nil {
		g.clock.Close()
	}
}
