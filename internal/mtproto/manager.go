package mtproto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/nofumex/telegram-aggregator/internal/domain"
	"github.com/nofumex/telegram-aggregator/internal/storage"
	"github.com/nofumex/telegram-aggregator/internal/telegramfeed"
)

var ErrNotAuthorized = errors.New("Telegram Account не авторизован; откройте Admin → Telegram Account")

type Manager struct {
	store           *storage.Store
	mu              sync.RWMutex
	client          *telegram.Client
	api             *tg.Client
	connected       bool
	restart         chan struct{}
	authMu          sync.Mutex
	phone, codeHash string
}

type sessionStore struct{ store *storage.Store }

func (s sessionStore) LoadSession(ctx context.Context) ([]byte, error) {
	return s.store.LoadMTProtoSession(ctx)
}
func (s sessionStore) StoreSession(ctx context.Context, b []byte) error {
	return s.store.SaveMTProtoSession(ctx, b)
}

func New(store *storage.Store) *Manager {
	return &Manager{store: store, restart: make(chan struct{}, 1)}
}

// Run owns the application's single long-lived MTProto client. gotd handles
// reconnects inside Client.Run; this loop only recreates it after credentials
// are deliberately changed or the account is logged out.
func (m *Manager) Run(ctx context.Context) {
	for ctx.Err() == nil {
		a, err := m.store.TelegramAccount(ctx)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
				continue
			}
		}
		client := telegram.NewClient(a.APIID, a.APIHash, telegram.Options{SessionStorage: sessionStore{m.store}, NoUpdates: true, AllowCDN: true, OnConnectionState: func(state telegram.ConnectionState) {
			m.mu.Lock()
			m.connected = state == telegram.ConnectionStateReady
			m.mu.Unlock()
		}})
		m.mu.Lock()
		m.client = client
		m.mu.Unlock()
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			_ = client.Run(runCtx, func(active context.Context) error {
				m.mu.Lock()
				m.api = client.API()
				m.connected = true
				m.mu.Unlock()
				status, e := client.Auth().Status(active)
				if e == nil && status.Authorized {
					_ = m.store.SetTelegramAccountStatus(context.WithoutCancel(active), "authorized", "")
				} else {
					_ = m.store.SetTelegramAccountStatus(context.WithoutCancel(active), "connected", "")
				}
				<-active.Done()
				return nil
			})
			m.mu.Lock()
			m.api = nil
			m.client = nil
			m.connected = false
			m.mu.Unlock()
			close(done)
		}()
		select {
		case <-ctx.Done():
			cancel()
			<-done
			return
		case <-m.restart:
			cancel()
			<-done
		}
	}
}

func (m *Manager) signalRestart() {
	select {
	case m.restart <- struct{}{}:
	default:
	}
}
func (m *Manager) Account(ctx context.Context) (domain.TelegramAccount, error) {
	a, err := m.store.TelegramAccount(ctx)
	m.mu.RLock()
	connected := m.connected
	m.mu.RUnlock()
	if err == nil && !connected && a.Status == "authorized" {
		a.Status = "reconnecting"
	}
	return a, err
}
func (m *Manager) Configure(ctx context.Context, apiID int, apiHash, phone string) error {
	if apiID <= 0 || strings.TrimSpace(apiHash) == "" {
		return errors.New("API ID и API Hash обязательны")
	}
	if err := m.store.SaveTelegramAccount(ctx, apiID, strings.TrimSpace(apiHash), strings.TrimSpace(phone)); err != nil {
		return err
	}
	m.signalRestart()
	return nil
}
func (m *Manager) apiClient() (*telegram.Client, *tg.Client, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.connected || m.client == nil || m.api == nil {
		return nil, nil, errors.New("MTProto соединение ещё не готово")
	}
	return m.client, m.api, nil
}
func (m *Manager) Authorized(ctx context.Context) bool {
	c, _, e := m.apiClient()
	if e != nil {
		return false
	}
	s, e := c.Auth().Status(ctx)
	return e == nil && s.Authorized
}

func (m *Manager) RequestCode(ctx context.Context, phone string) error {
	m.authMu.Lock()
	defer m.authMu.Unlock()
	c, _, err := m.apiClient()
	if err != nil {
		return err
	}
	sent, err := c.Auth().SendCode(ctx, strings.TrimSpace(phone), auth.SendCodeOptions{})
	if err != nil {
		return err
	}
	v, ok := sent.(*tg.AuthSentCode)
	if !ok {
		return errors.New("Telegram не запросил код авторизации")
	}
	m.phone, m.codeHash = strings.TrimSpace(phone), v.PhoneCodeHash
	a, _ := m.store.TelegramAccount(ctx)
	if err = m.store.SaveTelegramAccount(ctx, a.APIID, a.APIHash, m.phone); err != nil {
		return err
	}
	return m.store.SetTelegramAccountStatus(ctx, "code_sent", "")
}
func (m *Manager) SubmitCode(ctx context.Context, code string) (bool, error) {
	m.authMu.Lock()
	defer m.authMu.Unlock()
	c, _, err := m.apiClient()
	if err != nil {
		return false, err
	}
	if m.codeHash == "" {
		return false, errors.New("сначала запросите новый код")
	}
	_, err = c.Auth().SignIn(ctx, m.phone, strings.TrimSpace(code), m.codeHash)
	if tgerr.Is(err, "SESSION_PASSWORD_NEEDED") {
		_ = m.store.SetTelegramAccountStatus(ctx, "password_required", "")
		return true, nil
	}
	if err != nil {
		_ = m.store.SetTelegramAccountStatus(ctx, "error", err.Error())
		return false, err
	}
	m.codeHash = ""
	return false, m.store.SetTelegramAccountStatus(ctx, "authorized", "")
}
func (m *Manager) SubmitPassword(ctx context.Context, password string) error {
	c, _, err := m.apiClient()
	if err != nil {
		return err
	}
	_, err = c.Auth().Password(ctx, password)
	if err != nil {
		_ = m.store.SetTelegramAccountStatus(ctx, "error", err.Error())
		return err
	}
	return m.store.SetTelegramAccountStatus(ctx, "authorized", "")
}
func (m *Manager) Logout(ctx context.Context) error {
	_, api, err := m.apiClient()
	if err == nil {
		_, err = api.AuthLogOut(ctx)
	}
	clearErr := m.store.ClearMTProtoSession(ctx)
	m.signalRestart()
	if err != nil && strings.Contains(err.Error(), "соединение ещё не готово") {
		return clearErr
	}
	if err != nil {
		return err
	}
	return clearErr
}

