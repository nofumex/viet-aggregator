package telegramfeed

import (
	xhtml "golang.org/x/net/html"
	"strings"
	"testing"
)

func TestNormalizeUsername(t *testing.T) {
	for _, input := range []string{"https://t.me/lowrentnt", "https://t.me/s/lowrentnt", "t.me/lowrentnt", "@lowrentnt", "lowrentnt"} {
		got, err := NormalizeUsername(input)
		if err != nil || got != "lowrentnt" {
			t.Fatalf("%q => %q, %v", input, got, err)
		}
	}
}

func TestParseTelegramPreview(t *testing.T) {
	doc, _ := xhtml.Parse(strings.NewReader(`<html><head><meta property="og:title" content="Low Rent NT"></head><body><div class="tgme_widget_message" data-post="lowrentnt/42"><a class="tgme_widget_message_photo_wrap" style="background-image:url('https://cdn/photo.jpg')"></a><div class="tgme_widget_message_text">Oceanus<br>7 млн</div><time datetime="2026-09-20T10:00:00+00:00"></time></div></body></html>`))
	got := parseDocument(doc, "lowrentnt")
	if got.Name != "Low Rent NT" || len(got.Posts) != 1 || got.Posts[0].MessageID != 42 || got.Posts[0].PhotoURL != "https://cdn/photo.jpg" || got.Posts[0].Text != "Oceanus\n7 млн" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestParseTelegramPreviewDetectsPublicGroup(t *testing.T) {
	doc, _ := xhtml.Parse(strings.NewReader(`<div class="tgme_channel_info_counter"><span class="counter_value">12 345</span> members</div>`))
	if got := parseDocument(doc, "rentgroup").Kind; got != "mtproto_group" {
		t.Fatalf("kind=%q", got)
	}
}
