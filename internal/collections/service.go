package collections

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/nofumex/telegram-aggregator/internal/domain"
	"github.com/nofumex/telegram-aggregator/internal/ranking"
	"github.com/nofumex/telegram-aggregator/internal/storage"
)

var ErrSnapshotNotReady = errors.New("collection snapshot is not ready")

type collectionStore interface {
	Search(context.Context, int64, domain.SearchFilter) (domain.SearchPage, error)
}

type snapshot struct {
	items   []domain.CollectionItem
	builtAt time.Time
}

// Service serves immutable collection snapshots to Telegram. All database,
// ranking-dependent selection happens in Run.
type Service struct {
	store     collectionStore
	mu        sync.RWMutex
	snapshots map[string]snapshot
	log       *slog.Logger
}

func New(s *storage.Store) *Service {
	return newService(s, slog.Default())
}

// The ranking argument remains for source compatibility. Scores are maintained
// by the dedicated reranking worker and are never recalculated here.
func NewWithRanking(s *storage.Store, _ ranking.Engine, log *slog.Logger) *Service {
	return newService(s, log)
}

func newService(s collectionStore, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{store: s, snapshots: map[string]snapshot{}, log: log}
}

// Get is UI-only: it never performs database work or reranking.
func (s *Service) Get(_ context.Context, _ int64, city string, days int) ([]domain.CollectionItem, error) {
	days = normalizedDays(days)
	s.mu.RLock()
	current, ok := s.snapshots[snapshotKey(city, days)]
	s.mu.RUnlock()
	if !ok {
		return nil, ErrSnapshotNotReady
	}
	return append([]domain.CollectionItem(nil), current.items...), nil
}

// Run refreshes 1/7/30-day snapshots sequentially, keeping database pressure
// bounded. A failed refresh never replaces the last successfully built value.
func (s *Service) Run(ctx context.Context, interval, refreshTimeout time.Duration) {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	if refreshTimeout <= 0 {
		refreshTimeout = 90 * time.Second
	}
	refreshAll := func() {
		for _, city := range []string{domain.CityDaNang, domain.CityNhaTrang} {
			for _, days := range []int{1, 7, 30} {
				if ctx.Err() != nil {
					return
				}
				refreshCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
				err := s.refreshPeriod(refreshCtx, city, days)
				cancel()
				if err != nil && ctx.Err() == nil {
					s.log.Warn("collection snapshot refresh failed", "city", city, "days", days, "error", err)
				}
			}
		}
	}

	refreshAll()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refreshAll()
		}
	}
}

func (s *Service) refreshPeriod(ctx context.Context, city string, days int) error {
	items, stats, err := s.build(ctx, city, days)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.snapshots[snapshotKey(city, days)] = snapshot{items: items, builtAt: time.Now().UTC()}
	s.mu.Unlock()
	s.logBuilt(city, days, items, stats)
	return nil
}

type buildStats struct {
	totalPeriod, selected int
}

func (s *Service) build(ctx context.Context, city string, days int) ([]domain.CollectionItem, buildStats, error) {
	after := time.Now().Add(-time.Duration(normalizedDays(days)) * 24 * time.Hour)
	var stats buildStats
	page, err := s.store.Search(ctx, 0, domain.SearchFilter{City: city, FreshAfter: &after, Sort: "score", Limit: 15})
	if err != nil {
		return nil, stats, err
	}
	stats.totalPeriod = page.Total

	items := make([]domain.CollectionItem, 0, min(15, len(page.Items)))
	for i, listing := range page.Items {
		if i >= 15 {
			break
		}
		items = append(items, domain.CollectionItem{Listing: listing, Reason: fmt.Sprintf("Deal score %.0f/100.", listing.DealScore), Rank: i + 1})
	}
	stats.selected = len(items)
	return items, stats, nil
}

func (s *Service) logBuilt(city string, days int, items []domain.CollectionItem, stats buildStats) {
	attrs := []any{"city", city, "days", days, "total_period", stats.totalPeriod, "selected", stats.selected}
	if len(items) > 0 {
		oldest, newest := items[0].PublishedAt, items[0].PublishedAt
		for _, item := range items {
			if item.PublishedAt.Before(oldest) {
				oldest = item.PublishedAt
			}
			if item.PublishedAt.After(newest) {
				newest = item.PublishedAt
			}
		}
		attrs = append(attrs, "oldest_selected", oldest, "newest_selected", newest)
	}
	s.log.Info("collection snapshot built", attrs...)
}

func snapshotKey(city string, days int) string {
	return city + ":" + strconv.Itoa(normalizedDays(days))
}

func normalizedDays(days int) int {
	if days != 1 && days != 7 && days != 30 {
		return 7
	}
	return days
}

func Title(days int) string {
	if days == 1 {
		return "сегодня"
	}
	return fmt.Sprintf("за последние %d дней", days)
}
