# Telegram Rental Aggregator

Telegram-бот агрегирует аренду из публичных каналов через `https://t.me/s/<username>` и из публичных групп через отдельный лёгкий Python/Hydrogram sidecar. В Go-приложении MTProto-библиотеки нет. Web-preview каналов не зависит от MTProto account.

## Запуск

1. Скопируйте `.env.example` в `.env` и заполните `POSTGRES_PASSWORD`, `TELEGRAM_BOT_TOKEN`, `TELEGRAM_ADMIN_IDS`, `BASE_URL` и `FREE_LLM_API_KEY`.
2. Запустите:

```bash
docker compose up -d --build
```

Миграции выполняются ботом автоматически. Health checks: `/live` и `/ready`.

## Telegram Account и добавление источника

Для групп сначала откройте `🛠 Админка → Telegram Account`. По умолчанию используются API ID `2040` и API Hash Telegram Desktop; оба значения можно изменить. Укажите телефон, код Telegram и, если потребуется, пароль 2FA. MTProto session/auth key хранится в PostgreSQL volume, поэтому переживает restart, rebuild и deploy. Logout удаляет session.

Затем откройте `🛠 Админка → Telegram Channels → Добавить`, выберите город и отправьте канал или публичную группу в одном из форматов:

- `https://t.me/lowrentnt`
- `https://t.me/s/lowrentnt`
- `t.me/lowrentnt`
- `@lowrentnt`
- `lowrentnt`

Ссылка нормализуется до canonical username. Broadcast channel автоматически остаётся на существующем web-preview scraper, public group/supergroup использует общий long-lived Hydrogram client через внутренний HTTP API. В фоне бот:

1. получает до 20 последних непустых сообщений (минимум пять);
2. делает один LLM-вызов и сохраняет машиноисполняемый `ChannelParsingProfile`, включая channel-specific include/exclude правила `is_listing`;
3. сообщает администратору о готовом профиле;
4. импортирует последние 500 сообщений;
5. при следующих синхронизациях получает только новые сообщения.

LLM больше нигде не вызывается, кроме явной админской команды re-analyze. При sync каждое сообщение сначала проходит локальный `is_listing`; нерелевантное сохраняется как `ignored_non_listing`, а объявление разбирается только regex/mappings из сохранённого профиля. Глобального fallback нет. Если профиль не извлёк цену и ещё минимум два значения, оригинал сохраняется как `unparsed`. Оба статуса исключены из New/Search/Collections/Market.

## Архитектура

```text
Telegram web preview ─────────────┐
                                 ├→ local is_listing → per-source profile parser → PostgreSQL
Python Hydrogram sidecar ← HTTP ──┘
                                             ├→ city-aware deterministic ranking
                                             ├→ cached collection snapshots
                                             └→ Telegram UI
```

- `internal/telegramfeed` — URL normalization и HTML parsing публичного preview;
- `internal/mtproto` — маленький HTTP client к sidecar без MTProto-зависимостей;
- `mtproto-service` — один long-lived Hydrogram Client, авторизация, history и бинарная выдача первого фото;
- `internal/llm` — только создание профиля при добавлении канала;
- `internal/parser` — локальное исполнение сохранённого профиля;
- `internal/location` — data-driven normalization географии Nha Trang;
- `internal/ranking` — неизменённый Da Nang scoring и отдельный deterministic Nha Trang scoring;
- `internal/storage` — channels, posts, listings, favorites, hidden listings и statistics;
- `internal/collections` — immutable snapshots, обновляемые только в фоне.

Callback query подтверждается до обработки. Scraping, первичный LLM-анализ, импорт и reranking работают в фоновых goroutine и отдельном ограниченном DB pool.

## Данные

Source key — `(channel_username, message_id)`. Хранятся original URL/text, published time и только первая фотография. Карточка содержит кнопку `📨 Открыть объявление` на исходный Telegram-пост.

Nha Trang использует зоны `north / center / south / west`. Resolver различает Oceanus, непосредственное окружение Oceanus и прочий север. Aliases находятся в `internal/location/nha_trang.json` и расширяются без изменения business/ranking logic.

## Тесты

```bash
go test ./...
```

Покрыты Telegram preview parser, нормализация channel URL, создание LLM-профиля одним запросом, локальный profile parser без fallback, география и ranking Nha Trang, а также regression test Da Nang scoring.
