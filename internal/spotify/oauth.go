package spotify

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"spoticonn/internal/store"
)

// These match go-librespot v0.9.0's public OAuth client and loopback login
// route. No listener is started: the user copies the final browser URL back
// to the authenticated management page, including its one-time state.
const oauthClientID = "65b708073fc0480ea92a077233ca87bd"
const oauthRedirectURI = "http://127.0.0.1:36842/login"
const AuthorizationLifetime = 10 * time.Minute

type Authorization struct {
	URL       string
	ExpiresAt time.Time
	state     string
	verifier  string
}

// Login is only used to bootstrap stored device credentials. It must never be
// included in a management response or a log message.
type Login struct {
	Username    string
	AccessToken string
	ExpiresAt   time.Time
}

func NewAuthorization(expires time.Time) *Authorization {
	a := &Authorization{state: store.ID(32), verifier: store.ID(32), ExpiresAt: expires}
	challenge := sha256.Sum256([]byte(a.verifier))
	q := url.Values{
		"client_id": {oauthClientID}, "response_type": {"code"},
		"redirect_uri": {oauthRedirectURI}, "scope": {"streaming user-read-private"},
		"state": {a.state}, "code_challenge_method": {"S256"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"show_dialog":    {"true"},
	}
	a.URL = "https://accounts.spotify.com/authorize?" + q.Encode()
	return a
}

var ErrAuthorizationDenied = errors.New("已取消 Spotify 授权，请重新登录")

// Code validates data only; it never fetches the pasted URL. In particular,
// arbitrary URLs, repeated parameters and callbacks from another attempt are
// rejected before any request carrying a verifier can leave this process.
func (a *Authorization) Code(callback string) (string, error) {
	if !time.Now().Before(a.ExpiresAt) {
		return "", errors.New("Spotify 授权已超时，请重新登录")
	}
	u, err := url.Parse(strings.TrimSpace(callback))
	if err != nil || u.User != nil || u.Fragment != "" || u.Scheme+"://"+u.Host+u.EscapedPath() != oauthRedirectURI {
		return "", errors.New("请粘贴授权后地址栏中的完整回调地址")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q["state"]) != 1 || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(a.state)) != 1 {
		return "", errors.New("授权地址与本次登录不匹配，请使用当前登录链接重新授权")
	}
	if len(q["error"]) == 1 && q.Get("error") != "" && len(q["code"]) == 0 {
		return "", ErrAuthorizationDenied
	}
	if len(q["code"]) != 1 || q.Get("code") == "" || len(q["error"]) != 0 {
		return "", errors.New("回调地址缺少有效授权码，请重新授权")
	}
	return q.Get("code"), nil
}

func ExchangeAuthorization(ctx context.Context, a *Authorization, code string) (*Login, error) {
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return exchangeAuthorization(ctx, client, a, code)
}

func exchangeAuthorization(ctx context.Context, client *http.Client, a *Authorization, code string) (*Login, error) {
	form := url.Values{
		"grant_type": {"authorization_code"}, "client_id": {oauthClientID},
		"redirect_uri": {oauthRedirectURI}, "code": {code}, "code_verifier": {a.verifier},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", "https://accounts.spotify.com/api/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, errors.New("无法创建 Spotify 授权请求")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("无法连接 Spotify 授权服务，请重新登录")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("Spotify 授权兑换失败，请使用新的登录链接重试")
	}
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Username    string `json:"username"`
		ExpiresIn   int64  `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&token) != nil || token.AccessToken == "" ||
		!strings.EqualFold(token.TokenType, "Bearer") || token.ExpiresIn <= 0 || token.ExpiresIn > 86400 {
		return nil, errors.New("Spotify 返回的授权信息无效，请重新登录")
	}
	if token.Scope != "" && !strings.Contains(" "+token.Scope+" ", " streaming ") {
		return nil, errors.New("Spotify 未授予播放权限，请重新登录并允许播放")
	}
	if token.Username == "" {
		// Standard Spotify OAuth responses omit username; /me supplies the
		// stable user ID required by the engine's spotify_token credentials.
		req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.spotify.com/v1/me", nil)
		req.Header.Set("Authorization", "Bearer "+token.AccessToken)
		profile, err := client.Do(req)
		if err != nil {
			return nil, errors.New("无法读取 Spotify 账号信息，请重新登录")
		}
		defer profile.Body.Close()
		var user struct {
			ID string `json:"id"`
		}
		if profile.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(profile.Body, 64<<10)).Decode(&user) != nil || user.ID == "" {
			return nil, errors.New("无法读取 Spotify 账号信息，请重新登录")
		}
		token.Username = user.ID
	}
	return &Login{Username: token.Username, AccessToken: token.AccessToken, ExpiresAt: time.Now().Add(time.Duration(token.ExpiresIn) * time.Second)}, nil
}
