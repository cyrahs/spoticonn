package airplay

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"spoticonn/internal/model"
	"spoticonn/internal/store"
)

type Config struct {
	Binary, RuntimeDir, InterfaceIP string
	SharedPTP                       bool
	RemoteControl                   func(RemoteCommand) // must not block the engine reader
	Artwork                         *ArtworkCache
	Diagnostic                      func(string)
}
type Sender struct {
	id             string
	input          *os.File
	cmdFD          int
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{}
	ready          chan struct{}
	readyOnce      sync.Once
	failed         chan struct{}
	failOnce       sync.Once
	connectError   error
	sharedPTP      bool
	startSent      bool
	joinStart      bool
	anchor         int64
	startConfirmed chan struct{}
	mu             sync.Mutex // control state and command writes share one ordering
	closed         bool
	pendingStart   bool
	volume         int
	startEpoch     uint64
	startAcks      []uint64
	lastStartAt    int64
	volumeTimer    *time.Timer
	flushed        chan struct{}
	status         func(string)
	remoteControl  func(RemoteCommand)
	deviceID       string
	deviceKind     string
	diagnostic     func(string)
	runtimeDir     string
	artworkCache   *ArtworkCache
	artwork        *senderArtwork
}

func Open(ctx context.Context, cfg Config, d model.Device, pair model.PairingSecret, volume, rate int, status func(string)) (*Sender, error) {
	if err := os.MkdirAll(cfg.RuntimeDir, 0700); err != nil {
		return nil, err
	}
	cmdPath := filepath.Join(cfg.RuntimeDir, "airplay-"+store.ID(8)+".commands")
	if err := unix.Mkfifo(cmdPath, 0600); err != nil {
		return nil, err
	}
	fd, err := unix.Open(cmdPath, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0600)
	if err != nil {
		_ = os.Remove(cmdPath)
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		unix.Close(fd)
		os.Remove(cmdPath)
		return nil, err
	}
	child, cancel := context.WithCancel(ctx)
	args := []string{"--protocol", "auto", "--port", strconv.Itoa(d.Port), "--volume", strconv.Itoa(volume), "--samplerate", strconv.Itoa(rate), "--channels", "2", "--bitdepth", "16", "--cmdpipe", cmdPath, "--name", "Spoticonn", "--debug", "1"}
	if pair.Credentials != "" {
		args = append(args, "--auth", pair.Credentials, "--dacp", pair.DACP)
	}
	if pair.Password != "" {
		args = append(args, "--password", pair.Password)
	}
	if Authentication(d, pair).Requirement == "password" {
		args = append(args, "--pw", "true")
	}
	if cfg.InterfaceIP != "" {
		args = append(args, "--if", cfg.InterfaceIP)
	}
	if cfg.SharedPTP {
		args = append(args, "--ptp-shared", "--timing", "ptp")
	}
	if len(d.TXT) > 0 {
		var txt []string
		for k, v := range d.TXT {
			txt = append(txt, k+"="+v)
		}
		args = append(args, "--txt", strings.Join(txt, " "))
	}
	args = append(args, d.Address)
	cmd := exec.CommandContext(child, cfg.Binary, args...)
	configureCapabilities(cmd)
	cmd.Stdin = r
	cmd.Cancel = func() error {
		err := cmd.Process.Signal(syscall.SIGTERM)
		// Readers drain stdout/stderr before Wait; kill independently so a
		// child ignoring SIGTERM cannot keep those readers alive forever.
		time.AfterFunc(2*time.Second, func() { _ = cmd.Process.Kill() })
		return err
	}
	cmd.WaitDelay = 2 * time.Second
	s := &Sender{id: store.ID(8), input: w, cmdFD: fd, ctx: child, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}), volume: volume, status: status, failed: make(chan struct{}), sharedPTP: cfg.SharedPTP, startConfirmed: make(chan struct{})}
	s.remoteControl, s.deviceID = cfg.RemoteControl, d.ID
	s.deviceKind = "AirPlay"
	switch {
	case strings.HasPrefix(strings.ToLower(d.Model), "appletv"):
		s.deviceKind = "AppleTV"
	case strings.HasPrefix(strings.ToLower(d.Model), "audioaccessory"):
		s.deviceKind = "HomePod"
	}
	s.diagnostic, s.runtimeDir, s.artworkCache = cfg.Diagnostic, cfg.RuntimeDir, cfg.Artwork
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		r.Close()
		w.Close()
		unix.Close(fd)
		os.Remove(cmdPath)
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		r.Close()
		w.Close()
		unix.Close(fd)
		os.Remove(cmdPath)
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		cancel()
		r.Close()
		w.Close()
		unix.Close(fd)
		os.Remove(cmdPath)
		return nil, err
	}
	_ = r.Close()
	var scans sync.WaitGroup
	scans.Add(2)
	go func() { defer scans.Done(); s.scan(out) }()
	go func() { defer scans.Done(); s.scan(stderr) }()
	go func() {
		scans.Wait()
		_ = cmd.Wait()
		unexpected := child.Err() == nil
		cancel() // invalidate queued controls even on an unexpected process exit
		_ = w.Close()
		s.mu.Lock()
		s.closed = true
		s.cancelStartLocked()
		s.stopArtworkLocked()
		_ = unix.Close(fd)
		close(s.done)
		s.mu.Unlock()
		_ = os.Remove(cmdPath)
		if unexpected {
			s.status("disconnected")
		}
	}()
	select {
	case <-s.ready:
		// cliairplay v0.5.3 skips --volume=0 during AP2 setup. Explicitly
		// set every initial value through the pipe before accepting PCM/START.
		if err := s.Volume(volume); err != nil {
			s.Close()
			return nil, err
		}
		return s, nil
	case <-s.failed:
		s.Close()
		return nil, s.connectionError()
	case <-s.done:
		cancel()
		return nil, s.connectionError()
	case <-ctx.Done():
		s.Close()
		return nil, ctx.Err()
	case <-time.After(15 * time.Second):
		s.Close()
		return nil, errors.New("AirPlay 连接或时钟同步超时，请检查 UDP 319/320")
	}
}

