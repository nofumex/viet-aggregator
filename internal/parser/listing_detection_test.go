package parser

import (
	"github.com/nofumex/telegram-aggregator/internal/domain"
	"testing"
)

func TestIsListingUsesOnlySavedChannelRules(t *testing.T) {
	p := domain.ChannelParsingProfile{ListingDetection: domain.ListingDetection{IncludeMarkers: []string{"for rent"}, IncludeRegex: []string{`(?i)rent\s*:\s*\d+`}, ExcludeMarkers: []string{"weekly news"}}}
	for _, tc := range []struct {
		text string
		want bool
	}{{"FOR RENT apartment", true}, {"Rent: 7000000", true}, {"Weekly news: FOR RENT statistics", false}, {"Does anyone know a landlord?", false}} {
		if got := IsListing(p, tc.text); got != tc.want {
			t.Fatalf("IsListing(%q)=%v want %v", tc.text, got, tc.want)
		}
	}
}
func TestIsListingBackwardCompatibleWhenProfileHasNoClassifier(t *testing.T) {
	if !IsListing(domain.ChannelParsingProfile{}, "anything") {
		t.Fatal("legacy profile without rules must continue to parse")
	}
}
