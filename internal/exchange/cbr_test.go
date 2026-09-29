package exchange

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVNDToRUBParsesNominalAndCaches(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_, _ = io.WriteString(w, `<?xml version="1.0" encoding="windows-1251"?><ValCurs><Valute><CharCode>VND</CharCode><Nominal>10000</Nominal><Value>31,2500</Value></Valute></ValCurs>`)
	}))
	defer srv.Close()
	c := &CBR{client: srv.Client(), url: srv.URL, ttl: time.Hour}
	for range 2 {
		rate, err := c.VNDToRUB(context.Background())
		if err != nil || rate != .003125 {
			t.Fatalf("rate=%v err=%v", rate, err)
		}
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestForbiddenUsesNegativeCache(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	c := &CBR{client: srv.Client(), url: srv.URL, ttl: time.Hour}
	if _, err := c.VNDToRUB(context.Background()); err == nil {
		t.Fatal("first failure must be observable")
	}
	if rate, err := c.VNDToRUB(context.Background()); err != nil || rate != 0 {
		t.Fatalf("negative cache rate=%v err=%v", rate, err)
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestStaleRateSurvivesRefreshError(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := &CBR{client: srv.Client(), url: srv.URL, ttl: time.Hour, rate: .003, until: time.Now().Add(-time.Minute)}
	rate, err := c.VNDToRUB(context.Background())
	if err != nil || rate != .003 {
		t.Fatalf("rate=%v err=%v", rate, err)
	}
	if calls != 2 {
		t.Fatalf("bounded retries=%d", calls)
	}
}

func TestCachedVNDToRUBNeverPerformsHTTP(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer srv.Close()
	c := &CBR{client: srv.Client(), url: srv.URL, rate: .003}
	if got := c.CachedVNDToRUB(); got != .003 {
		t.Fatalf("rate=%v", got)
	}
	if calls != 0 {
		t.Fatalf("cached UI read performed %d HTTP calls", calls)
	}
}