func (s *Sender) connectionError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connectError != nil {
		return s.connectError
	}
	return errors.New("AirPlay 连接失败，请检查设备认证、访问设置、网络及共享时钟")
}

func (s *Sender) scan(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 256<<10)
	for scanner.Scan() {
		line := scanner.Text()
		if action := remoteAction(line); action != "" {
			if s.remoteControl != nil && s.ctx.Err() == nil {
				s.remoteControl(RemoteCommand{Context: s.ctx, DeviceID: s.deviceID, Action: action, ReceivedAt: time.Now()})
			}
			continue
		}
		i := strings.Index(line, "[STATUS] ")
		if i < 0 {
			continue
		}
		line = line[i+9:]
		switch {
		case strings.HasPrefix(line, "clock_ready "):
			if s.sharedPTP && strings.Contains(line, "mode=ntp") {
				s.failOnce.Do(func() { close(s.failed) })
				s.status("clock_stalled")
				continue
			}
			if strings.Contains(line, "mode=ntp") || strings.Contains(line, "state=ready") {
				s.readyOnce.Do(func() { close(s.ready) })
			}
			if strings.Contains(line, "state=stalled") {
				s.failOnce.Do(func() { close(s.failed) })
				s.status("clock_stalled")
			}
		case strings.HasPrefix(line, "audio "):
			s.mu.Lock()
			var err error
			if s.pendingStart && !s.closed {
				s.cancelStartLocked()
				s.prepareStartLocked(false)
				err = s.flushMetadataLocked()
				if err == nil {
					err = s.commandLocked("START_UNIX_MS=0\nACTION=START\n")
				}
				if err == nil {
					s.startAcks = append(s.startAcks, s.startEpoch)
				}
			}
			s.mu.Unlock()
			if err != nil {
				s.status("error")
			}
		case strings.HasPrefix(line, "started "):
			s.started(line)
		case strings.HasPrefix(line, "REANCHOR "), strings.HasPrefix(line, "anchor_corrected "):
			s.status("timeline_changed")
		case strings.HasPrefix(line, "flushed"):
			s.mu.Lock()
			if s.flushed != nil {
				close(s.flushed)
				s.flushed = nil
			}
			s.mu.Unlock()
		case strings.HasPrefix(line, "mrp "):
			if message := artworkStatusDiagnostic(line); message != "" {
				s.artworkDiagnostic(message)
			}
		case strings.HasPrefix(line, "error"):
			// Only trust the structured error code; engine detail may contain secrets.
			fields := strings.Fields(line)
			if len(fields) > 1 {
				s.mu.Lock()
				switch fields[1] {
				case "code=auth_required":
					s.connectError = ErrAuthRequired
				case "code=auth_failed":
					s.connectError = ErrAuthFailed
				}
				s.mu.Unlock()
			}
			s.failOnce.Do(func() { close(s.failed) })
			s.cancelStart()
			s.status("error")
		case strings.HasPrefix(line, "disconnected"), strings.HasPrefix(line, "stopped"):
			s.cancelStart()
			s.status("disconnected")
		}
	}
}

