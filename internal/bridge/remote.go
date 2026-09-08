package bridge

import (
	"context"
	"strings"
	"time"
)

// handleRemote runs under op, just like Spotify events and UI controls. Each
// callback is bound to both the output and the Spotify process that opened it.
func (m *Manager) handleRemote(e event) {
	c := e.remote
	if m.output == nil || e.generation != m.outputGeneration || c.Context == nil || c.Context.Err() != nil || c.ReceivedAt.Before(m.remoteBarrier) {
		return
	}
	m.mu.Lock()
	r := m.accounts[e.id]
	valid := m.playback.AccountID == e.id && r != nil && r.player != nil && r.generation == e.playerGeneration
	playback := m.playback
	device := m.devices[c.DeviceID]
	m.mu.Unlock()
	if !valid {
		return
	}

	key, window := "transport", 500*time.Millisecond
	switch c.Action {
	case "play", "pause", "play_pause":
	case "next", "previous":
		key, window = c.Action, 2*time.Second
	default:
		return
	}
	if m.remoteLast == nil {
		m.remoteLast = make(map[string]time.Time)
	}
	last := m.remoteLast[key]
	// Use arrival times, not dispatch times: commands can queue while connecting.
	// Both members share this sliding quiet window. A sustained receiver storm
	// cannot advance the queue every window or toggle a pause back into play.
	if !c.ReceivedAt.After(last) {
		return
	}
	m.remoteLast[key] = c.ReceivedAt
	if !last.IsZero() && c.ReceivedAt.Sub(last) < window {
		return
	}

	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	action := c.Action
	switch action {
	case "play", "pause", "play_pause":
		live, err := r.player.Status(ctx)
		if err != nil && action != "pause" {
			m.diagnostic("AirPlay 远程控制未执行：无法确认 Spotify 播放状态")
			return
		}
		paused := live.Paused || live.Stopped
		switch action {
		case "play":
			if !paused || live.Stopped {
				return // receiver confirmation, or a Spotify session that is no longer active
			}
			action = "resume"
		case "play_pause":
			if live.Stopped {
				return
			}
			action = "pause"
			if paused {
				action = "resume"
			}
		case "pause":
			if err == nil && paused && m.outputParked && playback.Status == "paused" && !playback.Recovering {
				return
			}
		}
	case "previous":
		action = "prev"
	}
	role := "接收端"
	switch {
	case strings.HasPrefix(strings.ToLower(device.Model), "appletv"):
		role = "Apple TV"
	case strings.HasPrefix(strings.ToLower(device.Model), "audioaccessory"):
		role = "HomePod"
	}
	// Only fixed roles and allowlisted actions are logged, never engine output,
	// device identifiers, credentials or account details.
	m.diagnostic("AirPlay 远程控制：" + role + " / " + c.Action + " → Spotify / " + action)
	if err := m.playbackCommand(ctx, action, 0); err != nil {
		m.diagnostic("AirPlay 远程控制执行失败，请通过 Spotify 或管理网页重试")
	}
}
