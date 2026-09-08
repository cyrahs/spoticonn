package airplay

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sync"
	"time"
)

const (
	artworkTimeout     = 5 * time.Second
	artworkMaxDownload = 5 << 20
	// cliairplay v0.5.3's MediaRemote staging guard is 1 MiB.
	artworkMaxBytes     = 1 << 20
	artworkCacheEntries = 8
)

// ArtworkCache shares bounded, in-memory downloads across group members and
// reconnects. Neither URLs nor raw HTTP/decoder errors reach diagnostics.
type ArtworkCache struct {
	mu       sync.Mutex
	entries  map[string]artworkEntry
	pending  map[string]*artworkRequest
	sequence uint64
	load     func(context.Context, string) ([]byte, error)
}

type artworkEntry struct {
	data    []byte
	err     error
	expires time.Time
	used    uint64
}

type artworkRequest struct {
	done   chan struct{}
	cancel context.CancelFunc
	users  int
	data   []byte
	err    error
}

func NewArtworkCache() *ArtworkCache {
	client := newArtworkClient()
	return newArtworkCache(func(ctx context.Context, source string) ([]byte, error) {
		return downloadArtwork(ctx, client, source)
	})
}

func newArtworkCache(load func(context.Context, string) ([]byte, error)) *ArtworkCache {
	return &ArtworkCache{entries: make(map[string]artworkEntry), pending: make(map[string]*artworkRequest), load: load}
}

var defaultArtworkCache = NewArtworkCache()

func (c *ArtworkCache) get(ctx context.Context, source string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.sequence++
	if entry, ok := c.entries[source]; ok && time.Now().Before(entry.expires) {
		entry.used = c.sequence
		c.entries[source] = entry
		c.mu.Unlock()
		return entry.data, entry.err
	}
	request := c.pending[source]
	if request == nil {
		fetchCtx, cancel := context.WithTimeout(context.Background(), artworkTimeout)
		request = &artworkRequest{done: make(chan struct{}), cancel: cancel}
		c.pending[source] = request
		go func() {
			data, err := c.load(fetchCtx, source)
			c.mu.Lock()
			request.data, request.err = data, err
			if c.pending[source] == request {
				delete(c.pending, source)
				// Cancelled last consumers must not poison the next request.
				if request.users > 0 {
					ttl := 10 * time.Minute
					if err != nil {
						ttl = 30 * time.Second
					}
					c.sequence++
					c.entries[source] = artworkEntry{data: data, err: err, expires: time.Now().Add(ttl), used: c.sequence}
					for len(c.entries) > artworkCacheEntries {
						var oldest string
						used := c.sequence + 1
						for key, entry := range c.entries {
							if entry.used < used {
								oldest, used = key, entry.used
							}
						}
						delete(c.entries, oldest)
					}
				}
			}
			close(request.done)
			c.mu.Unlock()
			cancel()
		}()
	}
	request.users++
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		request.users--
		if request.users == 0 {
			request.cancel()
			if c.pending[source] == request {
				delete(c.pending, source)
			}
		}
		c.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-request.done:
		return request.data, request.err
	}
}

func artworkURL(u *url.URL) error {
	if u == nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || (u.Port() != "" && u.Port() != "443") {
		return errors.New("封面 URL 不受支持")
	}
	return nil
}

func publicArtworkIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	// Also exclude shared, documentation, benchmark and reserved destinations,
	// including translation mechanisms that can hide private IPv4 endpoints.
	for _, prefix := range artworkBlockedNetworks {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

var artworkBlockedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
}

func dialArtwork(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != "443" {
		return nil, errors.New("封面地址不受支持")
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("封面域名解析失败")
	}
	for _, ip := range ips {
		if !publicArtworkIP(ip) {
			return nil, errors.New("封面地址不是公网地址")
		}
	}
	// Dial the validated IP itself; resolving again would allow DNS rebinding.
	dialer := net.Dialer{Timeout: 2 * time.Second}
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, errors.New("封面连接失败")
}

func newArtworkClient() *http.Client {
	return &http.Client{
		Timeout: artworkTimeout,
		Transport: &http.Transport{
			// No environment proxy: destination validation must cover every dial.
			DialContext: dialArtwork, TLSHandshakeTimeout: 2 * time.Second,
			ResponseHeaderTimeout: 3 * time.Second, MaxResponseHeaderBytes: 16 << 10,
			MaxIdleConns: 4, MaxIdleConnsPerHost: 2, IdleConnTimeout: 30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > 3 {
				return errors.New("封面重定向过多")
			}
			return artworkURL(req.URL)
		},
	}
}

func downloadArtwork(ctx context.Context, client *http.Client, source string) ([]byte, error) {
	if len(source) > 8192 {
		return nil, errors.New("封面 URL 过长")
	}
	u, err := url.Parse(source)
	if err != nil || artworkURL(u) != nil {
		return nil, errors.New("封面 URL 不受支持")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("封面 URL 不受支持")
	}
	req.Header.Set("Accept", "image/jpeg, image/png")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("封面下载失败或超时")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("封面服务器返回错误")
	}
	if resp.ContentLength > artworkMaxDownload {
		return nil, errors.New("封面超过大小限制")
	}
	contentType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || (contentType != "image/jpeg" && contentType != "image/png") {
		return nil, errors.New("封面类型不受支持")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, artworkMaxDownload+1))
	if err != nil {
		return nil, errors.New("封面下载不完整")
	}
	if len(data) > artworkMaxDownload {
		return nil, errors.New("封面超过大小限制")
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || "image/"+format != contentType || config.Width <= 0 || config.Height <= 0 || config.Width > 4096 || config.Height > 4096 || int64(config.Width)*int64(config.Height) > 4<<20 {
		return nil, errors.New("封面格式或尺寸无效")
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("封面图片损坏")
	}
	// MRP accepts JPEG only. Preserve existing JPEGs that fit; convert PNG and
	// larger JPEGs to bounded baseline JPEGs for the native Apple TV path.
	if format != "jpeg" || len(data) > artworkMaxBytes || !bytes.HasSuffix(data, []byte{0xff, 0xd9}) {
		var encoded bytes.Buffer
		if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: 85}); err != nil {
			return nil, errors.New("封面转换失败")
		}
		data = encoded.Bytes()
	}
	if len(data) > artworkMaxBytes {
		return nil, errors.New("封面超过发送大小限制")
	}
	return data, nil
}
