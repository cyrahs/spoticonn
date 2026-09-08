package airplay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testArtwork(t *testing.T, format string) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(0, 0, color.RGBA{R: 255, A: 255})
	var b bytes.Buffer
	var err error
	if format == "png" {
		err = png.Encode(&b, img)
	} else {
		err = jpeg.Encode(&b, img, nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

type artworkRoundTrip func(*http.Request) (*http.Response, error)

func (f artworkRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func artworkResponseClient(status int, mime string, data []byte, length int64) *http.Client {
	return &http.Client{Transport: artworkRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{mime}}, Body: io.NopCloser(bytes.NewReader(data)), ContentLength: length, Request: r}, nil
	})}
}

func TestDownloadArtworkValidatesAndConvertsImages(t *testing.T) {
	for _, format := range []string{"jpeg", "png"} {
		t.Run(format, func(t *testing.T) {
			data := testArtwork(t, format)
			got, err := downloadArtwork(t.Context(), artworkResponseClient(200, "image/"+format+"; charset=binary", data, int64(len(data))), "https://cover.example/album?token=private")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := jpeg.Decode(bytes.NewReader(got)); err != nil {
				t.Fatal("not an MRP-compatible JPEG", err)
			}
			if !bytes.Contains(got, []byte{0xff, 0xc0}) || bytes.Contains(got, []byte{0xff, 0xc2}) {
				t.Fatal("not a baseline JPEG")
			}
		})
	}
	// Go's decoder accepts trailing bytes; cliairplay's MRP probe requires a
	// terminal EOI marker, so normalize that case before handing it off.
	data := append(testArtwork(t, "jpeg"), []byte("trailing")...)
	got, err := downloadArtwork(t.Context(), artworkResponseClient(200, "image/jpeg", data, -1), "https://cover.example/image")
	if err != nil || !bytes.HasSuffix(got, []byte{0xff, 0xd9}) {
		t.Fatal("JPEG lacks the MRP envelope", err)
	}
}

func TestDownloadArtworkFailures(t *testing.T) {
	jpegData := testArtwork(t, "jpeg")
	for _, tc := range []struct {
		name, mime string
		status     int
		data       []byte
		length     int64
	}{
		{"http_error", "image/jpeg", 404, jpegData, -1},
		{"html", "text/html", 200, []byte("secret"), -1},
		{"type_mismatch", "image/png", 200, jpegData, -1},
		{"missing_type", "", 200, jpegData, -1},
		{"invalid", "image/jpeg", 200, []byte("secret"), -1},
		{"truncated", "image/jpeg", 200, jpegData[:len(jpegData)-5], -1},
		{"empty", "image/jpeg", 200, nil, 0},
		{"declared_large", "image/jpeg", 200, jpegData, artworkMaxDownload + 1},
		{"stream_large", "image/jpeg", 200, bytes.Repeat([]byte{0}, artworkMaxDownload+1), -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := downloadArtwork(t.Context(), artworkResponseClient(tc.status, tc.mime, tc.data, tc.length), "https://cover.example/secret?token=private")
			if err == nil {
				t.Fatal("accepted invalid artwork")
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "cover.example") {
				t.Fatal("sensitive download details leaked", err)
			}
		})
	}
	// A legal header with enormous dimensions must be rejected before decode.
	var large bytes.Buffer
	if err := png.Encode(&large, image.NewGray(image.Rect(0, 0, 4097, 1))); err != nil {
		t.Fatal(err)
	}
	if _, err := downloadArtwork(t.Context(), artworkResponseClient(200, "image/png", large.Bytes(), -1), "https://cover.example/large"); err == nil {
		t.Fatal("accepted oversized dimensions")
	}
}

