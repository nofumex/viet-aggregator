package parser

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/nofumex/telegram-aggregator/internal/domain"
)

var phone = regexp.MustCompile(`(?i)(?:\+?84|0)[\s.()-]*\d(?:[\s.()-]*\d){7,10}`)

// ParseSearch interprets only user-entered search text. Listing extraction is
// intentionally not implemented here: channel posts are parsed exclusively by
// their persisted channel profile.
func ParseSearch(q string) domain.SearchFilter {
	f := domain.SearchFilter{Query: strings.TrimSpace(q), Sort: "score", Limit: 20}
	s := strings.ToLower(q)
	if strings.Contains(s, "nha trang") || strings.Contains(s, "нячанг") {
		f.City = domain.CityNhaTrang
	}
	if strings.Contains(s, "da nang") || strings.Contains(s, "дананг") {
		f.City = domain.CityDaNang
	}
	for key, zone := range map[string]string{"север": domain.ZoneNorth, "north": domain.ZoneNorth, "центр": domain.ZoneCenter, "center": domain.ZoneCenter, "юг": domain.ZoneSouth, "south": domain.ZoneSouth, "запад": domain.ZoneWest, "west": domain.ZoneWest} {
		if strings.Contains(s, key) {
			f.Zone = zone
		}
	}
	if strings.Contains(s, "oceanus") {
		f.Location = "Oceanus"
	}
	if strings.Contains(s, "studio") || strings.Contains(s, "студи") {
		v := 0
		f.Bedrooms = &v
	}
	if m := regexp.MustCompile(`(?i)(\d+)\s*(?:спаль|bed|br)`).FindStringSubmatch(s); len(m) > 1 {
		v, _ := strconv.Atoi(m[1])
		f.Bedrooms = &v
	}
	if m := regexp.MustCompile(`(?i)(?:до|max)\s*(\d+(?:[.,]\d+)?)\s*(?:млн|m|tr)`).FindStringSubmatch(s); len(m) > 1 {
		v, _ := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", "."), 64)
		n := int64(v * 1_000_000)
		f.RentMax = &n
	}
	return f
}
func RedactContact(s string) string {
	return phone.ReplaceAllString(s, "[контакт в оригинале]")
}

// IsListing applies only the rules persisted in ChannelParsingProfile. It is
// deliberately free of global rental heuristics and never calls an LLM.
func IsListing(profile domain.ChannelParsingProfile, text string) bool {
	lower := strings.ToLower(text)
	for _, marker := range profile.ListingDetection.ExcludeMarkers {
		if marker != "" && strings.Contains(lower, strings.ToLower(marker)) {
			return false
		}
	}
	for _, pattern := range profile.ListingDetection.ExcludeRegex {
		if re, err := regexp.Compile(pattern); err == nil && re.MatchString(text) {
			return false
		}
	}
	hasInclude := len(profile.ListingDetection.IncludeMarkers)+len(profile.ListingDetection.IncludeRegex) > 0
	if !hasInclude {
		return true
	}
	for _, marker := range profile.ListingDetection.IncludeMarkers {
		if marker != "" && strings.Contains(lower, strings.ToLower(marker)) {
			return true
		}
	}
	for _, pattern := range profile.ListingDetection.IncludeRegex {
		if re, err := regexp.Compile(pattern); err == nil && re.MatchString(text) {
			return true
		}
	}
	return false
}

