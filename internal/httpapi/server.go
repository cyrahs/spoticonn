package httpapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	"spoticonn/internal/bridge"
	"spoticonn/internal/model"
	"spoticonn/internal/store"
)

type Server struct {
	manager  *bridge.Manager
	hash     []byte
	secure   bool
	mu       sync.Mutex
	sessions map[string]time.Time
	attempts map[string]attempt
	files    fs.FS
}
type attempt struct {
	count int
	until time.Time
}

func New(m *bridge.Manager, passwordHash string, secure bool, files fs.FS) (http.Handler, error) {
	if _, err := bcrypt.Cost([]byte(passwordHash)); err != nil {
		return nil, errors.New("invalid admin password hash")
	}
	s := &Server{manager: m, hash: []byte(passwordHash), secure: secure, sessions: map[string]time.Time{}, attempts: map[string]attempt{}, files: files}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { write(w, 200, map[string]bool{"ok": true}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) { write(w, 200, map[string]bool{"ready": true}) })
	mux.HandleFunc("POST /api/auth/login", s.login)
	mux.Handle("POST /api/auth/logout", s.auth(http.HandlerFunc(s.logout)))
	mux.Handle("GET /api/state", s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { write(w, 200, m.Snapshot()) })))
	mux.Handle("GET /api/accounts", s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { write(w, 200, m.Snapshot().Accounts) })))
	mux.Handle("POST /api/accounts", s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Label string `json:"label"`
		}
		if !decode(w, r, &b) {
			return
		}
		a, err := m.AddAccount(b.Label)
		if err != nil {
			fail(w, 400, err)
			return
		}
		write(w, 201, a)
	})))
	mux.Handle("DELETE /api/accounts/{id}", s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { respond(w, m.DeleteAccount(r.PathValue("id"))) })))
	mux.Handle("POST /api/accounts/{id}/rebind", s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { respond(w, m.Rebind(r.PathValue("id"))) })))
	mux.Handle("POST /api/accounts/{id}/oauth", s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			CallbackURL string `json:"callback_url"`
		}
		if !decode(w, r, &b) {
			return
		}
		respond(w, m.CompleteAuthorization(r.Context(), r.PathValue("id"), b.CallbackURL))
	})))
	mux.Handle("GET /api/airplay/devices", s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { write(w, 200, m.Snapshot().Devices) })))
	mux.Handle("POST /api/airplay/pairings", s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			DeviceID string `json:"device_id"`
			MemberID string `json:"member_id"`
		}
		if !decode(w, r, &b) {
			return
		}
		p, err := m.StartMemberPairing(b.DeviceID, b.MemberID)
		if err != nil {
			fail(w, 400, err)
			return
		}
		write(w, 201, p)
	})))
	mux.Handle("POST /api/airplay/pairings/{id}/pin", s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			PIN string `json:"pin"`
		}
		if !decode(w, r, &b) {
			return
		}
		respond(w, m.SubmitPIN(r.PathValue("id"), b.PIN))
	})))
	mux.Handle("POST /api/airplay/pairings/{id}/cancel", s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		respond(w, m.CancelPairing(r.PathValue("id")))
	})))
	mux.Handle("GET /api/settings", s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { write(w, 200, m.Snapshot().Settings) })))
	mux.Handle("PUT /api/settings", s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b model.Settings
		if !decode(w, r, &b) {
			return
		}
		respond(w, m.UpdateSettings(r.Context(), b))
	})))
	mux.Handle("POST /api/playback", s.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			Action string `json:"action"`
			Value  int64  `json:"value"`
		}
		if !decode(w, r, &b) {
			return
		}
		respond(w, m.Playback(r.Context(), b.Action, b.Value))
	})))
	mux.Handle("GET /api/events", s.auth(http.HandlerFunc(s.events)))
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, 404, errors.New("接口不存在")) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.static(w, r)
	})
	return s.headers(mux), nil
}

