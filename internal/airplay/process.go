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
}
type Sender struct {
	id           string
	input        *os.File
	cmdFD        int
	cancel       context.CancelFunc
	done         chan struct{}
	ready        chan struct{}
	readyOnce    sync.Once
	failed       chan struct{}
	failOnce     sync.Once
	sharedPTP    bool
	mu           sync.Mutex
	commandMu    sync.Mutex
	startMu      sync.Mutex // orders START against FLUSH/STANDBY
	closed       bool
	pendingStart bool
	startSent    bool
	anchor       int64
	started      chan struct{}
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
	s := &Sender{id: store.ID(8), input: w, cmdFD: fd, cancel: cancel, done: make(chan struct{}), ready: make(chan struct{}), failed: make(chan struct{}), sharedPTP: cfg.SharedPTP, started: make(chan struct{}), status: status}
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
		s.commandMu.Lock()
		_ = unix.Close(fd)
		s.commandMu.Unlock()
		_ = os.Remove(cmdPath)
		close(s.done)
		if child.Err() == nil {
			s.status("disconnected")
		}
	}()
	select {
	case <-s.ready:
		return s, nil
	case <-s.failed:
		s.Close()
		return nil, errors.New("AirPlay 连接或共享时钟失败")
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
			s.startMu.Lock()
			s.mu.Lock()
			start := s.pendingStart
			s.pendingStart = false
			s.mu.Unlock()
			if start {
				s.mu.Lock()
				s.startSent = true
				s.mu.Unlock()
				if err := s.command("START_UNIX_MS=0\nACTION=START\n"); err != nil {
					s.status("error")
				}
			}
			s.startMu.Unlock()
		case strings.HasPrefix(line, "started "):
			anchor, valid := statusInt(line, "at_unix_ms")
			s.mu.Lock()
			accepted := valid && anchor > 0 && s.startSent && s.anchor == 0
			if accepted {
				s.anchor = anchor
				close(s.started)
			}
			s.mu.Unlock()
			if accepted {
				s.status("playing")
			}
		case strings.HasPrefix(line, "REANCHOR "), strings.HasPrefix(line, "anchor_corrected "):
			// A late join may no longer be mapped onto this origin. The group
			// drops the optional member instead of restarting the audible stream.
			s.status("timeline_changed")
		case strings.HasPrefix(line, "flushed"):
			s.mu.Lock()
			if s.flushed != nil {
				close(s.flushed)
				s.flushed = nil
			}
			s.mu.Unlock()
		case strings.HasPrefix(line, "error"):
			s.failOnce.Do(func() { close(s.failed) })
			s.status("error")
		case strings.HasPrefix(line, "disconnected"), strings.HasPrefix(line, "stopped"):
			s.status("disconnected")
		}
	}
}

func (s *Sender) command(value string) error {
	s.commandMu.Lock()
	defer s.commandMu.Unlock()
	select {
	case <-s.done:
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
	if !s.closed && !s.startSent {
		s.pendingStart = true
	}
	s.mu.Unlock()
}

// Started returns the sender's acknowledged audible instant, never an assumed
// delay from Open. The caller must cancel its wait when abandoning a cycle.
func (s *Sender) Started(ctx context.Context) (int64, error) {
	s.mu.Lock()
	ch := s.started
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

// Join anchors BEFORE stdin is fed; START_JOIN makes the engine acknowledge
// the receiver's actual instant so the caller can map the live PCM onto it.
func (s *Sender) Join(ctx context.Context, at int64) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.startMu.Lock()
	s.mu.Lock()
	if s.closed || s.startSent || s.pendingStart {
		s.mu.Unlock()
		s.startMu.Unlock()
		return 0, errors.New("AirPlay 成员已经启动")
	}
	s.startSent = true
	s.mu.Unlock()
	err := s.command(fmt.Sprintf("START_UNIX_MS=%d\nSTART_JOIN=1\nACTION=START\n", at))
	s.startMu.Unlock()
	if err != nil {
		return 0, err
	}
	return s.Started(ctx)
}

func statusInt(line, key string) (int64, bool) {
	for _, field := range strings.Fields(line) {
		if value, ok := strings.CutPrefix(field, key+"="); ok {
			n, err := strconv.ParseInt(value, 10, 64)
			return n, err == nil
		}
	}
	return 0, false
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
	s.startMu.Lock()
	s.mu.Lock()
	s.pendingStart = false
	s.startSent = false
	s.anchor = 0
	s.started = make(chan struct{})
	ch := make(chan struct{})
	s.flushed = ch
	s.mu.Unlock()
	err := s.command("ACTION=FLUSH\n")
	s.startMu.Unlock()
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
	s.startMu.Lock()
	defer s.startMu.Unlock()
	s.mu.Lock()
	s.pendingStart = false
	s.mu.Unlock()
	return s.command("ACTION=STANDBY\n")
}
func (s *Sender) Volume(v int) error { return s.command(fmt.Sprintf("VOLUME=%d\n", v)) }
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
	s.pendingStart = false
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
