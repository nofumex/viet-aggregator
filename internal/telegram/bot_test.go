package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nofumex/telegram-aggregator/internal/domain"
)

type cachedRate float64

func (r cachedRate) CachedVNDToRUB() float64 { return float64(r) }

func TestCollectionNavigationSelectsCityBeforePeriod(t *testing.T) {
	cities := collectionCityMarkup().InlineKeyboard[0]
	if cities[0].Text != "🇻🇳 Дананг" || cities[0].CallbackData != "colcity:"+domain.CityDaNang || cities[1].Text != "🇻🇳 Нячанг" || cities[1].CallbackData != "colcity:"+domain.CityNhaTrang {
		t.Fatalf("city buttons=%+v", cities)
	}
	periods := collectionPeriodMarkup(domain.CityNhaTrang).InlineKeyboard[0]
	want := []string{"col:nha_trang:1", "col:nha_trang:7", "col:nha_trang:30"}
	for i := range want {
		if periods[i].CallbackData != want[i] {
			t.Fatalf("period %d=%+v", i, periods[i])
		}
	}
}

func TestChannelCardContainsReanalyzeButton(t *testing.T) {
	markup := channelAdminMarkup(42, true)
	for _, row := range markup.InlineKeyboard {
		for _, button := range row {
			if button.Text == "♻️ Переанализировать структуру" && button.CallbackData == "areanalyze:42" {
				return
			}
		}
	}
	t.Fatal("reanalyze button is missing")
}

func TestCardRateReadUsesMemoryProvider(t *testing.T) {
	b := &Bot{rates: cachedRate(.003125)}
	if got := b.vndToRUB(); got != .003125 {
		t.Fatalf("rate=%v", got)
	}
}

func TestPhotoFailureFallsBackToTextCard(t *testing.T) {
	textSent := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/broken.jpg":
			http.Error(w, "broken", http.StatusBadGateway)
		case "/sendMessage":
			textSent = true
			telegramOK(w)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	api := &Client{base: srv.URL, http: srv.Client(), mediaHTTP: srv.Client(), maxPhotoBytes: 1024}
	bot := &Bot{api: api, pages: map[string]pageCache{
		"page": {items: []domain.Listing{{ID: 1, OriginalURL: "https://t.me/example/1", MediaURLs: []string{srv.URL + "/broken.jpg"}, PublishedAt: time.Now()}}, total: 1, until: time.Now().Add(time.Minute)},
	}}
	bot.renderCard(context.Background(), 1, 0, "page", 0, false)
	if !textSent {
		t.Fatal("text fallback was not sent")
	}
}
