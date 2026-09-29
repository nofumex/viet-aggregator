package telegram

import (
	"testing"

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
