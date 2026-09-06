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

type Config struct{ Binary, RuntimeDir, InterfaceIP string }
type Sender struct {
	id           string
	input        *os.File
	cmdFD        int
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	ready        chan struct{}
	readyOnce    sync.Once
	mu           sync.Mutex // control state and command writes share one ordering
	closed       bool
	pendingStart bool
	volume       int
	startEpoch   uint64
	startAcks    []uint64
	lastStartAt  int64
	volumeTimer  *time.Timer
	flushed      chan struct{}
	status       func(string)
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
	if cfg.InterfaceIP != "" {
		args = append(args, "--if", cfg.InterfaceIP)
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
	s := &Sender{id: store.ID(8), input: w, cmdFD: fd, ctx: child, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}), volume: volume, status: status}
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
		_ = w.Close()
		s.mu.Lock()
		s.closed = true
		s.cancelStartLocked()
		_ = unix.Close(fd)
		close(s.done)
		s.mu.Unlock()
		_ = os.Remove(cmdPath)
		if child.Err() == nil {
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
	case <-s.done:
		cancel()
		return nil, errors.New("AirPlay 连接失败，请检查配对和网络")
	case <-ctx.Done():
		s.Close()
		return nil, ctx.Err()
	case <-time.After(15 * time.Second):
		s.Close()
		return nil, errors.New("AirPlay 连接或时钟同步超时，请检查 UDP 319/320")
	}
}

func (s *Sender) scan(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 256<<10)
	for scanner.Scan() {
		line := scanner.Text()
		i := strings.Index(line, "[STATUS] ")
		if i < 0 {
			continue
		}
		line = line[i+9:]
		switch {
		case strings.HasPrefix(line, "clock_ready "):
			if strings.Contains(line, "mode=ntp") || strings.Contains(line, "state=ready") {
				s.readyOnce.Do(func() { close(s.ready) })
			}
			if strings.Contains(line, "state=stalled") {
				s.status("clock_stalled")
			}
		case strings.HasPrefix(line, "audio "):
			s.mu.Lock()
			var err error
			if s.pendingStart && !s.closed {
				s.cancelStartLocked()
				err = s.commandLocked("START_UNIX_MS=0\nACTION=START\n")
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
		case strings.HasPrefix(line, "flushed"):
			s.mu.Lock()
			if s.flushed != nil {
				close(s.flushed)
				s.flushed = nil
			}
			s.mu.Unlock()
		case strings.HasPrefix(line, "error"):
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
	if !s.closed {
		s.pendingStart = true
	}
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
	s.cancelStartLocked()
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
	s.cancelStartLocked()
	return s.commandLocked("ACTION=STANDBY\n")
}
func (s *Sender) Volume(v int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.volume = v
	return s.commandLocked(fmt.Sprintf("VOLUME=%d\n", v))
}
func (s *Sender) Metadata(t *model.Track) error {
	if t == nil {
		return nil
	}
	clean := func(v string) string { return strings.NewReplacer("\r", " ", "\n", " ", "\x00", "").Replace(v) }
	return s.command(fmt.Sprintf("TITLE=%s\nARTIST=%s\nALBUM=%s\nITEMID=%s\nDURATION=%d\nPROGRESS=%d\nACTION=SENDMETA\n", clean(t.Name), clean(strings.Join(t.Artists, ", ")), clean(t.Album), clean(t.URI), t.Duration/1000, t.Position/1000))
}
func (s *Sender) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.cancelStartLocked()
	s.mu.Unlock()
	s.cancel()
	_ = s.input.Close()
	<-s.done
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
