package syncer

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/nofumex/telegram-aggregator/internal/domain"
	"github.com/nofumex/telegram-aggregator/internal/llm"
	"github.com/nofumex/telegram-aggregator/internal/location"
	"github.com/nofumex/telegram-aggregator/internal/parser"
	"github.com/nofumex/telegram-aggregator/internal/ranking"
	"github.com/nofumex/telegram-aggregator/internal/storage"
	"github.com/nofumex/telegram-aggregator/internal/telegramfeed"
)

type Service struct {
	store          *storage.Store
	feed           telegramfeed.Adapter
	llm            *llm.OpenAICompatible
	rank           ranking.Engine
	log            *slog.Logger
	concurrency    int
	trigger        chan int64
	mu             sync.Mutex
	running        map[int64]bool
	onProfileReady func(domain.Channel, domain.ChannelParsingProfile)
}

func New(store *storage.Store, feed telegramfeed.Adapter, profileLLM *llm.OpenAICompatible, rank ranking.Engine, log *slog.Logger, concurrency int) *Service {
	if concurrency < 1 {
		concurrency = 1
	}
	return &Service{store: store, feed: feed, llm: profileLLM, rank: rank, log: log, concurrency: concurrency, trigger: make(chan int64, 100), running: map[int64]bool{}}
}
func (s *Service) SetProfileReadyHandler(fn func(domain.Channel, domain.ChannelParsingProfile)) {
	s.onProfileReady = fn
}
func (s *Service) Trigger(id int64) bool {
	select {
	case s.trigger <- id:
		return true
	default:
		return false
	}
}
func (s *Service) Check(ctx context.Context, id int64) error {
	c, e := s.store.Channel(ctx, id)
	if e != nil {
		return e
	}
	return s.feed.Check(ctx, c.Username)
}
func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	sem := make(chan struct{}, s.concurrency)
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-s.trigger:
			if c, e := s.store.Channel(ctx, id); e == nil {
				s.launch(ctx, sem, c)
			}
		case <-ticker.C:
			channels, e := s.store.DueChannels(ctx, s.concurrency*2)
			if e != nil {
				s.log.Warn("load due channels", "error", e)
				continue
			}
			for _, c := range channels {
				s.launch(ctx, sem, c)
			}
		}
	}
}
func (s *Service) launch(ctx context.Context, sem chan struct{}, c domain.Channel) {
	s.mu.Lock()
	if s.running[c.ID] {
		s.mu.Unlock()
		return
	}
	s.running[c.ID] = true
	s.mu.Unlock()
	go func() {
		defer func() { s.mu.Lock(); delete(s.running, c.ID); s.mu.Unlock() }()
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return
		}
		if _, e := s.SyncChannel(ctx, c); e != nil {
			s.log.Warn("channel sync failed", "channel", c.Username, "error", e)
		}
	}()
}

