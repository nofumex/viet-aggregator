package exchange

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/encoding/charmap"
)

const dailyURL = "https://www.cbr.ru/scripts/XML_daily.asp"

type Provider interface {
	CachedVNDToRUB() float64
}

// CBR reads the official daily VND/RUB rate and caches it. If a refresh fails,
// the last successfully loaded rate remains usable instead of making cards
// lose their rouble equivalent during a transient outage.
type CBR struct {
	client     *http.Client
	url        string
	ttl        time.Duration
	mu         sync.Mutex
	rate       float64
	until      time.Time
	errorUntil time.Time
	lastErr    error
	refreshing bool
}

func NewCBR() *CBR {
	return &CBR{client: &http.Client{Timeout: 5 * time.Second}, url: dailyURL, ttl: 12 * time.Hour}
}

// CachedVNDToRUB is the UI-safe read path. It only reads memory and never
// waits for DNS, HTTP, or a remote server, even when the value is stale.
func (c *CBR) CachedVNDToRUB() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rate
}

// RunRefresh keeps the cache warm outside request/UI goroutines.
func (c *CBR) RunRefresh(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = c.ttl
	}
	if interval <= 0 {
		interval = 12 * time.Hour
	}
	refresh := func() { _, _ = c.VNDToRUB(ctx) }
	refresh()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh()
		}
	}
}

func (c *CBR) VNDToRUB(ctx context.Context) (float64, error) {
	c.mu.Lock()
	now := time.Now()
	if c.rate > 0 && now.Before(c.until) {
		rate := c.rate
		c.mu.Unlock()
		return rate, nil
	}
	if now.Before(c.errorUntil) {
		rate := c.rate
		c.mu.Unlock()
		return rate, nil
	}
	stale := c.rate
	if c.refreshing {
		c.mu.Unlock()
		return stale, nil
	}
	c.refreshing = true
	c.mu.Unlock()
	defer func() { c.mu.Lock(); c.refreshing = false; c.mu.Unlock() }()

	var resp *http.Response
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
		if e != nil {
			return c.fail(stale, e)
		}
		resp, err = c.client.Do(req)
		if err == nil && (resp.StatusCode < 500 || resp.StatusCode >= 600) {
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		if attempt == 0 {
			select {
			case <-time.After(250 * time.Millisecond):
			case <-ctx.Done():
				return c.fail(stale, ctx.Err())
			}
		}
	}
	if err != nil {
		return c.fail(stale, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return c.fail(stale, fmt.Errorf("CBR HTTP %d", resp.StatusCode))
	}
	var feed struct {
		Currencies []struct {
			Code    string `xml:"CharCode"`
			Nominal int64  `xml:"Nominal"`
			Value   string `xml:"Value"`
		} `xml:"Valute"`
	}
	decoder := xml.NewDecoder(resp.Body)
	decoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		if strings.EqualFold(charset, "windows-1251") {
			return charmap.Windows1251.NewDecoder().Reader(input), nil
		}
		return nil, fmt.Errorf("unsupported CBR XML charset %q", charset)
	}
	if err = decoder.Decode(&feed); err != nil {
		return c.fail(stale, err)
	}
	for _, v := range feed.Currencies {
		if v.Code != "VND" || v.Nominal <= 0 {
			continue
		}
		value, parseErr := strconv.ParseFloat(strings.ReplaceAll(v.Value, ",", "."), 64)
		if parseErr != nil || value <= 0 {
			return c.fail(stale, errors.New("invalid VND rate in CBR response"))
		}
		rate := value / float64(v.Nominal)
		c.mu.Lock()
		c.rate, c.until, c.errorUntil, c.lastErr = rate, time.Now().Add(c.ttl), time.Time{}, nil
		c.mu.Unlock()
		return rate, nil
	}
	return c.fail(stale, errors.New("VND rate is absent from CBR response"))
}

func (c *CBR) fail(stale float64, err error) (float64, error) {
	c.mu.Lock()
	c.errorUntil = time.Now().Add(15 * time.Minute)
	c.lastErr = err
	c.mu.Unlock()
	return staleOrError(stale, err)
}

func staleOrError(stale float64, err error) (float64, error) {
	if stale > 0 {
		return stale, nil
	}
	return 0, err
}