type Peer struct {
	Input tg.InputPeerClass
	Name  string
	Kind  string
}

func (m *Manager) Resolve(ctx context.Context, username string) (Peer, error) {
	if !m.Authorized(ctx) {
		return Peer{}, ErrNotAuthorized
	}
	_, api, err := m.apiClient()
	if err != nil {
		return Peer{}, err
	}
	r, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: username})
	if err != nil {
		return Peer{}, err
	}
	for _, raw := range r.Chats {
		if ch, ok := raw.(*tg.Channel); ok {
			kind := "web_channel"
			if ch.Megagroup || ch.Gigagroup {
				kind = "mtproto_group"
			}
			return Peer{Input: ch.AsInputPeer(), Name: ch.Title, Kind: kind}, nil
		}
	}
	return Peer{}, errors.New("username не является публичным каналом или supergroup")
}

func (m *Manager) Fetch(ctx context.Context, req telegramfeed.FetchRequest) (telegramfeed.FetchResult, error) {
	peer, err := m.Resolve(ctx, req.Username)
	if err != nil {
		return telegramfeed.FetchResult{}, err
	}
	if peer.Kind != "mtproto_group" {
		return telegramfeed.FetchResult{}, errors.New("источник не является группой")
	}
	_, api, _ := m.apiClient()
	limit := req.Limit
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	before := 0
	seen := map[int64]bool{}
	posts := make([]domain.TelegramPost, 0, limit)
	for len(posts) < limit {
		page, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: peer.Input, OffsetID: before, Limit: min(100, limit-len(posts)), MinID: int(req.AfterID)})
		if err != nil {
			return telegramfeed.FetchResult{}, err
		}
		modified, ok := page.AsModified()
		if !ok {
			break
		}
		messages := modified.GetMessages()
		if len(messages) == 0 {
			break
		}
		oldest := 0
		for _, raw := range messages {
			msg, ok := raw.(*tg.Message)
			if !ok || msg.ID <= int(req.AfterID) || seen[int64(msg.ID)] {
				continue
			}
			seen[int64(msg.ID)] = true
			if oldest == 0 || msg.ID < oldest {
				oldest = msg.ID
			}
			p := domain.TelegramPost{ChannelUsername: req.Username, MessageID: int64(msg.ID), URL: fmt.Sprintf("https://t.me/%s/%d", req.Username, msg.ID), Text: msg.Message, PublishedAt: time.Unix(int64(msg.Date), 0).UTC()}
			p.Raw, _ = json.Marshal(map[string]any{"id": msg.ID, "date": msg.Date})
			p.PhotoData, p.PhotoMime = m.firstPhoto(ctx, msg)
			posts = append(posts, p)
		}
		if oldest == 0 || oldest == before {
			break
		}
		before = oldest
	}
	sort.Slice(posts, func(i, j int) bool { return posts[i].MessageID > posts[j].MessageID })
	if len(posts) > limit {
		posts = posts[:limit]
	}
	return telegramfeed.FetchResult{Posts: posts, Name: peer.Name}, nil
}
func (m *Manager) firstPhoto(ctx context.Context, msg *tg.Message) ([]byte, string) {
	media, ok := msg.Media.(*tg.MessageMediaPhoto)
	if !ok {
		return nil, ""
	}
	raw, ok := media.GetPhoto()
	if !ok {
		return nil, ""
	}
	photo, ok := raw.(*tg.Photo)
	if !ok || len(photo.Sizes) == 0 {
		return nil, ""
	}
	thumb := photo.Sizes[len(photo.Sizes)-1].GetType()
	var b bytes.Buffer
	limited := &limitWriter{w: &b, n: 8 << 20}
	client, _, err := m.apiClient()
	if err != nil {
		return nil, ""
	}
	_, err = client.Download(photo.AsInputPhotoFileLocation(thumb)).Stream(ctx, limited)
	if err != nil || limited.exceeded {
		return nil, ""
	}
	return b.Bytes(), "image/jpeg"
}

type limitWriter struct {
	w        io.Writer
	n        int64
	exceeded bool
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.n {
		l.exceeded = true
		return 0, errors.New("photo too large")
	}
	n, e := l.w.Write(p)
	l.n -= int64(n)
	return n, e
}
