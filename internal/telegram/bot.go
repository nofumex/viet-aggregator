package telegram

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nofumex/telegram-aggregator/internal/collections"
	"github.com/nofumex/telegram-aggregator/internal/domain"
	"github.com/nofumex/telegram-aggregator/internal/exchange"
	"github.com/nofumex/telegram-aggregator/internal/mtproto"
	"github.com/nofumex/telegram-aggregator/internal/parser"
	"github.com/nofumex/telegram-aggregator/internal/storage"
	"github.com/nofumex/telegram-aggregator/internal/syncer"
	"github.com/nofumex/telegram-aggregator/internal/telegramfeed"
)

type Bot struct {
	api         *Client
	store       *storage.Store
	sync        *syncer.Service
	collections *collections.Service
	rates       exchange.Provider
	admins      map[int64]bool
	log         *slog.Logger
	defaultPoll time.Duration
	account     *mtproto.Manager
	mu          sync.Mutex
	states      map[int64]string
	filters     map[int64]domain.SearchFilter
	pages       map[string]pageCache
}
type pageCache struct {
	items   []domain.Listing
	total   int
	user    int64
	until   time.Time
	title   string
	reasons map[int64]string
	filter  *domain.SearchFilter
}

func NewBot(api *Client, store *storage.Store, sync *syncer.Service, c *collections.Service, rates exchange.Provider, admins map[int64]bool, log *slog.Logger, poll time.Duration, account ...*mtproto.Manager) *Bot {
	b := &Bot{api: api, store: store, sync: sync, collections: c, rates: rates, admins: admins, log: log, defaultPoll: poll, states: map[int64]string{}, filters: map[int64]domain.SearchFilter{}, pages: map[string]pageCache{}}
	if len(account) > 0 {
		b.account = account[0]
	}
	return b
}

func (b *Bot) NotifyProfileReady(channel domain.Channel, profile domain.ChannelParsingProfile) {
	raw, _ := json.MarshalIndent(profile, "", "  ")
	text := "✅ Структура парсинга канала @" + channel.Username + " получена от LLM\n\n<pre>" + html.EscapeString(truncate(string(raw), 3000)) + "</pre>\n\n⏳ Посты загружаются..."
	for adminID := range b.admins {
		b.send(context.Background(), adminID, text, Markup{})
	}
}

