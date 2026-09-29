package storage

import (
	"strings"
	"testing"
)

func TestNormalizeSearchWindowCapsNewFeedAt500(t *testing.T) {
	limit, offset, total := normalizeSearchWindow(50, 480, 500, 1200)
	if limit != 20 || offset != 480 || total != 500 {
		t.Fatalf("limit=%d offset=%d total=%d", limit, offset, total)
	}
	limit, offset, total = normalizeSearchWindow(50, 500, 500, 1200)
	if limit != 0 || offset != 500 || total != 500 {
		t.Fatalf("terminal page: limit=%d offset=%d total=%d", limit, offset, total)
	}
}

func TestStudioPredicateAcceptsBothRepresentations(t *testing.T) {
	if !strings.Contains(studioSearchPredicate, "bedrooms=0") || !strings.Contains(studioSearchPredicate, "property_type='studio'") || !strings.Contains(studioSearchPredicate, " OR ") {
		t.Fatalf("studio predicate=%q", studioSearchPredicate)
	}
}

func TestProfileErrorsRequireExplicitReanalysis(t *testing.T) {
	if strings.Contains(dueProfileStatuses, "'error'") {
		t.Fatalf("error status would cause an automatic LLM retry: %s", dueProfileStatuses)
	}
}

func TestScoreOrderBreaksTiesByNewestListing(t *testing.T) {
	want := "l.deal_score DESC,p.published_at DESC,l.id DESC"
	if got := listingOrder("score"); got != want {
		t.Fatalf("order=%q want=%q", got, want)
	}
}
