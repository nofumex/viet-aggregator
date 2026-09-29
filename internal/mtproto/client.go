package mtproto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nofumex/telegram-aggregator/internal/domain"
	"github.com/nofumex/telegram-aggregator/internal/storage"
	"github.com/nofumex/telegram-aggregator/internal/telegramfeed"
)

var ErrNotAuthorized = errors.New("Telegram Account не авторизован; откройте Admin → Telegram Account")

type Manager struct {
	store *storage.Store
	base  string
	http  *http.Client
}

func New(store *storage.Store) *Manager {
	base := strings.TrimRight(os.Getenv("MTPROTO_URL"), "/")
	if base == "" {
		base = "http://mtproto:8081"
	}
	return &Manager{store: store, base: base, http: &http.Client{Timeout: 30 * time.Second}}
}

// Run keeps the sidecar configured after either container restarts. The
// long-lived MTProto connection itself belongs exclusively to Hydrogram.
func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		_ = m.pushConfig(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) pushConfig(ctx context.Context) error {
	a, err := m.store.TelegramAccount(ctx)
	if err != nil {
		return err
	}
	session, _ := m.store.LoadMTProtoSession(ctx)
	var out statusResponse
	if err := m.call(ctx, http.MethodPost, "/configure", map[string]any{"api_id": a.APIID, "api_hash": a.APIHash, "phone": a.Phone, "session_string": string(session)}, &out); err != nil {
		return err
	}
	if out.SessionRejected {
		return m.store.ClearMTProtoSession(ctx)
	}
	return nil
}

func (m *Manager) Account(ctx context.Context) (domain.TelegramAccount, error) {
	a, err := m.store.TelegramAccount(ctx)
	if err != nil {
		return a, err
	}
	var remote statusResponse
	if e := m.call(ctx, http.MethodGet, "/status", nil, &remote); e != nil {
		a.Status = "sidecar_unavailable"
		a.LastError = e.Error()
		return a, nil
	}
	a.Status = remote.Status
	if remote.Error != "" {
		a.LastError = remote.Error
	}
	return a, nil
}
func (m *Manager) Configure(ctx context.Context, apiID int, apiHash, phone string) error {
	if apiID <= 0 || strings.TrimSpace(apiHash) == "" {
		return errors.New("API ID и API Hash обязательны")
	}
	if err := m.store.SaveTelegramAccount(ctx, apiID, strings.TrimSpace(apiHash), strings.TrimSpace(phone)); err != nil {
		return err
	}
	return m.pushConfig(ctx)
}
func (m *Manager) Authorized(ctx context.Context) bool {
	var s statusResponse
	return m.call(ctx, http.MethodGet, "/status", nil, &s) == nil && s.Authorized
}
func (m *Manager) RequestCode(ctx context.Context, phone string) error {
	if err := m.pushConfig(ctx); err != nil {
		return err
	}
	var out statusResponse
	if err := m.call(ctx, http.MethodPost, "/auth/send-code", map[string]string{"phone": strings.TrimSpace(phone)}, &out); err != nil {
		return err
	}
	a, _ := m.store.TelegramAccount(ctx)
	if err := m.store.SaveTelegramAccount(ctx, a.APIID, a.APIHash, strings.TrimSpace(phone)); err != nil {
		return err
	}
	return m.store.SetTelegramAccountStatus(ctx, "code_sent", "")
}
func (m *Manager) SubmitCode(ctx context.Context, code string) (bool, error) {
	var out authResponse
	err := m.call(ctx, http.MethodPost, "/auth/sign-in", map[string]string{"code": strings.TrimSpace(code)}, &out)
	if err != nil {
		return false, err
	}
	if out.PasswordRequired {
		_ = m.store.SetTelegramAccountStatus(ctx, "password_required", "")
		return true, nil
	}
	return false, m.saveAuthorized(ctx, out.SessionString)
}
func (m *Manager) SubmitPassword(ctx context.Context, password string) error {
	var out authResponse
	if err := m.call(ctx, http.MethodPost, "/auth/check-password", map[string]string{"password": password}, &out); err != nil {
		return err
	}
	return m.saveAuthorized(ctx, out.SessionString)
}
func (m *Manager) saveAuthorized(ctx context.Context, session string) error {
	if session == "" {
		return errors.New("sidecar не вернул session string")
	}
	if err := m.store.SaveMTProtoSession(ctx, []byte(session)); err != nil {
		return err
	}
	return m.store.SetTelegramAccountStatus(ctx, "authorized", "")
}
func (m *Manager) Logout(ctx context.Context) error {
	var out statusResponse
	remoteErr := m.call(ctx, http.MethodPost, "/auth/logout", map[string]any{}, &out)
	clearErr := m.store.ClearMTProtoSession(ctx)
	if remoteErr != nil {
		return remoteErr
	}
	return clearErr
}

type Peer struct{ Name, Kind string }

func (m *Manager) Resolve(ctx context.Context, username string) (Peer, error) {
	if !m.Authorized(ctx) {
		return Peer{}, ErrNotAuthorized
	}
	var out struct {
		Name string `json:"name"`
		Kind string `json:"kind"`
	}
	err := m.call(ctx, http.MethodGet, "/resolve/"+url.PathEscape(username), nil, &out)
	return Peer(out), err
}
func (m *Manager) Fetch(ctx context.Context, req telegramfeed.FetchRequest) (telegramfeed.FetchResult, error) {
	q := url.Values{}
	q.Set("after_id", strconv.FormatInt(req.AfterID, 10))
	q.Set("limit", strconv.Itoa(req.Limit))
	var out struct {
		Name     string `json:"name"`
		Messages []struct {
			MessageID   int64  `json:"message_id"`
			Text        string `json:"text"`
			PublishedAt string `json:"published_at"`
			OriginalURL string `json:"original_url"`
			HasPhoto    bool   `json:"has_photo"`
		} `json:"messages"`
	}
	if err := m.call(ctx, http.MethodGet, "/history/"+url.PathEscape(req.Username)+"?"+q.Encode(), nil, &out); err != nil {
		return telegramfeed.FetchResult{}, err
	}
	posts := make([]domain.TelegramPost, 0, len(out.Messages))
	for _, v := range out.Messages {
		published, _ := time.Parse(time.RFC3339, v.PublishedAt)
		posts = append(posts, domain.TelegramPost{ChannelUsername: req.Username, MessageID: v.MessageID, URL: v.OriginalURL, Text: v.Text, PublishedAt: published, HasPhoto: v.HasPhoto})
	}
	return telegramfeed.FetchResult{Name: out.Name, Posts: posts, Kind: "mtproto_group"}, nil
}
func (m *Manager) Photo(ctx context.Context, username string, messageID int64) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/photo/%s/%d", m.base, url.PathEscape(username), messageID), nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, "", responseError(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return data, resp.Header.Get("Content-Type"), err
}

type statusResponse struct {
	Status          string `json:"status"`
	Authorized      bool   `json:"authorized"`
	Error           string `json:"error"`
	SessionRejected bool   `json:"session_rejected"`
}
type authResponse struct {
	Status           string `json:"status"`
	PasswordRequired bool   `json:"password_required"`
	SessionString    string `json:"session_string"`
}

func (m *Manager) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.http.Do(req)
	if err != nil {
		return fmt.Errorf("MTProto sidecar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return responseError(resp)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(out)
}
func responseError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(b, &e)
	if e.Error == "" {
		e.Error = strings.TrimSpace(string(b))
	}
	return fmt.Errorf("MTProto sidecar HTTP %d: %s", resp.StatusCode, e.Error)
}