func ParseWithProfile(profile domain.ChannelParsingProfile, post domain.TelegramPost, configuredCity string) (domain.ProfileExtraction, error) {
	out := domain.ProfileExtraction{Utilities: map[string]any{}, Confidence: domain.Confidence{}}
	if len(profile.PostTypeIndicators) > 0 {
		matched := false
		lower := strings.ToLower(post.Text)
		for _, v := range profile.PostTypeIndicators {
			if strings.Contains(lower, strings.ToLower(v)) {
				matched = true
				break
			}
		}
		if !matched {
			return out, nil
		}
	}
	values := map[string]any{}
	for field, rule := range profile.FieldRules {
		v, ok, e := extractRule(rule, post.Text)
		if e != nil {
			return out, e
		}
		if ok {
			values[field] = v
			out.Confidence[field] = 1
		}
	}
	city := configuredCity
	out.City = &city
	setString := func(key string, target **string) {
		if v, ok := values[key].(string); ok && v != "" {
			*target = &v
		}
	}
	setInt := func(key string, target **int) {
		if v, ok := values[key].(int64); ok {
			n := int(v)
			*target = &n
		}
	}
	setInt64 := func(key string, target **int64) {
		if v, ok := values[key].(int64); ok {
			*target = &v
		}
	}
	setFloat := func(key string, target **float64) {
		if v, ok := values[key].(float64); ok {
			*target = &v
		} else if n, ok := values[key].(int64); ok {
			v := float64(n)
			*target = &v
		}
	}
	setBool := func(key string, target **bool) {
		if v, ok := values[key].(bool); ok {
			*target = &v
		}
	}
	setString("zone", &out.Zone)
	setString("district_area", &out.District)
	setString("street", &out.Street)
	setString("building_complex", &out.Building)
	setString("property_type", &out.PropertyType)
	setInt("bedrooms", &out.Bedrooms)
	setInt("rooms", &out.Rooms)
	setInt64("rent_vnd", &out.RentVND)
	setInt64("deposit_vnd", &out.DepositVND)
	setInt("lease_months_min", &out.LeaseMonthsMin)
	setFloat("area_m2", &out.AreaM2)
	setString("availability", &out.Availability)
	setString("furnished", &out.Furnished)
	setBool("near_beach", &out.NearBeach)
	setInt("beach_distance_m", &out.BeachDistanceM)
	setString("location_original", &out.LocationOriginal)
	setBool("is_oceanus", &out.IsOceanus)
	setBool("near_oceanus", &out.NearOceanus)
	if v, ok := values["utilities"].(map[string]any); ok {
		out.Utilities = v
	}
	if v, ok := values["amenities"].(map[string]any); ok {
		out.Amenities = make(map[string]bool, len(v))
		for key, raw := range v {
			if enabled, ok := raw.(bool); ok {
				out.Amenities[key] = enabled
			}
		}
	}
	for _, v := range values {
		if v != nil {
			out.ParsedFields++
		}
	}
	return out, nil
}

func extractRule(rule domain.FieldRule, text string) (any, bool, error) {
	group := rule.ValueGroup
	if group == "" {
		group = "value"
	}
	for _, pattern := range rule.Patterns {
		re, e := regexp.Compile(pattern)
		if e != nil {
			return nil, false, e
		}
		m := re.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		idx := re.SubexpIndex(group)
		if idx < 0 || idx >= len(m) {
			return nil, false, fmt.Errorf("profile pattern has no %q capture", group)
		}
		raw := strings.TrimSpace(m[idx])
		for _, mapping := range rule.Mappings {
			if strings.EqualFold(mapping.From, raw) {
				raw = mapping.To
				break
			}
		}
		switch rule.Unit {
		case "boolean":
			v, e := strconv.ParseBool(strings.ToLower(raw))
			if e != nil {
				if raw == "1" || raw != "" {
					v = true
					e = nil
				}
			}
			return v, e == nil, e
		case "vnd", "million_vnd":
			clean := strings.ReplaceAll(strings.ReplaceAll(raw, " ", ""), "₫", "")
			if rule.Unit == "vnd" {
				clean = strings.NewReplacer(".", "", ",", "").Replace(clean)
			} else {
				clean = strings.ReplaceAll(clean, ",", ".")
			}
			n, e := strconv.ParseFloat(clean, 64)
			if e != nil {
				return nil, false, e
			}
			if rule.Unit == "million_vnd" {
				n *= 1_000_000
			}
			return int64(n), true, nil
		case "m2":
			clean := strings.ReplaceAll(strings.ReplaceAll(raw, ",", "."), " ", "")
			n, e := strconv.ParseFloat(clean, 64)
			return n, e == nil, e
		case "json":
			var v map[string]any
			e := json.Unmarshal([]byte(raw), &v)
			return v, e == nil, e
		default:
			if rule.ValueType == "integer" {
				n, e := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
				return n, e == nil, e
			}
			return raw, true, nil
		}
	}
	return nil, false, nil
}