func (b *Bot) Run(ctx context.Context) error {
	offset := 0
	for {
		updates, err := b.api.Updates(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			b.log.Warn("telegram polling", "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
			}
			continue
		}
		for _, u := range updates {
			offset = u.UpdateID + 1
			if u.Callback != nil {
				_ = b.api.Answer(context.WithoutCancel(ctx), u.Callback.ID, "")
				go b.callback(context.WithoutCancel(ctx), u.Callback)
			} else if u.Message != nil {
				go b.message(context.WithoutCancel(ctx), u.Message)
			}
		}
	}
}
func (b *Bot) message(ctx context.Context, m *Message) {
	_ = b.store.EnsureUser(ctx, m.From.ID, m.From.Username, m.From.FirstName)
	text := strings.TrimSpace(m.Text)
	if text == "/start" || text == "/menu" {
		b.showMenu(ctx, m.Chat.ID, 0)
		return
	}
	b.mu.Lock()
	state := b.states[m.From.ID]
	delete(b.states, m.From.ID)
	b.mu.Unlock()
	if state != "" {
		b.handleState(ctx, m, state)
		return
	}
	if strings.HasPrefix(text, "/") {
		b.send(ctx, m.Chat.ID, "Неизвестная команда. Используйте меню.", mainKeyboard(b.admins[m.From.ID]))
		return
	}
	f := parser.ParseSearch(text)
	b.mu.Lock()
	b.filters[m.From.ID] = f
	b.mu.Unlock()
	b.runSearch(ctx, m.Chat.ID, 0, m.From.ID, f, "🔎 Результаты поиска")
}
func (b *Bot) callback(ctx context.Context, q *CallbackQuery) {
	_ = b.store.EnsureUser(ctx, q.From.ID, q.From.Username, q.From.FirstName)
	parts := strings.Split(q.Data, ":")
	switch parts[0] {
	case "menu":
		b.showMenu(ctx, q.Message.Chat.ID, q.Message.MessageID)
	case "new":
		f := domain.SearchFilter{Limit: 50, MaxResults: 500, Sort: "new"}
		b.runSearch(ctx, q.Message.Chat.ID, q.Message.MessageID, q.From.ID, f, "🏠 Новые объявления")
	case "search":
		b.showFilters(ctx, q)
	case "filter":
		b.applyFilter(ctx, q, parts)
	case "find":
		b.mu.Lock()
		f := b.filters[q.From.ID]
		b.mu.Unlock()
		b.runSearch(ctx, q.Message.Chat.ID, q.Message.MessageID, q.From.ID, f, "🔎 Подходящие варианты")
	case "page":
		b.showPage(ctx, q.Message.Chat.ID, q.Message.MessageID, q.From.ID, len(q.Message.Photo) > 0, parts)
	case "save":
		b.listAction(ctx, q, true, parts)
	case "hide":
		b.listAction(ctx, q, false, parts)
	case "detail":
		b.showDetails(ctx, q, parts)
	case "collections":
		b.showCollections(ctx, q)
	case "colcity":
		b.showCollectionPeriods(ctx, q, parts)
	case "market":
		city := ""
		if len(parts) > 1 {
			city = parts[1]
		}
		b.showMarket(ctx, q, city)
	case "col":
		b.runCollection(ctx, q, parts)
	case "admin":
		if b.admins[q.From.ID] {
			b.showAdmin(ctx, q)
		}
	case "agroups":
		if b.admins[q.From.ID] {
			b.showGroups(ctx, q)
		}
	case "mtaccount":
		if b.admins[q.From.ID] {
			b.showTelegramAccount(ctx, q)
		}
	case "mteditid":
		if b.admins[q.From.ID] {
			b.setState(q.From.ID, "mt_api_id")
			b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "Пришлите API ID.", back("mtaccount"))
		}
	case "mtedithash":
		if b.admins[q.From.ID] {
			b.setState(q.From.ID, "mt_api_hash")
			b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "Пришлите API Hash. Сообщение будет удалено после сохранения.", back("mtaccount"))
		}
	case "mtlogin":
		if b.admins[q.From.ID] {
			b.setState(q.From.ID, "mt_phone")
			b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "Пришлите номер телефона в международном формате, например <code>+849...</code>.", back("mtaccount"))
		}
	case "mtlogout":
		if b.admins[q.From.ID] && b.account != nil {
			if e := b.account.Logout(ctx); e != nil {
				b.fail(ctx, q.Message.Chat.ID, q.Message.MessageID, e)
			} else {
				b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "MTProto session удалена. Аккаунт отключён.", back("mtaccount"))
			}
		}
	case "ag":
		if b.admins[q.From.ID] {
			b.showGroup(ctx, q, parts)
		}
	case "aadd":
		if b.admins[q.From.ID] {
			k := Markup{[][]Button{{cb("Da Nang", "aaddcity:"+domain.CityDaNang), cb("Nha Trang", "aaddcity:"+domain.CityNhaTrang)}, {cb("← Каналы", "agroups")}}}
			b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "Выберите город канала:", k)
		}
	case "aaddcity":
		if b.admins[q.From.ID] && len(parts) > 1 {
			b.setState(q.From.ID, "add_channel:"+parts[1])
			b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "Пришлите публичный канал или группу: <code>@username</code>, <code>t.me/username</code> или <code>https://t.me/s/username</code>.", back("agroups"))
		}
	case "async":
		if b.admins[q.From.ID] && len(parts) > 1 {
			if id, _ := strconv.ParseInt(parts[1], 10, 64); b.sync.Trigger(id) {
				b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "Синхронизация поставлена в очередь.", back("agroups"))
			}
		}
	case "areanalyze":
		if b.admins[q.From.ID] && len(parts) > 1 {
			id, _ := strconv.ParseInt(parts[1], 10, 64)
			if err := b.store.SetProfilePending(ctx, id); err != nil {
				b.fail(ctx, q.Message.Chat.ID, q.Message.MessageID, err)
			} else if b.sync.Trigger(id) {
				b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "♻️ Профиль сброшен. До 20 свежих непустых сообщений поставлены на один LLM-анализ; новый профиль будет применён к последним объявлениям.", back("ag:"+parts[1]))
			} else {
				b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "Профиль сброшен. Фоновый worker подхватит переанализ по расписанию.", back("ag:"+parts[1]))
			}
		}
	case "atoggle":
		if b.admins[q.From.ID] {
			b.toggleGroup(ctx, q, parts)
		}
	case "adel":
		if b.admins[q.From.ID] {
			b.deleteGroup(ctx, q, parts)
		}
	case "arename":
		if b.admins[q.From.ID] && len(parts) > 1 {
			b.setState(q.From.ID, "rename:"+parts[1])
			b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "Пришлите новое название.", back("ag:"+parts[1]))
		}
	case "apoll":
		if b.admins[q.From.ID] && len(parts) > 1 {
			b.setState(q.From.ID, "poll:"+parts[1])
			b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "Пришлите интервал, например <code>5m</code> или <code>1h</code>.", back("ag:"+parts[1]))
		}
	case "apollall":
		if b.admins[q.From.ID] {
			b.setState(q.From.ID, "poll_global")
			b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "Пришлите глобальный интервал, например <code>5m</code>.", back("agroups"))
		}
	case "acheck":
		if b.admins[q.From.ID] {
			b.checkGroup(ctx, q, parts)
		}
	}
}

