package domain

import (
	"encoding/json"
	"time"
)

const (
	CityDaNang   = "da_nang"
	CityNhaTrang = "nha_trang"
	ZoneNorth    = "north"
	ZoneCenter   = "center"
	ZoneSouth    = "south"
	ZoneWest     = "west"
)

type ChannelParsingProfile struct {
	Version            string               `json:"version"`
	ChannelUsername    string               `json:"channel_username"`
	Language           string               `json:"language"`
	PostTypeIndicators []string             `json:"post_type_indicators"`
	ListingDetection   ListingDetection     `json:"listing_detection"`
	FieldRules         map[string]FieldRule `json:"field_rules"`
	NullPolicy         string               `json:"null_policy"`
	ExamplesSummary    string               `json:"examples_summary"`
}

// ListingDetection is generated once with the channel profile and then
// executed locally. Exclude rules take precedence over include rules.
type ListingDetection struct {
	IncludeMarkers []string `json:"include_markers"`
	ExcludeMarkers []string `json:"exclude_markers"`
	IncludeRegex   []string `json:"include_regex"`
	ExcludeRegex   []string `json:"exclude_regex"`
}

type FieldRule struct {
	Patterns   []string       `json:"patterns"`
	ValueGroup string         `json:"value_group"`
	ValueType  string         `json:"value_type"`
	Unit       string         `json:"unit"`
	Mappings   []ValueMapping `json:"mappings"`
}
type ValueMapping struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type Channel struct {
	ID                int64
	Username          string
	URL               string
	Name              string
	SourceType        string
	City              string
	Enabled           bool
	PollingInterval   time.Duration
	Profile           *ChannelParsingProfile
	ProfileStatus     string
	ProfileError      string
	LastSuccessAt     *time.Time
	LastAttemptAt     *time.Time
	LastMessageAt     *time.Time
	LastMessageID     int64
	PostsTotal        int64
	NewPostsLastRun   int
	ConsecutiveErrors int
	LastError         string
	NextPollAt        time.Time
	CreatedAt         time.Time
}

type TelegramPost struct {
	ChannelUsername string
	MessageID       int64
	URL             string
	Text            string
	PublishedAt     time.Time
	PhotoURL        string
	PhotoData       []byte
	PhotoMime       string
	HasPhoto        bool
	Raw             json.RawMessage
}

type Confidence map[string]float64

// ProfileExtraction is produced locally by the stored channel profile.
// Pointers distinguish explicit zero/false from unavailable facts.
type ProfileExtraction struct {
	City             *string         `json:"city"`
	Zone             *string         `json:"zone"`
	District         *string         `json:"district_area"`
	Street           *string         `json:"street"`
	Building         *string         `json:"building_complex"`
	PropertyType     *string         `json:"property_type"`
	Bedrooms         *int            `json:"bedrooms"`
	Rooms            *int            `json:"rooms"`
	RentVND          *int64          `json:"rent_vnd"`
	DepositVND       *int64          `json:"deposit_vnd"`
	LeaseMonthsMin   *int            `json:"lease_months_min"`
	AreaM2           *float64        `json:"area_m2"`
	Availability     *string         `json:"availability"`
	Utilities        map[string]any  `json:"utilities"`
	Amenities        map[string]bool `json:"amenities"`
	Furnished        *string         `json:"furnished"`
	NearBeach        *bool           `json:"near_beach"`
	BeachDistanceM   *int            `json:"beach_distance_m"`
	LocationOriginal *string         `json:"location_original"`
	IsOceanus        *bool           `json:"is_oceanus"`
	NearOceanus      *bool           `json:"near_oceanus"`
	ParsedFields     int             `json:"parsed_fields"`
	Confidence       Confidence      `json:"confidence"`
}

type Listing struct {
	ID, PostID                      int64
	ChannelID                       int64
	ChannelUsername, ChannelName    string
	TelegramMessageID               int64
	OriginalURL, OriginalText       string
	PublishedAt, CreatedAt          time.Time
	City, Zone                      string
	District, LocationOriginal      string
	Street, Address, Building       string
	RentMin, RentMax, DepositAmount *int64
	Bedrooms, Rooms, LeaseMonths    *int
	AreaM2                          *float64
	PropertyType, Availability      string
	Currency                        string
	IsOceanus, NearOceanus          *bool
	Utilities                       map[string]any
	Amenities                       map[string]bool
	Confidence                      Confidence
	DealScore, ScoreConfidence      float64
	MediaURLs                       []string
	PhotoData                       []byte
	PhotoMime                       string
	ExtractionVersion               string
	ExtractionStatus                string
	ProfileParsedAt                 *time.Time
	LastExtractionError             string
	ExtractionAttempts              int
	NextExtractionRetryAt, RankedAt *time.Time

	// Existing UI fields retained where they are source-neutral.
	IsRental, NearBeach                                *bool
	PetsAllowed, ForeignersAccepted                    *bool
	TemporaryResidence                                 *bool
	BeachDistanceM                                     *int
	Furnished                                          string
	ForeignerPrice                                     *int64
	EstimatedMonthlyTotalMin, EstimatedMonthlyTotalMax *int64
	Restrictions, RawValues                            map[string]any
}

type TelegramAccount struct {
	APIID       int
	APIHash     string
	Phone       string
	Status      string
	LastError   string
	ConnectedAt *time.Time
	UpdatedAt   time.Time
}

type SearchFilter struct {
	Query                          string
	City, Zone, District, Location string
	PropertyType, Furnished        string
	RentMin, RentMax               *int64
	Bedrooms                       *int
	AreaMin, AreaMax               *float64
	NearBeach, ForeignersAccepted  *bool
	FreshAfter                     *time.Time
	Sort                           string
	Limit, Offset, MaxResults      int
}

type SearchPage struct {
	Items []Listing
	Total int
}
type CollectionItem struct {
	Listing
	Reason string
	Rank   int
}
type DistrictStat struct {
	District                  string
	Listings                  int
	MedianRent, MedianPriceM2 int64
}
type MarketSummary struct {
	City                      string
	Listings30d               int
	MedianRent, MedianPriceM2 int64
	Districts                 []DistrictStat
}
type ChannelSyncResult struct {
	Fetched, Inserted int
	NewestID          int64
	NewestAt          time.Time
	ReachedOld        bool
}
