package mtproto

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nofumex/telegram-aggregator/internal/telegramfeed"
)

func TestFetchMapsSidecarHistoryWithoutDownloadingPhoto(t *testing.T) {
	photoCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/photo/group/42" {
			photoCalls++
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"name":"Group","messages":[{"message_id":42,"text":"rent","published_at":"2026-09-30T01:02:03Z","original_url":"https://t.me/group/42","has_photo":true}]}`))
	}))
	defer srv.Close()
	m := &Manager{base: srv.URL, http: &http.Client{Timeout: time.Second}}
	got, err := m.Fetch(context.Background(), telegramfeed.FetchRequest{Username: "group", AfterID: 10, Limit: 500})
	if err != nil || len(got.Posts) != 1 || !got.Posts[0].HasPhoto || got.Posts[0].PhotoData != nil {
		t.Fatalf("result=%+v err=%v", got, err)
	}
	if photoCalls != 0 {
		t.Fatalf("history eagerly downloaded %d photos", photoCalls)
	}
}