func (b *Bot) showMenu(ctx context.Context, chat int64, msg int) {
	rows := [][]Button{{cb("🏠 Новые", "new"), cb("🔎 Поиск и фильтры", "search")}, {cb("🔥 Подборки", "collections"), cb("❤️ Избранное", "filter:favorites")}, {cb("📊 Рынок / статистика", "filter:market"), cb("⚙️ Настройки", "filter:settings")}}
	if b.admins[chat] {
		rows = append(rows, []Button{cb("🛠 Админка", "admin")})
	}
	b.editOrSend(ctx, chat, msg, "<b>Аренда в Da Nang и Nha Trang</b>\n\nСвежие объявления из публичных Telegram-каналов.", Markup{rows})
}
func mainKeyboard(admin bool) Markup {
	rows := [][]Button{{cb("🏠 Новые", "new"), cb("🔎 Поиск", "search")}, {cb("🔥 Подборки", "collections"), cb("❤️ Избранное", "filter:favorites")}}
	if admin {
		rows = append(rows, []Button{cb("🛠 Админка", "admin")})
	}
	return Markup{rows}
}
func (b *Bot) showFilters(ctx context.Context, q *CallbackQuery) {
	b.mu.Lock()
	f := b.filters[q.From.ID]
	b.mu.Unlock()
	text := "<b>🔎 Поиск и фильтры</b>\n\n" + filterSummary(f) + "\n\nМожно также просто написать: <code>2 спальни son tra до 6 млн</code>"
	k := Markup{[][]Button{{cb("Da Nang", "filter:city:da_nang"), cb("Nha Trang", "filter:city:nha_trang")}, {cb("Север", "filter:zone:north"), cb("Центр", "filter:zone:center"), cb("Юг", "filter:zone:south"), cb("Запад", "filter:zone:west")}, {cb("от 5 млн", "filter:min:5000000"), cb("до 7 млн", "filter:max:7000000"), cb("до 10 млн", "filter:max:10000000")}, {cb("Studio", "filter:beds:0"), cb("1 спальня", "filter:beds:1"), cb("2 спальни", "filter:beds:2")}, {cb("Oceanus", "filter:location:Oceanus"), cb("Sơn Trà", "filter:district:Son Tra")}, {cb("Квартира", "filter:type:apartment"), cb("Дом", "filter:type:house"), cb("Комната", "filter:type:room")}, {cb("Сначала выгодные", "filter:sort:score"), cb("Сначала новые", "filter:sort:new")}, {cb("✅ Показать", "find"), cb("♻️ Сбросить", "filter:reset")}, {cb("← Меню", "menu")}}}
	b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, text, k)
}
func (b *Bot) applyFilter(ctx context.Context, q *CallbackQuery, p []string) {
	if len(p) < 2 {
		return
	}
	if p[1] == "favorites" {
		b.runFavorites(ctx, q)
		return
	}
	if p[1] == "market" {
		k := Markup{[][]Button{{cb("Da Nang", "market:"+domain.CityDaNang), cb("Nha Trang", "market:"+domain.CityNhaTrang)}, {cb("Оба города", "market:all")}, {cb("← Меню", "menu")}}}
		b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "<b>📊 Рынок / статистика</b>\n\nВыберите город.", k)
		return
	}
	if p[1] == "settings" {
		b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "⚙️ Пользовательские фильтры сохраняются в текущей сессии. Постоянные подписки будут использовать ту же модель фильтров.", back("menu"))
		return
	}
	b.mu.Lock()
	f := b.filters[q.From.ID]
	if p[1] == "reset" {
		f = domain.SearchFilter{Limit: 20, Sort: "score"}
	} else if len(p) > 2 {
		switch p[1] {
		case "max":
			v, _ := strconv.ParseInt(p[2], 10, 64)
			f.RentMax = &v
		case "min":
			v, _ := strconv.ParseInt(p[2], 10, 64)
			f.RentMin = &v
		case "city":
			f.City = p[2]
		case "zone":
			f.Zone = p[2]
		case "location":
			f.Location = p[2]
		case "beds":
			v, _ := strconv.Atoi(p[2])
			f.Bedrooms = &v
		case "district":
			f.District = p[2]
		case "type":
			f.PropertyType = p[2]
		case "area":
			v, _ := strconv.ParseFloat(p[2], 64)
			f.AreaMin = &v
		case "furnished":
			f.Furnished = p[2]
		case "sort":
			f.Sort = p[2]
		case "beach":
			v := true
			f.NearBeach = &v
		case "foreign":
			v := true
			f.ForeignersAccepted = &v
		}
	}
	b.filters[q.From.ID] = f
	b.mu.Unlock()
	b.showFilters(ctx, q)
}

