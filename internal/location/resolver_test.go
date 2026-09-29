package location

import (
	"github.com/nofumex/telegram-aggregator/internal/domain"
	"testing"
)

func TestNhaTrangResolver(t *testing.T) {
	tests := []struct {
		text, zone    string
		oceanus, near bool
	}{
		{"1BR OCEANUS 7tr", domain.ZoneNorth, true, false},
		{"Apartment Vĩnh Phước Hòn Chồng", domain.ZoneNorth, false, false},
		{"Apartment near Oceanus", domain.ZoneNorth, false, true},
		{"Hung Vuong", domain.ZoneCenter, false, false},
		{"Phước Long", domain.ZoneSouth, false, false},
		{"Ha Quang Phước Hải", domain.ZoneWest, false, false},
	}
	for _, tt := range tests {
		got := NhaTrang().Resolve(tt.text)
		if got.Zone != tt.zone || got.IsOceanus != tt.oceanus || got.NearOceanus != tt.near {
			t.Errorf("%q: %+v", tt.text, got)
		}
	}
}

func TestNorthDoesNotImplyNearOceanus(t *testing.T) {
	got := NhaTrang().Resolve("Vĩnh Hải")
	if got.Zone != domain.ZoneNorth || got.NearOceanus || got.IsOceanus {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestVinhPhuocAndHonChongDoNotImplyNearOceanus(t *testing.T) {
	for _, text := range []string{"Vĩnh Phước", "Hòn Chồng"} {
		got := NhaTrang().Resolve(text)
		if got.NearOceanus || got.IsOceanus {
			t.Fatalf("%q unexpectedly resolved as Oceanus proximity: %+v", text, got)
		}
	}
}