func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' https://i.scdn.co https://mosaic.scdn.co data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if r.Header.Get("X-Spoticonn-Request") != "1" {
				fail(w, 403, errors.New("请求缺少来源校验"))
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host || (u.Scheme != "http" && u.Scheme != "https") {
					fail(w, 403, errors.New("请求来源不匹配"))
					return
				}
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				fail(w, 403, errors.New("不接受跨站请求"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func digest(v string) string { sum := sha256.Sum256([]byte(v)); return hex.EncodeToString(sum[:]) }
func (s *Server) authorized(r *http.Request) bool {
	c, err := r.Cookie("spoticonn_session")
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.sessions[digest(c.Value)]
	if !ok || time.Now().After(expires) {
		delete(s.sessions, digest(c.Value))
		return false
	}
	return true
}
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			fail(w, 401, errors.New("请先登录"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &b) {
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	s.mu.Lock()
	now := time.Now()
	for k, v := range s.attempts {
		if now.After(v.until) {
			delete(s.attempts, k)
		}
	}
	a := s.attempts[ip]
	if a.count >= 8 && now.Before(a.until) {
		s.mu.Unlock()
		w.Header().Set("Retry-After", "60")
		fail(w, 429, errors.New("尝试过多，请稍后重试"))
		return
	}
	if len(s.attempts) > 4096 {
		s.mu.Unlock()
		fail(w, 429, errors.New("请稍后重试"))
		return
	}
	a.count++
	a.until = now.Add(time.Minute)
	s.attempts[ip] = a
	s.mu.Unlock()
	if len(b.Password) > 72 || bcrypt.CompareHashAndPassword(s.hash, []byte(b.Password)) != nil {
		fail(w, 401, errors.New("管理密码不正确"))
		return
	}
	token := store.ID(32)
	expires := time.Now().Add(12 * time.Hour)
	s.mu.Lock()
	delete(s.attempts, ip)
	for k, v := range s.sessions {
		if now.After(v) {
			delete(s.sessions, k)
		}
	}
	if len(s.sessions) >= 128 {
		s.mu.Unlock()
		fail(w, 429, errors.New("登录会话过多，请稍后重试"))
		return
	}
	s.sessions[digest(token)] = expires
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: "spoticonn_session", Value: token, Path: "/", Expires: expires, MaxAge: 43200, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteStrictMode})
	write(w, 200, map[string]bool{"ok": true})
}
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("spoticonn_session"); err == nil {
		s.mu.Lock()
		delete(s.sessions, digest(c.Value))
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "spoticonn_session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteStrictMode})
	write(w, 200, map[string]bool{"ok": true})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	f, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, errors.New("不支持事件流"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, unsubscribe := s.manager.Subscribe()
	defer unsubscribe()
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	send := func() error {
		b, err := json.Marshal(s.manager.Snapshot())
		if err != nil {
			return err
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Second))
		_, err = w.Write(append(append([]byte("event: state\ndata: "), b...), []byte("\n\n")...))
		f.Flush()
		return err
	}
	if send() != nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ch:
			if !s.authorized(r) || send() != nil {
				return
			}
		case <-t.C:
			if !s.authorized(r) || send() != nil {
				return
			}
		}
	}
}

func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	if !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	b, err := fs.ReadFile(s.files, name)
	if err != nil && !strings.Contains(name, ".") {
		name = "index.html"
		b, err = fs.ReadFile(s.files, name)
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if strings.HasSuffix(name, ".js") {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	} else if strings.HasSuffix(name, ".css") {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	} else if strings.HasSuffix(name, ".html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
	}
	_, _ = w.Write(b)
}
func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		fail(w, 400, errors.New("请求格式无效"))
		return false
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		fail(w, 400, errors.New("请求只能包含一个 JSON 对象"))
		return false
	}
	return true
}
func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, err error) {
	write(w, status, map[string]string{"error": err.Error()})
}
func respond(w http.ResponseWriter, err error) {
	if err != nil {
		fail(w, 400, err)
		return
	}
	write(w, 200, map[string]bool{"ok": true})
}