func (b *Bot) runSearch(ctx context.Context, chat int64, msg int, user int64, f domain.SearchFilter, title string) {
	if f.Limit < 1 {
		f.Limit = 50
	}
	f.Offset = 0
	page, err := b.store.Search(ctx, user, f)
	if err != nil {
		b.fail(ctx, chat, msg, err)
		return
	}
	b.cacheAndShow(ctx, chat, msg, user, page.Items, page.Total, title, nil, &f)
}
func (b *Bot) cacheAndShow(ctx context.Context, chat int64, msg int, user int64, items []domain.Listing, total int, title string, reasons map[int64]string, filter *domain.SearchFilter) {
	if len(items) == 0 {
		b.editOrSend(ctx, chat, msg, title+"\n\nНичего не найдено. Попробуйте ослабить фильтры.", back("menu"))
		return
	}
	token := shortToken()
	b.mu.Lock()
	b.pages[token] = pageCache{items: items, total: total, user: user, until: time.Now().Add(5 * time.Minute), title: title, reasons: reasons, filter: filter}
	for k, v := range b.pages {
		if time.Now().After(v.until) {
			delete(b.pages, k)
		}
	}
	b.mu.Unlock()
	b.renderCard(ctx, chat, msg, token, 0, false)
}
func (b *Bot) showPage(ctx context.Context, chat int64, msg int, user int64, currentPhoto bool, p []string) {
	if len(p) < 3 {
		return
	}
	idx, _ := strconv.Atoi(p[2])
	b.mu.Lock()
	c, ok := b.pages[p[1]]
	b.mu.Unlock()
	if !ok || c.user != user || time.Now().After(c.until) {
		b.editOrSend(ctx, chat, msg, "Результаты устарели — откройте поиск снова.", back("menu"))
		return
	}
	if idx < 0 {
		idx = 0
	}
	if idx >= len(c.items) && idx < c.total && c.filter != nil {
		nextFilter := *c.filter
		nextFilter.Offset = len(c.items)
		nextFilter.Limit = 50
		page, err := b.store.Search(ctx, user, nextFilter)
		if err != nil {
			b.fail(ctx, chat, msg, err)
			return
		}
		c.items = append(c.items, page.Items...)
		c.total = page.Total
		b.mu.Lock()
		b.pages[p[1]] = c
		b.mu.Unlock()
	}
	if idx >= len(c.items) {
		idx = len(c.items) - 1
	}
	b.renderCard(ctx, chat, msg, p[1], idx, currentPhoto)
}
func (b *Bot) renderCard(ctx context.Context, chat int64, msg int, token string, idx int, currentPhoto bool) {
	b.mu.Lock()
	c := b.pages[token]
	b.mu.Unlock()
	l := c.items[idx]
	head := c.title
	if reason := c.reasons[l.ID]; reason != "" {
		head += fmt.Sprintf("\n\n🔥 <b>#%d</b>\n<i>%s</i>", idx+1, html.EscapeString(reason))
	}
	rate := b.vndToRUB()
	text := head + "\n\n" + card(l, rate)
	prev := idx - 1
	if prev < 0 {
		prev = 0
	}
	next := idx + 1
	if next >= len(c.items) {
		if c.filter == nil || next >= c.total {
			next = len(c.items) - 1
		}
	}
	rows := [][]Button{{urlb("📨 Открыть объявление", l.OriginalURL)}}
	photos := photoURLs(l.MediaURLs)
	rows = append(rows, []Button{cb("❤️ Сохранить", fmt.Sprintf("save:%d", l.ID)), cb("🙈 Скрыть", fmt.Sprintf("hide:%d", l.ID)), cb("Подробнее", fmt.Sprintf("detail:%d", l.ID))}, []Button{cb("◀️", fmt.Sprintf("page:%s:%d", token, prev)), cb(fmt.Sprintf("%d/%d", idx+1, c.total), "noop"), cb("▶️", fmt.Sprintf("page:%s:%d", token, next))}, []Button{cb("← Меню", "menu")})
	k := Markup{rows}
	if len(l.PhotoData) > 0 {
		if msg > 0 {
			_ = b.api.Delete(ctx, chat, msg)
		}
		if _, e := b.api.SendPhoto(ctx, chat, l.PhotoData, text, k); e == nil {
			return
		} else if b.log != nil {
			b.log.Warn("stored MTProto photo unavailable; rendering text card", "listing_id", l.ID, "error", e)
		}
	}
	if len(photos) > 0 {
		photo, photoErr := b.api.DownloadPhoto(ctx, photos[0])
		if msg > 0 {
			_ = b.api.Delete(ctx, chat, msg)
		}
		if photoErr == nil {
			_, photoErr = b.api.SendPhoto(ctx, chat, photo, text, k)
		}
		if photoErr == nil {
			return
		} else if b.log != nil {
			b.log.Warn("photo card unavailable; rendering text card", "listing_id", l.ID, "error", photoErr)
		}
		_, err := b.api.Send(ctx, chat, text, k)
		if err != nil && b.log != nil {
			b.log.Warn("telegram text card fallback", "error", err)
		}
		return
	}
	if currentPhoto && msg > 0 {
		_ = b.api.Delete(ctx, chat, msg)
		_, err := b.api.Send(ctx, chat, text, k)
		if err != nil && b.log != nil {
			b.log.Warn("telegram text card", "error", err)
		}
		return
	}
	b.editOrSend(ctx, chat, msg, text, k)
}
func card(l domain.Listing, vndToRUB float64) string {
	var lines []string
	kind := map[string]string{"apartment": "Квартира", "house": "Дом", "room": "Комната", "studio": "Студия"}[l.PropertyType]
	var specs []string
	if kind != "" {
		specs = append(specs, kind)
	}
	if l.Bedrooms != nil && *l.Bedrooms > 0 {
		specs = append(specs, fmt.Sprintf("%d сп.", *l.Bedrooms))
	}
	if l.AreaM2 != nil {
		specs = append(specs, fmt.Sprintf("%.0f м²", *l.AreaM2))
	}
	if len(specs) > 0 {
		lines = append(lines, "🏠 <b>"+strings.Join(specs, " · ")+"</b>")
	}
	if l.City != "" || l.Zone != "" || l.District != "" || l.LocationOriginal != "" || l.Address != "" {
		locations := make([]string, 0, 5)
		city := map[string]string{domain.CityDaNang: "Da Nang", domain.CityNhaTrang: "Nha Trang"}[l.City]
		zone := map[string]string{domain.ZoneNorth: "Север", domain.ZoneCenter: "Центр", domain.ZoneSouth: "Юг", domain.ZoneWest: "Запад"}[l.Zone]
		district := domain.DistrictLabel(l.District)
		if district == "" {
			district = l.District
		}
		for _, value := range []string{city, zone, district, l.LocationOriginal, l.Address} {
			if value != "" {
				locations = append(locations, value)
			}
		}
		lines = append(lines, "📍 "+html.EscapeString(strings.Join(locations, " · ")))
	}
	if l.IsOceanus != nil && *l.IsOceanus {
		lines = append(lines, "🌊 Oceanus")
	} else if l.NearOceanus != nil && *l.NearOceanus {
		lines = append(lines, "🌊 Рядом с Oceanus")
	}
	if l.RentMin != nil {
		price := money(*l.RentMin)
		roubles := ""
		if l.RentMax != nil && *l.RentMax != *l.RentMin {
			price += "–" + money(*l.RentMax)
			if vndToRUB > 0 {
				roubles = money(int64(math.Round(float64(*l.RentMin)*vndToRUB))) + "–" + money(int64(math.Round(float64(*l.RentMax)*vndToRUB)))
			}
		} else if vndToRUB > 0 {
			roubles = money(int64(math.Round(float64(*l.RentMin) * vndToRUB)))
		}
		if roubles != "" {
			price += " ₫ (≈ " + roubles + " ₽)"
		} else {
			price += " ₫"
		}
		lines = append(lines, "\n💰 <b>"+price+" / мес.</b>")
	}
	if l.DepositAmount != nil {
		lines = append(lines, "🔐 Депозит: "+money(*l.DepositAmount)+" ₫")
	}
	if l.LeaseMonths != nil {
		lines = append(lines, fmt.Sprintf("📅 Договор от %d мес.", *l.LeaseMonths))
	}
	if l.Availability != "" {
		lines = append(lines, "✅ "+html.EscapeString(l.Availability))
	}
	if gov, ok := l.Utilities["government_rate"].(bool); ok && gov {
		lines = append(lines, "⚡ Электричество и вода: гос. тариф")
	}
	if l.Furnished != "" {
		lines = append(lines, "🪑 "+map[string]string{"full": "Полная мебель", "partial": "Частичная мебель", "basic": "Базовая мебель", "none": "Без мебели"}[l.Furnished])
	}
	lines = append(lines, fmt.Sprintf("\n⭐ Deal score: <b>%.0f/100</b>", l.DealScore), "🕒 "+ago(l.PublishedAt))
	return strings.Join(lines, "\n")
}
func (b *Bot) showDetails(ctx context.Context, q *CallbackQuery, p []string) {
	if len(p) < 2 {
		return
	}
	id, _ := strconv.ParseInt(p[1], 10, 64)
	l, e := b.store.Listing(ctx, id)
	if e != nil {
		b.fail(ctx, q.Message.Chat.ID, q.Message.MessageID, e)
		return
	}
	text := card(l, b.vndToRUB()) + "\n\n<b>Исходное объявление</b>\n" + html.EscapeString(truncate(l.OriginalText, 1800))
	k := Markup{[][]Button{{urlb("📨 Открыть объявление", l.OriginalURL)}, {cb("← Назад", "menu")}}}
	b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, text, k)
}

