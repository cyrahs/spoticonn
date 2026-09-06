package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"spoticonn/internal/airplay"
	"spoticonn/internal/model"
	"spoticonn/internal/spotify"
	"spoticonn/internal/store"
)

type Player interface {
	Status(context.Context) (model.PlayerStatus, error)
	Command(context.Context, string, any) error
	Route(int, func([]byte) error)
	Drain()
	Close()
}
type Output interface {
	Begin()
	Write([]byte) error
	Flush(context.Context) error
	Standby() error
	Volume(int) error
	Metadata(*model.Track) error
	Close()
}
type Config struct {
	SpotifyBinary, AirPlayBinary, RuntimeDir, Interface string
	StartPlayer                                         func(context.Context, spotify.Config, func(spotify.Event), func(error)) (Player, error)
	OpenOutput                                          func(context.Context, airplay.Config, model.Device, model.PairingSecret, int, int, func(string)) (Output, error)
	DisableDiscovery                                    bool
	ExchangeAuthorization                               func(context.Context, *spotify.Authorization, string) (*spotify.Login, error)
}
type runtimeAccount struct {
	player            Player
	generation        uint64
	status, errorText string
	retryAt           time.Time
	failures          int
	statusFailures    int
	last              model.PlayerStatus
	authorization     *spotify.Authorization
	login             *spotify.Login
}
type event struct {
	kind, id   string
	generation uint64
	spotify    spotify.Event
	status     string
}
type Manager struct {
	mu                                 sync.Mutex // snapshots and runtime fields
	op                                 sync.Mutex // all lifecycle and playback mutations, including event handling
	store                              *store.Store
	cfg                                Config
	ctx                                context.Context
	cancel                             context.CancelFunc
	done                               chan struct{}
	accounts                           map[string]*runtimeAccount
	devices                            map[string]model.Device
	output                             Output
	outputGeneration                   uint64
	outputRetryAt                      time.Time
	outputRetryAccount                 string
	outputFailures                     int
	playback                           model.Playback
	diagnostics                        []model.Diagnostic
	pairing                            *airplay.Pairing
	pairingView                        *model.PairingView
	eventMu                            sync.Mutex
	events                             []event
	wake                               chan struct{}
	subs                               map[chan struct{}]struct{}
	spotifyAvailable, airplayAvailable bool
}

func New(parent context.Context, s *store.Store, cfg Config) *Manager {
	ctx, cancel := context.WithCancel(parent)
	if cfg.ExchangeAuthorization == nil {
		cfg.ExchangeAuthorization = spotify.ExchangeAuthorization
	}
	if cfg.StartPlayer == nil {
		cfg.StartPlayer = func(c context.Context, v spotify.Config, e func(spotify.Event), x func(error)) (Player, error) {
			return spotify.Start(c, v, e, x)
		}
	}
	if cfg.OpenOutput == nil {
		cfg.OpenOutput = func(c context.Context, v airplay.Config, d model.Device, p model.PairingSecret, vol, rate int, cb func(string)) (Output, error) {
			return airplay.Open(c, v, d, p, vol, rate, cb)
		}
	}
	_, se := exec.LookPath(cfg.SpotifyBinary)
	_, ae := exec.LookPath(cfg.AirPlayBinary)
	m := &Manager{store: s, cfg: cfg, ctx: ctx, cancel: cancel, done: make(chan struct{}), accounts: map[string]*runtimeAccount{}, devices: s.Snapshot().Devices, wake: make(chan struct{}, 1), subs: map[chan struct{}]struct{}{}, spotifyAvailable: se == nil, airplayAvailable: ae == nil, playback: model.Playback{Status: "idle", OutputStatus: "disconnected", UpdatedAt: time.Now()}}
	return m
}