func TestArtworkTimeoutAndNetworkErrorsAreRedacted(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		client := &http.Client{Timeout: 20 * time.Millisecond, Transport: artworkRoundTrip(func(r *http.Request) (*http.Response, error) {
			if timeout {
				<-r.Context().Done()
			}
			return nil, errors.New("secret response from private-host?token=private")
		})}
		started := time.Now()
		_, err := downloadArtwork(t.Context(), client, "https://cover.example/secret?token=private")
		if err == nil || strings.Contains(err.Error(), "private") || strings.Contains(err.Error(), "secret") {
			t.Fatal("unredacted failure", err)
		}
		if time.Since(started) > time.Second {
			t.Fatal("download ignored timeout")
		}
	}
}

func TestArtworkURLsAndAddresses(t *testing.T) {
	for _, source := range []string{"http://cover.example/a", "file:///etc/passwd", "https://user:pass@cover.example/a", "https://cover.example:8443/a", "https://cover.example/a#fragment", "https:///a", "/tmp/cover", "https://cover.example/\nARTWORK=/secret"} {
		_, err := downloadArtwork(t.Context(), &http.Client{Transport: artworkRoundTrip(func(*http.Request) (*http.Response, error) {
			t.Error("unsafe URL reached transport")
			return nil, errors.New("unexpected dial")
		})}, source)
		if err == nil {
			t.Errorf("accepted URL %q", source)
		}
	}
	for _, address := range []string{"0.0.0.0", "127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "100.100.100.200", "224.0.0.1", "240.0.0.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "::", "::1", "::ffff:127.0.0.1", "fe80::1", "fc00::1", "ff02::1", "64:ff9b::a00:1", "2002:a00:1::", "2001:db8::1"} {
		if publicArtworkIP(netip.MustParseAddr(address)) {
			t.Errorf("accepted non-public IP %s", address)
		}
	}
	for _, address := range []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"} {
		if !publicArtworkIP(netip.MustParseAddr(address)) {
			t.Errorf("rejected public IP %s", address)
		}
	}
	for _, address := range []string{"127.0.0.1:443", "[::1]:443", "169.254.169.254:443"} {
		if conn, err := dialArtwork(t.Context(), "tcp", address); err == nil {
			conn.Close()
			t.Fatal("dialled a blocked address")
		}
	}
}

func TestArtworkRedirectsCannotReachLocalServices(t *testing.T) {
	var localHits atomic.Int32
	local := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { localHits.Add(1); w.WriteHeader(200) }))
	defer local.Close()
	data := testArtwork(t, "jpeg")
	var destination atomic.Value
	destination.Store("/image")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/image" {
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(data)
			return
		}
		http.Redirect(w, r, destination.Load().(string), http.StatusFound)
	}))
	defer server.Close()
	// Route only our synthetic public host to the TLS fixture. Every other
	// destination still runs through the production validating dialer.
	client := newArtworkClient()
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.ServerName = "127.0.0.1" // fixture certificate; verification remains enabled
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address == "cover.example:443" {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		}
		return dialArtwork(ctx, network, address)
	}
	client.Transport = transport
	defer transport.CloseIdleConnections()
	if _, err := downloadArtwork(t.Context(), client, "https://cover.example/start"); err != nil {
		t.Fatal("relative redirect failed", err)
	}
	for _, target := range []string{local.URL, "https://127.0.0.1/image", "https://169.254.169.254/image", "http://cover.example/image", "https://user:secret@cover.example/image", "/loop"} {
		destination.Store(target)
		if _, err := downloadArtwork(t.Context(), client, "https://cover.example/start"); err == nil {
			t.Errorf("accepted redirect %q", target)
		}
	}
	if localHits.Load() != 0 {
		t.Fatal("redirect reached a local service")
	}
	u, _ := url.Parse("https://cover.example/image")
	if err := client.CheckRedirect(&http.Request{URL: u}, make([]*http.Request, 4)); err == nil {
		t.Fatal("redirect limit missing")
	}
}

