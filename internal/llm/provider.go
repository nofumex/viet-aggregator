package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/nofumex/telegram-aggregator/internal/domain"
)

type Config struct {
	Provider, BaseURL, APIKey, Model string
	Timeout                          time.Duration
	Concurrency                      int
}
type OpenAICompatible struct {
	cfg    Config
	client *http.Client
}

func New(cfg Config) *OpenAICompatible {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 45 * time.Second
	}
	if cfg.Model == "" {
		cfg.Model = "gpt-oss-20b"
	}
	return &OpenAICompatible{cfg: cfg, client: &http.Client{Timeout: cfg.Timeout}}
}
func (p *OpenAICompatible) Name() string { return p.cfg.Provider + ":" + p.cfg.Model }

var ProfileFields = []string{"city", "zone", "district_area", "street", "building_complex", "property_type", "bedrooms", "rooms", "rent_vnd", "deposit_vnd", "lease_months_min", "area_m2", "availability", "furnished", "near_beach", "beach_distance_m", "amenities", "utilities", "location_original", "is_oceanus", "near_oceanus"}

// AnalyzeProfile is the application's only LLM call. The returned profile is
// executable by the local profile parser during every import and later sync.
func (p *OpenAICompatible) AnalyzeProfile(ctx context.Context, username, city string, posts []domain.TelegramPost) (domain.ChannelParsingProfile, string, error) {
	if len(posts) < 5 {
		return domain.ChannelParsingProfile{}, "", errors.New("at least 5 real posts are required")
	}
	samples := make([]map[string]any, 0, len(posts))
	for _, post := range posts {
		samples = append(samples, map[string]any{"message_id": post.MessageID, "text": post.Text})
	}
	system := `Analyze the exact recurring rental-ad format of this single Telegram channel and produce a machine-executable extraction profile. Each field rule contains Go RE2-compatible regular expressions. Each regex must capture the extracted scalar in a named group (?P<value>...). patterns must be empty when the samples provide no reliable rule. post_type_indicators must be empty unless a marker is reliably present in every rental post. Mappings maps normalized lowercase captured values to canonical values. Canonical city values are da_nang/nha_trang, zones north/center/south/west, property types apartment/studio/house/room, and furnished values full/partial/none. amenities and utilities are JSON objects. unit is one of plain,vnd,million_vnd,m2,boolean,json. Never create a broad generic rental heuristic. Never guess missing values. is_oceanus means the Oceanus/Muong Thanh Vien Trieu complex itself; near_oceanus is only an explicitly stated immediate Oceanus vicinity, never Vinh Phuoc, Hon Chong, or all of north Nha Trang.`
	in, _ := json.Marshal(map[string]any{"channel": "@" + username, "configured_city": city, "required_fields": ProfileFields, "real_posts": samples})
	mapping := objectSchema(map[string]any{"from": stringSchema(), "to": stringSchema()}, []string{"from", "to"})
	rule := objectSchema(map[string]any{"patterns": arrayStringSchema(), "value_group": stringSchema(), "value_type": enumSchema("string", "integer", "number", "boolean", "json"), "unit": enumSchema("plain", "vnd", "million_vnd", "m2", "boolean", "json"), "mappings": map[string]any{"type": "array", "items": mapping}}, []string{"patterns", "value_group", "value_type", "unit", "mappings"})
	rules := map[string]any{}
	for _, key := range ProfileFields {
		rules[key] = rule
	}
	schema := objectSchema(map[string]any{"version": stringSchema(), "channel_username": stringSchema(), "language": stringSchema(), "post_type_indicators": arrayStringSchema(), "field_rules": objectSchema(rules, ProfileFields), "null_policy": stringSchema(), "examples_summary": stringSchema()}, []string{"version", "channel_username", "language", "post_type_indicators", "field_rules", "null_policy", "examples_summary"})
	raw, e := p.chatJSON(ctx, "channel_parsing_profile", system, string(in), schema)
	if e != nil {
		return domain.ChannelParsingProfile{}, raw, e
	}
	var profile domain.ChannelParsingProfile
	if e = json.Unmarshal([]byte(raw), &profile); e != nil {
		return profile, raw, e
	}
	profile.ChannelUsername = username
	if profile.Version == "" {
		profile.Version = "channel-profile-v1"
	}
	for _, key := range ProfileFields {
		rule, ok := profile.FieldRules[key]
		if !ok {
			return profile, raw, fmt.Errorf("profile missing rule %s", key)
		}
		if rule.ValueGroup == "" {
			rule.ValueGroup = "value"
			profile.FieldRules[key] = rule
		}
		for _, pattern := range rule.Patterns {
			compiled, compileErr := regexp.Compile(pattern)
			if compileErr != nil {
				return profile, raw, fmt.Errorf("invalid %s pattern: %w", key, compileErr)
			}
			if compiled.SubexpIndex(rule.ValueGroup) < 0 {
				return profile, raw, fmt.Errorf("%s pattern has no %q capture", key, rule.ValueGroup)
			}
		}
	}
	return profile, raw, nil
}

func (p *OpenAICompatible) chatJSON(ctx context.Context, name, system, user string, schema map[string]any) (string, error) {
	body := map[string]any{"model": p.cfg.Model, "temperature": 0, "messages": []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": user}}, "response_format": map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": name, "strict": true, "schema": schema}}}
	b, _ := json.Marshal(body)
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.cfg.BaseURL, "/")+"/chat/completions", bytes.NewReader(b))
	if e != nil {
		return "", e
	}
	req.Header.Set("Authorization", "Bearer "+p.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, e := p.client.Do(req)
	if e != nil {
		return "", e
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if e != nil {
		return "", e
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("LLM HTTP %d: %s", resp.StatusCode, truncate(string(raw), 300))
	}
	var env struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &env) != nil || len(env.Choices) == 0 {
		return "", errors.New("invalid LLM response")
	}
	content := strings.TrimSpace(env.Choices[0].Message.Content)
	content = strings.TrimPrefix(strings.TrimSuffix(content, "```"), "```json")
	if !json.Valid([]byte(content)) {
		return content, errors.New("LLM returned invalid JSON")
	}
	return content, nil
}
func objectSchema(p map[string]any, r []string) map[string]any {
	return map[string]any{"type": "object", "properties": p, "required": r, "additionalProperties": false}
}
func stringSchema() map[string]any { return map[string]any{"type": "string"} }
func arrayStringSchema() map[string]any {
	return map[string]any{"type": "array", "items": stringSchema()}
}
func enumSchema(values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values}
}
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