func (b *Bot) vndToRUB() float64 {
	if b.rates == nil {
		return 0
	}
	return b.rates.CachedVNDToRUB()
}

func photoURLs(items []string) []string {
	for _, raw := range items {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
			continue
		}
		return []string{raw}
	}
	return nil
}
func (b *Bot) listAction(ctx context.Context, q *CallbackQuery, save bool, p []string) {
	if len(p) < 2 {
		return
	}
	id, _ := strconv.ParseInt(p[1], 10, 64)
	var e error
	if save {
		e = b.store.Favorite(ctx, q.From.ID, id)
	} else {
		e = b.store.Hide(ctx, q.From.ID, id)
	}
	if e != nil {
		b.fail(ctx, q.Message.Chat.ID, q.Message.MessageID, e)
		return
	}
	_ = b.api.Answer(ctx, q.ID, map[bool]string{true: "Сохранено ❤️", false: "Скрыто"}[save])
}
func (b *Bot) runFavorites(ctx context.Context, q *CallbackQuery) {
	page, e := b.store.Favorites(ctx, q.From.ID, 50)
	if e != nil {
		b.fail(ctx, q.Message.Chat.ID, q.Message.MessageID, e)
		return
	}
	b.cacheAndShow(ctx, q.Message.Chat.ID, q.Message.MessageID, q.From.ID, page, len(page), "❤️ Избранное", nil, nil)
}
func (b *Bot) showMarket(ctx context.Context, q *CallbackQuery, city string) {
	if city == "all" {
		city = ""
	}
	m, e := b.store.MarketByCity(ctx, city)
	if e != nil {
		b.fail(ctx, q.Message.Chat.ID, q.Message.MessageID, e)
		return
	}
	text := fmt.Sprintf("<b>📊 Рынок за 30 дней</b>\n\nОбъявлений с ценой: %d\nМедианная аренда: <b>%s ₫</b>\nМедиана за м²: %s ₫", m.Listings30d, money(m.MedianRent), money(m.MedianPriceM2))
	if len(m.Districts) > 0 {
		text += "\n\n<b>По районам</b>"
		for _, d := range m.Districts {
			label := domain.DistrictLabel(d.District)
			if label == "" {
				label = d.District
			}
			text += fmt.Sprintf("\n%s · %d объявл. · %s ₫", html.EscapeString(label), d.Listings, money(d.MedianRent))
		}
	}
	if m.Listings30d < 20 {
		text += "\n\n<i>Выборка пока мала; статистика и deal score будут стабилизироваться по мере накопления данных.</i>"
	}
	b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, text, back("menu"))
}
func (b *Bot) showCollections(ctx context.Context, q *CallbackQuery) {
	k := collectionCityMarkup()
	b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "<b>🔥 Подборки</b>\n\nВыберите город. Объявления разных городов ранжируются независимо.", k)
}

func (b *Bot) showCollectionPeriods(ctx context.Context, q *CallbackQuery, p []string) {
	if len(p) < 2 || (p[1] != domain.CityDaNang && p[1] != domain.CityNhaTrang) {
		return
	}
	city := p[1]
	k := collectionPeriodMarkup(city)
	b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "<b>🔥 Подборки · "+cityLabel(city)+"</b>\n\nВыберите период:", k)
}
func (b *Bot) runCollection(ctx context.Context, q *CallbackQuery, p []string) {
	days := 7
	if len(p) < 3 || (p[1] != domain.CityDaNang && p[1] != domain.CityNhaTrang) {
		return
	}
	city := p[1]
	days, _ = strconv.Atoi(p[2])
	items, e := b.collections.Get(ctx, q.From.ID, city, days)
	if e != nil {
		if errors.Is(e, collections.ErrSnapshotNotReady) {
			b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "<b>🔥 "+cityLabel(city)+" · лучшие "+collections.Title(days)+"</b>\n\nПодборка готовится в фоне. Попробуйте открыть её ещё раз через минуту.", back("colcity:"+city))
			return
		}
		b.fail(ctx, q.Message.Chat.ID, q.Message.MessageID, e)
		return
	}
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	visibilityCtx, cancelVisibility := context.WithTimeout(ctx, 300*time.Millisecond)
	hidden, hiddenErr := b.store.HiddenListingIDs(visibilityCtx, q.From.ID, ids)
	cancelVisibility()
	if hiddenErr == nil {
		visible := items[:0]
		for _, item := range items {
			if !hidden[item.ID] {
				visible = append(visible, item)
			}
		}
		items = visible
	} else {
		b.log.Warn("filter collection snapshot visibility", "user_id", q.From.ID, "error", hiddenErr)
	}
	list := make([]domain.Listing, 0, len(items))
	reasons := map[int64]string{}
	for _, x := range items {
		list = append(list, x.Listing)
		reasons[x.ID] = x.Reason
	}
	if len(list) == 0 {
		b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "<b>🔥 "+cityLabel(city)+" · лучшие "+collections.Title(days)+"</b>\n\nПока нет вариантов, которые прошли строгую проверку данных и качества. Слабые или неполные объявления в подборку не добавлены.", back("colcity:"+city))
		return
	}
	b.cacheAndShow(ctx, q.Message.Chat.ID, q.Message.MessageID, q.From.ID, list, len(list), "🔥 "+cityLabel(city)+" · лучшие "+collections.Title(days), reasons, nil)
}

func cityLabel(city string) string {
	if city == domain.CityNhaTrang {
		return "Нячанг"
	}
	return "Дананг"
}

