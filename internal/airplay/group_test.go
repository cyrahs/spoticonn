package airplay

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"spoticonn/internal/model"
)

type testMember struct {
	mu                                     sync.Mutex
	starts, joins, closes, flushes, volume int
	data                                   []byte
	anchor                                 int64
	ack                                    chan struct{}
	joinGate                               chan struct{}
	writeGate                              chan struct{}
	joinErr                                error
	callback                               func(string)
	ctx                                    context.Context
}

func (f *testMember) Begin() { f.mu.Lock(); f.starts++; f.mu.Unlock() }
func (f *testMember) Started(ctx context.Context) (int64, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-f.ack:
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.anchor, nil
}
func (f *testMember) Join(ctx context.Context, _ int64) (int64, error) {
	f.mu.Lock()
	f.joins++
	f.mu.Unlock()
	if f.joinGate != nil {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-f.joinGate:
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.anchor, f.joinErr
}
func (f *testMember) Write(b []byte) error {
	if f.writeGate != nil {
		select {
		case <-f.ctx.Done():
			return f.ctx.Err()
		case <-f.writeGate:
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data = append(f.data, b...)
	return nil
}
func (f *testMember) Flush(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushes++
	f.data = nil
	f.anchor = time.Now().Add(-time.Second).UnixMilli()
	return nil
}
func (f *testMember) Standby() error              { return nil }
func (f *testMember) Volume(v int) error          { f.mu.Lock(); f.volume = v; f.mu.Unlock(); return nil }
func (f *testMember) Metadata(*model.Track) error { return nil }
func (f *testMember) Close()                      { f.mu.Lock(); f.closes++; f.mu.Unlock() }

func testGroup(t *testing.T) (*groupOutput, *testMember, chan string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	primary := &testMember{anchor: time.Now().Add(-time.Second).UnixMilli(), ack: make(chan struct{})}
	events := make(chan string, 100)
	g := &groupOutput{ctx: ctx, cancel: cancel, cfg: Config{SharedPTP: true}, primary: primary, tv: model.Device{ID: "tv", Online: true, Address: "192.0.2.10"}, pair: model.PairingSecret{Credentials: "tv-secret"}, rate: 44100, volume: 27, status: func(s string) { events <- s }, diagnostic: func(string) {}}
	t.Cleanup(g.Close)
	return g, primary, events
}
func waitGroup(t *testing.T, events <-chan string, want string) {
	t.Helper()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case s := <-events:
			if s == want {
				return
			}
		case <-timeout.C:
			t.Fatalf("missing group state %s", want)
		}
	}
}
func eventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not reached")
}

func TestStagedJoinWaitsForAcknowledgedHomePodAndKeepsItsStream(t *testing.T) {
	g, p, events := testGroup(t)
	tv := &testMember{anchor: p.anchor + 10, joinGate: make(chan struct{})}
	var opens atomic.Int32
	opened := make(chan struct{})
	g.open = func(ctx context.Context, cfg Config, d model.Device, pair model.PairingSecret, vol, rate int, cb func(string)) (timedOutput, error) {
		if !cfg.SharedPTP || d.ID != "tv" || d.Address != "192.0.2.10" || pair.Credentials != "tv-secret" || vol != 27 || rate != 44100 {
			t.Error("incorrect member transport")
		}
		tv.ctx, tv.callback = ctx, cb
		opens.Add(1)
		close(opened)
		return tv, nil
	}
	g.Begin()
	g.Begin()
	initial := bytes.Repeat([]byte{1, 2, 3, 4}, 882) // 20ms
	if err := g.Write(initial); err != nil {
		t.Fatal(err)
	}
	select {
	case <-opened:
		t.Fatal("joined before HomePod start acknowledgement")
	case <-time.After(20 * time.Millisecond):
	}
	close(p.ack)
	select {
	case <-opened:
	case <-time.After(time.Second):
		t.Fatal("did not join")
	}
	// A slow Apple TV START cannot block or restart HomePod.
	continuation := bytes.Repeat([]byte{5, 6, 7, 8}, 441)
	if err := g.Write(continuation); err != nil {
		t.Fatal(err)
	}
	_ = g.Volume(19)
	close(tv.joinGate)
	waitGroup(t, events, "group_joined")
	eventually(t, func() bool {
		tv.mu.Lock()
		defer tv.mu.Unlock()
		return len(tv.data) == len(initial)+len(continuation)-1764 && tv.volume == 19
	})
	p.mu.Lock()
	if p.starts != 1 || p.flushes != 0 || p.closes != 0 || !bytes.Equal(p.data, append(initial, continuation...)) {
		t.Error("joining interrupted HomePod")
	}
	p.mu.Unlock()
	tv.mu.Lock()
	if tv.starts != 0 || tv.joins != 1 || !bytes.Equal(tv.data, append(initial[1764:], continuation...)) {
		t.Error("TV did not receive correctly aligned PCM")
	}
	tv.mu.Unlock()
	if opens.Load() != 1 {
		t.Fatal("duplicate member Open")
	}
	select {
	case <-tv.ctx.Done():
		t.Fatal("TV inherited expired setup context")
	default:
	}
}