// Invalidating a start does not discard its expected ack: cliairplay can
// acknowledge a superseded start before acknowledging the next one. Retain
// their command order so an old ack cannot schedule work for the new epoch.
func (s *Sender) cancelStartLocked() {
	s.startEpoch++
	s.pendingStart = false
	if s.volumeTimer != nil {
		s.volumeTimer.Stop()
		s.volumeTimer = nil
	}
}

func (s *Sender) cancelStart() {
	s.mu.Lock()
	s.cancelStartLocked()
	s.mu.Unlock()
}

func (s *Sender) started(line string) {
	var at int64
	for _, field := range strings.Fields(line) {
		if value, ok := strings.CutPrefix(field, "at_unix_ms="); ok {
			if parsed, err := strconv.ParseInt(value, 10, 64); err == nil {
				at = parsed
			}
			break
		}
	}
	s.mu.Lock()
	if s.closed || len(s.startAcks) == 0 || (at > 0 && at == s.lastStartAt) {
		s.mu.Unlock()
		return
	}
	epoch := s.startAcks[0]
	s.startAcks = s.startAcks[1:]
	s.lastStartAt = at
	if epoch != s.startEpoch {
		s.mu.Unlock()
		return
	}
	if at <= 0 {
		s.cancelStartLocked()
		s.mu.Unlock()
		s.status("error")
		return
	}
	// Publish the acknowledged anchor immediately: late joiners must map PCM
	// before the scheduled instant. The existing volume timer still controls
	// the later playing notification and reapplies the latest volume.
	s.anchor = at
	if s.startConfirmed != nil {
		close(s.startConfirmed)
	}
	// The ack confirms a scheduled instant, not necessarily audible playback
	// yet. Apply once that instant arrives, without blocking the status reader.
	s.volumeTimer = time.AfterFunc(time.Until(time.UnixMilli(at)), func() {
		s.mu.Lock()
		if s.closed || s.ctx.Err() != nil || epoch != s.startEpoch {
			s.mu.Unlock()
			return
		}
		s.volumeTimer = nil
		// Read the latest value while holding the same lock as Volume and
		// the pipe write, so a concurrent mute cannot be overwritten.
		err := s.commandLocked(fmt.Sprintf("VOLUME=%d\n", s.volume))
		s.mu.Unlock()
		if err != nil {
			s.status("error")
		} else {
			s.status("playing")
		}
	})
	s.mu.Unlock()
}

func (s *Sender) command(value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commandLocked(value)
}

func (s *Sender) commandLocked(value string) error {
	if s.closed {
		return errors.New("AirPlay 已断开")
	}
	select {
	case <-s.done:
		return errors.New("AirPlay 已断开")
	case <-s.ctx.Done():
		return errors.New("AirPlay 已断开")
	default:
	}
	b := []byte(value)
	deadline := time.Now().Add(time.Second)
	for len(b) > 0 {
		n, err := unix.Write(s.cmdFD, b)
		if n > 0 {
			b = b[n:]
		}
		if err != nil && !errors.Is(err, unix.EAGAIN) {
			return errors.New("AirPlay 控制通道失败")
		}
		if time.Now().After(deadline) {
			return errors.New("AirPlay 控制超时")
		}
		if n <= 0 {
			time.Sleep(5 * time.Millisecond)
		}
	}
	return nil
}

func (s *Sender) Begin() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed && !s.joinStart {
		s.resumeArtworkLocked()
		s.pendingStart = true
	}
}

// prepareStartLocked preserves a waiting observer on the first start after a
// flush, and replaces the acknowledged channel when a new audio cycle begins.
func (s *Sender) prepareStartLocked(join bool) {
	if s.startConfirmed == nil || s.anchor != 0 {
		s.startConfirmed = make(chan struct{})
	}
	s.startSent, s.joinStart, s.anchor = true, join, 0
}

func (s *Sender) resetStartLocked() {
	s.startSent, s.joinStart, s.anchor = false, false, 0
	s.startConfirmed = make(chan struct{})
}

