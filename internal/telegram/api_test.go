package telegram

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func telegramOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
}

func TestSendOmitsEmptyReplyMarkup(t *testing.T) {
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		telegramOK(w)
	}))
	defer srv.Close()
	c := &Client{base: srv.URL, http: srv.Client()}
	if _, err := c.Send(context.Background(), 1, "hello", Markup{}); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["reply_markup"]; exists {
		t.Fatalf("empty reply_markup was sent: %#v", payload["reply_markup"])
	}
}

func TestPhotoIsDownloadedAndUploadedAsMultipart(t *testing.T) {
	want := []byte("fake-image-bytes")
	var uploaded []byte
	var replyMarkupPresent bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/photo.jpg":
			_, _ = w.Write(want)
		case "/sendPhoto":
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data;") {
				t.Errorf("content-type=%q", r.Header.Get("Content-Type"))
			}
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Fatal(err)
			}
			replyMarkupPresent = r.FormValue("reply_markup") != ""
			file, _, err := r.FormFile("photo")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			uploaded, _ = io.ReadAll(file)
			telegramOK(w)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &Client{base: srv.URL, http: srv.Client(), mediaHTTP: srv.Client(), maxPhotoBytes: 1024}
	photo, err := c.DownloadPhoto(context.Background(), srv.URL+"/photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.SendPhoto(context.Background(), 1, photo, "caption", Markup{}); err != nil {
		t.Fatal(err)
	}
	if string(uploaded) != string(want) {
		t.Fatalf("uploaded=%q want=%q", uploaded, want)
	}
	if replyMarkupPresent {
		t.Fatal("empty reply_markup was included in multipart form")
	}
}

func TestDownloadPhotoEnforcesSizeLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "12345")
	}))
	defer srv.Close()
	c := &Client{mediaHTTP: srv.Client(), maxPhotoBytes: 4}
	if _, err := c.DownloadPhoto(context.Background(), srv.URL); err == nil {
		t.Fatal("oversized photo was accepted")
	}
}

func TestDownloadPhotoUsesHTTPTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = io.WriteString(w, "image")
	}))
	defer srv.Close()
	c := &Client{mediaHTTP: &http.Client{Timeout: 10 * time.Millisecond}, maxPhotoBytes: 1024}
	if _, err := c.DownloadPhoto(context.Background(), srv.URL); err == nil {
		t.Fatal("slow photo download did not time out")
	}
}