func TestGroupJoinFailuresNeverStopOrRetryHomePod(t *testing.T) {
	for _, scenario := range []string{"offline", "clock", "open", "auth_required", "auth_failed", "start", "timeline", "disconnect", "backpressure"} {
		t.Run(scenario, func(t *testing.T) {
			g, p, events := testGroup(t)
			tv := &testMember{anchor: p.anchor + 10}
			var opens atomic.Int32
			switch scenario {
			case "offline":
				g.tv.Online = false
			case "clock":
				g.cfg.SharedPTP = false
			case "start":
				tv.joinErr = errors.New("secret must never surface")
			case "timeline":
				tv.anchor = p.anchor - 1
			case "backpressure":
				tv.writeGate = make(chan struct{})
			}
			g.open = func(ctx context.Context, _ Config, _ model.Device, _ model.PairingSecret, _, _ int, cb func(string)) (timedOutput, error) {
				opens.Add(1)
				tv.ctx, tv.callback = ctx, cb
				switch scenario {
				case "open":
					return nil, errors.New("secret must never surface")
				case "auth_required":
					return nil, ErrAuthRequired
				case "auth_failed":
					return nil, ErrAuthFailed
				}
				return tv, nil
			}
			close(p.ack)
			g.Begin()
			_ = g.Write(make([]byte, 3528))
			states := map[string]string{"offline": "group_degraded_offline", "clock": "group_degraded_clock", "open": "group_degraded_connect", "auth_required": "group_degraded_auth", "auth_failed": "group_degraded_auth", "start": "group_degraded_start", "timeline": "group_degraded_timeline", "disconnect": "group_degraded_member", "backpressure": "group_degraded_member"}
			if scenario == "disconnect" {
				waitGroup(t, events, "group_joined")
				tv.callback("disconnected")
			}
			if scenario == "backpressure" {
				eventually(t, func() bool { g.mu.Lock(); defer g.mu.Unlock(); return g.cycle.feeding })
				// Bounded queue fills while TV's Write is blocked. HomePod keeps accepting PCM.
				for i := 0; i < 140; i++ {
					if err := g.Write(make([]byte, 1764)); err != nil {
						t.Fatal(err)
					}
				}
				close(tv.writeGate)
			}
			waitGroup(t, events, states[scenario])
			for i := 0; i < 3; i++ {
				g.Begin()
				if err := g.Write(make([]byte, 1764)); err != nil {
					t.Fatal(err)
				}
			}
			p.mu.Lock()
			if p.closes != 0 || p.flushes != 0 || p.starts != 1 {
				t.Error("optional failure interrupted primary")
			}
			p.mu.Unlock()
			if opens.Load() > 1 {
				t.Fatal("failed member retried within a cycle")
			}
		})
	}
}

func TestGroupCancellationDrainsPendingJoinBeforeNextCycle(t *testing.T) {
	for _, phase := range []string{"before_ack", "opening", "joining", "joined"} {
		t.Run(phase, func(t *testing.T) {
			g, p, events := testGroup(t)
			entered := make(chan struct{}, 1)
			var active, maxActive atomic.Int32
			g.open = func(ctx context.Context, _ Config, _ model.Device, _ model.PairingSecret, _, _ int, _ func(string)) (timedOutput, error) {
				n := active.Add(1)
				if n > maxActive.Load() {
					maxActive.Store(n)
				}
				if phase == "opening" {
					entered <- struct{}{}
					<-ctx.Done()
					active.Add(-1)
					return nil, ctx.Err()
				}
				tv := &countedMember{testMember: testMember{ctx: ctx, anchor: p.anchor + 10}, active: &active}
				if phase == "joining" {
					tv.joinGate = make(chan struct{})
				}
				entered <- struct{}{}
				return tv, nil
			}
			g.Begin()
			if phase != "before_ack" {
				close(p.ack)
				_ = g.Write(make([]byte, 3528))
				<-entered
			}
			if phase == "joined" {
				waitGroup(t, events, "group_joined")
			}
			g.CancelJoin()
			if active.Load() != 0 {
				t.Fatal("pending member survived cancellation")
			}
			if err := g.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			g.Begin()
			g.CancelJoin()
			if maxActive.Load() > 1 {
				t.Fatal("overlapping member sessions")
			}
			p.mu.Lock()
			if p.closes != 0 || p.flushes != 1 {
				t.Error("cancelling join closed primary")
			}
			p.mu.Unlock()
		})
	}
}

