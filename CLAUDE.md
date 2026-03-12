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
# Flags: -addr :8080, -db botdata.db, -webhook URL, -tg-api URL, -demo
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

Monolithic Go app (all `package main`), 10 source files + 1 test file + 1 embedded SPA template:

- **main.go** — Entry point. Token is optional — if provided, registers CLI bot; otherwise uses bots from DB. Starts ProxyManager for all bots, BridgeManager for protocol bridges, launches HTTP server. Supports `-demo` flag for demo mode and `-tg-api` for custom Telegram API URL.
- **demo.go** — `DemoManager` for demo mode. Creates isolated per-browser-session environments: each session gets its own temp SQLite DB, `Store`, `ProxyManager`, `BridgeManager`, and `Server` with a dedicated HTTP mux. Login with `demo:demo`, no password change. Sessions expire after 30 minutes of inactivity. Reaper goroutine cleans up expired sessions. `beforeunload` beacon for early cleanup.
- **bot.go** — `Bot` struct wrapping `OvyFlash/telegram-bot-api`. All Telegram API calls (send, ban, pin, admin management). `processUpdate()` dispatches to message/chat/member handlers. `onMessageSent` callback notifies bridges of outgoing messages.
- **proxy.go** — `ProxyManager` manages ALL bots uniformly (no CLI vs web distinction). Runs independent `pollLoop` per bot with raw JSON `getUpdates`. Dual-mode per bot: forwards updates to backend URL (proxy) and/or processes them for chat tracking (management). `WebhookHandler()` for bots in webhook mode. Creates managed Bot instances automatically at Start(). Periodic backend health checks every 60s.
- **bridge.go** — `BridgeManager` for multi-protocol bridges. Core infrastructure: translates external messages into Telegram Update JSON format and injects them into `processUpdate()`. Intercepts outgoing bot messages via `onMessageSent` hook. Routes to protocol-specific handlers (Slack, webhook). Maintains chat/message mappings for threading. Synthetic chat/user IDs avoid collision with real Telegram IDs. Generic webhook: incoming/outgoing via callback URL.
- **slack.go** — Native Slack protocol bridge. Handles Slack Events API (URL verification, HMAC-SHA256 signature validation, event parsing). Resolves Slack user display names via `users.info` API. Sends outgoing messages via `chat.postMessage` with thread support. All Slack-specific types (`SlackConfig`, `SlackEventPayload`, `SlackMessageEvent`) and methods isolated here.
- **auth.go** — Authentication & authorization. `AuthUser` struct, bcrypt password hashing, session token generation (32 bytes hex). `authMiddleware` checks Bearer API key first, falls back to session cookies. `adminOnly` requires admin role. `checkBotAccess` verifies user has access to specific bot (admin=all, user=assigned only). API keys (bmx_ prefix) use SHA-256 hashing.
- **server.go** — HTTP server with `embed.FS` for SPA. REST API for all bot/chat/message/admin/bridge operations. Telegram API proxy at `/tgapi/` captures outgoing bot messages. Multi-bot: resolves bot instances via `getBotFromRequest()` / `resolveBot()`. Auth endpoints at `/api/auth/*` for login/logout/user management. Bridge endpoints at `/api/bridges/*` for CRUD and `/bridge/{id}/incoming` for external webhook.
- **llm.go** — `LLMRouter` for AI-based message routing via OpenAI-compatible API. `LLMConfig` (api_url, api_key, model, system_prompt, enabled). `RouteMessage()` builds context from all bots+descriptions+chats, calls LLM Chat Completions, parses JSON routing decision. Works with any OpenAI-compatible endpoint (OpenAI, Ollama, LM Studio, etc.).
- **store.go** — SQLite with WAL mode. All data models and DB operations. Auto-migrates schema on startup. Includes `llm_config` table, bot `description` column, auth tables (`auth_users`, `auth_sessions`, `user_bots`, `api_keys`), and bridge tables (`bridges`, `bridge_chat_mappings`, `bridge_msg_mappings`). Auto-creates default admin on first run.
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

**Authentication**: Dual auth: cookie-based sessions (30-day expiry, HttpOnly, SameSite=Strict) and Bearer API keys (`Authorization: Bearer bmx_...`). `authMiddleware` checks Bearer token first, falls back to cookie. Passwords hashed with bcrypt, API keys with SHA-256. Two roles: `admin` (full access to all bots and settings) and `user` (access only to assigned bots). Many-to-many user↔bot via `user_bots` junction table. API keys stored in `api_keys` table, bound to users (inherit role/permissions). Default admin auto-created with `must_change_password=true`. Auth endpoints at `/api/auth/*`. API key management at `/api/auth/api-keys/*` (admin only). No auth on `/tgapi/` (backends use it), `/` (SPA handles client-side), `/api/health`. Admin-only: bot CRUD, user management, API key management, routes, LLM config. Frontend hides admin controls for regular users and handles 401 → login redirect.

**Custom Telegram API**: Package-level `telegramAPIURL` var (default `https://api.telegram.org`), set via `-tg-api` flag or `TELEGRAM_API_URL` env var. All direct HTTP calls in `proxy.go`, `server.go` use it. `bot.go` uses `NewBotAPIWithAPIEndpoint()` from the library when custom URL is set.

**Demo mode** (`-demo` flag or `DEMO_MODE=true`): Per-session isolation via `DemoManager`. Each `demo:demo` login creates a temp SQLite DB, seeds 3 bots and a demo admin user, spins up its own `Store`+`ProxyManager`+`BridgeManager`+`Server` with a dedicated `http.ServeMux`. Forces `telegramAPIURL` to `https://telegram-bot-api.exe.xyz`. Sessions tracked by cookie, expire after 30 min inactivity. Reaper goroutine + `beforeunload` beacon for cleanup. Zero changes to existing handlers — isolation achieved by per-session Server instances.

**Media handling**: Messages store `media_type` and `file_id`. `bot.go:extractMedia()` detects photo/video/animation/sticker/voice/audio/document/video_note from Telegram updates. For stickers, uses `Thumbnail.FileID` (static preview) instead of main file (which may be TGS/WebM). `server.go:captureSentMessage` extracts media from API responses. `/api/media?file_id=&bot_id=` proxies file downloads from Telegram with automatic WebP→PNG conversion via `go-webp` for browser compatibility. Frontend renders images with lightbox overlay (click to zoom), video players, audio players inline. Messages support reply-to with `reply_to_message_id` in send API and visual reply badges in the UI.

## Dependencies

- `github.com/OvyFlash/telegram-bot-api` — Telegram Bot API (actively maintained fork)
- `modernc.org/sqlite` — SQLite driver (pure Go, no CGO)
- `github.com/skrashevich/go-webp` — Pure Go WebP codec for sticker conversion (WebP→PNG)
- `golang.org/x/crypto/bcrypt` — Password hashing for authentication

## Language

Frontend supports English and Russian via i18n system in `templates/index.html`. Translations are in the `i18n` object (keys `en`/`ru`). `t(key)` returns the current language string. `applyLang()` re-renders all static and dynamic content. Language preference stored in localStorage. Comments may be in English or Russian. README is English.

## Screenshots

Screenshots in `screenshots/` use redacted data (fake usernames/URLs). Regenerate with a puppeteer script if UI changes significantly. Use `evaluateOnNewDocument` to monkey-patch `fetch` for data redaction (DOM replacement doesn't work reliably with innerHTML-rendered content).
