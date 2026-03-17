# botmux

Web-based command center for managing Telegram groups and channels via Bot API, with built-in reverse proxy for legacy webhook bots.

Give it a bot token — it discovers which chats the bot is in, whether it has admin privileges, and provides a full-featured web dashboard for monitoring, analytics, and administration. Manage multiple bots from a single instance.

## Screenshots

| Login | Dashboard |
|-------|-----------|
| ![Login](screenshots/01-login.png) | ![Dashboard](screenshots/02-dashboard.png) |

| Bot Detail | Messages |
|------------|----------|
| ![Bot Detail](screenshots/03-bot-detail.png) | ![Messages](screenshots/04-messages.png) |

| Analytics | Admins |
|-----------|--------|
| ![Analytics](screenshots/05-analytics.png) | ![Admins](screenshots/06-admins.png) |

| Users | Tags |
|-------|------|
| ![Users](screenshots/07-users.png) | ![Tags](screenshots/08-tags.png) |

| Audit Log | Add Bot |
|-----------|---------|
| ![Audit Log](screenshots/09-audit-log.png) | ![Add Bot](screenshots/10-add-bot.png) |

| User Management | API Keys |
|-----------------|----------|
| ![User Management](screenshots/11-user-management.png) | ![API Keys](screenshots/12-api-keys.png) |

## Features

### How It Works

**Important:** BotMux takes over long polling for every bot it manages. Only one client can poll a bot token at a time (Telegram limitation), so:

- If your bot currently **polls** (`getUpdates`) — you have two options:
  1. **Long Polling mode** (recommended, zero code changes) — enable "Long Poll" in bot settings, then point your backend's API base URL to BotMux (`http://botmux:8080/tgapi/`). Your bot keeps calling `getUpdates` as before, but now gets updates from BotMux instead of Telegram.
  2. **Push mode** — switch the backend to accept webhook-style HTTP POST requests. BotMux will poll Telegram and forward updates to your backend URL.
- If your bot already uses **webhooks** — BotMux will switch it to polling and proxy updates back to the webhook endpoint. No changes needed on the backend side.

This applies to **all modes** — monitoring, admin actions, and reverse proxy all require BotMux to own the polling loop.

**Capturing outgoing messages:** Telegram's `getUpdates` only returns incoming messages — it does not include messages sent by the bot itself. If your backend sends replies directly via `api.telegram.org`, BotMux won't see them. To fix this, point your backend at BotMux's built-in **API proxy** (`/tgapi/`) instead of `api.telegram.org`. The proxy forwards requests to Telegram transparently and captures outgoing messages into the database. See [Capturing Bot Replies](#capturing-bot-replies-api-proxy) for details.

### Multi-Bot Management
- Run multiple bots in a single instance
- Add bots via CLI flag or through the web UI — no functional difference
- Each bot can operate in **Management** mode (chat tracking, admin actions), **Proxy** mode (reverse proxy to legacy webhook bots), or both simultaneously
- Per-bot status monitoring, health checks, and configuration

### Reverse Proxy (Push mode)
- Poll Telegram for updates via long polling and forward them as webhook POST requests to legacy bot backends
- Supports `X-Telegram-Bot-Api-Secret-Token` header for backend authentication
- Proxies webhook-style responses back to Telegram API (if backend responds with JSON containing a `method` field)
- Periodic backend health monitoring (every 60s) with status visible in the dashboard
- Manual health check button in the web UI

### Long Polling (Pull mode)
- External bots/backends can pull raw Telegram updates directly from BotMux instead of Telegram
- **Telegram-compatible**: backends call `getUpdates` via the API proxy (`/tgapi/bot{TOKEN}/getUpdates`) — no code changes needed, just change the API base URL
- Response format identical to Telegram: `{"ok": true, "result": [...]}`
- Supports `offset`, `limit` (max 100), and `timeout` (max 60s) parameters — same semantics as Telegram's `getUpdates`
- Enable per bot via the "Long Poll" toggle in bot settings
- In-memory ring buffer (1000 updates per bot) with waiter notification for efficient long polling
- Multiple clients can poll the same bot simultaneously
- Works alongside push proxy — both can be active for the same bot
- Also available via authenticated API: `GET /api/updates/poll?bot_id=X` (Bearer API key or session cookie)