type countedMember struct {
	testMember
	active *atomic.Int32
}

func (f *countedMember) Close() { f.testMember.Close(); f.active.Add(-1) }

func TestJoinPCMUsesFrameAlignedAcknowledgedTimeline(t *testing.T) {
	for _, rate := range []int{44100, 48000} {
		total := int64(rate*4*2 + 3)
		ring := make([]byte, 10000)
		for i := range ring {
			ring[i] = byte(i % 251)
		}
		// The buffered write head and ring head are deliberately mid-frame.
		prime, skip, err := joinPCM(ring, total, 1000, 2990, rate)
		first := int64((1990*rate + 999) / 1000 * 4)
		if err != nil || skip != 0 || !bytes.Equal(prime, ring[first-(total-int64(len(ring))):]) {
			t.Fatal("incorrect prime mapping")
		}
		prime, skip, err = joinPCM(ring, total, 1000, 3010, rate)
		if err != nil || len(prime) != 0 || (total+skip)%4 != 0 || skip != int64((2010*rate+999)/1000*4)-total {
			t.Fatal("incorrect future skip")
		}
		if _, _, err = joinPCM(ring, total, 1000, 1001, rate); err == nil {
			t.Fatal("accepted lost history")
		}
	}
}

func TestHomeTheaterReadinessAndScope(t *testing.T) {
	devices := livingRoom(t)
	tv := devices["0603165e17b1"]
	tv.Online = false
	devices[tv.ID] = tv
	target := Targets(devices, time.Now())[0]
	if target.Ready() != nil || !target.View(nil).Online || target.Device.ID != tv.ID {
		t.Fatal("offline TV blocked HomePod or changed display identity")
	}
	pod := devices["6a9cdd7721c7"]
	pod.Online = false
	devices[pod.ID] = pod
	if Targets(devices, time.Now())[0].Ready() == nil {
		t.Fatal("offline primary is ready")
	}
	pod.ID = "second-pod"
	devices[pod.ID] = pod
	if _, _, ok := Targets(devices, time.Now())[0].HomeTheater(); ok {
		t.Fatal("unvalidated stereo topology enabled staged mode")
	}
}

func TestGroupDiagnosticsOnlyContainWhitelistedData(t *testing.T) {
	g, p, events := testGroup(t)
	var mu sync.Mutex
	var logs []string
	g.diagnostic = func(s string) { mu.Lock(); logs = append(logs, s); mu.Unlock() }
	g.open = func(context.Context, Config, model.Device, model.PairingSecret, int, int, func(string)) (timedOutput, error) {
		return nil, errors.New("--auth tv-secret RAW_AUDIO")
	}
	close(p.ack)
	g.Begin()
	_ = g.Write([]byte("RAW_AUDIO"))
	waitGroup(t, events, "group_degraded_connect")
	g.CancelJoin()
	mu.Lock()
	defer mu.Unlock()
	for _, line := range logs {
		if strings.Contains(line, "tv-secret") || strings.Contains(line, "RAW_AUDIO") || strings.Contains(line, "--auth") {
			t.Fatal("private engine data leaked")
		}
	}
}

func TestGroupDoesNotJoinBeforeAcknowledgedAudibleInstant(t *testing.T) {
	g, p, events := testGroup(t)
	p.anchor = time.Now().Add(100 * time.Millisecond).UnixMilli()
	opened := make(chan struct{})
	g.open = func(context.Context, Config, model.Device, model.PairingSecret, int, int, func(string)) (timedOutput, error) {
		close(opened)
		return nil, errors.New("unavailable")
	}
	close(p.ack)
	g.Begin()
	_ = g.Write(make([]byte, 1764))
	select {
	case <-opened:
		t.Fatal("start acknowledgement bypassed future audible instant")
	case <-time.After(20 * time.Millisecond):
	}
	for time.Now().UnixMilli() <= p.anchor {
		time.Sleep(time.Millisecond)
	}
	_ = g.Write(make([]byte, 1764))
	waitGroup(t, events, "group_degraded_connect")
}
