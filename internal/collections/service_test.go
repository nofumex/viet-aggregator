package collections

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/nofumex/telegram-aggregator/internal/domain"
)

type snapshotStore struct {
	mu      sync.Mutex
	items   []domain.Listing
	err     error
	started chan struct{}
	release chan struct{}
	once    sync.Once
	calls   int
	filters []domain.SearchFilter
}

func (s *snapshotStore) Search(ctx context.Context, _ int64, filter domain.SearchFilter) (domain.SearchPage, error) {
	s.mu.Lock()
	s.calls++
	s.filters = append(s.filters, filter)
	err := s.err
	items := append([]domain.Listing(nil), s.items...)
	s.mu.Unlock()
	if s.started != nil {
		s.once.Do(func() { close(s.started) })
	}
	if s.release != nil {
		select {
		case <-s.release:
		case <-ctx.Done():
			return domain.SearchPage{}, ctx.Err()
		}
	}
	return domain.SearchPage{Items: items, Total: len(items)}, err
}

func eligibleListing(id int64) domain.Listing {
	rent := int64(5_000_000)
	beds := 1
	return domain.Listing{ID: id, RentMin: &rent, Bedrooms: &beds, District: domain.DistrictSonTra, DealScore: 80, ScoreConfidence: .8, Confidence: domain.Confidence{"price": .9}, PublishedAt: time.Now()}
}

func TestGetNeverWaitsForBackgroundRefresh(t *testing.T) {
	store := &snapshotStore{items: []domain.Listing{eligibleListing(1)}, started: make(chan struct{}), release: make(chan struct{})}
	svc := newService(store, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		svc.Run(ctx, time.Hour, time.Second)
		close(done)
	}()
	<-store.started

	started := time.Now()
	_, err := svc.Get(context.Background(), 123, domain.CityDaNang, 7)
	if !errors.Is(err, ErrSnapshotNotReady) {
		t.Fatalf("err=%v", err)
	}
	if elapsed := time.Since(started); elapsed > 50*time.Millisecond {
		t.Fatalf("Get blocked for %v", elapsed)
	}

	close(store.release)
	deadline := time.Now().Add(time.Second)
	for {
		items, getErr := svc.Get(context.Background(), 123, domain.CityDaNang, 1)
		if getErr == nil {
			if len(items) != 1 || items[0].ID != 1 {
				t.Fatalf("items=%+v", items)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("snapshot was not published")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}

func TestFailedRefreshKeepsLastReadySnapshot(t *testing.T) {
	store := &snapshotStore{items: []domain.Listing{eligibleListing(10)}}
	svc := newService(store, nil)
	if err := svc.refreshPeriod(context.Background(), domain.CityDaNang, 7); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.items = []domain.Listing{eligibleListing(20)}
	store.err = errors.New("database busy")
	store.mu.Unlock()
	if err := svc.refreshPeriod(context.Background(), domain.CityDaNang, 7); err == nil {
		t.Fatal("refresh unexpectedly succeeded")
	}
	items, err := svc.Get(context.Background(), 1, domain.CityDaNang, 7)
	if err != nil || len(items) != 1 || items[0].ID != 10 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
}

func TestRefreshInProgressServesPreviousSnapshot(t *testing.T) {
	store := &snapshotStore{items: []domain.Listing{eligibleListing(10)}}
	svc := newService(store, nil)
	if err := svc.refreshPeriod(context.Background(), domain.CityDaNang, 7); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.items = []domain.Listing{eligibleListing(20)}
	store.started = make(chan struct{})
	store.release = make(chan struct{})
	store.once = sync.Once{}
	started, release := store.started, store.release
	store.mu.Unlock()
	done := make(chan error, 1)
	go func() { done <- svc.refreshPeriod(context.Background(), domain.CityDaNang, 7) }()
	<-started

	items, err := svc.Get(context.Background(), 1, domain.CityDaNang, 7)
	if err != nil || len(items) != 1 || items[0].ID != 10 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	close(release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	items, err = svc.Get(context.Background(), 1, domain.CityDaNang, 7)
	if err != nil || len(items) != 1 || items[0].ID != 20 {
		t.Fatalf("updated items=%+v err=%v", items, err)
	}
}

func TestSnapshotsAreIsolatedByCityAndPeriod(t *testing.T) {
	store := &snapshotStore{items: []domain.Listing{eligibleListing(1)}}
	svc := newService(store, nil)
	if err := svc.refreshPeriod(context.Background(), domain.CityDaNang, 7); err != nil {
		t.Fatal(err)
	}
	store.items = []domain.Listing{eligibleListing(2)}
	if err := svc.refreshPeriod(context.Background(), domain.CityNhaTrang, 7); err != nil {
		t.Fatal(err)
	}
	daNang, _ := svc.Get(context.Background(), 1, domain.CityDaNang, 7)
	nhaTrang, _ := svc.Get(context.Background(), 1, domain.CityNhaTrang, 7)
	if len(daNang) != 1 || daNang[0].ID != 1 || len(nhaTrang) != 1 || nhaTrang[0].ID != 2 {
		t.Fatalf("da_nang=%v nha_trang=%v", daNang, nhaTrang)
	}
	if len(store.filters) != 2 || store.filters[0].City != domain.CityDaNang || store.filters[1].City != domain.CityNhaTrang {
		t.Fatalf("collection searches were not city-scoped: %+v", store.filters)
	}
}

func TestBuildReturnsTop15WithoutQualityGates(t *testing.T) {
	items := make([]domain.Listing, 20)
	for i := range items {
		items[i] = domain.Listing{ID: int64(i + 1), DealScore: float64(20 - i), PublishedAt: time.Now().Add(-time.Duration(i) * time.Minute)}
	}
	store := &snapshotStore{items: items}
	svc := newService(store, nil)
	got, _, err := svc.build(context.Background(), domain.CityDaNang, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 15 {
		t.Fatalf("got %d items, want 15", len(got))
	}
	if got[0].ID != 1 || got[14].ID != 15 {
		t.Fatalf("unexpected TOP-15 boundaries: first=%d last=%d", got[0].ID, got[14].ID)
	}
	if store.calls != 1 || len(store.filters) != 1 {
		t.Fatalf("calls=%d filters=%+v", store.calls, store.filters)
	}
	f := store.filters[0]
	if f.City != domain.CityDaNang || f.FreshAfter == nil || f.Sort != "score" || f.Limit != 15 || f.Offset != 0 {
		t.Fatalf("filter=%+v", f)
	}
}