func (m *Manager) Run() {
	go m.loop()
	if !m.cfg.DisableDiscovery {
		go m.discover()
	}
}
func (m *Manager) Close() { m.cancel(); <-m.done }
func (m *Manager) emit(e event) {
	if m.ctx.Err() != nil {
		return
	}
	// Engine callbacks must never wait for the lifecycle lock: closing a
	// process waits for its output readers. Coalesce repeated notifications,
	// keeping their latest order; playback events are checked against live state.
	m.eventMu.Lock()
	for i := len(m.events) - 1; i >= 0; i-- {
		old := m.events[i]
		if old.kind == e.kind && old.id == e.id && (old.generation < e.generation ||
			(old.generation == e.generation && old.spotify.Type == e.spotify.Type && old.status == e.status)) {
			m.events = append(m.events[:i], m.events[i+1:]...)
		}
	}
	m.events = append(m.events, e)
	m.eventMu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

func (m *Manager) takeEvents() []event {
	m.eventMu.Lock()
	defer m.eventMu.Unlock()
	events := m.events
	m.events = nil
	return events
}
func (m *Manager) changed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for ch := range m.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
func (m *Manager) Subscribe() (chan struct{}, func()) {
	m.mu.Lock()
	ch := make(chan struct{}, 1)
	m.subs[ch] = struct{}{}
	m.mu.Unlock()
	return ch, func() { m.mu.Lock(); delete(m.subs, ch); m.mu.Unlock() }
}
func (m *Manager) diagnostic(message string) {
	m.mu.Lock()
	m.diagnostics = append(m.diagnostics, model.Diagnostic{Time: time.Now(), Message: message})
	if len(m.diagnostics) > 60 {
		m.diagnostics = m.diagnostics[len(m.diagnostics)-60:]
	}
	m.mu.Unlock()
	m.changed()
}
func (m *Manager) setPlayback(fn func(*model.Playback)) {
	m.mu.Lock()
	fn(&m.playback)
	m.playback.UpdatedAt = time.Now()
	m.mu.Unlock()
	m.changed()
}

func (m *Manager) Snapshot() model.Snapshot {
	state := m.store.Snapshot()
	m.mu.Lock()
	defer m.mu.Unlock()
	v := model.Snapshot{Settings: state.Settings, Accounts: []model.AccountView{}, Devices: []model.DeviceView{}, Playback: m.playback, Diagnostics: append([]model.Diagnostic{}, m.diagnostics...), SpotifyAvailable: m.spotifyAvailable, AirPlayAvailable: m.airplayAvailable}
	if m.playback.Track != nil {
		t := *m.playback.Track
		v.Playback.Track = &t
	}
	if m.pairingView != nil {
		p := *m.pairingView
		v.Pairing = &p
	}
	for _, a := range state.Accounts {
		av := model.AccountView{Account: a, Status: "starting"}
		if r := m.accounts[a.ID]; r != nil {
			av.Status, av.Error = r.status, r.errorText
			if r.authorization != nil && r.status == "waiting_oauth" && time.Now().Before(r.authorization.ExpiresAt) {
				av.Authorization = &model.AuthorizationView{URL: r.authorization.URL, ExpiresAt: r.authorization.ExpiresAt}
			}
		}
		v.Accounts = append(v.Accounts, av)
	}
	for _, target := range airplay.Targets(m.devices, time.Now()) {
		v.Devices = append(v.Devices, target.View(state.Pairings))
		// Older installations may have selected a member before grouping was
		// supported. Keep the corresponding group selected without moving keys.
		if target.Matches(state.Settings.TargetID) {
			v.Settings.TargetID = target.Device.ID
		}
		if v.Pairing != nil && target.Matches(v.Pairing.DeviceID) {
			v.Pairing.DeviceID = target.Device.ID
		}
	}
	return v
}

func (m *Manager) loop() {
	defer close(m.done)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	m.op.Lock()
	m.reconcile()
	m.op.Unlock()
	for {
		select {
		case <-m.ctx.Done():
			m.op.Lock()
			m.detach()
			m.closeOutput()
			m.mu.Lock()
			var players []Player
			for _, r := range m.accounts {
				if r.player != nil {
					players = append(players, r.player)
				}
			}
			pair := m.pairing
			m.mu.Unlock()
			if pair != nil {
				pair.Close()
			}
			for _, p := range players {
				p.Close()
			}
			m.op.Unlock()
			return
		case <-m.wake:
			m.op.Lock()
			for _, e := range m.takeEvents() {
				m.handle(e)
			}
			m.op.Unlock()
		case <-tick.C:
			m.op.Lock()
			m.reconcile()
			m.op.Unlock()
		}
	}
}

func (m *Manager) discover() {
	for m.ctx.Err() == nil {
		ctx, cancel := context.WithTimeout(m.ctx, 25*time.Second)
		err := airplay.Discover(ctx, m.cfg.Interface, func(d model.Device) { m.mu.Lock(); m.devices[d.ID] = d; m.mu.Unlock(); m.changed() })
		if err != nil {
			m.diagnostic("AirPlay 发现失败，请检查 SPOTICONN_INTERFACE 和主机网络")
		}
		<-ctx.Done()
		cancel()
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

func (m *Manager) airplayConfig() airplay.Config {
	c := airplay.Config{Binary: m.cfg.AirPlayBinary, RuntimeDir: m.cfg.RuntimeDir}
	if m.cfg.Interface != "" {
		if i, err := net.InterfaceByName(m.cfg.Interface); err == nil {
			if addresses, err := i.Addrs(); err == nil {
				for _, a := range addresses {
					ip, _, err := net.ParseCIDR(a.String())
					if err == nil && ip.To4() != nil {
						c.InterfaceIP = ip.String()
						break
					}
				}
			}
		}
	}
	return c
}

func (m *Manager) start(a model.Account) {
	m.mu.Lock()
	r := m.accounts[a.ID]
	if r == nil {
		r = &runtimeAccount{}
		m.accounts[a.ID] = r
	}
	if !a.Bound && r.login == nil {
		if r.status != "oauth_error" && r.status != "authorizing" {
			if r.authorization == nil {
				r.authorization = spotify.NewAuthorization(a.AddedAt.Add(spotify.AuthorizationLifetime))
			}
			r.status, r.errorText = "waiting_oauth", ""
		}
		m.mu.Unlock()
		return
	}
	r.generation++
	generation := r.generation
	r.status = "starting"
	r.errorText = ""
	r.statusFailures = 0
	login := r.login
	m.mu.Unlock()
	state := m.store.Snapshot()
	name := state.Settings.Name
	cfg := spotify.Config{Binary: m.cfg.SpotifyBinary, Dir: m.store.AccountDir(a.ID), RuntimeDir: m.cfg.RuntimeDir, Name: name, Account: a, Login: login, Volume: state.Settings.Volume}
	p, err := m.cfg.StartPlayer(m.ctx, cfg, func(ev spotify.Event) { m.emit(event{kind: "spotify", id: a.ID, generation: generation, spotify: ev}) }, func(error) { m.emit(event{kind: "exit", id: a.ID, generation: generation}) })
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		r.status = "error"
		r.errorText = "Spotify 引擎启动失败，请检查二进制、配置和凭据"
		r.retryAt = time.Now().Add(30 * time.Second)
		return
	}
	r.player = p
	r.status = "connecting"
}

func (m *Manager) reconcile() {
	for _, a := range m.store.Snapshot().Accounts {
		m.mu.Lock()
		r := m.accounts[a.ID]
		if r == nil {
			r = &runtimeAccount{}
			m.accounts[a.ID] = r
		}
		m.mu.Unlock()
		// Recover credentials saved just before a crash, even when the initial
		// authorization window has elapsed since the service stopped.
		if !a.Bound {
			if username, err := spotify.Credentials(m.store.AccountDir(a.ID)); err == nil {
				m.bind(a, username)
				continue
			}
		}
		if !a.Bound && time.Since(a.AddedAt) > spotify.AuthorizationLifetime {
			if r != nil && r.player != nil {
				m.stopAccount(a.ID)
			}
			m.mu.Lock()
			if m.accounts[a.ID] == nil {
				m.accounts[a.ID] = &runtimeAccount{}
			}
			r.authorization, r.login = nil, nil
			if r.status != "duplicate" {
				r.status = "expired"
				r.errorText = "授权已超时，请重新登录"
			}
			m.mu.Unlock()
			continue
		}
		if !a.Bound && r.login == nil {
			m.start(a)
			continue
		}
		if !a.Bound && !time.Now().Before(r.login.ExpiresAt) {
			m.stopAccount(a.ID)
			m.mu.Lock()
			r.login, r.authorization = nil, nil
			r.status, r.errorText = "oauth_error", "Spotify 登录信息已过期，请重新登录"
			m.mu.Unlock()
			continue
		}
		if r == nil || (r.player == nil && time.Now().After(r.retryAt)) {
			m.start(a)
			continue
		}
		if r.player == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(m.ctx, 1500*time.Millisecond)
		status, err := r.player.Status(ctx)
		cancel()
		if err == nil {
			m.mu.Lock()
			r.last = status
			r.statusFailures = 0
			r.errorText = ""
			r.failures = 0
			if a.Bound {
				r.status = "online"
			}
			current := m.playback.AccountID == a.ID
			m.mu.Unlock()
			if current {
				m.setPlayback(func(p *model.Playback) {
					p.Track = status.Track
					if status.Paused || status.Stopped {
						if p.Status == "playing" {
							p.Status = "paused"
						}
					}
				})
			}
		} else if a.Bound {
			m.mu.Lock()
			r.statusFailures++
			unresponsive := r.statusFailures >= 5
			m.mu.Unlock()
			if unresponsive {
				m.stopAccount(a.ID)
				m.mu.Lock()
				r.status = "error"
				r.errorText = "Spotify 控制连接无响应，正在重启会话"
				r.retryAt = time.Now().Add(5 * time.Second)
				m.mu.Unlock()
				m.diagnostic("Spotify 控制连接无响应，正在重启会话")
			}
		}
	}
	if m.outputRetryAccount != "" && time.Now().After(m.outputRetryAt) {
		m.mu.Lock()
		r := m.accounts[m.outputRetryAccount]
		current := m.playback.AccountID == m.outputRetryAccount
		m.mu.Unlock()
		if !current {
			m.cancelOutputRecovery()
		} else if r != nil && r.player != nil {
			ctx, cancel := context.WithTimeout(m.ctx, 2*time.Second)
			status, err := r.player.Status(ctx)
			cancel()
			if err == nil && !status.Stopped && status.Track != nil {
				if err = m.acquire(m.outputRetryAccount, status); err != nil {
					m.failOutput(err.Error())
				} else {
					m.cancelOutputRecovery()
					m.outputFailures = 0
				}
			} else {
				m.outputRetryAt = time.Now().Add(5 * time.Second)
			}
		}
	}
	m.changed()
}

func (m *Manager) bind(a model.Account, username string) {
	for _, existing := range m.store.Snapshot().Accounts {
		if existing.ID != a.ID && existing.Username == username && existing.Bound {
			m.stopAccount(a.ID)
			// Do not let a restart briefly log this duplicate account in again.
			_ = os.RemoveAll(m.store.AccountDir(a.ID))
			_ = m.store.Update(func(s *store.State) error {
				for i := range s.Accounts {
					if s.Accounts[i].ID == a.ID {
						s.Accounts[i].AddedAt = time.Now().Add(-11 * time.Minute)
					}
				}
				return nil
			})
			m.mu.Lock()
			r := m.accounts[a.ID]
			r.login, r.authorization = nil, nil
			r.status = "duplicate"
			r.errorText = "此 Spotify 账号已绑定，请删除这条重复记录"
			r.retryAt = time.Now().Add(365 * 24 * time.Hour)
			m.mu.Unlock()
			return
		}
	}
	m.stopAccount(a.ID)
	m.mu.Lock()
	if r := m.accounts[a.ID]; r != nil {
		r.login, r.authorization = nil, nil
	}
	m.mu.Unlock()
	err := m.store.Update(func(s *store.State) error {
		for i := range s.Accounts {
			if s.Accounts[i].ID == a.ID {
				s.Accounts[i].Username = username
				s.Accounts[i].Bound = true
				a = s.Accounts[i]
				return nil
			}
		}
		return errors.New("account removed")
	})
	if err != nil {
		m.diagnostic("保存 Spotify 账号失败，请检查持久卷")
		return
	}
	m.start(a)
	m.diagnostic("Spotify 账号已绑定，正在建立常驻会话")
}

func (m *Manager) stopAccount(id string) {
	m.mu.Lock()
	r := m.accounts[id]
	if r == nil {
		m.mu.Unlock()
		return
	}
	r.generation++
	p := r.player
	r.player = nil
	current := m.playback.AccountID == id
	m.mu.Unlock()
	if p != nil {
		p.Route(44100, nil)
		p.Close()
	}
	if current {
		m.cancelOutputRecovery()
		m.closeOutput()
		m.setPlayback(func(v *model.Playback) {
			v.AccountID = ""
			v.Status = "idle"
			v.OutputStatus = "disconnected"
			v.Track = nil
		})
	}
}

func (m *Manager) handle(e event) {
	if e.kind == "output" {
		if e.generation != m.outputGeneration {
			return
		}
		switch e.status {
		case "playing":
			m.setPlayback(func(p *model.Playback) {
				if p.Status != "paused" {
					p.Status = "playing"
					p.OutputStatus = "playing"
					p.Error = ""
				}
			})
		case "clock_stalled", "error", "disconnected":
			m.failOutput("AirPlay 连接中断；Spotify 已暂停，可重试播放")
		}
		return
	}
	m.mu.Lock()
	r := m.accounts[e.id]
	valid := r != nil && r.generation == e.generation
	p := Player(nil)
	if valid {
		p = r.player
	}
	m.mu.Unlock()
	if !valid {
		return
	}
	if e.kind == "exit" {
		m.mu.Lock()
		r.player = nil
		r.status = "error"
		r.errorText = "Spotify 会话已退出，正在自动重连"
		r.failures++
		delay := min(60, 1<<min(r.failures, 6))
		r.retryAt = time.Now().Add(time.Duration(delay) * time.Second)
		current := m.playback.AccountID == e.id
		m.mu.Unlock()
		if current {
			m.failOutput("Spotify 会话断开，正在重连")
		}
		m.changed()
		return
	}
	if p == nil {
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	switch e.spotify.Type {
	case "playing":
		status, err := p.Status(ctx)
		if err != nil || status.Paused || status.Stopped {
			return
		} // stale events must never take over
		m.mu.Lock()
		r.last = status
		m.mu.Unlock()
		if err = m.acquire(e.id, status); err != nil {
			m.failOutput(err.Error())
		}
	case "paused", "inactive", "stopped", "not_playing":
		m.mu.Lock()
		current := m.playback.AccountID == e.id
		m.mu.Unlock()
		if current {
			// Our own pause/seek/resume sequence can enqueue a pause event that
			// arrives after resume. Reconcile it with the live player first.
			if live, err := p.Status(ctx); err == nil && !live.Paused && !live.Stopped && e.spotify.Type != "inactive" {
				return
			}
			m.detach()
			if m.output != nil {
				if err := m.output.Flush(ctx); err != nil {
					m.failOutput(err.Error())
					return
				}
				_ = m.output.Standby()
			}
			if e.spotify.Type == "inactive" || e.spotify.Type == "stopped" {
				m.cancelOutputRecovery()
			}
			m.setPlayback(func(v *model.Playback) { v.Status = "paused" })
		}
	case "will_play", "seek":
		m.mu.Lock()
		current := m.playback.AccountID == e.id
		m.mu.Unlock()
		if current && m.output != nil {
			m.detach()
			p.Drain()
			if err := m.output.Flush(ctx); err != nil {
				m.failOutput(err.Error())
				return
			}
			if e.spotify.Type == "seek" {
				status, err := p.Status(ctx)
				if err == nil && !status.Paused && !status.Stopped {
					m.output.Begin()
					p.Route(sampleRate(status.Track), m.output.Write)
					_ = m.output.Metadata(status.Track)
				}
			}
		}
	case "metadata":
		m.mu.Lock()
		current := m.playback.AccountID == e.id
		m.mu.Unlock()
		if current {
			var t model.Track
			if json.Unmarshal(e.spotify.Data, &t) == nil {
				m.setPlayback(func(v *model.Playback) { v.Track = &t })
				if m.output != nil {
					_ = m.output.Metadata(&t)
				}
			}
		}
	case "volume":
		m.mu.Lock()
		current := m.playback.AccountID == e.id
		m.mu.Unlock()
		if current {
			var data struct {
				Value int `json:"value"`
				Max   int `json:"max"`
			}
			if json.Unmarshal(e.spotify.Data, &data) == nil && data.Max > 0 {
				_ = m.setVolume(ctx, data.Value*100/data.Max, false)
			}
		}
	case "audio_error":
		m.mu.Lock()
		current := m.playback.AccountID == e.id
		m.mu.Unlock()
		if current {
			m.failOutput("音频管道中断，正在恢复 Spotify 会话")
		}
	}
	m.changed()
}

func sampleRate(t *model.Track) int {
	if t != nil && t.SampleRate > 0 {
		return t.SampleRate
	}
	return 44100
}
func (m *Manager) detach() {
	m.mu.Lock()
	var ps []Player
	for _, r := range m.accounts {
		if r.player != nil {
			ps = append(ps, r.player)
		}
	}
	m.mu.Unlock()
	for _, p := range ps {
		p.Route(44100, nil)
	}
}
func (m *Manager) closeOutput() {
	m.outputGeneration++
	if m.output != nil {
		m.output.Close()
		m.output = nil
	}
}

func (m *Manager) acquire(id string, status model.PlayerStatus) error {
	m.mu.Lock()
	old := m.playback.AccountID
	r := m.accounts[id]
	previous := m.accounts[old]
	m.mu.Unlock()
	if r == nil || r.player == nil {
		return errors.New("Spotify 会话不可用")
	}
	state := m.store.Snapshot()
	m.mu.Lock()
	target, exists := airplay.ResolveTarget(m.devices, state.Settings.TargetID, time.Now())
	m.mu.Unlock()
	if !exists || state.Settings.TargetID == "" {
		_ = r.player.Command(m.ctx, "pause", nil)
		return errors.New("请先选择 AirPlay 输出设备")
	}
	rate := sampleRate(status.Track)
	if rate != 44100 && rate != 48000 {
		_ = r.player.Command(m.ctx, "pause", nil)
		return errors.New("当前音频采样率不受支持")
	}
	ctx, cancel := context.WithTimeout(m.ctx, 20*time.Second)
	defer cancel()
	if old == id && m.output != nil {
		m.output.Begin()
		r.player.Route(rate, m.output.Write)
		_ = m.output.Metadata(status.Track)
		m.setPlayback(func(v *model.Playback) { v.Status = "playing"; v.Error = ""; v.Track = status.Track })
		return nil
	}
	if err := target.Ready(); err != nil {
		_ = r.player.Command(m.ctx, "pause", nil)
		return err
	}
	m.detach()
	if previous != nil && previous.player != nil && old != id {
		if err := previous.player.Command(ctx, "pause", nil); err != nil {
			m.diagnostic("旧账号暂停失败，音频输出已隔离")
		}
		previous.player.Drain()
	}
	// Freeze the new account while AirPlay connects, then rewind to its request
	// position so draining startup PCM cannot skip the beginning of a song.
	if err := r.player.Command(ctx, "pause", nil); err != nil {
		return err
	}
	r.player.Drain()
	m.closeOutput()
	m.setPlayback(func(v *model.Playback) {
		v.AccountID = id
		v.Status = "buffering"
		v.OutputStatus = "connecting"
		v.Error = ""
		v.Track = status.Track
	})
	generation := m.outputGeneration
	output, err := m.cfg.OpenOutput(m.ctx, m.airplayConfig(), target.Device, state.Pairings[target.Device.ID], state.Settings.Volume, rate, func(s string) { m.emit(event{kind: "output", generation: generation, status: s}) })
	if err != nil {
		return err
	}
	m.output = output
	m.cancelOutputRecovery()
	if err = r.player.Command(ctx, "volume", map[string]any{"volume": state.Settings.Volume}); err != nil {
		m.closeOutput()
		return err
	}
	if status.Track != nil {
		if err = r.player.Command(ctx, "seek", map[string]any{"position": status.Track.Position}); err != nil {
			m.closeOutput()
			return err
		}
	}
	r.player.Drain()
	output.Begin()
	_ = output.Metadata(status.Track)
	r.player.Route(rate, output.Write)
	if err = r.player.Command(ctx, "resume", nil); err != nil {
		m.detach()
		m.closeOutput()
		return err
	}
	m.setPlayback(func(v *model.Playback) { v.Status = "buffering"; v.OutputStatus = "connected" })
	return nil
}

func (m *Manager) failOutput(message string) {
	m.detach()
	m.closeOutput()
	m.mu.Lock()
	r := m.accounts[m.playback.AccountID]
	id := m.playback.AccountID
	m.mu.Unlock()
	if r != nil && r.player != nil {
		ctx, cancel := context.WithTimeout(m.ctx, 3*time.Second)
		_ = r.player.Command(ctx, "pause", nil)
		cancel()
	}
	m.setPlayback(func(p *model.Playback) { p.Status = "paused"; p.OutputStatus = "error"; p.Error = message })
	if id != "" && m.store.Snapshot().Settings.TargetID != "" {
		m.outputFailures++
		m.outputRetryAccount = id
		m.outputRetryAt = time.Now().Add(time.Duration(min(60, 1<<min(m.outputFailures, 6))) * time.Second)
		m.setPlayback(func(p *model.Playback) { p.Recovering = true })
	}
	m.diagnostic(message)
}

func (m *Manager) cancelOutputRecovery() {
	m.outputRetryAccount = ""
	m.setPlayback(func(p *model.Playback) { p.Recovering = false })
}

func (m *Manager) AddAccount(label string) (model.Account, error) {
	m.op.Lock()
	defer m.op.Unlock()
	label = strings.TrimSpace(label)
	if label == "" || len([]rune(label)) > 40 {
		return model.Account{}, errors.New("账号备注需为 1–40 个字符")
	}
	state := m.store.Snapshot()
	for _, a := range state.Accounts {
		if !a.Bound && time.Since(a.AddedAt) < spotify.AuthorizationLifetime {
			return model.Account{}, errors.New("请先完成或删除正在登录的账号")
		}
	}
	a := model.Account{ID: store.ID(12), DeviceID: store.ID(20), Label: label, AddedAt: time.Now()}
	if err := m.store.Update(func(s *store.State) error { s.Accounts = append(s.Accounts, a); return nil }); err != nil {
		return model.Account{}, err
	}
	m.start(a)
	m.changed()
	return a, nil
}
func (m *Manager) DeleteAccount(id string) error {
	m.op.Lock()
	defer m.op.Unlock()
	exists := false
	for _, a := range m.store.Snapshot().Accounts {
		if a.ID == id {
			exists = true
		}
	}
	if !exists {
		return errors.New("账号不存在")
	}
	m.stopAccount(id)
	if err := m.store.Update(func(s *store.State) error {
		for i, a := range s.Accounts {
			if a.ID == id {
				s.Accounts = append(s.Accounts[:i], s.Accounts[i+1:]...)
				break
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := os.RemoveAll(m.store.AccountDir(id)); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.accounts, id)
	m.mu.Unlock()
	m.changed()
	return nil
}
func (m *Manager) Rebind(id string) error {
	m.op.Lock()
	defer m.op.Unlock()
	state := m.store.Snapshot()
	var account *model.Account
	for i := range state.Accounts {
		a := &state.Accounts[i]
		if a.ID == id {
			account = a
		} else if !a.Bound && time.Since(a.AddedAt) < spotify.AuthorizationLifetime {
			return errors.New("请先完成其他账号的登录")
		}
	}
	if account == nil {
		return errors.New("账号不存在")
	}
	m.stopAccount(id)
	// Preserve the device identity, but remove both upstream credential formats.
	for _, name := range []string{"credentials.json", "state.json", "config.yml"} {
		if err := os.Remove(filepath.Join(m.store.AccountDir(id), name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	account.Bound = false
	account.Username = ""
	account.AddedAt = time.Now()
	if err := m.store.Update(func(s *store.State) error {
		for i := range s.Accounts {
			if s.Accounts[i].ID == id {
				s.Accounts[i] = *account
			}
		}
		return nil
	}); err != nil {
		return err
	}
	m.mu.Lock()
	if r := m.accounts[id]; r != nil {
		r.login, r.authorization = nil, nil
		r.status = ""
	}
	m.mu.Unlock()
	m.start(*account)
	m.changed()
	return nil
}

func (m *Manager) UpdateSettings(ctx context.Context, s model.Settings) error {
	m.op.Lock()
	defer m.op.Unlock()
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" || len([]rune(s.Name)) > 60 || strings.ContainsAny(s.Name, "\r\n") {
		return errors.New("播放器名称需为 1–60 个字符")
	}
	if s.Volume < 0 || s.Volume > 100 {
		return errors.New("音量范围为 0–100")
	}
	old := m.store.Snapshot().Settings
	m.mu.Lock()
	target, exists := airplay.ResolveTarget(m.devices, s.TargetID, time.Now())
	previousTarget, previousExists := airplay.ResolveTarget(m.devices, old.TargetID, time.Now())
	active := m.playback.AccountID
	m.mu.Unlock()
	if s.TargetID != "" && !exists {
		return errors.New("输出设备不存在")
	}
	if exists {
		s.TargetID = target.Device.ID
	}
	if previousExists {
		old.TargetID = previousTarget.Device.ID
	}
	if old.TargetID != s.TargetID && exists {
		if err := target.Ready(); err != nil {
			return err
		}
	}
	// A placeholder is only for presenting a group whose leader is unknown.
	// Preserve an existing physical selection while changing name or volume.
	if exists && target.Group && target.LeaderID == "" {
		s.TargetID = m.store.Snapshot().Settings.TargetID
		old.TargetID = s.TargetID
	}
	var resume *model.PlayerStatus
	if old.TargetID != s.TargetID {
		m.cancelOutputRecovery()
		m.mu.Lock()
		r := m.accounts[active]
		m.mu.Unlock()
		if r != nil && r.player != nil {
			v, err := r.player.Status(ctx)
			if err == nil && !v.Paused && !v.Stopped {
				resume = &v
			}
			m.detach()
			if err := r.player.Command(ctx, "pause", nil); err != nil {
				return err
			}
		}
		m.closeOutput()
		m.setPlayback(func(v *model.Playback) {
			v.Status = "paused"
			v.OutputStatus = "disconnected"
			v.Error = ""
		})
	}
	if err := m.store.Update(func(st *store.State) error {
		st.Settings = s
		if exists {
			for _, d := range target.Members {
				st.Devices[d.ID] = d
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if old.Name != s.Name {
		for _, a := range m.store.Snapshot().Accounts {
			m.stopAccount(a.ID)
			m.start(a)
		}
		resume = nil
		m.diagnostic("播放器名称已更新，Spotify 会话正在重新连接")
	}
	if err := m.setVolume(ctx, s.Volume, true); err != nil {
		return err
	}
	if resume != nil && s.TargetID != "" {
		if err := m.acquire(active, *resume); err != nil {
			m.failOutput(err.Error())
			return err
		}
	}
	m.changed()
	return nil
}
func (m *Manager) setVolume(ctx context.Context, v int, spotifyToo bool) error {
	if v < 0 || v > 100 {
		return errors.New("音量范围为 0–100")
	}
	if err := m.store.Update(func(s *store.State) error { s.Settings.Volume = v; return nil }); err != nil {
		return err
	}
	if m.output != nil {
		if err := m.output.Volume(v); err != nil {
			return err
		}
	}
	if spotifyToo {
		m.mu.Lock()
		r := m.accounts[m.playback.AccountID]
		m.mu.Unlock()
		if r != nil && r.player != nil {
			if err := r.player.Command(ctx, "volume", map[string]any{"volume": v}); err != nil {
				return err
			}
		}
	}
	m.changed()
	return nil
}
func (m *Manager) Playback(ctx context.Context, action string, value int64) error {
	m.op.Lock()
	defer m.op.Unlock()
	if action == "pause" {
		m.cancelOutputRecovery()
	}
	if action == "volume" {
		return m.setVolume(ctx, int(value), true)
	}
	m.mu.Lock()
	r := m.accounts[m.playback.AccountID]
	m.mu.Unlock()
	if r == nil || r.player == nil {
		return errors.New("请先在 Spotify 中选择此设备并播放")
	}
	if action == "seek" {
		if value < 0 {
			return errors.New("播放位置不可为负数")
		}
		return r.player.Command(ctx, action, map[string]any{"position": value})
	}
	return r.player.Command(ctx, action, nil)
}

func (m *Manager) StartPairing(deviceID string) (model.PairingView, error) {
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	target, exists := airplay.ResolveTarget(m.devices, deviceID, time.Now())
	busy := m.pairingView != nil && (m.pairingView.Status == "starting" || m.pairingView.Status == "waiting_pin" || m.pairingView.Status == "verifying")
	m.mu.Unlock()
	if !exists {
		return model.PairingView{}, errors.New("设备不存在")
	}
	if err := target.Ready(); err != nil {
		return model.PairingView{}, err
	}
	d := target.Device
	deviceID = d.ID
	if busy {
		return model.PairingView{}, errors.New("已有配对正在进行")
	}
	v := model.PairingView{ID: store.ID(12), DeviceID: deviceID, Status: "starting"}
	dacp := strings.ToUpper(store.ID(8))
	m.mu.Lock()
	m.pairingView = &v
	m.mu.Unlock()
	m.changed()
	p, err := airplay.Pair(m.ctx, m.airplayConfig(), d, dacp, func(status string) {
		m.mu.Lock()
		if m.pairingView != nil && m.pairingView.ID == v.ID && m.pairingView.Status != "cancelled" {
			m.pairingView.Status = status
		}
		m.mu.Unlock()
		m.changed()
	}, func(credentials string, err error) {
		m.op.Lock()
		defer m.op.Unlock()
		m.mu.Lock()
		cancelled := m.pairingView == nil || m.pairingView.ID != v.ID || m.pairingView.Status == "cancelled"
		m.mu.Unlock()
		if cancelled {
			return
		}
		if err == nil {
			err = m.store.Update(func(s *store.State) error {
				s.Pairings[deviceID] = model.PairingSecret{DACP: dacp, Credentials: credentials}
				for _, member := range target.Members {
					s.Devices[member.ID] = member
				}
				return nil
			})
		}
		m.mu.Lock()
		if m.pairingView != nil && m.pairingView.ID == v.ID && m.pairingView.Status != "cancelled" {
			if err != nil {
				m.pairingView.Status = "error"
				m.pairingView.Error = "配对失败，请检查配对码和设备连接"
			} else {
				m.pairingView.Status = "paired"
			}
		}
		m.mu.Unlock()
		m.changed()
	})
	if err != nil {
		m.mu.Lock()
		m.pairingView.Status = "error"
		m.pairingView.Error = "AirPlay 配对进程启动失败"
		m.mu.Unlock()
		m.changed()
		return model.PairingView{}, errors.New("AirPlay 配对进程启动失败")
	}
	m.mu.Lock()
	m.pairing = p
	result := *m.pairingView
	m.mu.Unlock()
	return result, nil
}

func (m *Manager) CancelPairing(id string) error {
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	if m.pairingView == nil || m.pairingView.ID != id {
		m.mu.Unlock()
		return errors.New("配对不存在")
	}
	m.pairingView.Status = "cancelled"
	p := m.pairing
	m.mu.Unlock()
	if p != nil {
		p.Close()
	}
	m.changed()
	return nil
}
func (m *Manager) SubmitPIN(id, pin string) error {
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	p, v := m.pairing, m.pairingView
	valid := p != nil && v != nil && v.ID == id && v.Status == "waiting_pin"
	m.mu.Unlock()
	if !valid {
		return errors.New("没有等待配对码的设备")
	}
	if err := p.PIN(pin); err != nil {
		return err
	}
	m.mu.Lock()
	v.Status = "verifying"
	m.mu.Unlock()
	m.changed()
	return nil
}

func (m *Manager) String() string { return fmt.Sprintf("Spoticonn (%s)", m.cfg.Interface) }
