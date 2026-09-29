package storage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nofumex/telegram-aggregator/internal/domain"
	"github.com/nofumex/telegram-aggregator/internal/ranking"
)

type Store struct{ DB *pgxpool.Pool }

func Open(ctx context.Context, url string, poolSize ...int) (*Store, error) {
	cfg, e := pgxpool.ParseConfig(url)
	if e != nil {
		return nil, e
	}
	max, minc := 5, 1
	if len(poolSize) > 0 && poolSize[0] > 0 {
		max = poolSize[0]
	}
	if len(poolSize) > 1 && poolSize[1] >= 0 {
		minc = poolSize[1]
	}
	if minc > max {
		minc = max
	}
	cfg.MaxConns = int32(max)
	cfg.MinConns = int32(minc)
	cfg.MaxConnLifetime = time.Hour
	db, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		return nil, e
	}
	if e = db.Ping(ctx); e != nil {
		db.Close()
		return nil, e
	}
	return &Store{db}, nil
}
func (s *Store) Close() { s.DB.Close() }

const channelCols = `id,username,url,name,city,enabled,polling_interval_seconds,parsing_profile,profile_status,profile_error,last_success_at,last_attempt_at,last_message_at,coalesce(last_message_id,0),posts_total,new_posts_last_run,consecutive_errors,last_error,next_poll_at,created_at`
const dueProfileStatuses = "('pending','ready','error')"
const studioSearchPredicate = "(l.bedrooms=0 OR l.property_type='studio')"