func collectionCityMarkup() Markup {
	return Markup{[][]Button{{cb("🇻🇳 Дананг", "colcity:"+domain.CityDaNang), cb("🇻🇳 Нячанг", "colcity:"+domain.CityNhaTrang)}, {cb("← Меню", "menu")}}}
}

func collectionPeriodMarkup(city string) Markup {
	return Markup{[][]Button{{cb("Сегодня", "col:"+city+":1"), cb("7 дней", "col:"+city+":7"), cb("30 дней", "col:"+city+":30")}, {cb("← Города", "collections")}}}
}

func (b *Bot) showAdmin(ctx context.Context, q *CallbackQuery) {
	k := Markup{[][]Button{{cb("Telegram Channels", "agroups")}, {cb("Telegram Account", "mtaccount")}, {cb("← Меню", "menu")}}}
	b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "<b>🛠 Админка</b>\n\nКаналы работают через web preview, публичные группы — через единый MTProto account.", k)
}
func (b *Bot) showTelegramAccount(ctx context.Context, q *CallbackQuery) {
	if b.account == nil {
		b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "MTProto account manager недоступен.", back("admin"))
		return
	}
	a, e := b.account.Account(ctx)
	if e != nil {
		b.fail(ctx, q.Message.Chat.ID, q.Message.MessageID, e)
		return
	}
	masked := "—"
	if len(a.APIHash) >= 8 {
		masked = a.APIHash[:4] + "…" + a.APIHash[len(a.APIHash)-4:]
	}
	phone := a.Phone
	if phone == "" {
		phone = "—"
	}
	text := fmt.Sprintf("<b>Telegram Account</b>\n\nСтатус: <b>%s</b>\nAPI ID: <code>%d</code>\nAPI Hash: <code>%s</code>\nТелефон: <code>%s</code>", html.EscapeString(a.Status), a.APIID, html.EscapeString(masked), html.EscapeString(phone))
	if a.LastError != "" {
		text += "\nОшибка: <code>" + html.EscapeString(truncate(a.LastError, 300)) + "</code>"
	}
	k := Markup{[][]Button{{cb("API ID", "mteditid"), cb("API Hash", "mtedithash")}, {cb("Войти / войти заново", "mtlogin")}, {cb("Logout", "mtlogout")}, {cb("← Админка", "admin")}}}
	b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, text, k)
}
func (b *Bot) showGroups(ctx context.Context, q *CallbackQuery) {
	groups, e := b.store.Channels(ctx)
	if e != nil {
		b.fail(ctx, q.Message.Chat.ID, q.Message.MessageID, e)
		return
	}
	rows := make([][]Button, 0, len(groups)+3)
	for _, g := range groups {
		icon := "✅"
		if !g.Enabled {
			icon = "⏸"
		} else if g.ConsecutiveErrors > 0 {
			icon = "⚠️"
		}
		rows = append(rows, []Button{cb(icon+" "+truncate(g.Name, 28), fmt.Sprintf("ag:%d", g.ID))})
	}
	rows = append(rows, []Button{cb("➕ Добавить", "aadd")}, []Button{cb("⏱ Общий интервал", "apollall")}, []Button{cb("← Админка", "admin")})
	b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, fmt.Sprintf("<b>Telegram Channels</b>\nВсего: %d", len(groups)), Markup{rows})
}
func (b *Bot) showGroup(ctx context.Context, q *CallbackQuery, p []string) {
	if len(p) < 2 {
		return
	}
	id, _ := strconv.ParseInt(p[1], 10, 64)
	g, e := b.store.Channel(ctx, id)
	if e != nil {
		b.fail(ctx, q.Message.Chat.ID, q.Message.MessageID, e)
		return
	}
	last := "ещё не было"
	if g.LastSuccessAt != nil {
		last = ago(*g.LastSuccessAt)
	}
	lastMessage := "—"
	if g.LastMessageAt != nil {
		lastMessage = ago(*g.LastMessageAt)
	}
	text := fmt.Sprintf("<b>%s</b>\n<code>@%s</code>\n\nТип: %s\nГород: %s\nСтатус: %s\nПрофиль: %s\nИнтервал: %s\nПоследний успех: %s\nПоследнее сообщение: %s (#%d)\nВсего импортировано: %d\nНовых за цикл: %d", html.EscapeString(g.Name), html.EscapeString(g.Username), g.SourceType, g.City, map[bool]string{true: "включён", false: "выключен"}[g.Enabled], g.ProfileStatus, g.PollingInterval, last, lastMessage, g.LastMessageID, g.PostsTotal, g.NewPostsLastRun)
	if g.LastError != "" {
		text += "\nОшибка: <code>" + html.EscapeString(truncate(g.LastError, 300)) + "</code>"
	}
	k := channelAdminMarkup(id, g.Enabled)
	b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, text, k)
}

