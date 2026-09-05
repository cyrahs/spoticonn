package spotify

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func callbackFor(a *Authorization, code string) string {
	return oauthRedirectURI + "?" + url.Values{"state": {a.state}, "code": {code}}.Encode()
}

func TestAuthorizationUsesIndependentPKCEAndState(t *testing.T) {
	a := NewAuthorization(time.Now().Add(time.Minute))
	b := NewAuthorization(time.Now().Add(time.Minute))
	u, _ := url.Parse(a.URL)
	q := u.Query()
	hash := sha256.Sum256([]byte(a.verifier))
	if u.Host != "accounts.spotify.com" || q.Get("redirect_uri") != oauthRedirectURI || q.Get("state") != a.state ||
		q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(hash[:]) {
		t.Fatal("invalid PKCE authorization request")
	}
	if a.state == b.state || a.verifier == b.verifier || len(a.verifier) < 43 || strings.Contains(a.URL, a.verifier) {
		t.Fatal("authorization attempts share or expose their verifier")
	}
	if code, err := a.Code(callbackFor(a, "success")); err != nil || code != "success" {
		t.Fatal("valid callback rejected")
	}
	for _, callback := range []string{
		callbackFor(b, "other-attempt"),
		"https://attacker.example/login?code=bad&state=" + a.state,
		strings.Replace(callbackFor(a, "bad"), "127.0.0.1", "localhost", 1),
		strings.Replace(callbackFor(a, "bad"), "36842", "8080", 1),
		strings.Replace(callbackFor(a, "bad"), "/login", "/login/", 1),
		strings.Replace(callbackFor(a, "bad"), "127.0.0.1", "user@127.0.0.1", 1),
		callbackFor(a, "bad") + "#fragment",
		callbackFor(a, "bad") + "&state=" + a.state,
		callbackFor(a, "bad") + "&code=duplicate",
		callbackFor(a, "bad") + "&error=access_denied",
		callbackFor(a, "bad") + "&invalid=%zz",
		callbackFor(a, ""), "just-the-code",
	} {
		if _, err := a.Code(callback); err == nil {
			t.Errorf("accepted invalid callback: %s", callback)
		}
	}
	if _, err := a.Code(oauthRedirectURI + "?state=" + a.state + "&error=access_denied"); !errors.Is(err, ErrAuthorizationDenied) {
		t.Fatal("authorization denial not recognized")
	}
	a.ExpiresAt = time.Now().Add(-time.Second)
	if _, err := a.Code(callbackFor(a, "expired")); err == nil {
		t.Fatal("expired authorization accepted")
	}
}

type oauthTransport func(*http.Request) (*http.Response, error)

func (f oauthTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestExchangeSendsVerifierAndResolvesUsername(t *testing.T) {
	for _, inlineUsername := range []bool{true, false} {
		t.Run(map[bool]string{true: "upstream-username", false: "standard-profile"}[inlineUsername], func(t *testing.T) {
			a := NewAuthorization(time.Now().Add(time.Minute))
			requests := 0
			client := &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				body := `{"id":"alice"}`
				if requests == 1 {
					_ = r.ParseForm()
					if r.URL.String() != "https://accounts.spotify.com/api/token" || r.Method != "POST" ||
						r.Form.Get("code_verifier") != a.verifier || r.Form.Get("code") != "the-code" ||
						r.Form.Get("redirect_uri") != oauthRedirectURI || r.Form.Get("client_id") != oauthClientID {
						t.Fatal("incorrect token exchange request")
					}
					body = `{"access_token":"SECRET-TOKEN","token_type":"Bearer","expires_in":3600,"scope":"streaming user-read-private"`
					if inlineUsername {
						body += `,"username":"alice"`
					}
					body += `}`
				} else if r.URL.String() != "https://api.spotify.com/v1/me" || r.Header.Get("Authorization") != "Bearer SECRET-TOKEN" {
					t.Fatal("token sent to unexpected endpoint")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
			})}
			login, err := exchangeAuthorization(context.Background(), client, a, "the-code")
			if err != nil || login.Username != "alice" || login.AccessToken != "SECRET-TOKEN" || !time.Now().Before(login.ExpiresAt) {
				t.Fatal("failed to obtain bootstrap credentials")
			}
			if requests != map[bool]int{true: 1, false: 2}[inlineUsername] {
				t.Fatal("unexpected profile request")
			}
		})
	}
}

func TestExchangeRejectsBadResponsesWithoutLeakingSecrets(t *testing.T) {
	for _, body := range []string{
		`{"error":"SECRET-RESPONSE"}`,
		`{"access_token":"SECRET-TOKEN","token_type":"Bearer","expires_in":0}`,
		`{"access_token":"SECRET-TOKEN","token_type":"other","expires_in":3600}`,
		`{"access_token":"SECRET-TOKEN","token_type":"Bearer","expires_in":3600,"scope":"user-read-private"}`,
		`{"access_token":"SECRET-TOKEN","token_type":"Bearer","expires_in":3600}`, // invalid profile response
		strings.Repeat("SECRET", 20000),
	} {
		client := &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		_, err := exchangeAuthorization(context.Background(), client, NewAuthorization(time.Now().Add(time.Minute)), "SECRET-CODE")
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("invalid response accepted or leaked")
		}
	}
	for _, status := range []int{302, 400, 500} {
		client := &http.Client{Transport: oauthTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("SECRET"))}, nil
		})}
		_, err := exchangeAuthorization(context.Background(), client, NewAuthorization(time.Now().Add(time.Minute)), "SECRET-CODE")
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("HTTP failure accepted or leaked")
		}
	}
}