func scanChannel(row pgx.Row) (domain.Channel, error) {
	var c domain.Channel
	var sec int
	var raw []byte
	e := row.Scan(&c.ID, &c.Username, &c.URL, &c.Name, &c.City, &c.Enabled, &sec, &raw, &c.ProfileStatus, &c.ProfileError, &c.LastSuccessAt, &c.LastAttemptAt, &c.LastMessageAt, &c.LastMessageID, &c.PostsTotal, &c.NewPostsLastRun, &c.ConsecutiveErrors, &c.LastError, &c.NextPollAt, &c.CreatedAt)
	c.PollingInterval = time.Duration(sec) * time.Second
	if len(raw) > 0 && string(raw) != "null" {
		var p domain.ChannelParsingProfile
		if json.Unmarshal(raw, &p) == nil {
			c.Profile = &p
		}
	}
	return c, e
}
func (s *Store) channels(ctx context.Context, where string, args ...any) ([]domain.Channel, error) {
	rows, e := s.DB.Query(ctx, "SELECT "+channelCols+" FROM telegram_channels "+where, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []domain.Channel
	for rows.Next() {
		c, e := scanChannel(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Store) Channels(ctx context.Context) ([]domain.Channel, error) {
	return s.channels(ctx, "ORDER BY enabled DESC,name")
}
func (s *Store) DueChannels(ctx context.Context, limit int) ([]domain.Channel, error) {
	return s.channels(ctx, "WHERE enabled AND next_poll_at<=now() AND profile_status IN "+dueProfileStatuses+" ORDER BY next_poll_at LIMIT $1", limit)
}
func (s *Store) EnabledChannels(ctx context.Context) ([]domain.Channel, error) {
	return s.channels(ctx, "WHERE enabled ORDER BY id")
}
func (s *Store) Channel(ctx context.Context, id int64) (domain.Channel, error) {
	return scanChannel(s.DB.QueryRow(ctx, "SELECT "+channelCols+" FROM telegram_channels WHERE id=$1", id))
}
func (s *Store) AddChannel(ctx context.Context, username, name, city string, poll time.Duration) (domain.Channel, error) {
	if poll < 30*time.Second {
		poll = 5 * time.Minute
	}
	if city != domain.CityNhaTrang {
		city = domain.CityDaNang
	}
	return scanChannel(s.DB.QueryRow(ctx, "INSERT INTO telegram_channels(username,url,name,city,polling_interval_seconds) VALUES($1,$2,$3,$4,$5) RETURNING "+channelCols, username, "https://t.me/"+username, name, city, int(poll.Seconds())))
}
func (s *Store) UpdateChannel(ctx context.Context, id int64, name, city string, enabled bool, poll time.Duration) error {
	_, e := s.DB.Exec(ctx, "UPDATE telegram_channels SET name=$2,city=$3,enabled=$4,polling_interval_seconds=$5,updated_at=now(),next_poll_at=LEAST(next_poll_at,now()) WHERE id=$1", id, name, city, enabled, int(poll.Seconds()))
	return e
}
func (s *Store) ResolveChannel(ctx context.Context, id int64, username, name, url string) error {
	_, e := s.DB.Exec(ctx, "UPDATE telegram_channels SET username=$2,name=CASE WHEN name='' OR name LIKE '@%' THEN $3 ELSE name END,url=$4,updated_at=now() WHERE id=$1", id, username, name, url)
	return e
}
func (s *Store) SetProfilePending(ctx context.Context, id int64) error {
	_, e := s.DB.Exec(ctx, "UPDATE telegram_channels SET parsing_profile=NULL,profile_status='pending',profile_error='',next_poll_at=now(),updated_at=now() WHERE id=$1", id)
	return e
}
func (s *Store) SaveProfile(ctx context.Context, id int64, p domain.ChannelParsingProfile) error {
	raw, _ := json.Marshal(p)
	_, e := s.DB.Exec(ctx, "UPDATE telegram_channels SET parsing_profile=$2,profile_status='ready',profile_error='',next_poll_at=now(),updated_at=now() WHERE id=$1", id, raw)
	return e
}
func (s *Store) FailProfile(ctx context.Context, id int64, err error) {
	_, _ = s.DB.Exec(ctx, "UPDATE telegram_channels SET profile_status='error',profile_error=$2,last_error=$2,next_poll_at=now()+interval '15 minutes',updated_at=now() WHERE id=$1", id, err.Error())
}
func (s *Store) DeleteChannel(ctx context.Context, id int64) error {
	_, e := s.DB.Exec(ctx, "DELETE FROM telegram_channels WHERE id=$1", id)
	return e
}
func (s *Store) ForceChannel(ctx context.Context, id int64) error {
	_, e := s.DB.Exec(ctx, "UPDATE telegram_channels SET next_poll_at=now() WHERE id=$1", id)
	return e
}
func (s *Store) SetAllPolling(ctx context.Context, poll time.Duration) error {
	_, e := s.DB.Exec(ctx, "UPDATE telegram_channels SET polling_interval_seconds=$1,updated_at=now()", int(poll.Seconds()))
	return e
}
func (s *Store) SyncStarted(ctx context.Context, id int64) (int64, error) {
	var run int64
	e := s.DB.QueryRow(ctx, "INSERT INTO sync_runs(channel_id) VALUES($1) RETURNING id", id).Scan(&run)
	_, _ = s.DB.Exec(ctx, "UPDATE telegram_channels SET last_attempt_at=now() WHERE id=$1", id)
	return run, e
}
func (s *Store) SyncFinished(ctx context.Context, run, id int64, r domain.ChannelSyncResult, syncErr error, poll time.Duration) {
	status, msg := "ok", ""
	if syncErr != nil {
		status = "error"
		msg = syncErr.Error()
	}
	_, _ = s.DB.Exec(ctx, "UPDATE sync_runs SET finished_at=now(),fetched=$2,inserted=$3,status=$4,error_message=NULLIF($5,'') WHERE id=$1", run, r.Fetched, r.Inserted, status, msg)
	if syncErr == nil {
		_, _ = s.DB.Exec(ctx, `UPDATE telegram_channels SET last_success_at=now(),last_message_at=CASE WHEN $2::timestamptz>'epoch' THEN $2 ELSE last_message_at END,last_message_id=GREATEST(last_message_id,$3),posts_total=posts_total+$4,new_posts_last_run=$4,consecutive_errors=0,last_error='',next_poll_at=now()+make_interval(secs=>$5),updated_at=now() WHERE id=$1`, id, r.NewestAt, r.NewestID, r.Inserted, int(poll.Seconds()))
	} else {
		_, _ = s.DB.Exec(ctx, `UPDATE telegram_channels SET new_posts_last_run=0,consecutive_errors=consecutive_errors+1,last_error=$2,next_poll_at=CASE WHEN profile_status='error' THEN next_poll_at ELSE now()+make_interval(secs=>LEAST($3*power(2,LEAST(consecutive_errors,6))::int,21600)) END,updated_at=now() WHERE id=$1`, id, msg, int(poll.Seconds()))
	}
}

func (s *Store) InsertListing(ctx context.Context, p domain.TelegramPost, l domain.Listing) (bool, error) {
	tx, e := s.DB.Begin(ctx)
	if e != nil {
		return false, e
	}
	defer tx.Rollback(ctx)
	hash := sha256.Sum256([]byte(p.Text))
	var postID int64
	e = tx.QueryRow(ctx, `INSERT INTO posts(channel_id,channel_username,message_id,original_url,original_text,published_at,photo_url,raw_payload,content_hash) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9) ON CONFLICT(channel_username,message_id) DO NOTHING RETURNING id`, l.ChannelID, p.ChannelUsername, p.MessageID, p.URL, p.Text, p.PublishedAt, p.PhotoURL, p.Raw, hash[:]).Scan(&postID)
	isNew := true
	if e == pgx.ErrNoRows {
		isNew = false
		e = tx.QueryRow(ctx, "SELECT id FROM posts WHERE channel_username=$1 AND message_id=$2 FOR UPDATE", p.ChannelUsername, p.MessageID).Scan(&postID)
	}
	if e != nil {
		return false, e
	}
	if !isNew {
		_, e = tx.Exec(ctx, `UPDATE posts SET original_url=$2,original_text=$3,published_at=$4,photo_url=NULLIF($5,''),raw_payload=$6,content_hash=$7,updated_at=now() WHERE id=$1`, postID, p.URL, p.Text, p.PublishedAt, p.PhotoURL, p.Raw, hash[:])
		if e != nil {
			return false, e
		}
	}
	j := func(v any) []byte { b, _ := json.Marshal(v); return b }
	_, e = tx.Exec(ctx, `INSERT INTO listings(post_id,city,zone,district,location_original,street,address,building,property_type,bedrooms,rooms,rent_min,rent_max,deposit_amount,lease_months,area_m2,availability,furnished,near_beach,beach_distance_m,amenities,utilities,is_oceanus,near_oceanus,confidence,raw_values,deal_score,score_confidence,extraction_version,extraction_status,profile_parsed_at,last_extraction_error,ranked_at)
	VALUES($1,$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),NULLIF($9,''),$10,$11,$12,$13,$14,$15,$16,NULLIF($17,''),NULLIF($18,''),$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,NULLIF($32,''),now())
	ON CONFLICT(post_id) DO UPDATE SET city=$2,zone=NULLIF($3,''),district=NULLIF($4,''),location_original=NULLIF($5,''),street=NULLIF($6,''),address=NULLIF($7,''),building=NULLIF($8,''),property_type=NULLIF($9,''),bedrooms=$10,rooms=$11,rent_min=$12,rent_max=$13,deposit_amount=$14,lease_months=$15,area_m2=$16,availability=NULLIF($17,''),furnished=NULLIF($18,''),near_beach=$19,beach_distance_m=$20,amenities=$21,utilities=$22,is_oceanus=$23,near_oceanus=$24,confidence=$25,raw_values=$26,deal_score=$27,score_confidence=$28,extraction_version=$29,extraction_status=$30,profile_parsed_at=$31,last_extraction_error=NULLIF($32,''),ranked_at=now(),updated_at=now()`, postID, l.City, l.Zone, l.District, l.LocationOriginal, l.Street, l.Address, l.Building, l.PropertyType, l.Bedrooms, l.Rooms, l.RentMin, l.RentMax, l.DepositAmount, l.LeaseMonths, l.AreaM2, l.Availability, l.Furnished, l.NearBeach, l.BeachDistanceM, j(l.Amenities), j(l.Utilities), l.IsOceanus, l.NearOceanus, j(l.Confidence), j(l.RawValues), l.DealScore, l.ScoreConfidence, l.ExtractionVersion, l.ExtractionStatus, l.ProfileParsedAt, l.LastExtractionError)
	if e != nil {
		return false, e
	}
	return isNew, tx.Commit(ctx)
}

const listingSelect = `SELECT l.id,p.id,p.channel_id,p.channel_username,c.name,p.message_id,p.original_url,p.original_text,p.published_at,l.created_at,l.city,coalesce(l.zone,''),coalesce(l.district,''),coalesce(l.location_original,''),coalesce(l.street,''),coalesce(l.address,''),coalesce(l.building,''),l.rent_min,l.rent_max,l.deposit_amount,l.bedrooms,l.rooms,l.lease_months,l.area_m2,coalesce(l.property_type,''),coalesce(l.availability,''),coalesce(l.furnished,''),l.near_beach,l.beach_distance_m,l.amenities,l.is_oceanus,l.near_oceanus,l.utilities,l.confidence,l.raw_values,l.deal_score,l.score_confidence,CASE WHEN p.photo_url IS NULL THEN '[]'::jsonb ELSE jsonb_build_array(p.photo_url) END,coalesce(l.extraction_version,''),l.extraction_status,l.profile_parsed_at,l.extraction_attempts,l.next_extraction_retry_at,coalesce(l.last_extraction_error,''),l.ranked_at FROM listings l JOIN posts p ON p.id=l.post_id JOIN telegram_channels c ON c.id=p.channel_id`

func scanListing(row pgx.Row) (domain.Listing, error) {
	var l domain.Listing
	var amenities, util, conf, raw, media []byte
	e := row.Scan(&l.ID, &l.PostID, &l.ChannelID, &l.ChannelUsername, &l.ChannelName, &l.TelegramMessageID, &l.OriginalURL, &l.OriginalText, &l.PublishedAt, &l.CreatedAt, &l.City, &l.Zone, &l.District, &l.LocationOriginal, &l.Street, &l.Address, &l.Building, &l.RentMin, &l.RentMax, &l.DepositAmount, &l.Bedrooms, &l.Rooms, &l.LeaseMonths, &l.AreaM2, &l.PropertyType, &l.Availability, &l.Furnished, &l.NearBeach, &l.BeachDistanceM, &amenities, &l.IsOceanus, &l.NearOceanus, &util, &conf, &raw, &l.DealScore, &l.ScoreConfidence, &media, &l.ExtractionVersion, &l.ExtractionStatus, &l.ProfileParsedAt, &l.ExtractionAttempts, &l.NextExtractionRetryAt, &l.LastExtractionError, &l.RankedAt)
	if e == nil {
		l.Currency = "VND"
		_ = json.Unmarshal(amenities, &l.Amenities)
		_ = json.Unmarshal(util, &l.Utilities)
		_ = json.Unmarshal(conf, &l.Confidence)
		_ = json.Unmarshal(raw, &l.RawValues)
		_ = json.Unmarshal(media, &l.MediaURLs)
	}
	return l, e
}
func (s *Store) Listing(ctx context.Context, id int64) (domain.Listing, error) {
	return scanListing(s.DB.QueryRow(ctx, listingSelect+" WHERE l.id=$1", id))
}
func (s *Store) HiddenListingIDs(ctx context.Context, user int64, ids []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, e := s.DB.Query(ctx, "SELECT listing_id FROM hidden_listings WHERE telegram_user_id=$1 AND listing_id=ANY($2)", user, ids)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if e = rows.Scan(&id); e != nil {
			return nil, e
		}
		out[id] = true
	}
	return out, rows.Err()
}
func (s *Store) Benchmarks(ctx context.Context, l domain.Listing) (ranking.Benchmarks, error) {
	var b ranking.Benchmarks
	e := s.DB.QueryRow(ctx, `SELECT coalesce(percentile_cont(.5) within group(order by rent_min),0),coalesce(percentile_cont(.5) within group(order by rent_min/nullif(area_m2,0)),0),count(*) FROM listings x JOIN posts p ON p.id=x.post_id WHERE p.published_at>now()-interval '90 days' AND x.extraction_status='success' AND x.city=$1 AND x.rent_min IS NOT NULL AND ($2='' OR x.district=$2) AND ($3='' OR x.property_type=$3) AND ($4::smallint IS NULL OR abs(x.bedrooms-$4)<=1)`, l.City, l.District, l.PropertyType, l.Bedrooms).Scan(&b.MedianRent, &b.MedianPriceM2, &b.SimilarCount)
	return b, e
}
func (s *Store) StaleRankingBatch(ctx context.Context, before time.Time, limit int) ([]domain.Listing, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, e := s.DB.Query(ctx, listingSelect+` WHERE l.extraction_status='success' AND l.rent_min IS NOT NULL AND (l.ranked_at IS NULL OR l.ranked_at<$1) ORDER BY l.ranked_at NULLS FIRST,l.id LIMIT $2`, before, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []domain.Listing
	for rows.Next() {
		l, e := scanListing(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
func (s *Store) UpdateScore(ctx context.Context, id int64, score, confidence float64) error {
	_, e := s.DB.Exec(ctx, "UPDATE listings SET deal_score=$2,score_confidence=$3,ranked_at=now(),updated_at=now() WHERE id=$1", id, score, confidence)
	return e
}

func (s *Store) Search(ctx context.Context, user int64, f domain.SearchFilter) (domain.SearchPage, error) {
	args := []any{user}
	where := []string{"l.extraction_status='success'", "l.rent_min IS NOT NULL", "NOT EXISTS(SELECT 1 FROM hidden_listings h WHERE h.telegram_user_id=$1 AND h.listing_id=l.id)"}
	add := func(cond string, v any) { args = append(args, v); where = append(where, fmt.Sprintf(cond, len(args))) }
	if f.MaxResults > 0 {
		// Bound the candidate set before applying per-user visibility. This
		// prevents hidden recent items from being backfilled with listings
		// older than the globally latest window.
		add("l.id IN (SELECT recent.id FROM listings recent JOIN posts recent_post ON recent_post.id=recent.post_id WHERE recent.extraction_status='success' AND recent.rent_min IS NOT NULL ORDER BY recent_post.published_at DESC,recent.id DESC LIMIT $%d)", min(f.MaxResults, 500))
	}
	if f.City != "" {
		add("l.city=$%d", f.City)
	}
	if f.Zone != "" {
		add("l.zone=$%d", f.Zone)
	}
	if f.District != "" {
		add("l.district=$%d", f.District)
	}
	if f.Location != "" {
		add("(l.location_original ILIKE '%%'||$%d||'%%' OR l.building ILIKE '%%'||$%d||'%%' OR l.street ILIKE '%%'||$%d||'%%')", f.Location)
		n := len(args)
		where[len(where)-1] = fmt.Sprintf("(l.location_original ILIKE '%%'||$%d||'%%' OR l.building ILIKE '%%'||$%d||'%%' OR l.street ILIKE '%%'||$%d||'%%')", n, n, n)
	}
	if f.RentMin != nil {
		add("l.rent_min>=$%d", *f.RentMin)
	}
	if f.RentMax != nil {
		add("l.rent_min<=$%d", *f.RentMax)
	}
	if f.Bedrooms != nil {
		if *f.Bedrooms == 0 {
			where = append(where, studioSearchPredicate)
		} else {
			add("l.bedrooms=$%d", *f.Bedrooms)
		}
	}
	if f.PropertyType != "" {
		if f.PropertyType == "studio" {
			where = append(where, studioSearchPredicate)
		} else {
			add("l.property_type=$%d", f.PropertyType)
		}
	}
	if f.Furnished != "" {
		add("l.furnished=$%d", f.Furnished)
	}
	if f.NearBeach != nil {
		add("l.near_beach=$%d", *f.NearBeach)
	}
	if f.AreaMin != nil {
		add("l.area_m2>=$%d", *f.AreaMin)
	}
	if f.AreaMax != nil {
		add("l.area_m2<=$%d", *f.AreaMax)
	}
	if f.FreshAfter != nil {
		add("p.published_at>=$%d", *f.FreshAfter)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		add("(p.original_text ILIKE '%%'||$%d||'%%' OR l.location_original ILIKE '%%'||$%d||'%%')", q)
		n := len(args)
		where[len(where)-1] = fmt.Sprintf("(p.original_text ILIKE '%%'||$%d||'%%' OR l.location_original ILIKE '%%'||$%d||'%%')", n, n)
	}
	w := " WHERE " + strings.Join(where, " AND ")
	var total int
	if e := s.DB.QueryRow(ctx, "SELECT count(*) FROM listings l JOIN posts p ON p.id=l.post_id"+w, args...).Scan(&total); e != nil {
		return domain.SearchPage{}, e
	}
	limit, offset, total := normalizeSearchWindow(f.Limit, f.Offset, f.MaxResults, total)
	if offset >= total {
		return domain.SearchPage{Total: total}, nil
	}
	order := "l.deal_score DESC,p.published_at DESC,l.id DESC"
	if f.Sort == "new" {
		order = "p.published_at DESC,l.id DESC"
	} else if f.Sort == "price" {
		order = "l.rent_min ASC,l.id DESC"
	}
	args = append(args, limit, offset)
	rows, e := s.DB.Query(ctx, listingSelect+w+fmt.Sprintf(" ORDER BY %s LIMIT $%d OFFSET $%d", order, len(args)-1, len(args)), args...)
	if e != nil {
		return domain.SearchPage{}, e
	}
	defer rows.Close()
	out := domain.SearchPage{Total: total}
	for rows.Next() {
		l, e := scanListing(rows)
		if e != nil {
			return out, e
		}
		out.Items = append(out.Items, l)
	}
	return out, rows.Err()
}

func normalizeSearchWindow(limit, offset, maxResults, total int) (int, int, int) {
	if limit < 1 || limit > 250 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	if maxResults > 0 {
		if maxResults > 500 {
			maxResults = 500
		}
		if total > maxResults {
			total = maxResults
		}
		if remaining := total - offset; remaining < limit {
			limit = remaining
		}
	}
	return limit, offset, total
}
func (s *Store) EnsureUser(ctx context.Context, id int64, username, first string) error {
	_, e := s.DB.Exec(ctx, `INSERT INTO bot_users(telegram_user_id,username,first_name) VALUES($1,$2,$3) ON CONFLICT(telegram_user_id) DO UPDATE SET username=$2,first_name=$3,last_seen_at=now()`, id, username, first)
	return e
}
func (s *Store) Favorite(ctx context.Context, user, listing int64) error {
	_, e := s.DB.Exec(ctx, "INSERT INTO favorites(telegram_user_id,listing_id) VALUES($1,$2) ON CONFLICT DO NOTHING", user, listing)
	return e
}
func (s *Store) Hide(ctx context.Context, user, listing int64) error {
	_, e := s.DB.Exec(ctx, "INSERT INTO hidden_listings(telegram_user_id,listing_id) VALUES($1,$2) ON CONFLICT DO NOTHING", user, listing)
	return e
}
func (s *Store) Favorites(ctx context.Context, user int64, limit int) ([]domain.Listing, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	rows, e := s.DB.Query(ctx, listingSelect+" JOIN favorites f ON f.listing_id=l.id WHERE f.telegram_user_id=$1 ORDER BY f.created_at DESC LIMIT $2", user, limit)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []domain.Listing
	for rows.Next() {
		l, e := scanListing(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
func (s *Store) Market(ctx context.Context) (domain.MarketSummary, error) {
	return s.MarketByCity(ctx, "")
}
func (s *Store) MarketByCity(ctx context.Context, city string) (domain.MarketSummary, error) {
	m := domain.MarketSummary{City: city}
	e := s.DB.QueryRow(ctx, `SELECT count(*),coalesce(percentile_cont(.5) within group(order by l.rent_min),0)::bigint,coalesce(percentile_cont(.5) within group(order by l.rent_min/nullif(l.area_m2,0)),0)::bigint FROM listings l JOIN posts p ON p.id=l.post_id WHERE p.published_at>=now()-interval '30 days' AND l.extraction_status='success' AND l.rent_min IS NOT NULL AND ($1='' OR l.city=$1)`, city).Scan(&m.Listings30d, &m.MedianRent, &m.MedianPriceM2)
	if e != nil {
		return m, e
	}
	rows, e := s.DB.Query(ctx, `SELECT coalesce(district,'—'),count(*),percentile_cont(.5) within group(order by rent_min)::bigint,coalesce(percentile_cont(.5) within group(order by rent_min/nullif(area_m2,0)),0)::bigint FROM listings l JOIN posts p ON p.id=l.post_id WHERE p.published_at>=now()-interval '30 days' AND l.extraction_status='success' AND rent_min IS NOT NULL AND ($1='' OR city=$1) GROUP BY district ORDER BY count(*) DESC LIMIT 8`, city)
	if e != nil {
		return m, e
	}
	defer rows.Close()
	for rows.Next() {
		var d domain.DistrictStat
		if e = rows.Scan(&d.District, &d.Listings, &d.MedianRent, &d.MedianPriceM2); e != nil {
			return m, e
		}
		m.Districts = append(m.Districts, d)
	}
	return m, rows.Err()
}
