# AGENTS.md

This file provides guidance to Codex (Codex.ai/code) when working with code in this repository.

## Build & Run

```bash
# Build (pure Go, no CGO required)
go build -o botmux .

# Run with token (registers CLI bot)
./botmux -token "BOT_TOKEN"

# Run without token (uses bots from database only)
./botmux

# Env var also works: TELEGRAM_BOT_TOKEN="..." ./botmux
# Flags: -addr :8080, -db botdata.db, -webhook URL
```

No tests, no linter configured. Single `go build` produces the binary.

## Architecture

Monolithic Go app (all `package main`), 5 source files + 1 embedded SPA template:

- **main.go** — Entry point. Token is optional — if provided, registers CLI bot; otherwise uses bots from DB. Starts ProxyManager for all bots, launches HTTP server.
- **bot.go** — `Bot` struct wrapping `OvyFlash/telegram-bot-api`. All Telegram API calls (send, ban, pin, admin management). `processUpdate()` dispatches to message/chat/member handlers.
- **proxy.go** — `ProxyManager` manages ALL bots uniformly (no CLI vs web distinction). Runs independent `pollLoop` per bot with raw JSON `getUpdates`. Dual-mode per bot: forwards updates to backend URL (proxy) and/or processes them for chat tracking (management). `WebhookHandler()` for bots in webhook mode. Creates managed Bot instances automatically at Start(). Periodic backend health checks every 60s.
- **server.go** — HTTP server with `embed.FS` for SPA. REST API for all bot/chat/message/admin operations. Telegram API proxy at `/tgapi/` captures outgoing bot messages. Multi-bot: resolves bot instances via `getBotFromRequest()` / `resolveBot()`.
- **store.go** — SQLite with WAL mode. All data models and DB operations. Auto-migrates schema on startup.
- **templates/index.html** — Complete SPA (vanilla JS, no framework). Dark/light theme (Sora + JetBrains Mono, auto-switches via `prefers-color-scheme`). i18n with EN/RU support via `i18n` object and `t(key)` function. Compiled into binary via `//go:embed`.

## Key Design Decisions

**No CLI vs web bot distinction**: All bots are functionally identical. The `source` field ('cli' or 'web') is informational only. Both types support management, proxy, and all configuration options. ProxyManager handles polling for all bots. Token is optional at startup.

**Multi-bot with unified table**: Single `bots` table with `manage_enabled` and `proxy_enabled` flags. Each bot can be proxy-only, management-only, or both.

**Chat isolation**: `chats` table uses compound PK `(bot_id, id)`. Messages and tags are keyed by `chat_id` (globally unique in Telegram).

**Bot resolution**: API endpoints accept `bot_id` param. Server checks registered bots map first, then ProxyManager's managed bots (`resolveBot` fallback chain).

**Telegram API proxy** (`/tgapi/bot{TOKEN}/{method}`): Reverse-proxies backend API calls to `api.telegram.org`. For send methods (`sendMessage`, `sendPhoto`, etc.), parses the Telegram response to extract the sent `Message` and saves it to DB. This captures outgoing bot messages that don't appear in `getUpdates`.

**Webhook mode**: Bots marked via `SetWebhookMode()` are not polled by ProxyManager but still show as running via `IsRunning()`. `WebhookHandler()` supports both management processing and proxy forwarding.

**Media handling**: Messages store `media_type` and `file_id`. `bot.go:extractMedia()` detects photo/video/animation/sticker/voice/audio/document/video_note from Telegram updates. `server.go:captureSentMessage` extracts media from API responses. `/api/media?file_id=&bot_id=` proxies file downloads from Telegram. Frontend renders images, video players, audio players inline.

## Dependencies

- `github.com/OvyFlash/telegram-bot-api` — Telegram Bot API (actively maintained fork)
- `modernc.org/sqlite` — SQLite driver (pure Go, no CGO)

## Language

Frontend supports English and Russian via i18n system in `templates/index.html`. Translations are in the `i18n` object (keys `en`/`ru`). `t(key)` returns the current language string. `applyLang()` re-renders all static and dynamic content. Language preference stored in localStorage. Comments may be in English or Russian. README is English.

## Screenshots

Screenshots in `screenshots/` use redacted data (fake usernames/URLs). Regenerate with a puppeteer script if UI changes significantly. Use `evaluateOnNewDocument` to monkey-patch `fetch` for data redaction (DOM replacement doesn't work reliably with innerHTML-rendered content).