func channelAdminMarkup(id int64, enabled bool) Markup {
	return Markup{[][]Button{{cb("🔄 Синхронизировать", fmt.Sprintf("async:%d", id)), cb("🔌 Проверить", fmt.Sprintf("acheck:%d", id))}, {cb("♻️ Переанализировать структуру", fmt.Sprintf("areanalyze:%d", id))}, {cb("✏️ Имя", fmt.Sprintf("arename:%d", id)), cb("⏱ Интервал", fmt.Sprintf("apoll:%d", id))}, {cb(map[bool]string{true: "⏸ Выключить", false: "▶️ Включить"}[enabled], fmt.Sprintf("atoggle:%d", id))}, {cb("🗑 Удалить", fmt.Sprintf("adel:%d:confirm", id))}, {cb("← Каналы", "agroups")}}}
}
func (b *Bot) toggleGroup(ctx context.Context, q *CallbackQuery, p []string) {
	if len(p) < 2 {
		return
	}
	id, _ := strconv.ParseInt(p[1], 10, 64)
	g, e := b.store.Channel(ctx, id)
	if e == nil {
		e = b.store.UpdateChannel(ctx, id, g.Name, g.City, !g.Enabled, g.PollingInterval)
	}
	if e != nil {
		b.fail(ctx, q.Message.Chat.ID, q.Message.MessageID, e)
		return
	}
	b.showGroup(ctx, q, []string{"ag", p[1]})
}
func (b *Bot) deleteGroup(ctx context.Context, q *CallbackQuery, p []string) {
	if len(p) < 3 {
		return
	}
	id, _ := strconv.ParseInt(p[1], 10, 64)
	if p[2] == "confirm" {
		k := Markup{[][]Button{{cb("Да, удалить данные канала", fmt.Sprintf("adel:%d:yes", id))}, {cb("Отмена", fmt.Sprintf("ag:%d", id))}}}
		b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "Удалить канал и все его объявления? Действие необратимо.", k)
		return
	}
	if p[2] == "yes" {
		if e := b.store.DeleteChannel(ctx, id); e != nil {
			b.fail(ctx, q.Message.Chat.ID, q.Message.MessageID, e)
			return
		}
		b.showGroups(ctx, q)
	}
}
func (b *Bot) checkGroup(ctx context.Context, q *CallbackQuery, p []string) {
	if len(p) < 2 {
		return
	}
	id, _ := strconv.ParseInt(p[1], 10, 64)
	e := b.sync.Check(ctx, id)
	if e != nil {
		b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "❌ Проверка не пройдена: <code>"+html.EscapeString(truncate(e.Error(), 350))+"</code>", back("ag:"+p[1]))
		return
	}
	b.editOrSend(ctx, q.Message.Chat.ID, q.Message.MessageID, "✅ Публичный web preview канала доступен.", back("ag:"+p[1]))
}

