package telegramfeed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nofumex/telegram-aggregator/internal/domain"
	xhtml "golang.org/x/net/html"
)

var usernameRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{4,31}$`)

func NormalizeUsername(input string) (string, error) {
	s := strings.TrimSpace(input)
	s = strings.TrimPrefix(s, "@")
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || !strings.EqualFold(strings.TrimPrefix(u.Hostname(), "www."), "t.me") {
			return "", errors.New("invalid Telegram channel URL")
		}
		s = strings.Trim(u.Path, "/")
	} else if strings.HasPrefix(strings.ToLower(s), "t.me/") || strings.HasPrefix(strings.ToLower(s), "www.t.me/") {
		s = strings.TrimPrefix(strings.TrimPrefix(s, "www."), "t.me/")
	}
	s = strings.TrimPrefix(strings.Trim(s, "/"), "s/")
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	if !usernameRE.MatchString(s) {
		return "", errors.New("invalid public Telegram channel username")
	}
	return strings.ToLower(s), nil
}

type FetchRequest struct {
	Username string
	AfterID  int64
	Limit    int
}
type FetchResult struct {
	Posts []domain.TelegramPost
	Name  string
	Kind  string
}
type Adapter interface {
	Resolve(context.Context, string) (username, name, canonicalURL string, err error)
	Fetch(context.Context, FetchRequest) (FetchResult, error)
	Check(context.Context, string) error
}

type Client struct {
	HTTP    *http.Client
	BaseURL string
}

func New() *Client {
	return &Client{HTTP: &http.Client{Timeout: 20 * time.Second}, BaseURL: "https://t.me/s"}
}

func (c *Client) Resolve(ctx context.Context, ref string) (string, string, string, error) {
	username, err := NormalizeUsername(ref)
	if err != nil {
		return "", "", "", err
	}
	result, err := c.fetchPage(ctx, username, 0)
	if err != nil {
		return "", "", "", err
	}
	if len(result.Posts) == 0 {
		return "", "", "", errors.New("public channel has no visible posts")
	}
	name := result.Name
	if name == "" {
		name = "@" + username
	}
	return username, name, "https://t.me/" + username, nil
}

func (c *Client) Check(ctx context.Context, username string) error {
	r, err := c.fetchPage(ctx, username, 0)
	if err == nil && len(r.Posts) == 0 {
		return errors.New("channel preview contains no posts")
	}
	return err
}

func (c *Client) Fetch(ctx context.Context, req FetchRequest) (FetchResult, error) {
	username, err := NormalizeUsername(req.Username)
	if err != nil {
		return FetchResult{}, err
	}
	limit := req.Limit
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	seen := map[int64]bool{}
	var all []domain.TelegramPost
	var name string
	before := int64(0)
	for len(all) < limit {
		page, e := c.fetchPage(ctx, username, before)
		if e != nil {
			return FetchResult{}, e
		}
		if name == "" {
			name = page.Name
		}
		if len(page.Posts) == 0 {
			break
		}
		oldest := int64(0)
		for _, post := range page.Posts {
			if oldest == 0 || post.MessageID < oldest {
				oldest = post.MessageID
			}
			if post.MessageID <= req.AfterID || seen[post.MessageID] {
				continue
			}
			seen[post.MessageID] = true
			all = append(all, post)
		}
		if oldest == 0 || oldest == before || oldest <= req.AfterID+1 {
			break
		}
		before = oldest
	}
	sort.Slice(all, func(i, j int) bool { return all[i].MessageID > all[j].MessageID })
	if len(all) > limit {
		all = all[:limit]
	}
	return FetchResult{Posts: all, Name: name}, nil
}

func (c *Client) fetchPage(ctx context.Context, username string, before int64) (FetchResult, error) {
	endpoint := strings.TrimRight(c.BaseURL, "/") + "/" + username
	if before > 0 {
		endpoint += "?before=" + strconv.FormatInt(before, 10)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return FetchResult{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; RentalAggregator/1.0)")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return FetchResult{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return FetchResult{}, fmt.Errorf("Telegram preview HTTP %d", resp.StatusCode)
	}
	doc, err := xhtml.Parse(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return FetchResult{}, err
	}
	return parseDocument(doc, username), nil
}

func parseDocument(doc *xhtml.Node, username string) FetchResult {
	result := FetchResult{}
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode && n.Data == "meta" && attr(n, "property") == "og:title" {
			result.Name = attr(n, "content")
		}
		if n.Type == xhtml.ElementNode && hasClass(n, "tgme_channel_info_counter") {
			counter := strings.ToLower(nodeText(n))
			if strings.Contains(counter, "member") {
				result.Kind = "mtproto_group"
			}
			if strings.Contains(counter, "subscriber") {
				result.Kind = "web_channel"
			}
		}
		if n.Type == xhtml.ElementNode && hasClass(n, "tgme_widget_message") {
			dataPost := attr(n, "data-post")
			parts := strings.Split(dataPost, "/")
			id, _ := strconv.ParseInt(parts[len(parts)-1], 10, 64)
			if id > 0 {
				post := domain.TelegramPost{ChannelUsername: username, MessageID: id, URL: fmt.Sprintf("https://t.me/%s/%d", username, id)}
				if raw, e := json.Marshal(map[string]any{"data_post": dataPost}); e == nil {
					post.Raw = raw
				}
				findPostFields(n, &post)
				result.Posts = append(result.Posts, post)
			}
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	return result
}

func findPostFields(n *xhtml.Node, post *domain.TelegramPost) {
	if n.Type == xhtml.ElementNode {
		if hasClass(n, "tgme_widget_message_text") {
			post.Text = strings.TrimSpace(nodeText(n))
		}
		if n.Data == "time" {
			if parsed, e := time.Parse(time.RFC3339, attr(n, "datetime")); e == nil {
				post.PublishedAt = parsed
			}
		}
		if hasClass(n, "tgme_widget_message_photo_wrap") && post.PhotoURL == "" {
			style := html.UnescapeString(attr(n, "style"))
			if i := strings.Index(style, "url("); i >= 0 {
				v := strings.TrimSpace(style[i+4:])
				if j := strings.IndexByte(v, ')'); j >= 0 {
					post.PhotoURL = strings.Trim(v[:j], "'\"")
				}
			}
		}
	}
	for child := n.FirstChild; child != nil; child = child.NextSibling {
		findPostFields(child, post)
	}
}

func nodeText(n *xhtml.Node) string {
	var b strings.Builder
	var walk func(*xhtml.Node)
	walk = func(x *xhtml.Node) {
		if x.Type == xhtml.TextNode {
			b.WriteString(x.Data)
		}
		if x.Type == xhtml.ElementNode && x.Data == "br" {
			b.WriteByte('\n')
		}
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}
func attr(n *xhtml.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}
func hasClass(n *xhtml.Node, class string) bool {
	for _, v := range strings.Fields(attr(n, "class")) {
		if v == class {
			return true
		}
	}
	return false
}
