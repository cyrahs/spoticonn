package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"golang.org/x/crypto/bcrypt"
	"spoticonn/internal/bridge"
	"spoticonn/internal/model"
	"spoticonn/internal/store"
)

func testHandler(t *testing.T) http.Handler {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Update(func(v *store.State) error {
		v.Pairings["tv"] = model.PairingSecret{Credentials: "SECRET-PAIRING-KEY"}
		return nil
	})
	m := bridge.New(context.Background(), s, bridge.Config{DisableDiscovery: true})
	hash, _ := bcrypt.GenerateFromPassword([]byte("test-admin-password"), bcrypt.MinCost)
	h, err := New(m, string(hash), false, fstest.MapFS{"index.html": {Data: []byte("<html>Spoticonn</html>")}})
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func request(h http.Handler, method, path, body string, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Spoticonn-Request", "1")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func login(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	w := request(h, "POST", "/api/auth/login", `{"password":"test-admin-password"}`, nil, "")
	if w.Code != 200 {
		t.Fatalf("login: %d %s", w.Code, w.Body)
	}
	cs := w.Result().Cookies()
	if len(cs) == 0 {
		t.Fatal("missing session cookie")
	}
	return cs[0]
}
func TestAuthenticationAndSecretRedaction(t *testing.T) {
	h := testHandler(t)
	if w := request(h, "GET", "/api/state", "", nil, ""); w.Code != 401 {
		t.Fatal("unauthenticated state exposed")
	}
	c := login(t, h)
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatal("insecure cookie")
	}
	w := request(h, "GET", "/api/state", "", c, "")
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	if strings.Contains(w.Body.String(), "SECRET-PAIRING-KEY") {
		t.Fatal("credential leaked")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("sensitive response cacheable")
	}
	var v model.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	request(h, "POST", "/api/auth/logout", "", c, "")
	if request(h, "GET", "/api/state", "", c, "").Code != 401 {
		t.Fatal("logout did not revoke session")
	}
}
func TestCSRFAndBodyValidation(t *testing.T) {
	h := testHandler(t)
	c := login(t, h)
	if request(h, "POST", "/api/playback", `{"action":"pause"}`, c, "https://attacker.example").Code != 403 {
		t.Fatal("cross-origin request accepted")
	}
	r := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"password":"test-admin-password"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("missing custom header accepted")
	}
	for _, body := range []string{`{"action":"pause","unexpected":1}`, `{} {}`, `{"action":`} {
		if request(h, "POST", "/api/playback", body, c, "").Code != 400 {
			t.Fatalf("invalid JSON accepted: %s", body)
		}
	}
	if request(h, "POST", "/api/playback", `{"action":"volume","value":101}`, c, "").Code != 400 {
		t.Fatal("out of range volume accepted")
	}
}
func TestHealthIndependentOfSpeakersAndAPINotSPA(t *testing.T) {
	h := testHandler(t)
	for _, path := range []string{"/healthz", "/readyz"} {
		if request(h, "GET", path, "", nil, "").Code != 200 {
			t.Fatal("offline speaker made server unhealthy")
		}
	}
	if request(h, "GET", "/api/missing", "", nil, "").Code != 404 {
		t.Fatal("API typo returned SPA")
	}
	if !strings.Contains(request(h, "GET", "/", "", nil, "").Body.String(), "Spoticonn") {
		t.Fatal("frontend not served")
	}
}
func TestLoginRateLimit(t *testing.T) {
	h := testHandler(t)
	for i := 0; i < 8; i++ {
		if request(h, "POST", "/api/auth/login", `{"password":"wrong"}`, nil, "").Code != 401 {
			t.Fatal("unexpected initial limit")
		}
	}
	if request(h, "POST", "/api/auth/login", `{"password":"wrong"}`, nil, "").Code != 429 {
		t.Fatal("brute-force attempts not limited")
	}
}
