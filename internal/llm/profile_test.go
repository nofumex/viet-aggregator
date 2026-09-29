package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nofumex/telegram-aggregator/internal/domain"
)

func TestProfileFieldsContainAllDaNangRankingInputs(t *testing.T) {
	want := []string{"furnished", "near_beach", "beach_distance_m", "amenities", "utilities"}
	set := map[string]bool{}
	for _, field := range ProfileFields {
		set[field] = true
	}
	for _, field := range want {
		if !set[field] {
			t.Fatalf("ProfileFields misses %s", field)
		}
	}
}

func TestAnalyzeProfileUsesOneStrictRequest(t *testing.T) {
	rules := map[string]domain.FieldRule{}
	for _, key := range ProfileFields {
		rules[key] = domain.FieldRule{Patterns: []string{}, ValueGroup: "value", ValueType: "string", Unit: "plain", Mappings: []domain.ValueMapping{}}
	}
	rules["rent_vnd"] = domain.FieldRule{Patterns: []string{`(?i)price:\s*(?P<value>[0-9.]+)`}, ValueGroup: "value", ValueType: "number", Unit: "million_vnd", Mappings: []domain.ValueMapping{}}
	profile := domain.ChannelParsingProfile{Version: "v1", ChannelUsername: "lowrentnt", Language: "vi", PostTypeIndicators: []string{"rent"}, FieldRules: rules, NullPolicy: "null when absent", ExamplesSummary: "sample"}
	content, _ := json.Marshal(profile)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["response_format"] == nil {
			t.Error("missing strict response format")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`, string(content))
	}))
	defer srv.Close()
	p := New(Config{Provider: "compatible", BaseURL: srv.URL, APIKey: "x", Model: "test", Timeout: time.Second})
	posts := make([]domain.TelegramPost, 5)
	for i := range posts {
		posts[i] = domain.TelegramPost{MessageID: int64(i + 1), Text: "rent price: 7"}
	}
	got, _, err := p.AnalyzeProfile(context.Background(), "lowrentnt", domain.CityNhaTrang, posts)
	if err != nil || got.FieldRules["rent_vnd"].Patterns[0] == "" {
		t.Fatalf("profile=%+v err=%v", got, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("LLM calls=%d, want 1", calls.Load())
	}
}