func (s *Service) SyncChannel(ctx context.Context, c domain.Channel) (result domain.ChannelSyncResult, err error) {
	run, e := s.store.SyncStarted(ctx, c.ID)
	if e != nil {
		return result, e
	}
	defer func() { s.store.SyncFinished(context.WithoutCancel(ctx), run, c.ID, result, err, c.PollingInterval) }()
	if c.Username == "" {
		c.Username, c.Name, c.URL, err = s.feed.Resolve(ctx, c.URL)
		if err != nil {
			return result, err
		}
		if err = s.store.ResolveChannel(ctx, c.ID, c.Username, c.Name, c.URL); err != nil {
			return result, err
		}
	}
	profileCreated := false
	if c.Profile == nil || c.ProfileStatus != "ready" {
		var sample telegramfeed.FetchResult
		sample, err = s.feed.Fetch(ctx, telegramfeed.FetchRequest{Username: c.Username, Limit: 5})
		if err != nil {
			s.store.FailProfile(context.WithoutCancel(ctx), c.ID, err)
			return result, err
		}
		if sample.Name != "" {
			c.Name = sample.Name
			_ = s.store.ResolveChannel(ctx, c.ID, c.Username, c.Name, c.URL)
		}
		posts := make([]domain.TelegramPost, 0, 5)
		for _, p := range sample.Posts {
			if p.Text != "" {
				posts = append(posts, p)
			}
			if len(posts) == 5 {
				break
			}
		}
		var profile domain.ChannelParsingProfile
		var raw string
		profile, raw, err = s.llm.AnalyzeProfile(ctx, c.Username, c.City, posts)
		if err != nil {
			s.store.FailProfile(context.WithoutCancel(ctx), c.ID, err)
			return result, err
		}
		_ = raw
		if err = s.store.SaveProfile(ctx, c.ID, profile); err != nil {
			return result, err
		}
		c.Profile = &profile
		c.ProfileStatus = "ready"
		profileCreated = true
		if s.onProfileReady != nil {
			go s.onProfileReady(c, profile)
		}
	}
	limit, after := 500, c.LastMessageID
	if profileCreated {
		// A newly created or manually rebuilt profile must be applied again to
		// the recent stored window, not only to posts newer than the last sync.
		after = 0
	}
	var fetched telegramfeed.FetchResult
	fetched, err = s.feed.Fetch(ctx, telegramfeed.FetchRequest{Username: c.Username, AfterID: after, Limit: limit})
	if err != nil {
		return result, err
	}
	result.Fetched = len(fetched.Posts)
	for i := len(fetched.Posts) - 1; i >= 0; i-- {
		post := fetched.Posts[i]
		if post.MessageID > result.NewestID {
			result.NewestID = post.MessageID
			result.NewestAt = post.PublishedAt
		}
		listing := domain.Listing{ChannelID: c.ID, ChannelUsername: c.Username, ChannelName: c.Name, TelegramMessageID: post.MessageID, OriginalURL: post.URL, OriginalText: post.Text, PublishedAt: post.PublishedAt, City: c.City, Currency: "VND", Utilities: map[string]any{}, Amenities: map[string]bool{}, Confidence: domain.Confidence{}, RawValues: map[string]any{}, ExtractionVersion: c.Profile.Version, ExtractionStatus: "unparsed"}
		extracted, parseErr := parser.ParseWithProfile(*c.Profile, post, c.City)
		now := time.Now().UTC()
		listing.ProfileParsedAt = &now
		if parseErr != nil {
			listing.LastExtractionError = parseErr.Error()
		} else {
			applyExtraction(&listing, extracted)
			listing.City = c.City
			if extracted.RentVND != nil && extracted.ParsedFields >= 3 {
				listing.ExtractionStatus = "success"
			}
			if listing.City == domain.CityNhaTrang {
				normalizeNhaTrang(&listing)
			}
			if listing.ExtractionStatus == "success" {
				b, bErr := s.store.Benchmarks(ctx, listing)
				if bErr == nil {
					listing.DealScore, listing.ScoreConfidence = s.rank.Score(listing, b, time.Now())
				}
			}
		}
		inserted, insertErr := s.store.InsertListing(ctx, post, listing)
		if insertErr != nil {
			return result, insertErr
		}
		if inserted {
			result.Inserted++
		}
	}
	return result, nil
}

func applyExtraction(l *domain.Listing, x domain.ProfileExtraction) {
	l.RentMin = x.RentVND
	l.DepositAmount = x.DepositVND
	l.Bedrooms = x.Bedrooms
	l.Rooms = x.Rooms
	l.LeaseMonths = x.LeaseMonthsMin
	l.AreaM2 = x.AreaM2
	l.IsOceanus = x.IsOceanus
	l.NearOceanus = x.NearOceanus
	l.Utilities = x.Utilities
	l.Amenities = x.Amenities
	if l.Amenities == nil {
		l.Amenities = map[string]bool{}
	}
	l.NearBeach = x.NearBeach
	l.BeachDistanceM = x.BeachDistanceM
	l.Confidence = x.Confidence
	if l.Confidence == nil {
		l.Confidence = domain.Confidence{}
	}
	if v, ok := l.Confidence["rent_vnd"]; ok {
		l.Confidence["price"] = v
	}
	if v, ok := l.Confidence["district_area"]; ok {
		l.Confidence["district"] = v
	}
	if v, ok := l.Confidence["location_original"]; ok {
		l.Confidence["location_original"] = v
	}
	if x.City != nil {
		l.City = *x.City
	}
	if x.Zone != nil {
		l.Zone = *x.Zone
	}
	if x.District != nil {
		l.District = *x.District
	}
	if x.Street != nil {
		l.Street = *x.Street
	}
	if x.Building != nil {
		l.Building = *x.Building
	}
	if x.PropertyType != nil {
		l.PropertyType = *x.PropertyType
	}
	if x.Availability != nil {
		l.Availability = *x.Availability
	}
	if x.Furnished != nil {
		l.Furnished = *x.Furnished
	}
	if x.LocationOriginal != nil {
		l.LocationOriginal = *x.LocationOriginal
	}
	raw, _ := json.Marshal(x)
	_ = json.Unmarshal(raw, &l.RawValues)
}
func normalizeNhaTrang(l *domain.Listing) {
	r := location.NhaTrang().Resolve(l.LocationOriginal, l.District, l.Street, l.Building)
	no := false
	l.IsOceanus = &no
	l.NearOceanus = &no
	if r.Zone != "" {
		l.Zone = r.Zone
	}
	if r.Canonical != "" && l.District == "" {
		l.District = r.Canonical
	}
	if r.IsOceanus {
		v := true
		l.IsOceanus = &v
	} else if r.NearOceanus {
		v := true
		l.NearOceanus = &v
	}
}