### Message Monitoring
- Real-time collection of all messages and channel posts the bot can see
- Full-text search across message history
- Paginated message feed with sender, timestamp, and content
- **Reply to messages** — click the reply button on any message to send a reply in-context
- Reply chain visualization — messages show "Replying to @user" badge with click-to-scroll
- Automatic chat/channel discovery — the bot detects groups it's added to
- Soft-deleted message tracking (visually marked in the UI)

### Analytics
- Total message count, daily activity, weekly active users
- Hourly activity distribution chart (7-day window)
- Top 10 contributors leaderboard

### Admin Actions
Requires the bot to have corresponding admin rights in the chat:
- **Send messages** — broadcast to any chat with HTML formatting (`<b>`, `<i>`, `<code>`, `<a>`)
- **Pin / unpin messages**
- **Delete messages**
- **Ban / unban users**

### Administrator Management
- View all chat administrators with their permission breakdown
- **Promote users** to admin with granular permission control:
  - Manage chat
  - Delete messages
  - Restrict members
  - Invite users
  - Pin messages
  - Change chat info
  - Promote other members
- **Edit permissions** of existing admins
- **Set custom titles** for admins (up to 16 characters)
- **Demote** administrators

### Audit Log
Every admin action performed through the web UI is recorded:
- Bans and unbans
- Message deletions and pins
- Admin promotions and demotions
- Title changes
- Tag assignments

The log shows the action type, actor, target, details, and timestamp.

### User Tags
Custom classification system for chat members:
- Assign arbitrary tags to users (VIP, Moderator, Spammer, Trusted, etc.)
- Color-coded tags for visual distinction
- Multiple tags per user
- Tags are per-chat — the same user can have different tags in different chats

### Inter-Bot Routing (Source-NAT)
- Route messages between bots based on configurable conditions:
  - **Text match** — regex pattern matching on message text (case-insensitive)
  - **User ID** — messages from a specific Telegram user
  - **Chat ID** — all messages from a specific chat/group
  - **LLM (AI)** — describe the routing rule in natural language; an LLM decides which bot should handle the message
- Three routing actions:
  - **Forward** — sends message text as a new message via the target bot
  - **Copy** — forwards the original message (preserving author attribution) via Telegram's `forwardMessage`
  - **Drop** — silently ignore the message
- **Source-NAT return path** — replies to routed messages are automatically sent back through the original source bot to the original chat, maintaining conversational context
- Bidirectional conversation tracking: each reply creates a new mapping, enabling ongoing cross-bot dialogs
- Route rules managed per-bot from the web UI with enable/disable toggle
- Loop protection: bot-originated messages are not reverse-routed

### Protocol Bridges
- Bridge external messaging protocols (Slack, Discord, Meshtastic, HTTP webhooks) to Telegram bots
- External messages are translated into Telegram Update format and injected into the bot's processing pipeline
- All existing features work automatically: routing rules, LLM routing, reverse routing (Source-NAT), message tracking
- **Bidirectional**: outgoing bot messages are sent back to the source protocol automatically
- Chat and message mappings maintained for threading and reply context
- Bridges are managed per-bot from the web UI (admin only)
- **Generic webhook bridge**: `POST /bridge/{id}/incoming` with `{chat_id, user_id, username, text, message_id}`. Outgoing via callback URL
- **Native Slack bridge**: full integration with Slack Events API and Web API (see setup guide below)
- Supports any protocol via the generic webhook bridge; native protocol support available for Slack

#### Slack Bridge Setup Guide

**1. Create a Slack App**

