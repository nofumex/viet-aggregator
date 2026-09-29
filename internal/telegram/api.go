package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

const (
	maxDownloadedPhotoBytes = 8 << 20
	photoDownloadTimeout    = 8 * time.Second
)

type User struct {
	ID        int64  `json:"id"`
	Username  string `json:"username"`
	FirstName string `json:"first_name"`
}
type Chat struct {
	ID int64 `json:"id"`
}
type Message struct {
	MessageID int    `json:"message_id"`
	From      User   `json:"from"`
	Chat      Chat   `json:"chat"`
	Text      string `json:"text"`
	Caption   string `json:"caption"`
	Photo     []struct {
		FileID string `json:"file_id"`
	} `json:"photo"`
}
type CallbackQuery struct {
	ID      string  `json:"id"`
	From    User    `json:"from"`
	Message Message `json:"message"`
	Data    string  `json:"data"`
}
type Update struct {
	UpdateID int            `json:"update_id"`
	Message  *Message       `json:"message"`
	Callback *CallbackQuery `json:"callback_query"`
}
type Button struct {
	Text         string `json:"text"`
	URL          string `json:"url,omitempty"`
	CallbackData string `json:"callback_data,omitempty"`
}
type Markup struct {
	InlineKeyboard [][]Button `json:"inline_keyboard"`
}
type InputMediaPhoto struct {
	Type      string `json:"type"`
	Media     string `json:"media"`
	Caption   string `json:"caption,omitempty"`
	ParseMode string `json:"parse_mode,omitempty"`
}
type Client struct {
	base          string
	http          *http.Client
	mediaHTTP     *http.Client
	maxPhotoBytes int64
}

func NewClient(token string) *Client {
	return &Client{
		base:          "https://api.telegram.org/bot" + token,
		http:          &http.Client{Timeout: 35 * time.Second},
		mediaHTTP:     &http.Client{Timeout: photoDownloadTimeout},
		maxPhotoBytes: maxDownloadedPhotoBytes,
	}
}
func (c *Client) call(ctx context.Context, method string, payload any, out any) error {
	b, e := json.Marshal(payload)
	if e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/"+method, bytes.NewReader(b))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	resp, e := c.http.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	return decodeTelegramResponse(resp.Body, method, out)
}

func decodeTelegramResponse(body io.Reader, method string, out any) error {
	var env struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if e := json.NewDecoder(body).Decode(&env); e != nil {
		return e
	}
	if !env.OK {
		return fmt.Errorf("telegram %s: %s", method, env.Description)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

func withMarkup(payload map[string]any, k Markup) map[string]any {
	if len(k.InlineKeyboard) > 0 {
		payload["reply_markup"] = k
	}
	return payload
}
func (c *Client) Updates(ctx context.Context, offset int) ([]Update, error) {
	var out []Update
	e := c.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 25, "allowed_updates": []string{"message", "callback_query"}}, &out)
	return out, e
}
func (c *Client) Send(ctx context.Context, chat int64, text string, k Markup) (Message, error) {
	var out Message
	e := c.call(ctx, "sendMessage", withMarkup(map[string]any{"chat_id": chat, "text": text, "parse_mode": "HTML", "disable_web_page_preview": true}, k), &out)
	return out, e
}
func (c *Client) Edit(ctx context.Context, chat int64, msg int, text string, k Markup) error {
	return c.call(ctx, "editMessageText", withMarkup(map[string]any{"chat_id": chat, "message_id": msg, "text": text, "parse_mode": "HTML", "disable_web_page_preview": true}, k), nil)
}

func (c *Client) DownloadPhoto(ctx context.Context, photoURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, photoURL, nil)
	if err != nil {
		return nil, err
	}
	client := c.mediaHTTP
	if client == nil {
		client = &http.Client{Timeout: photoDownloadTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download photo: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("download photo: HTTP %d", resp.StatusCode)
	}
	limit := c.maxPhotoBytes
	if limit <= 0 {
		limit = maxDownloadedPhotoBytes
	}
	if resp.ContentLength > limit {
		return nil, fmt.Errorf("download photo: content length %d exceeds %d bytes", resp.ContentLength, limit)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("download photo: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("download photo: exceeds %d bytes", limit)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("download photo: empty response")
	}
	return data, nil
}

func (c *Client) SendPhoto(ctx context.Context, chat int64, photo []byte, caption string, k Markup) (Message, error) {
	var out Message
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	fields := map[string]string{"chat_id": fmt.Sprint(chat), "caption": caption, "parse_mode": "HTML"}
	if len(k.InlineKeyboard) > 0 {
		raw, err := json.Marshal(k)
		if err != nil {
			return out, err
		}
		fields["reply_markup"] = string(raw)
	}
	for name, value := range fields {
		if err := w.WriteField(name, value); err != nil {
			return out, err
		}
	}
	part, err := w.CreateFormFile("photo", "listing.jpg")
	if err != nil {
		return out, err
	}
	if _, err = part.Write(photo); err != nil {
		return out, err
	}
	if err = w.Close(); err != nil {
		return out, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/sendPhoto", &body)
	if err != nil {
		return out, err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	err = decodeTelegramResponse(resp.Body, "sendPhoto", &out)
	return out, err
}
func (c *Client) EditPhoto(ctx context.Context, chat int64, msg int, photo, caption string, k Markup) error {
	media := map[string]any{"type": "photo", "media": photo, "caption": caption, "parse_mode": "HTML"}
	return c.call(ctx, "editMessageMedia", withMarkup(map[string]any{"chat_id": chat, "message_id": msg, "media": media}, k), nil)
}
func (c *Client) SendMediaGroup(ctx context.Context, chat int64, media []InputMediaPhoto) ([]Message, error) {
	if len(media) < 1 || len(media) > 10 {
		return nil, fmt.Errorf("sendMediaGroup requires 1..10 items")
	}
	if len(media) == 1 {
		photo, err := c.DownloadPhoto(ctx, media[0].Media)
		if err != nil {
			return nil, err
		}
		message, err := c.SendPhoto(ctx, chat, photo, media[0].Caption, Markup{})
		if err != nil {
			return nil, err
		}
		return []Message{message}, nil
	}
	var out []Message
	err := c.call(ctx, "sendMediaGroup", map[string]any{"chat_id": chat, "media": media}, &out)
	return out, err
}
func (c *Client) Answer(ctx context.Context, id, text string) error {
	return c.call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": id, "text": text}, nil)
}
func (c *Client) Delete(ctx context.Context, chat int64, message int) error {
	return c.call(ctx, "deleteMessage", map[string]any{"chat_id": chat, "message_id": message}, nil)
}