// Started returns the sender's acknowledged audible instant. Volume is applied
// by the existing timer at that instant; the playing event follows its success.
func (s *Sender) Started(ctx context.Context) (int64, error) {
	s.mu.Lock()
	ch := s.startConfirmed
	s.mu.Unlock()
	select {
	case <-ch:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.anchor, nil
	case <-s.done:
		return 0, errors.New("AirPlay 已断开")
	case <-s.failed:
		return 0, errors.New("AirPlay 启动失败")
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// Join anchors before stdin is fed. Its acknowledgement uses the same epoch
// ordering and post-start volume handling as an ordinary cold or warm start.
func (s *Sender) Join(ctx context.Context, at int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	if s.closed || s.startSent || s.pendingStart {
		s.mu.Unlock()
		return 0, errors.New("AirPlay 成员已经启动")
	}
	s.cancelStartLocked()
	s.prepareStartLocked(true)
	err := s.flushMetadataLocked()
	if err == nil {
		err = s.commandLocked(fmt.Sprintf("START_UNIX_MS=%d\nSTART_JOIN=1\nACTION=START\n", at))
	}
	if err == nil {
		s.startAcks = append(s.startAcks, s.startEpoch)
	}
	s.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return s.Started(ctx)
}

func (s *Sender) Write(b []byte) error {
	_ = s.input.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	for len(b) > 0 {
		n, err := s.input.Write(b)
		if err != nil {
			return errors.New("AirPlay 音频写入失败")
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func (s *Sender) Flush(ctx context.Context) error {
	s.mu.Lock()
	s.suspendArtworkLocked()
	s.cancelStartLocked()
	s.resetStartLocked()
	ch := make(chan struct{})
	s.flushed = ch
	err := s.commandLocked("ACTION=FLUSH\n")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	select {
	case <-ch:
		return nil
	case <-s.done:
		return errors.New("AirPlay 已断开")
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(3 * time.Second):
		return errors.New("AirPlay 清空缓存超时")
	}
}
func (s *Sender) Standby() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.suspendArtworkLocked()
	s.cancelStartLocked()
	s.resetStartLocked()
	return s.commandLocked("ACTION=STANDBY\n")
}
func (s *Sender) Volume(v int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.volume = v
	return s.commandLocked(fmt.Sprintf("VOLUME=%d\n", v))
}
func (s *Sender) Close() {
	s.mu.Lock()
	if s.closed {
		a := s.artwork
		s.mu.Unlock()
		if a != nil {
			<-a.done
		}
		return
	}
	s.closed = true
	s.cancelStartLocked()
	s.stopArtworkLocked()
	a := s.artwork
	s.mu.Unlock()
	s.cancel()
	_ = s.input.Close()
	<-s.done
	if a != nil {
		<-a.done
	}
}

type Pairing struct {
	cancel context.CancelFunc
	input  io.WriteCloser
	mu     sync.Mutex
	sent   bool
}

func Pair(ctx context.Context, cfg Config, d model.Device, dacp string, update func(string), complete func(string, error)) (*Pairing, error) {
	child, cancel := context.WithTimeout(ctx, 2*time.Minute)
	cmd := exec.CommandContext(child, cfg.Binary, "--pair-setup", "--port", strconv.Itoa(d.Port), "--dacp", dacp, d.Address)
	input, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	p := &Pairing{cancel: cancel, input: input}
	var wg sync.WaitGroup
	wg.Add(2)
	var credentials string
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "CREDENTIALS: ") {
				credentials = strings.TrimPrefix(sc.Text(), "CREDENTIALS: ")
			}
		}
	}()
	go func() {
		defer wg.Done()
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "Enter the PIN") {
				update("waiting_pin")
			}
		}
	}()
	go func() {
		wg.Wait()
		err := cmd.Wait()
		if err == nil && len(credentials) != 192 {
			err = errors.New("invalid pairing credentials")
		}
		if err != nil {
			err = errors.New("配对失败或已超时，请重新配对")
		}
		complete(credentials, err)
		cancel()
	}()
	return p, nil
}
func (p *Pairing) PIN(pin string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sent {
		return errors.New("配对码已提交")
	}
	if len(pin) != 4 {
		return errors.New("请输入四位配对码")
	}
	for _, c := range pin {
		if c < '0' || c > '9' {
			return errors.New("配对码只能包含数字")
		}
	}
	_, err := io.WriteString(p.input, pin+"\n")
	if err == nil {
		p.sent = true
	}
	return err
}
func (p *Pairing) Close() { p.cancel(); _ = p.input.Close() }
