# gobotmygod

Web-based command center for managing Telegram groups and channels via Bot API.

Give it a bot token — it discovers which chats the bot is in, whether it has admin privileges, and provides a full-featured web dashboard for monitoring, analytics, and administration.

## Features

### Message Monitoring
- Real-time collection of all messages and channel posts the bot can see
- Full-text search across message history
- Paginated message feed with sender, timestamp, and content
- Automatic chat/channel discovery — the bot detects groups it's added to

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

### Chat Info
- Chat ID, type, title, username
- Member count
- Description
- Bot admin status
- Last refresh timestamp

## Requirements

- Go 1.21+
- CGO enabled (required by SQLite driver)
- A Telegram bot token (create via [@BotFather](https://t.me/BotFather))

## Installation

```bash
git clone <repo-url>
cd gobotmygod
go build -o gobotmygod .
```

Or install directly:

```bash
go install gobotmygod@latest
```

## Usage

### Basic (auto-detect mode)

```bash
./gobotmygod -token "123456:ABC-DEF..."
```

The bot checks if a webhook is already set:
- **No webhook** → starts long polling, receives all updates
- **Webhook exists** → runs in management-only mode (see below)

Then open http://localhost:8080 in your browser.

### With environment variable

```bash
export TELEGRAM_BOT_TOKEN="123456:ABC-DEF..."
./gobotmygod
```

### Command-line flags

| Flag | Default | Description |
|------|---------|-------------|
| `-token` | `""` | Telegram bot token (or use `TELEGRAM_BOT_TOKEN` env var) |
| `-addr` | `:8080` | HTTP server listen address |
| `-db` | `botdata.db` | Path to SQLite database file |
| `-webhook` | `""` | Set webhook URL for receiving updates |
| `-polling` | `false` | Force long polling mode (removes any existing webhook) |

## Update Reception Modes

The Telegram Bot API only allows one method of receiving updates at a time: either long polling (`getUpdates`) or a webhook. gobotmygod handles this automatically.

### Polling (default when no webhook exists)

```bash
./gobotmygod -token "TOKEN"
```

The bot calls `getUpdates` in a loop. Simple, works everywhere, no public URL needed. If another service had set a webhook on this bot, gobotmygod will **not** touch it and will fall back to management-only mode instead.

### Webhook

```bash
./gobotmygod -token "TOKEN" -webhook "https://myserver.com/tghook"
```

Registers a webhook with Telegram. Updates are delivered via `POST /tghook` to your server. Requires:
- A publicly accessible HTTPS URL
- Port 443, 80, 88, or 8443 (Telegram requirement)

The webhook endpoint is served on the same HTTP server as the web UI. If you need HTTPS, put a reverse proxy (nginx, caddy) in front.

### Management-only

```bash
# Triggered automatically when another webhook is detected
./gobotmygod -token "TOKEN"
# Output:
# Mode: management-only (existing webhook detected: https://other-service.com/bot)
# Updates will NOT be received — another service owns the webhook.
```

In this mode:
- The bot does **not** receive messages (no polling, no webhook changes)
- All management API calls work normally (send messages, ban users, manage admins, etc.)
- The web UI works fully for administration
- Message history and analytics only show data collected during previous polling/webhook sessions

This is the safe default when the bot token is shared with another service.

### Force polling

```bash
./gobotmygod -token "TOKEN" -polling
```

**Warning:** This removes any existing webhook. Use only when you're sure no other service needs it.

## Architecture

```
gobotmygod/
├── main.go         Entry point, flag parsing, mode selection
├── bot.go          Telegram Bot API wrapper (polling, webhook, all bot methods)
├── server.go       HTTP server, REST API endpoints
├── store.go        SQLite storage (chats, messages, admin log, user tags)
└── templates/
    └── index.html  Single-page web application (embedded at compile time)
```

### Data flow

```
Telegram ──updates──> Bot (polling or webhook)
                        │
                        ├── trackChat() ──> SQLite (chats table)
                        └── saveMessage() ──> SQLite (messages table)

Browser ──HTTP──> Server ──API──> Bot ──Bot API──> Telegram
                    │
                    └──queries──> SQLite (read)
```

### Storage

SQLite with WAL mode. Tables:

- **chats** — tracked chats/channels with metadata
- **messages** — all observed messages (composite PK: chat_id + message_id)
- **admin_log** — audit trail of actions performed via web UI
- **user_tags** — custom per-chat user classifications

The database file is created automatically on first run. To reset, delete `botdata.db`.

## REST API

All endpoints return JSON. Errors return `{"error": "message"}` with HTTP 500.

### Bot

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/bot` | Bot username |

### Chats

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/chats` | List all tracked chats |
| GET | `/api/chats/refresh?chat_id=` | Refresh chat info from Telegram |
| POST | `/api/chats/delete?chat_id=` | Remove chat from tracking |

### Messages

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/messages?chat_id=&limit=&offset=` | Get messages (paginated) |
| GET | `/api/messages/search?chat_id=&q=` | Search messages |
| POST | `/api/messages/send` | Send message (JSON body: `{chat_id, text}`) |
| POST | `/api/messages/pin?chat_id=&message_id=` | Pin a message |
| POST | `/api/messages/unpin?chat_id=&message_id=` | Unpin a message |
| POST | `/api/messages/delete?chat_id=&message_id=` | Delete a message |

### Users

| Method | Endpoint | Description |
|--------|----------|-------------|
| POST | `/api/users/ban?chat_id=&user_id=` | Ban user |
| POST | `/api/users/unban?chat_id=&user_id=` | Unban user |

### Admins

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/admins?chat_id=` | List administrators |
| POST | `/api/admins/promote` | Promote user (JSON body: `{chat_id, user_id, perms}`) |
| POST | `/api/admins/demote?chat_id=&user_id=` | Demote admin |
| POST | `/api/admins/title` | Set admin title (JSON body: `{chat_id, user_id, title}`) |

### Audit Log

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/adminlog?chat_id=&limit=&offset=` | Get admin action log |

### Tags

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/tags?chat_id=` | Get all tags for a chat |
| GET | `/api/tags/user?chat_id=&user_id=` | Get tags for a specific user |
| POST | `/api/tags/add` | Add tag (JSON body: `{chat_id, user_id, username, tag, color}`) |
| POST | `/api/tags/remove?id=` | Remove tag by ID |

### Analytics

| Method | Endpoint | Description |
|--------|----------|-------------|
| GET | `/api/stats?chat_id=` | Chat statistics (messages, users, hourly, top contributors) |

## Bot Setup

1. Create a bot via [@BotFather](https://t.me/BotFather)
2. Disable [privacy mode](https://core.telegram.org/bots/features#privacy-mode) if you want the bot to see all group messages (BotFather → `/setprivacy` → Disable)
3. Add the bot to your group or channel
4. Make it an administrator (with the permissions you need)
5. Run gobotmygod — the chat will appear in the sidebar automatically

### Required bot permissions

For full functionality, the bot should be an admin with:
- **Delete messages** — to delete messages from the UI
- **Ban users** — to ban/unban
- **Pin messages** — to pin/unpin
- **Invite users** — for invite link management
- **Add new admins** — to promote/demote other admins
- **Change group info** — to modify chat settings

The bot works with any subset of these permissions — features that require missing permissions will return errors when used.

## Security Notes

- The web UI has **no authentication**. Do not expose it to the public internet without adding auth (reverse proxy with basic auth, VPN, etc.)
- The SQLite database contains all collected messages. Protect the `botdata.db` file accordingly.
- Bot tokens are sensitive. Use environment variables or secure flag passing, not shell history.

## License

MIT
