package airplay

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
}
type groupCycle struct {
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	changed chan struct{}
	failed  chan string
	queue   chan []byte
	feeding bool
	skip    int64
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
	if s == "timeline_changed" {
		g.mu.Lock()
		if c := g.cycle; c != nil {
			select {
			case c.failed <- "timeline_changed":
			default:
			}
		}
		g.mu.Unlock()
		return
	}
	g.status(s)
}

func (g *groupOutput) report(state string) {
	// Only fixed role/state codes reach diagnostics; no engine output or args.
	g.status(state)
	g.diagnostic("组合状态：" + state)
}

func (g *groupOutput) Begin() {
	g.lifecycle.Lock()
	defer g.lifecycle.Unlock()
	g.mu.Lock()
	if g.closed || g.cycle != nil {
		g.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(g.ctx)
	c := &groupCycle{ctx: ctx, cancel: cancel, done: make(chan struct{}), changed: make(chan struct{}, 1), failed: make(chan string, 1), queue: make(chan []byte, 128)}
	g.cycle = c
	g.mu.Unlock()
	g.report("group_starting")
	g.primary.Begin()
	go g.run(c)
}

func (g *groupOutput) run(c *groupCycle) {
	defer close(c.done)
	defer func() { g.mu.Lock(); c.feeding = false; g.mu.Unlock() }()
	ctx, cancel := context.WithTimeout(c.ctx, 20*time.Second)
	defer cancel()
	anchor, err := g.primary.Started(ctx)
	if err != nil {
		if c.ctx.Err() == nil {
			g.status("error")
		}
		return
	}
	g.diagnostic("HomePod 已确认开始；等待实际开始时间及持续音频")
	// Wait on confirmed playout plus continued PCM, not a guessed join delay.
	for {
		select {
		case <-ctx.Done():
			if c.ctx.Err() == nil {
				g.report("group_degraded_unstable")
			}
			return
		case <-c.failed:
			g.report("group_degraded_timeline")
			return
		case <-c.changed:
			if time.Now().UnixMilli() >= anchor {
				goto stable
			}
		}
	}
stable:
	if !g.tv.Online {
		g.report("group_degraded_offline")
		return
	}
	if !g.cfg.SharedPTP {
		g.report("group_degraded_clock")
		return
	}
	g.report("group_joining")
	g.mu.Lock()
	volume := g.volume
	g.mu.Unlock()
	tv, err := g.open(c.ctx, g.cfg, g.tv, g.pair, volume, g.rate, func(s string) {
		switch s {
		case "error", "disconnected", "clock_stalled", "timeline_changed":
			select {
			case c.failed <- s:
			default:
			}
		}
	})
	if err != nil {
		if c.ctx.Err() == nil {
			if errors.Is(err, ErrAuthRequired) || errors.Is(err, ErrAuthFailed) {
				g.report("group_degraded_auth")
			} else {
				g.report("group_degraded_connect")
			}
		}
		return
	}
	defer tv.Close()
	// The process inherits the cycle, not the setup timeout. Open itself has a
	// bounded readiness wait; only Join uses the remaining setup deadline.
	g.mu.Lock()
	volume, track := g.volume, g.track
	g.mu.Unlock()
	if err := tv.Volume(volume); err != nil {
		g.report("group_degraded_control")
		return
	}
	if err := tv.Metadata(track); err != nil {
		g.report("group_degraded_control")
		return
	}
	g.diagnostic(fmt.Sprintf("Apple TV 加入前应用当前音量：%d", volume))
	actual, err := tv.Join(ctx, time.Now().Add(500*time.Millisecond).UnixMilli())
	if err != nil {
		if c.ctx.Err() == nil {
			g.report("group_degraded_start")
		}
		return
	}
	select {
	case <-c.failed:
		g.report("group_degraded_timeline")
		return
	default:
	}
	g.mu.Lock()
	// Reject a lost/changed timeline; do not restart HomePod to rescue the TV.
	prime, skip, mapErr := joinPCM(g.ring, g.total, anchor, actual, g.rate)
	if mapErr == nil && c.ctx.Err() == nil {
		c.skip, c.feeding = skip, true
	}
	g.mu.Unlock()
	if mapErr != nil {
		g.report("group_degraded_timeline")
		return
	}
	if c.ctx.Err() != nil {
		return
	}
	select {
	case c.changed <- struct{}{}:
	default:
	}
	g.diagnostic(fmt.Sprintf("Apple TV 开始确认：相对 HomePod %d ms；待跳过 %d 帧", actual-anchor, skip/4))
	if len(prime) > 0 {
		if err := tv.Write(prime); err != nil {
			g.report("group_degraded_audio")
			return
		}
	}
	g.report("group_joined")
	// Setup succeeded. Keep the TV alive until the parent cycle ends.
	for {
		select {
		case <-c.ctx.Done():
			return
		case <-c.failed:
			g.report("group_degraded_member")
			return
		case data := <-c.queue:
			if err := tv.Write(data); err != nil {
				g.report("group_degraded_audio")
				return
			}
		case <-c.changed:
			g.mu.Lock()
			volume, track := g.volume, g.track
			g.mu.Unlock()
			if err := tv.Volume(volume); err != nil {
				g.report("group_degraded_control")
				return
			}
			if err := tv.Metadata(track); err != nil {
				g.report("group_degraded_control")
				return
			}
		}
	}
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
	if c := g.cycle; c != nil {
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
					select {
					case c.failed <- "backpressure":
					default:
					}
				}
			}
		}
	}
	return nil
}

func (g *groupOutput) cancelCycle() {
	g.mu.Lock()
	c := g.cycle
	if c != nil {
		c.cancel()
	}
	g.mu.Unlock()
	if c != nil {
		<-c.done
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
	}
	g.report("group_waiting")
}

func (g *groupOutput) Flush(ctx context.Context) error {
	g.lifecycle.Lock()
	defer g.lifecycle.Unlock()
	g.cancelCycle()
	g.report("group_waiting")
	return g.primary.Flush(ctx)
}
func (g *groupOutput) Standby() error {
	g.lifecycle.Lock()
	defer g.lifecycle.Unlock()
	g.cancelCycle()
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
	g.cancelCycle()
	g.primary.Close()
	if g.clock != nil {
		g.clock.Close()
	}
}
