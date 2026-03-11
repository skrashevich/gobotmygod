# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

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

No linter configured. Single `go build` produces the binary.

```bash
# Docker
TELEGRAM_BOT_TOKEN="..." docker compose up -d

# Multi-arch build
docker buildx build --platform linux/amd64,linux/arm64 -t botmux .
```

```bash
# Run tests
go test -v ./...
```

## Architecture

Monolithic Go app (all `package main`), 6 source files + 1 test file + 1 embedded SPA template:

- **main.go** — Entry point. Token is optional — if provided, registers CLI bot; otherwise uses bots from DB. Starts ProxyManager for all bots, launches HTTP server.
- **bot.go** — `Bot` struct wrapping `OvyFlash/telegram-bot-api`. All Telegram API calls (send, ban, pin, admin management). `processUpdate()` dispatches to message/chat/member handlers.
- **proxy.go** — `ProxyManager` manages ALL bots uniformly (no CLI vs web distinction). Runs independent `pollLoop` per bot with raw JSON `getUpdates`. Dual-mode per bot: forwards updates to backend URL (proxy) and/or processes them for chat tracking (management). `WebhookHandler()` for bots in webhook mode. Creates managed Bot instances automatically at Start(). Periodic backend health checks every 60s.
- **server.go** — HTTP server with `embed.FS` for SPA. REST API for all bot/chat/message/admin operations. Telegram API proxy at `/tgapi/` captures outgoing bot messages. Multi-bot: resolves bot instances via `getBotFromRequest()` / `resolveBot()`.
- **llm.go** — `LLMRouter` for AI-based message routing via OpenAI-compatible API. `LLMConfig` (api_url, api_key, model, system_prompt, enabled). `RouteMessage()` builds context from all bots+descriptions+chats, calls LLM Chat Completions, parses JSON routing decision. Works with any OpenAI-compatible endpoint (OpenAI, Ollama, LM Studio, etc.).
- **store.go** — SQLite with WAL mode. All data models and DB operations. Auto-migrates schema on startup. Includes `llm_config` table and bot `description` column.
- **server_capture_test.go** — Tests for `captureSentMessage` (copyMessage and sendMessage scenarios). Uses temp DB via `t.TempDir()`.
- **templates/index.html** — Complete SPA (vanilla JS, no framework). Dark/light theme (Sora + JetBrains Mono, auto-switches via `prefers-color-scheme`). i18n with EN/RU support via `i18n` object and `t(key)` function. Compiled into binary via `//go:embed`.

## Key Design Decisions

**No CLI vs web bot distinction**: All bots are functionally identical. The `source` field ('cli' or 'web') is informational only. Both types support management, proxy, and all configuration options. ProxyManager handles polling for all bots. Token is optional at startup.

**Multi-bot with unified table**: Single `bots` table with `manage_enabled` and `proxy_enabled` flags. Each bot can be proxy-only, management-only, or both.

**Chat isolation**: `chats` table uses compound PK `(bot_id, id)`. Messages and tags are keyed by `chat_id` (globally unique in Telegram).

**Bot resolution**: API endpoints accept `bot_id` param. Server checks registered bots map first, then ProxyManager's managed bots (`resolveBot` fallback chain).

**Telegram API proxy** (`/tgapi/bot{TOKEN}/{method}`): Reverse-proxies backend API calls to `api.telegram.org`. For send methods (`sendMessage`, `sendPhoto`, etc.), parses the Telegram response to extract the sent `Message` and saves it to DB. This captures outgoing bot messages that don't appear in `getUpdates`.

**Webhook mode**: Bots marked via `SetWebhookMode()` are not polled by ProxyManager but still show as running via `IsRunning()`. `WebhookHandler()` supports both management processing and proxy forwarding.

**LLM routing**: `ProxyManager` has `llmRouter *LLMRouter`. `applyLLMRoutes()` runs after rule-based `applyRoutes()` in `processUpdate()`. LLM receives message + all bot descriptions/chats and returns `{target_bot_id, target_chat_id, action, reason}`. Reverse routing works via existing `route_mappings` (RouteID=0 for LLM routes). Config managed via `/api/llm-config` and `/api/llm-config/save`. Bot descriptions via `/api/bots/description`.

**Media handling**: Messages store `media_type` and `file_id`. `bot.go:extractMedia()` detects photo/video/animation/sticker/voice/audio/document/video_note from Telegram updates. For stickers, uses `Thumbnail.FileID` (static preview) instead of main file (which may be TGS/WebM). `server.go:captureSentMessage` extracts media from API responses. `/api/media?file_id=&bot_id=` proxies file downloads from Telegram with automatic WebP→PNG conversion via `go-webp` for browser compatibility. Frontend renders images with lightbox overlay (click to zoom), video players, audio players inline. Messages support reply-to with `reply_to_message_id` in send API and visual reply badges in the UI.

## Dependencies

- `github.com/OvyFlash/telegram-bot-api` — Telegram Bot API (actively maintained fork)
- `modernc.org/sqlite` — SQLite driver (pure Go, no CGO)
- `github.com/skrashevich/go-webp` — Pure Go WebP codec for sticker conversion (WebP→PNG)

## Language

Frontend supports English and Russian via i18n system in `templates/index.html`. Translations are in the `i18n` object (keys `en`/`ru`). `t(key)` returns the current language string. `applyLang()` re-renders all static and dynamic content. Language preference stored in localStorage. Comments may be in English or Russian. README is English.

## Screenshots

Screenshots in `screenshots/` use redacted data (fake usernames/URLs). Regenerate with a puppeteer script if UI changes significantly. Use `evaluateOnNewDocument` to monkey-patch `fetch` for data redaction (DOM replacement doesn't work reliably with innerHTML-rendered content).
