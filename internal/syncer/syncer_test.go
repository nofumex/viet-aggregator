package syncer

import (
	"testing"

	"github.com/nofumex/telegram-aggregator/internal/domain"
)

func TestApplyExtractionRestoresDaNangRankingInputs(t *testing.T) {
	furnished := "full"
	near := true
	distance := 250
	l := domain.Listing{}
	applyExtraction(&l, domain.ProfileExtraction{
		Furnished:      &furnished,
		NearBeach:      &near,
		BeachDistanceM: &distance,
		Amenities:      map[string]bool{"balcony": true},
		Utilities:      map[string]any{"government_rate": true},
		Confidence:     domain.Confidence{"furnished": 1, "near_beach": 1},
	})
	if l.Furnished != furnished || l.NearBeach == nil || !*l.NearBeach || l.BeachDistanceM == nil || *l.BeachDistanceM != distance || !l.Amenities["balcony"] {
		t.Fatalf("listing ranking inputs=%+v", l)
	}
}