func TestArtworkCacheSharesDownloadsAndBoundsEntries(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	cache := newArtworkCache(func(ctx context.Context, source string) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-release:
			}
		}
		if source == "failure" {
			return nil, errors.New("download failed")
		}
		return []byte(source), nil
	})
	firstCtx, cancel := context.WithCancel(t.Context())
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { _, err := cache.get(firstCtx, "shared"); first <- err }()
	<-started
	go func() { _, err := cache.get(t.Context(), "shared"); second <- err }()
	eventually(t, func() bool { cache.mu.Lock(); defer cache.mu.Unlock(); return cache.pending["shared"].users == 2 })
	cancel()
	if !errors.Is(<-first, context.Canceled) {
		t.Fatal("first consumer did not cancel")
	}
	close(release)
	if err := <-second; err != nil {
		t.Fatal("one member cancelled another's download", err)
	}
	if data, err := cache.get(t.Context(), "shared"); err != nil || string(data) != "shared" || calls.Load() != 1 {
		t.Fatal("cache miss", err, calls.Load())
	}
	for i := 0; i < 2; i++ {
		if _, err := cache.get(t.Context(), "failure"); err == nil {
			t.Fatal("lost failure")
		}
	}
	if calls.Load() != 2 {
		t.Fatal("failed download was not cached")
	}
	for i := 0; i < artworkCacheEntries+1; i++ {
		_, _ = cache.get(t.Context(), fmt.Sprint(i))
	}
	cache.mu.Lock()
	if len(cache.entries) != artworkCacheEntries {
		t.Error("unbounded cache", len(cache.entries))
	}
	_, retained := cache.entries["shared"]
	cache.mu.Unlock()
	if retained {
		t.Fatal("oldest image was not evicted")
	}
}

func TestArtworkCacheCancelsUnneededDownloadWithoutPoisoningNewRequest(t *testing.T) {
	started, cancelled, finish := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	cache := newArtworkCache(func(ctx context.Context, _ string) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			close(cancelled)
			<-finish
			return nil, ctx.Err()
		}
		return []byte("new"), nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := cache.get(ctx, "same"); done <- err }()
	<-started
	cancel()
	<-done
	<-cancelled
	data, err := cache.get(t.Context(), "same")
	if err != nil || string(data) != "new" {
		t.Fatal("cancelled request reused", err)
	}
	close(finish)
}

func TestArtworkFlattensTransparencyAndBoundsDimensions(t *testing.T) {
	for _, size := range []image.Point{{1024, 512}, {512, 1024}, {20, 10}} {
		img := image.NewNRGBA(image.Rect(0, 0, size.X, size.Y))
		for y := size.Y / 2; y < size.Y; y++ {
			for x := 0; x < size.X; x++ {
				img.SetNRGBA(x, y, color.NRGBA{R: 255, A: 128})
			}
		}
		var input bytes.Buffer
		if err := png.Encode(&input, img); err != nil {
			t.Fatal(err)
		}
		data, err := downloadArtwork(t.Context(), artworkResponseClient(200, "image/png", input.Bytes(), -1), "https://cover.example/image")
		if err != nil {
			t.Fatal(err)
		}
		got, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil || len(data) > artworkMaxBytes {
			t.Fatal("invalid output JPEG", err)
		}
		w, h := got.Bounds().Dx(), got.Bounds().Dy()
		if w > artworkSize || h > artworkSize || w*size.Y != h*size.X {
			t.Fatal("incorrect resized dimensions", got.Bounds())
		}
		r, g, b, _ := got.At(w/2, 0).RGBA()
		if r < 0xf000 || g < 0xf000 || b < 0xf000 {
			t.Fatal("transparent region was not flattened to white", r, g, b)
		}
		r, g, b, _ = got.At(w/2, h-1).RGBA()
		if r < 0xf000 || g < 0x7000 || g > 0x9000 || b < 0x7000 || b > 0x9000 {
			t.Fatal("partial transparency was not composited on white", r, g, b)
		}
	}
}
