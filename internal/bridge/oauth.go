package bridge

import (
	"context"
	"errors"
	"time"

	"spoticonn/internal/model"
	"spoticonn/internal/spotify"
)

func (m *Manager) CompleteAuthorization(ctx context.Context, id, callback string) error {
	m.op.Lock()
	var account *model.Account
	for _, a := range m.store.Snapshot().Accounts {
		if a.ID == id && !a.Bound {
			account = &a
			break
		}
	}
	m.mu.Lock()
	r := m.accounts[id]
	if account == nil || r == nil || r.authorization == nil || r.status != "waiting_oauth" || m.ctx.Err() != nil {
		m.mu.Unlock()
		m.op.Unlock()
		return errors.New("没有等待授权的账号，请重新登录")
	}
	flow := r.authorization
	code, err := flow.Code(callback)
	if err != nil {
		if errors.Is(err, spotify.ErrAuthorizationDenied) {
			r.authorization = nil
			r.status, r.errorText = "oauth_error", err.Error()
		}
		m.mu.Unlock()
		m.op.Unlock()
		m.changed()
		return err
	}
	// Consume this attempt before releasing the lock, so replayed/concurrent
	// submissions cannot exchange the same code more than once.
	r.status, r.errorText = "authorizing", ""
	m.mu.Unlock()
	m.op.Unlock()
	m.changed()

	// Spotify requests must not hold the playback/lifecycle lock. Deletion,
	// re-login and shutdown can invalidate the attempt while the request runs.
	ctx, cancel := context.WithDeadline(ctx, flow.ExpiresAt)
	stop := context.AfterFunc(m.ctx, cancel)
	login, exchangeErr := m.cfg.ExchangeAuthorization(ctx, flow, code)
	stop()
	cancel()

	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	if m.accounts[id] != r || r.authorization != flow || m.ctx.Err() != nil || !time.Now().Before(flow.ExpiresAt) {
		m.mu.Unlock()
		return errors.New("本次授权已失效，请使用当前登录链接重试")
	}
	r.authorization = nil
	if exchangeErr != nil || login == nil || login.Username == "" || login.AccessToken == "" || !time.Now().Before(login.ExpiresAt) {
		r.status, r.errorText = "oauth_error", "Spotify 授权失败，请重新登录并粘贴新的回调地址"
		message := r.errorText
		m.mu.Unlock()
		m.changed()
		return errors.New(message)
	}
	m.mu.Unlock()
	for _, existing := range m.store.Snapshot().Accounts {
		if existing.ID != id && existing.Bound && existing.Username == login.Username {
			m.bind(*account, login.Username)
			m.changed()
			return errors.New("此 Spotify 账号已登录，请删除这条重复记录")
		}
	}
	m.mu.Lock()
	r.login = login
	m.mu.Unlock()
	m.start(*account)
	m.changed()
	return nil
}