Go to [api.slack.com/apps](https://api.slack.com/apps) and click **Create New App** → **From scratch**.

**2. Configure Bot Token Scopes**

Navigate to **OAuth & Permissions** → **Scopes** → **Bot Token Scopes** and add:

| Scope | Purpose |
|-------|---------|
| `chat:write` | Send messages to channels |
| `users:read` | Resolve user display names |
| `channels:history` | Receive messages from public channels |
| `groups:history` | Receive messages from private channels |
| `im:history` | Receive direct messages |
| `mpim:history` | Receive group DMs |

**3. Install App to Workspace**

Click **Install to Workspace** and authorize. Copy the **Bot User OAuth Token** (`xoxb-...`).

**4. Get the Signing Secret**

Go to **Basic Information** → **App Credentials** → copy the **Signing Secret**.

**5. Create a Bridge in BotMux**

In the BotMux web UI:
1. Select a bot → **Bridges** panel → **Add Bridge**
2. Set **Protocol** to `Slack`
3. Set **Config** to:
   ```json
   {
     "bot_token": "xoxb-your-bot-token",
     "signing_secret": "your-signing-secret"
   }
   ```
4. **Callback URL** is not needed for Slack — outgoing messages are sent directly via Slack API
5. Enable and save

**6. Configure Slack Events API**

1. In Slack App settings, go to **Event Subscriptions** → toggle **Enable Events**
2. Set **Request URL** to: `https://your-botmux-host/bridge/{id}/incoming`
   - Replace `{id}` with the bridge ID shown in BotMux UI
   - BotMux will automatically respond to Slack's URL verification challenge
   - **HTTPS is required** — Slack does not accept HTTP endpoints
3. Under **Subscribe to bot events**, add:
   - `message.channels` — messages in public channels
   - `message.groups` — messages in private channels
   - `message.im` — direct messages
   - `message.mpim` — group direct messages
4. Save changes

**7. Invite the Bot to Channels**

In Slack, invite the bot to channels where you want to receive messages:
```
/invite @YourBotName
```

#### How it works

```
Slack channel                    BotMux                         Telegram
    │                              │                               │
    │  message event (POST)        │                               │
    ├─────────────────────────────►│  verify signature             │
    │                              │  parse event                  │
    │                              │  resolve username             │
    │                              │  inject as Telegram Update    │
    │                              ├──────────────────────────────►│
    │                              │                               │  bot processes
    │                              │◄──────────────────────────────┤  bot replies
    │  chat.postMessage            │                               │
    │◄─────────────────────────────┤  send via Slack API           │
    │                              │  (with thread support)        │
```

- **Incoming**: Slack sends message events → BotMux verifies HMAC-SHA256 signature → translates to Telegram Update format → bot processes as a normal Telegram message
- **Outgoing**: Bot sends a reply → BotMux calls Slack `chat.postMessage` API → message appears in the Slack channel (with thread support via `thread_ts`)
- **Threading**: Slack thread replies are mapped to Telegram reply-to; bot replies to threaded messages maintain the thread context
- **User names**: Display names are resolved via Slack `users.info` API (falls back to user ID)
- **Security**: Requests are verified using HMAC-SHA256 signature (`X-Slack-Signature` header) with replay attack protection (5-minute window). **Strongly recommended** to configure `signing_secret`

### LLM-Based Smart Routing
- Uses any **OpenAI-compatible API** (OpenAI, Ollama, LM Studio, etc.) for intelligent message routing
- The LLM receives the message text, sender info, and a list of all available bots with their descriptions and chats
- Each bot can have a **description** explaining what it does, helping the LLM make better routing decisions
- Configurable via the web UI: API URL, API key, model name, custom system prompt
- Works alongside rule-based routing — LLM routing runs after traditional condition-based routes
- Reverse routing (Source-NAT) works automatically for LLM-routed messages

### Authentication & Authorization
- **Session-based authentication** with secure HTTP-only cookies (30-day sessions)
- **Role-based access control** — two roles: `admin` (full access) and `user` (assigned bots only)
- **Default admin account** — auto-created on first run (`admin` / `admin`) with mandatory password change
- **User management** — admin panel for adding, editing, and deleting users
- **Bot access control** — many-to-many: one bot can be assigned to multiple users, one user can access multiple bots
- **Admin delegation** — any admin can promote other users to admin role
- **Password management** — admins can reset user passwords; users can change their own
- **Granular UI enforcement** — non-admin users cannot see bot management, routing, or LLM configuration controls
- **API key authentication** — Bearer token in `Authorization` header as alternative to cookies for programmatic access
- **API key management** — admin can create, disable, and delete keys; keys are bound to users and inherit their permissions
- Password hashing with bcrypt; API keys hashed with SHA-256

### Internationalization (i18n)
- Interface available in **English** and **Russian**
- Language toggle button in the sidebar header (EN/RU)
- Preference saved in localStorage and applied instantly without page reload

### Media Support
- Inline rendering of photos, videos, animations (GIF), stickers, voice messages, and audio
- **Image lightbox** — click any photo to view full-size in an overlay (close with click or Escape)
- WebP sticker conversion — stickers are automatically converted from WebP to PNG via [go-webp](https://github.com/skrashevich/go-webp) for universal browser compatibility
- Video and audio players embedded directly in the message feed
- File downloads for documents and unsupported media types

### Chat Info
- Chat ID, type, title, username
- Member count
- Description
- Bot admin status
- Last refresh timestamp

## Requirements

- Go 1.21+
- A Telegram bot token (create via [@BotFather](https://t.me/BotFather))

## Installation

```bash
git clone https://github.com/skrashevich/botmux.git
cd botmux
go build -o botmux .
```

Or install directly:

```bash
go install github.com/skrashevich/botmux@latest
```

### Docker

```bash
# Run with docker compose (pulls from GitHub Container Registry)
TELEGRAM_BOT_TOKEN="YOUR_TOKEN" docker compose up -d
```

Or run the image directly:

```bash
docker run -d -p 8080:8080 -v botmux-data:/data \
  -e TELEGRAM_BOT_TOKEN="YOUR_TOKEN" ghcr.io/skrashevich/botmux:main
```

To build locally (multi-arch supported):

```bash
docker build -t botmux .
docker buildx build --platform linux/amd64,linux/arm64 -t botmux .
```

Data is stored in the `/data` volume inside the container.

## Usage

### Basic (polling mode, default)

```bash
./botmux -token "123456:ABC-DEF..."
```

The bot removes any existing webhook and starts long polling. Open http://localhost:8080 in your browser.

### With environment variable

```bash
export TELEGRAM_BOT_TOKEN="123456:ABC-DEF..."
./botmux
```

### Command-line flags

| Flag | Default | Description |
|------|---------|-------------|
| `-token` | `""` | Telegram bot token (or use `TELEGRAM_BOT_TOKEN` env var) |
| `-addr` | `:8080` | HTTP server listen address |
| `-db` | `botdata.db` | Path to SQLite database file |
| `-webhook` | `""` | Set webhook URL for receiving updates (instead of polling) |
| `-tg-api` | `""` | Custom Telegram API base URL (or `TELEGRAM_API_URL` env var) |
| `-demo` | `false` | Enable demo mode (or `DEMO_MODE=true` env var) |

### Webhook mode

```bash
./botmux -token "TOKEN" -webhook "https://myserver.com/tghook"
```

Registers a webhook with Telegram. Updates are delivered via `POST /tghook` to your server. Requires:
- A publicly accessible HTTPS URL
- Port 443, 80, 88, or 8443 (Telegram requirement)

The webhook endpoint is served on the same HTTP server as the web UI. If you need HTTPS, put a reverse proxy (nginx, caddy) in front.

**Note:** Webhook mode supports reverse proxy — updates received via webhook are also forwarded to the backend URL if proxy mode is enabled for the bot.

### Demo mode

Demo mode creates isolated per-session environments for public demonstrations. Each browser session gets its own temporary database with pre-configured bots.

```bash
# Via flag
./botmux -demo

# Via environment variable
DEMO_MODE=true ./botmux

# Docker
docker run -e DEMO_MODE=true -p 8080:8080 botmux
```

- Login: `demo` / `demo` (no password change required)
- Each session gets 3 pre-configured demo bots
- Uses a fake Telegram API (`telegram-bot-api.exe.xyz`) — no real bot tokens needed
- Sessions expire after 30 minutes of inactivity
- Temporary databases are cleaned up automatically on browser close or expiration

### Custom Telegram API server

Use `-tg-api` flag or `TELEGRAM_API_URL` env var to point botmux at a custom Telegram Bot API server (e.g., [tdlib/telegram-bot-api](https://github.com/tdlib/telegram-bot-api)):

```bash
./botmux -token "TOKEN" -tg-api "http://localhost:8081"
```

## Adding Bots

### Via CLI

The bot token passed via `-token` flag or `TELEGRAM_BOT_TOKEN` env var is automatically registered as a bot. It can be configured (management, proxy, backend URL) through the web UI just like any other bot.

### Via Web UI

Click **+ ADD** in the sidebar to add a new bot:
1. Enter the bot token and click **CHECK** to validate it
2. Give it a name
3. Enable **Management** (chat tracking, admin actions) and/or **Proxy** (reverse proxy to backend)
4. If proxy is enabled, enter the backend webhook URL and optional secret token
5. Click **SAVE**

The bot starts polling immediately. No restart needed.

## Reverse Proxy Setup

To use botmux as a reverse proxy for a legacy webhook bot:

1. Add or edit the bot in the web UI
2. Enable **Proxy** mode
3. Set **Backend URL** to the legacy bot's webhook endpoint (e.g., `https://legacy-bot.example.com/webhook`)
4. Optionally set **Secret Token** (sent as `X-Telegram-Bot-Api-Secret-Token` header)
5. Save — botmux will poll Telegram for updates and forward them as POST requests to the backend

The backend can respond with a [webhook-style reply](https://core.telegram.org/bots/api#making-requests-when-getting-updates) — JSON with a `method` field — and botmux will proxy it back to the Telegram API.

Use **CHECK WEBHOOK** button in the bot detail view to verify the backend is reachable. Health is also monitored automatically every 60 seconds.

## Long Polling Setup

To use BotMux as a `getUpdates` source for an existing polling bot:

1. Add or edit the bot in the web UI
2. Enable the **Long Poll** toggle in bot settings
3. Save — BotMux will start buffering raw Telegram updates for this bot
4. In your backend, change the Telegram API base URL:

```
# Before (direct to Telegram)
https://api.telegram.org/bot{TOKEN}/getUpdates

# After (via BotMux)
http://localhost:8080/tgapi/bot{TOKEN}/getUpdates
```

That's it — no other code changes needed. Your backend calls `getUpdates` with the same parameters (`offset`, `limit`, `timeout`) and gets the same response format. BotMux acts as a transparent intermediary.

**How it works:**

```
Telegram ──getUpdates──> BotMux (polls Telegram)
                            │
                            ├── saves to UpdateQueue (ring buffer, 1000 updates)
                            ├── Management: tracks chats/messages in DB
                            │
Backend ──getUpdates──> /tgapi/ (long poll) ──> returns from UpdateQueue
Backend ──sendMessage──> /tgapi/ (proxy) ──> Telegram API
```

**Notes:**
- No additional authentication required — the bot token in the URL is the authorization (same as Telegram API)
- Multiple backends can poll the same bot simultaneously
- Push proxy and long polling can be active at the same time for the same bot
- If your backend also sends messages, point those at `/tgapi/` too — see [Capturing Bot Replies](#capturing-bot-replies-api-proxy)
- The authenticated endpoint `GET /api/updates/poll?bot_id=X&timeout=T` is also available for programmatic access from the web UI or scripts

### Capturing Bot Replies (API Proxy)

By default, messages sent by the backend directly via the Telegram API (e.g., `sendMessage`) are not visible to botmux — Telegram does not include a bot's own outgoing messages in `getUpdates`.

To capture these messages, botmux provides a **Telegram API proxy** at `/tgapi/`. Configure your backend to use it as the API base URL instead of `api.telegram.org`:

```
# Before (direct)
https://api.telegram.org/bot{TOKEN}/sendMessage

# After (via botmux proxy)
http://localhost:8080/tgapi/bot{TOKEN}/sendMessage
```

The proxy transparently forwards all requests to Telegram and returns the responses unchanged. Additionally, for message-sending methods (`sendMessage`, `sendPhoto`, `sendDocument`, `forwardMessage`, `editMessageText`, etc.), it extracts the sent message from the Telegram response and saves it to the database, so it appears in the web UI message feed.

The **API Proxy URL** is displayed in the bot detail view when proxy mode is enabled (click to copy).

**Supported methods for message capture:** `sendMessage`, `sendPhoto`, `sendAudio`, `sendDocument`, `sendVideo`, `sendAnimation`, `sendVoice`, `sendVideoNote`, `sendSticker`, `sendLocation`, `sendVenue`, `sendContact`, `sendPoll`, `sendDice`, `forwardMessage`, `copyMessage`, `editMessageText`.

## Architecture

```
botmux/
├── main.go         Entry point, flag parsing, bot registration
├── bot.go          Telegram Bot API wrapper (all bot methods)
├── proxy.go        ProxyManager: polling, forwarding, health checks for all bots
├── server.go       HTTP server, REST API endpoints
├── store.go        SQLite storage (bots, chats, messages, admin log, user tags)
└── templates/
    └── index.html  Single-page web application (embedded at compile time)
```

### Data flow

```
Telegram ──getUpdates──> ProxyManager (polling loop per bot)
                            │
                            ├── Management: trackChat() / saveMessage() ──> SQLite
                            ├── Proxy (push): POST update ──> Backend URL
                            │                    │
                            │                    └── webhook reply ──> Telegram API
                            │
                            ├── Long Poll: enqueue ──> UpdateQueue (ring buffer)
                            │                              │
                            │            Backend ──getUpdates──> /tgapi/ ──> dequeue
                            │
                            ├── Routing: match rules ──> send via Target Bot ──> Telegram
                            │                │
                            │                └── save mapping ──> route_mappings (SQLite)
                            │
                            └── Reverse Route: check mappings ──> reply via Source Bot ──> Telegram
                                                                       (Source-NAT return)

Backend ──sendMessage──> /tgapi/ (API proxy) ──> Telegram API
                            │
                            └── capture response ──> saveMessage() ──> SQLite

Browser ──HTTP──> Server ──API──> Bot ──Bot API──> Telegram
                    │
                    └──queries──> SQLite (read)
```

### Storage

SQLite with WAL mode. Tables:

- **bots** — bot configurations (token, modes, backend URL, status, health)
- **chats** — tracked chats/channels with metadata (compound PK: bot_id + chat_id)
- **messages** — all observed messages (composite PK: chat_id + message_id)
- **known_users** — users seen in chats
- **admin_log** — audit trail of actions performed via web UI
- **user_tags** — custom per-chat user classifications
- **routes** — inter-bot routing rules (condition type/value, action, target bot/chat)
- **route_mappings** — Source-NAT tracking of source↔target message pairs for bidirectional routing

The database file is created automatically on first run. Schema migrations run automatically.

## REST API

All endpoints return JSON. Errors return `{"error": "message"}` with HTTP 500. Most endpoints require a `bot_id` query parameter.

### Bots

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/bots` | List all bots with status |
| POST | `/api/bots/add` | Add a new bot (JSON body) |
| POST | `/api/bots/update` | Update bot config (JSON body) |
| POST | `/api/bots/delete?id=` | Delete a bot |
| GET | `/api/bots/validate?token=` | Validate a bot token |
| GET | `/api/bots/health?id=` | Check backend health |

### Chats

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/chats?bot_id=` | List all tracked chats for a bot |
| GET | `/api/chats/refresh?bot_id=&chat_id=` | Refresh chat info from Telegram |
| POST | `/api/chats/delete?bot_id=&chat_id=` | Remove chat from tracking |

### Messages

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/messages?chat_id=&limit=&offset=` | Get messages (paginated) |
| GET | `/api/messages/search?chat_id=&q=` | Search messages |
| POST | `/api/messages/send` | Send message (JSON body: `{bot_id, chat_id, text, reply_to_message_id?}`) |
| POST | `/api/messages/pin?bot_id=&chat_id=&message_id=` | Pin a message |
| POST | `/api/messages/unpin?bot_id=&chat_id=&message_id=` | Unpin a message |
| POST | `/api/messages/delete?bot_id=&chat_id=&message_id=` | Delete a message |

### Users

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/users/list?chat_id=&q=&limit=&offset=` | List users in a chat |
| POST | `/api/users/ban?bot_id=&chat_id=&user_id=` | Ban user |
| POST | `/api/users/unban?bot_id=&chat_id=&user_id=` | Unban user |

### Admins

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/admins?bot_id=&chat_id=` | List administrators |
| POST | `/api/admins/promote` | Promote user (JSON body: `{bot_id, chat_id, user_id, perms}`) |
| POST | `/api/admins/demote?bot_id=&chat_id=&user_id=` | Demote admin |
| POST | `/api/admins/title` | Set admin title (JSON body: `{bot_id, chat_id, user_id, title}`) |

### Audit Log

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/adminlog?chat_id=&limit=&offset=` | Get admin action log |

### Tags

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/tags?chat_id=` | Get all tags for a chat |
| GET | `/api/tags/user?chat_id=&user_id=` | Get tags for a specific user |
| POST | `/api/tags/add` | Add tag (JSON body: `{bot_id, chat_id, user_id, username, tag, color}`) |
| POST | `/api/tags/remove?id=` | Remove tag by ID |

### Analytics

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/stats?chat_id=` | Chat statistics (messages, users, hourly, top contributors) |

### Routes

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/routes?bot_id=` | List routing rules for a bot |
| POST | `/api/routes/add` | Add route (JSON body: `{source_bot_id, target_bot_id, condition_type, condition_value, action, target_chat_id, enabled, description}`) |
| POST | `/api/routes/update` | Update route (JSON body with `id`) |
| POST | `/api/routes/delete?id=` | Delete route |

### Long Polling

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/updates/poll?bot_id=&offset=&limit=&timeout=` | Poll for updates (auth required) |

### Telegram API Proxy

| Method | Endpoint | Description |
|--------|----------|-------------|
| ANY | `/tgapi/bot{TOKEN}/{method}` | Proxies to `api.telegram.org`, captures sent messages |
| GET/POST | `/tgapi/bot{TOKEN}/getUpdates` | Returns updates from BotMux queue (if long poll enabled) |

## Bot Setup

1. Create a bot via [@BotFather](https://t.me/BotFather)
2. Disable [privacy mode](https://core.telegram.org/bots/features#privacy-mode) if you want the bot to see all group messages (BotFather → `/setprivacy` → Disable)
3. Add the bot to your group or channel
4. Make it an administrator (with the permissions you need)
5. Run botmux — the chat will appear in the sidebar automatically

### Required bot permissions

For full functionality, the bot should be an admin with:
- **Delete messages** — to delete messages from the UI
- **Ban users** — to ban/unban
- **Pin messages** — to pin/unpin
- **Invite users** — for invite link management
- **Add new admins** — to promote/demote other admins
- **Change group info** — to modify chat settings

The bot works with any subset of these permissions — features that require missing permissions will return errors when used.

## Resource Requirements

Botmux is a lightweight single-binary application with minimal resource needs.

| Resource | Minimum | Recommended | Notes |
|----------|---------|-------------|-------|
| **CPU** | 1 core | 1–2 cores | Each bot runs as a lightweight goroutine |
| **RAM** | 20–30 MB | 50–100 MB | Pure Go, no CGO. Grows with number of bots and message volume |
| **Disk** | ~16 MB (binary) | 100+ MB | SQLite DB grows with stored messages and chats |
| **Docker image** | ~25–30 MB | — | Alpine-based, stripped binary |

**Network:**
- Port 8080 (HTTP) for the web UI and REST API
- Outbound HTTPS to `api.telegram.org` (long polling per bot)
- Outbound to backend URL (if proxy mode is enabled)
- Outbound to LLM API endpoint (if LLM routing is enabled)

**Scaling reference:** A minimal VPS (1 vCPU, 512 MB RAM, 10 GB disk) comfortably handles dozens of bots and thousands of messages per day.

## Security Notes

- The web UI requires **authentication** — a default admin account (`admin` / `admin`) is created on first run with mandatory password change.
- For production use, place the server behind a reverse proxy with HTTPS (nginx, caddy, etc.).
- The SQLite database contains all collected messages. Protect the `botdata.db` file accordingly.
- Bot tokens are sensitive. Use environment variables or secure flag passing, not shell history.

## License

MIT