func (b *Bot) handleState(ctx context.Context, m *Message, state string) {
	switch state {
	case "mt_api_id":
		if b.account == nil {
			return
		}
		a, e := b.account.Account(ctx)
		id, pErr := strconv.Atoi(strings.TrimSpace(m.Text))
		if e == nil && pErr == nil {
			e = b.account.Configure(ctx, id, a.APIHash, a.Phone)
		}
		if e != nil || pErr != nil {
			b.send(ctx, m.Chat.ID, "Некорректный API ID.", back("mtaccount"))
		} else {
			b.send(ctx, m.Chat.ID, "API ID сохранён; MTProto client перезапускается.", back("mtaccount"))
		}
		return
	case "mt_api_hash":
		_ = b.api.Delete(ctx, m.Chat.ID, m.MessageID)
		if b.account == nil {
			return
		}
		a, e := b.account.Account(ctx)
		if e == nil {
			e = b.account.Configure(ctx, a.APIID, strings.TrimSpace(m.Text), a.Phone)
		}
		if e != nil {
			b.send(ctx, m.Chat.ID, "Не удалось сохранить API Hash: "+html.EscapeString(e.Error()), back("mtaccount"))
		} else {
			b.send(ctx, m.Chat.ID, "API Hash сохранён; MTProto client перезапускается.", back("mtaccount"))
		}
		return
	case "mt_phone":
		if b.account == nil {
			return
		}
		e := b.account.RequestCode(ctx, m.Text)
		if e != nil {
			b.send(ctx, m.Chat.ID, "Не удалось отправить код: "+html.EscapeString(e.Error()), back("mtaccount"))
		} else {
			b.setState(m.From.ID, "mt_code")
			b.send(ctx, m.Chat.ID, "Код отправлен Telegram. Пришлите его следующим сообщением.", back("mtaccount"))
		}
		return
	case "mt_code":
		_ = b.api.Delete(ctx, m.Chat.ID, m.MessageID)
		if b.account == nil {
			return
		}
		needsPassword, e := b.account.SubmitCode(ctx, m.Text)
		if e != nil {
			b.send(ctx, m.Chat.ID, "Код не принят: "+html.EscapeString(e.Error()), back("mtaccount"))
		} else if needsPassword {
			b.setState(m.From.ID, "mt_password")
			b.send(ctx, m.Chat.ID, "Аккаунт защищён 2FA. Пришлите пароль; сообщение будет удалено.", back("mtaccount"))
		} else {
			b.send(ctx, m.Chat.ID, "✅ Telegram Account авторизован.", back("mtaccount"))
		}
		return
	case "mt_password":
		_ = b.api.Delete(ctx, m.Chat.ID, m.MessageID)
		if b.account == nil {
			return
		}
		if e := b.account.SubmitPassword(ctx, m.Text); e != nil {
			b.send(ctx, m.Chat.ID, "2FA пароль не принят: "+html.EscapeString(e.Error()), back("mtaccount"))
		} else {
			b.send(ctx, m.Chat.ID, "✅ Telegram Account авторизован.", back("mtaccount"))
		}
		return
	case "poll_global":
		d, e := time.ParseDuration(strings.TrimSpace(m.Text))
		if e == nil && d >= 30*time.Second {
			e = b.store.SetAllPolling(ctx, d)
		}
		if e != nil || d < 30*time.Second {
			b.send(ctx, m.Chat.ID, "Некорректный интервал (минимум 30s).", back("agroups"))
		} else {
			b.send(ctx, m.Chat.ID, "Общий интервал обновлён.", back("agroups"))
		}
	default:
		if strings.HasPrefix(state, "add_channel:") {
			city := strings.TrimPrefix(state, "add_channel:")
			username, e := telegramfeed.NormalizeUsername(m.Text)
			if e != nil {
				b.send(ctx, m.Chat.ID, "Некорректный Telegram-канал: "+html.EscapeString(e.Error()), back("agroups"))
				return
			}
			channel, e := b.store.AddChannel(ctx, username, "@"+username, city, b.defaultPoll)
			if e == nil && !b.sync.Trigger(channel.ID) {
				e = errors.New("очередь синхронизации заполнена")
			}
			if e != nil {
				b.send(ctx, m.Chat.ID, "Не удалось добавить канал: "+html.EscapeString(e.Error()), back("agroups"))
			} else {
				b.send(ctx, m.Chat.ID, "⏳ Источник @"+username+" добавлен. Определяю тип, получаю реальные сообщения и строю LLM-профиль в фоне…", back("ag:"+strconv.FormatInt(channel.ID, 10)))
			}
			return
		}
		if strings.HasPrefix(state, "rename:") {
			id, _ := strconv.ParseInt(strings.TrimPrefix(state, "rename:"), 10, 64)
			g, e := b.store.Channel(ctx, id)
			if e == nil {
				e = b.store.UpdateChannel(ctx, id, strings.TrimSpace(m.Text), g.City, g.Enabled, g.PollingInterval)
			}
			if e != nil {
				b.send(ctx, m.Chat.ID, "Не удалось переименовать.", back("agroups"))
			} else {
				b.send(ctx, m.Chat.ID, "Название обновлено.", back("ag:"+strconv.FormatInt(id, 10)))
			}
			return
		}
		if strings.HasPrefix(state, "poll:") {
			id, _ := strconv.ParseInt(strings.TrimPrefix(state, "poll:"), 10, 64)
			d, e := time.ParseDuration(strings.TrimSpace(m.Text))
			g, gErr := b.store.Channel(ctx, id)
			if e == nil && gErr == nil && d >= 30*time.Second {
				e = b.store.UpdateChannel(ctx, id, g.Name, g.City, g.Enabled, d)
			}
			if e != nil || gErr != nil || d < 30*time.Second {
				b.send(ctx, m.Chat.ID, "Некорректный интервал (минимум 30s).", back("ag:"+strconv.FormatInt(id, 10)))
			} else {
				b.send(ctx, m.Chat.ID, "Интервал обновлён.", back("ag:"+strconv.FormatInt(id, 10)))
			}
			return
		}
	}
}
func (b *Bot) setState(user int64, s string) { b.mu.Lock(); b.states[user] = s; b.mu.Unlock() }
func (b *Bot) editOrSend(ctx context.Context, chat int64, msg int, text string, k Markup) {
	var e error
	if msg > 0 {
		e = b.api.Edit(ctx, chat, msg, text, k)
	} else {
		_, e = b.api.Send(ctx, chat, text, k)
	}
	if e != nil && strings.Contains(e.Error(), "message is not modified") {
		return
	}
	if e != nil && msg > 0 && (strings.Contains(e.Error(), "there is no text") || strings.Contains(e.Error(), "message can't be edited")) {
		_ = b.api.Delete(ctx, chat, msg)
		_, e = b.api.Send(ctx, chat, text, k)
	}
	if e != nil {
		b.log.Warn("telegram render", "error", e)
	}
}
func (b *Bot) send(ctx context.Context, chat int64, text string, k Markup) {
	_, e := b.api.Send(ctx, chat, text, k)
	if e != nil {
		b.log.Warn("telegram send", "error", e)
	}
}
func (b *Bot) fail(ctx context.Context, chat int64, msg int, e error) {
	b.log.Error("telegram action", "error", e)
	b.editOrSend(ctx, chat, msg, "Не удалось выполнить действие. Попробуйте ещё раз чуть позже.", back("menu"))
}
func cb(text, data string) Button  { return Button{Text: text, CallbackData: data} }
func urlb(text, url string) Button { return Button{Text: text, URL: url} }
func back(data string) Markup      { return Markup{[][]Button{{cb("← Назад", data)}}} }
func shortToken() string           { b := make([]byte, 4); _, _ = rand.Read(b); return hex.EncodeToString(b) }
func money(v int64) string {
	s := strconv.FormatInt(v, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + " " + s[i:]
	}
	return s
}
func ago(t time.Time) string {
	d := time.Since(t)
	if d < time.Minute {
		return "только что"
	}
	if d < time.Hour {
		return fmt.Sprintf("%d мин. назад", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%d ч. назад", int(d.Hours()))
	}
	return fmt.Sprintf("%d дн. назад", int(d.Hours()/24))
}
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
func filterSummary(f domain.SearchFilter) string {
	var x []string
	if f.City != "" {
		x = append(x, map[string]string{domain.CityDaNang: "Da Nang", domain.CityNhaTrang: "Nha Trang"}[f.City])
	}
	if f.Zone != "" {
		x = append(x, map[string]string{domain.ZoneNorth: "Север", domain.ZoneCenter: "Центр", domain.ZoneSouth: "Юг", domain.ZoneWest: "Запад"}[f.Zone])
	}
	if f.RentMin != nil {
		x = append(x, "от "+money(*f.RentMin)+" ₫")
	}
	if f.RentMax != nil {
		x = append(x, "до "+money(*f.RentMax)+" ₫")
	}
	if f.Bedrooms != nil {
		if *f.Bedrooms == 0 {
			x = append(x, "studio")
		} else {
			x = append(x, fmt.Sprintf("%d сп.", *f.Bedrooms))
		}
	}
	if f.District != "" {
		label := domain.DistrictLabel(f.District)
		if label == "" {
			label = f.District
		}
		x = append(x, label)
	}
	if f.Location != "" {
		x = append(x, f.Location)
	}
	if f.PropertyType != "" {
		x = append(x, map[string]string{"apartment": "квартира", "house": "дом", "room": "комната"}[f.PropertyType])
	}
	if f.AreaMin != nil {
		x = append(x, fmt.Sprintf("от %.0f м²", *f.AreaMin))
	}
	if f.Furnished != "" {
		x = append(x, "с мебелью")
	}
	if f.NearBeach != nil && *f.NearBeach {
		x = append(x, "у моря")
	}
	if f.ForeignersAccepted != nil && *f.ForeignersAccepted {
		x = append(x, "для иностранцев")
	}
	if f.Sort == "new" {
		x = append(x, "сначала новые")
	}
	if len(x) == 0 {
		return "Фильтры не выбраны"
	}
	return "Выбрано: " + strings.Join(x, " · ")
}
