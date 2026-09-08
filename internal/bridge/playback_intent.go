package bridge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"spoticonn/internal/model"
)

var errPlaybackSuperseded = errors.New("播放请求已失效")

// These fields are protected by Manager.mu, including on engine callback
// threads. Callbacks invalidate work without waiting for the lifecycle lock.
type playbackIntent struct {
	revision       uint64
	wantPlay       bool
	internalPaused bool
	pauseAck       chan struct{}
	cancel         context.CancelFunc
	joinContext    context.Context
	joinCancel     context.CancelFunc
	joinRevision   uint64
}

type playbackRequest struct {
	id                 string
	account            *runtimeAccount
	player             Player
	generation, intent uint64
	joinRevision       uint64
	target             string
}

func invalidateIntent(r *runtimeAccount, pause bool) {
	invalidateJoin(r)
	r.intent.revision++
	if pause {
		r.intent.wantPlay = false
		r.intent.internalPaused = false
	}
	if r.intent.cancel != nil {
		r.intent.cancel()
		r.intent.cancel = nil
	}
}

func invalidateJoin(r *runtimeAccount) {
	r.intent.joinRevision++
	cancelJoin(r)
}

func cancelJoin(r *runtimeAccount) {
	if r.intent.joinCancel != nil {
		r.intent.joinCancel()
	}
	r.intent.joinContext, r.intent.joinCancel = nil, nil
}

// Member retries share the current user intent, never the Spotify resume path.
// Repeated playing notifications reuse the lifetime until a transport change.
func (m *Manager) beginOutput(q playbackRequest) {
	m.mu.Lock()
	r := m.accounts[q.id]
	if r == nil || r != q.account || r.generation != q.generation || r.intent.revision != q.intent || r.intent.joinRevision != q.joinRevision || !r.intent.wantPlay {
		m.mu.Unlock()
		return
	}
	if r.intent.joinContext == nil {
		r.intent.joinContext, r.intent.joinCancel = context.WithCancel(m.ctx)
	}
	ctx := r.intent.joinContext
	m.mu.Unlock()
	if group, ok := m.output.(interface{ BeginWithContext(context.Context) }); ok {
		group.BeginWithContext(ctx)
	} else {
		m.output.Begin()
	}
}

func (m *Manager) observePlaybackEvent(e event) event {
	if e.observed || (e.kind != "spotify" && e.kind != "exit") {
		return e
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e.observed = true
	r := m.accounts[e.id]
	if r == nil || r.generation != e.generation {
		return e
	}
	switch e.spotify.Type {
	case "will_play", "seek":
		invalidateJoin(r)
	case "paused":
		// The pinned engine emits one paused event per pause command, even
		// when already paused. Consume exactly one acknowledgement, before
		// mailbox coalescing; never ignore every pause during recovery.
		if r.intent.pauseAck != nil {
			e.internalPause = true
			close(r.intent.pauseAck)
			r.intent.pauseAck = nil
		} else {
			invalidateIntent(r, true)
		}
	case "inactive", "stopped", "not_playing":
		invalidateIntent(r, true)
	}
	if e.kind == "exit" {
		invalidateIntent(r, true)
	}
	e.intent = r.intent.revision
	e.joinRevision = r.intent.joinRevision
	return e
}

// Interrupt requests at the API boundary, before waiting for a slow Open or
// Flush. The returned identity prevents a queued web command hitting a new
// account after a takeover.
func (m *Manager) interruptPlayback(id string, pause bool) (string, uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == "" {
		id = m.playback.AccountID
	}
	if r := m.accounts[id]; r != nil {
		invalidateIntent(r, pause)
		return id, r.generation
	}
	return id, 0
}

func (m *Manager) playbackRequest(id string) playbackRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.accounts[id]
	q := playbackRequest{id: id, account: r, target: m.store.Snapshot().Settings.TargetID}
	if r != nil {
		q.player, q.generation, q.intent = r.player, r.generation, r.intent.revision
		q.joinRevision = r.intent.joinRevision
	}
	return q
}

func (m *Manager) requestCurrent(q playbackRequest) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.accounts[q.id]
	return r != nil && r == q.account && r.player == q.player && r.generation == q.generation &&
		r.intent.revision == q.intent && r.intent.wantPlay && m.ctx.Err() == nil &&
		m.store.Snapshot().Settings.TargetID == q.target
}

func (m *Manager) wantsPlayback(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.accounts[id]
	return r != nil && r.player != nil && r.intent.wantPlay
}

func (m *Manager) pauseForOutput(ctx context.Context, q playbackRequest, source string) error {
	if !m.requestCurrent(q) {
		return errPlaybackSuperseded
	}
	live, err := q.player.Status(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	internal := q.account.intent.internalPaused
	m.mu.Unlock()
	m.diagnostic(fmt.Sprintf("播放控制：%s 暂停检查 paused=%t stopped=%t，操作=%d，会话=%d", source, live.Paused, live.Stopped, q.intent, q.generation))
	if live.Stopped || (live.Paused && !internal) {
		m.interruptPlayback(q.id, true)
		return errPlaybackSuperseded
	}
	if live.Paused {
		return nil
	}
	ack := make(chan struct{})
	m.mu.Lock()
	q.account.intent.pauseAck = ack
	m.mu.Unlock()
	if err := q.player.Command(ctx, "pause", nil); err != nil {
		m.mu.Lock()
		q.account.intent.pauseAck = nil
		invalidateIntent(q.account, true)
		m.mu.Unlock()
		return err
	}
	// Missing/ambiguous confirmation must fail closed rather than leaving a
	// pause allowance that could swallow a future user pause.
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-ack:
	case <-ctx.Done():
	case <-timer.C:
	}
	m.mu.Lock()
	confirmed := q.account.intent.pauseAck == nil
	q.account.intent.pauseAck = nil
	if !confirmed {
		invalidateIntent(q.account, true)
	}
	if q.account.intent.revision == q.intent && q.account.intent.wantPlay && confirmed {
		q.account.intent.internalPaused = true
	}
	m.mu.Unlock()
	if !confirmed {
		m.diagnostic("播放控制：内部暂停未确认，取消自动恢复")
	}
	if !m.requestCurrent(q) {
		return errPlaybackSuperseded
	}
	return nil
}

func (m *Manager) pauseOutput(ctx context.Context, stop bool) error {
	m.cancelOutputRecovery()
	m.detach()
	m.mu.Lock()
	r := m.accounts[m.playback.AccountID]
	m.mu.Unlock()
	if r != nil && r.player != nil {
		r.player.Drain()
	}
	m.setPlayback(func(p *model.Playback) { p.Status = "paused" })
	if m.output == nil {
		m.setPlayback(func(p *model.Playback) { p.OutputStatus = "disconnected" })
		return nil
	}
	if !stop && m.outputParked {
		return nil // a delayed Spotify acknowledgement must not flush twice
	}
	if group, ok := m.output.(interface{ CancelJoin() }); ok {
		group.CancelJoin()
	}
	if stop {
		m.closeOutput()
		m.setPlayback(func(p *model.Playback) { p.OutputStatus = "disconnected" })
		return nil
	}
	err := m.output.Flush(ctx)
	m.diagnostic(fmt.Sprintf("播放控制：用户暂停清缓冲 success=%t", err == nil))
	if err == nil {
		err = m.output.Standby()
	}
	if err != nil {
		m.failOutput("暂停时 AirPlay 清缓冲或待机失败，输出已关闭")
	} else {
		m.outputParked = true
		m.setPlayback(func(p *model.Playback) { p.OutputStatus = "connected" })
	}
	return err
}
