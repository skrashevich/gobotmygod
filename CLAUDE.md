# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Run

```bash
# Build (requires CGO for SQLite)
CGO_ENABLED=1 go build -o gobotmygod .

# Run
./gobotmygod -token "BOT_TOKEN"
# or: TELEGRAM_BOT_TOKEN="..." ./gobotmygod

# Flags: -addr :8080, -db botdata.db, -webhook URL, -polling
```

No tests, no linter configured. Single `go build` produces the binary.

## Architecture

Monolithic Go app (all `package main`), 5 source files + 1 embedded SPA template:

- **main.go** — Entry point. Determines update mode (polling/webhook/management-only), creates CLI bot, starts ProxyManager for web-added bots, launches HTTP server.
- **bot.go** — `Bot` struct wrapping `go-telegram-bot-api/v5`. All Telegram API calls (send, ban, pin, admin management). Handles both polling and webhook update reception. `processUpdate()` dispatches to message/chat/member handlers.
- **proxy.go** — `ProxyManager` manages non-CLI bots. Runs independent `pollLoop` per bot with raw JSON `getUpdates`. Dual-mode per bot: forwards updates to backend URL (proxy) and/or processes them for chat tracking (management). Periodic backend health checks every 60s.
- **server.go** — HTTP server with `embed.FS` for SPA. REST API for all bot/chat/message/admin operations. Multi-bot: resolves bot instances via `getBotFromRequest()` / `resolveBot()` from registered bots or ProxyManager's managed bots.
- **store.go** — SQLite with WAL mode. All data models and DB operations. Auto-migrates schema on startup.
- **templates/index.html** — Complete SPA (vanilla JS, no framework). Cyberpunk theme with IBM Plex Mono. Compiled into binary via `//go:embed`.

## Key Design Decisions

**Multi-bot with unified table**: Single `bots` table with `manage_enabled` and `proxy_enabled` flags. Each bot can be proxy-only, management-only, or both. CLI bot has `source='cli'` and cannot be deleted via UI.

**Chat isolation**: `chats` table uses compound PK `(bot_id, id)`. Messages and tags are keyed by `chat_id` (globally unique in Telegram).

**Bot resolution**: API endpoints accept `bot_id` param. Server checks registered bots map first, then ProxyManager's managed bots (`resolveBot` fallback chain).

**CLI bot lifecycle**: Created with temporary `botID=0`, registered via `RegisterCLIBot()` to get real ID, then `MigrateLegacyChats()` moves old data.

## Dependencies

- `github.com/go-telegram-bot-api/telegram-bot-api/v5` — Telegram Bot API
- `github.com/mattn/go-sqlite3` — SQLite driver (CGO required)

## Language

Project uses Russian for UI labels, comments may be in English or Russian. README is English.
