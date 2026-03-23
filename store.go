package main

import (
	"database/sql"
	"log"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db     *sql.DB
	subsMu sync.RWMutex
	subs   map[chan Message]struct{}
}

// BotConfig represents a bot in the unified bots table
type BotConfig struct {
	ID               int64  `json:"id"`
	Name             string `json:"name"`
	Token            string `json:"token"`
	BotUsername       string `json:"bot_username"`
	ManageEnabled    bool   `json:"manage_enabled"`
	ProxyEnabled     bool   `json:"proxy_enabled"`
	BackendURL       string `json:"backend_url"`
	SecretToken      string `json:"secret_token"`
	PollingTimeout   int    `json:"polling_timeout"`
	Offset           int64  `json:"offset"`
	LastError        string `json:"last_error,omitempty"`
	LastActivity     string `json:"last_activity,omitempty"`
	UpdatesForwarded  int64  `json:"updates_forwarded"`
	Source            string `json:"source"` // "cli" or "web"
	BackendStatus     string `json:"backend_status"`
	BackendCheckedAt  string `json:"backend_checked_at"`
	LongPollEnabled   bool   `json:"long_poll_enabled"`
}

type Chat struct {
	ID          int64  `json:"id"`
	Type        string `json:"type"`
	Title       string `json:"title"`
	Username    string `json:"username"`
	MemberCount int    `json:"member_count"`
	Description string `json:"description"`
	IsAdmin     bool   `json:"is_admin"`
	UpdatedAt   string `json:"updated_at"`
	LastMsgText string `json:"last_msg_text,omitempty"`
	LastMsgFrom string `json:"last_msg_from,omitempty"`
	LastMsgDate int64  `json:"last_msg_date,omitempty"`
}

type Message struct {
	ID        int    `json:"id"`
	BotID     int64  `json:"bot_id"`
	ChatID    int64  `json:"chat_id"`
	FromUser  string `json:"from_user"`
	FromID    int64  `json:"from_id"`
	Text      string `json:"text"`
	Date      int64  `json:"date"`
	DateStr   string `json:"date_str"`
	ReplyToID int    `json:"reply_to_id,omitempty"`
	Deleted   bool   `json:"deleted"`
	MediaType string `json:"media_type,omitempty"` // photo, video, animation, sticker, voice, audio, document, video_note
	FileID    string `json:"file_id,omitempty"`
}

type ChatStats struct {
	ChatID        int64          `json:"chat_id"`
	Title         string         `json:"title"`
	TotalMessages int            `json:"total_messages"`
	TodayMessages int            `json:"today_messages"`
	ActiveUsers   int            `json:"active_users"`
	TopUsers      []UserActivity `json:"top_users"`
	HourlyStats   []HourlyStat  `json:"hourly_stats"`
}

type UserActivity struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	Count    int    `json:"count"`
}

type HourlyStat struct {
	Hour  int `json:"hour"`
	Count int `json:"count"`
}

type AdminLog struct {
	ID         int64  `json:"id"`
	ChatID     int64  `json:"chat_id"`
	Action     string `json:"action"`
	ActorName  string `json:"actor_name"`
	TargetID   int64  `json:"target_id,omitempty"`
	TargetName string `json:"target_name,omitempty"`
	Details    string `json:"details,omitempty"`
	CreatedAt  string `json:"created_at"`
}

type UserTag struct {
	ID       int64  `json:"id"`
	ChatID   int64  `json:"chat_id"`
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	Tag      string `json:"tag"`
	Color    string `json:"color"`
}

type ChatUser struct {
	UserID       int64     `json:"user_id"`
	Username     string    `json:"username"`
	MessageCount int       `json:"message_count"`
	LastSeen     string    `json:"last_seen"`
	Tags         []UserTag `json:"tags"`
}

// RouteMapping tracks source↔target message pairs for reverse routing (Source-NAT)
type RouteMapping struct {
	ID            int64 `json:"id"`
	RouteID       int64 `json:"route_id"`
	SourceBotID   int64 `json:"source_bot_id"`
	SourceChatID  int64 `json:"source_chat_id"`
	SourceMsgID   int   `json:"source_msg_id"`
	TargetBotID   int64 `json:"target_bot_id"`
	TargetChatID  int64 `json:"target_chat_id"`
	TargetMsgID   int   `json:"target_msg_id"`
	CreatedAt     string `json:"created_at"`
}

// Route defines a routing rule: updates matching conditions on source bot get forwarded to target bot
type Route struct {
	ID             int64  `json:"id"`
	SourceBotID    int64  `json:"source_bot_id"`
	TargetBotID    int64  `json:"target_bot_id"`
	SourceChatID   int64  `json:"source_chat_id"`   // filter by source chat (0 = any chat)
	ConditionType  string `json:"condition_type"`    // "text", "user_id", "chat_id"
	ConditionValue string `json:"condition_value"`   // regex pattern for text, ID for user/chat
	Action         string `json:"action"`            // "forward", "copy", or "drop" (ignore message)
	TargetChatID   int64  `json:"target_chat_id"`    // chat to forward/copy to (0 = same chat)
	Enabled        bool   `json:"enabled"`
	Description    string `json:"description"`
	CreatedAt      string `json:"created_at"`
}

type AdminInfo struct {
	UserID             int64  `json:"user_id"`
	Username           string `json:"username"`
	Status             string `json:"status"`
	CustomTitle        string `json:"custom_title"`
	CanDeleteMessages  bool   `json:"can_delete_messages"`
	CanRestrictMembers bool   `json:"can_restrict_members"`
	CanPromoteMembers  bool   `json:"can_promote_members"`
	CanChangeInfo      bool   `json:"can_change_info"`
	CanInviteUsers     bool   `json:"can_invite_users"`
	CanPinMessages     bool   `json:"can_pin_messages"`
	CanManageChat      bool   `json:"can_manage_chat"`
}

func NewStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, err
	}

	s := &Store{db: db, subs: make(map[chan Message]struct{})}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

// Subscribe returns a channel that receives all saved messages
func (s *Store) Subscribe() chan Message {
	ch := make(chan Message, 64)
	s.subsMu.Lock()
	s.subs[ch] = struct{}{}
	s.subsMu.Unlock()
	return ch
}

// Unsubscribe removes a subscriber channel
func (s *Store) Unsubscribe(ch chan Message) {
	s.subsMu.Lock()
	delete(s.subs, ch)
	s.subsMu.Unlock()
}

func (s *Store) notifySubscribers(m Message) {
	s.subsMu.RLock()
	defer s.subsMu.RUnlock()
	for ch := range s.subs {
		select {
		case ch <- m:
		default: // drop if subscriber is slow
		}
	}
}

func (s *Store) migrate() error {
	// Create bots table
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS bots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL DEFAULT '',
			token TEXT NOT NULL DEFAULT '',
			bot_username TEXT NOT NULL DEFAULT '',
			manage_enabled INTEGER NOT NULL DEFAULT 0,
			proxy_enabled INTEGER NOT NULL DEFAULT 0,
			backend_url TEXT NOT NULL DEFAULT '',
			secret_token TEXT NOT NULL DEFAULT '',
			polling_timeout INTEGER NOT NULL DEFAULT 30,
			offset_id INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			last_activity TEXT NOT NULL DEFAULT '',
			updates_forwarded INTEGER NOT NULL DEFAULT 0,
			source TEXT NOT NULL DEFAULT 'web'
		)
	`)
	if err != nil {
		return err
	}

	// Migrate proxy_bots -> bots if exists
	var hasProxyBots int
	s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='proxy_bots'`).Scan(&hasProxyBots)
	if hasProxyBots > 0 {
		s.db.Exec(`INSERT INTO bots (name, token, bot_username, proxy_enabled, backend_url, secret_token, polling_timeout, offset_id, last_error, last_activity, updates_forwarded, source)
			SELECT name, token, bot_username, enabled, backend_url, secret_token, polling_timeout, offset_id, last_error, last_activity, updates_forwarded, 'web' FROM proxy_bots`)
		s.db.Exec(`DROP TABLE proxy_bots`)
	}

	// Migrate chats table to include bot_id
	var hasBotID int
	s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('chats') WHERE name='bot_id'`).Scan(&hasBotID)
	if hasBotID == 0 {
		var hasOldChats int
		s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='chats'`).Scan(&hasOldChats)
		if hasOldChats > 0 {
			s.db.Exec(`ALTER TABLE chats RENAME TO _chats_old`)
		}
		s.db.Exec(`CREATE TABLE IF NOT EXISTS chats (
			bot_id INTEGER NOT NULL DEFAULT 0,
			id INTEGER NOT NULL,
			type TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL DEFAULT '',
			username TEXT NOT NULL DEFAULT '',
			member_count INTEGER NOT NULL DEFAULT 0,
			description TEXT NOT NULL DEFAULT '',
			is_admin INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (bot_id, id)
		)`)
		if hasOldChats > 0 {
			s.db.Exec(`INSERT INTO chats (bot_id, id, type, title, username, member_count, description, is_admin, updated_at)
				SELECT 0, id, type, title, username, member_count, description, is_admin, updated_at FROM _chats_old`)
			s.db.Exec(`DROP TABLE _chats_old`)
		}
	}

	// Create other tables
	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS messages (
			id INTEGER NOT NULL,
			bot_id INTEGER NOT NULL DEFAULT 0,
			chat_id INTEGER NOT NULL,
			from_user TEXT NOT NULL DEFAULT '',
			from_id INTEGER NOT NULL DEFAULT 0,
			text TEXT NOT NULL DEFAULT '',
			date INTEGER NOT NULL DEFAULT 0,
			reply_to_id INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (bot_id, chat_id, id)
		);
		CREATE INDEX IF NOT EXISTS idx_messages_date ON messages(date);
		CREATE INDEX IF NOT EXISTS idx_messages_from ON messages(chat_id, from_id);

		CREATE TABLE IF NOT EXISTS admin_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id INTEGER NOT NULL,
			action TEXT NOT NULL DEFAULT '',
			actor_name TEXT NOT NULL DEFAULT '',
			target_id INTEGER NOT NULL DEFAULT 0,
			target_name TEXT NOT NULL DEFAULT '',
			details TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_admin_log_chat ON admin_log(chat_id, id DESC);

		CREATE TABLE IF NOT EXISTS user_tags (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id INTEGER NOT NULL,
			user_id INTEGER NOT NULL,
			username TEXT NOT NULL DEFAULT '',
			tag TEXT NOT NULL DEFAULT '',
			color TEXT NOT NULL DEFAULT '#6c5ce7'
		);
		CREATE TABLE IF NOT EXISTS known_users (
			chat_id INTEGER NOT NULL,
			user_id INTEGER NOT NULL,
			username TEXT NOT NULL DEFAULT '',
			first_seen TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (chat_id, user_id)
		);

		CREATE UNIQUE INDEX IF NOT EXISTS idx_user_tags_unique ON user_tags(chat_id, user_id, tag);
		CREATE INDEX IF NOT EXISTS idx_user_tags_chat ON user_tags(chat_id);
	`)
	if err != nil {
		return err
	}

	// Add deleted column if missing
	var colCount int
	s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name='deleted'`).Scan(&colCount)
	if colCount == 0 {
		if _, err := s.db.Exec(`ALTER TABLE messages ADD COLUMN deleted INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}

	// Add media columns if missing
	var hasMediaType int
	s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name='media_type'`).Scan(&hasMediaType)
	if hasMediaType == 0 {
		s.db.Exec(`ALTER TABLE messages ADD COLUMN media_type TEXT NOT NULL DEFAULT ''`)
		s.db.Exec(`ALTER TABLE messages ADD COLUMN file_id TEXT NOT NULL DEFAULT ''`)
	}

	// Add backend health columns if missing
	var hasBackendStatus int
	s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('bots') WHERE name='backend_status'`).Scan(&hasBackendStatus)
	if hasBackendStatus == 0 {
		s.db.Exec(`ALTER TABLE bots ADD COLUMN backend_status TEXT NOT NULL DEFAULT ''`)
		s.db.Exec(`ALTER TABLE bots ADD COLUMN backend_checked_at TEXT NOT NULL DEFAULT ''`)
	}

	// Create routes table
	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS routes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			source_bot_id INTEGER NOT NULL,
			target_bot_id INTEGER NOT NULL,
			condition_type TEXT NOT NULL DEFAULT 'text',
			condition_value TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL DEFAULT 'forward',
			target_chat_id INTEGER NOT NULL DEFAULT 0,
			enabled INTEGER NOT NULL DEFAULT 1,
			description TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_routes_source ON routes(source_bot_id);

		CREATE TABLE IF NOT EXISTS route_mappings (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			route_id INTEGER NOT NULL,
			source_bot_id INTEGER NOT NULL,
			source_chat_id INTEGER NOT NULL,
			source_msg_id INTEGER NOT NULL,
			target_bot_id INTEGER NOT NULL,
			target_chat_id INTEGER NOT NULL,
			target_msg_id INTEGER NOT NULL,
			created_at TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_route_mappings_target ON route_mappings(target_bot_id, target_chat_id);
		CREATE INDEX IF NOT EXISTS idx_route_mappings_source ON route_mappings(source_bot_id, source_chat_id, source_msg_id);
	`)
	if err != nil {
		return err
	}

	// Migrate date column from seconds to milliseconds
	var maxDate int64
	s.db.QueryRow(`SELECT COALESCE(MAX(date), 0) FROM messages`).Scan(&maxDate)
	if maxDate > 0 && maxDate < 2000000000 { // seconds-range timestamps (before year 2033)
		s.db.Exec(`UPDATE messages SET date = date * 1000 WHERE date > 0 AND date < 2000000000`)
		log.Printf("[store] migrated message dates from seconds to milliseconds")
	}

	// Add source_chat_id column to routes if missing
	var hasSourceChatID int
	s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('routes') WHERE name='source_chat_id'`).Scan(&hasSourceChatID)
	if hasSourceChatID == 0 {
		s.db.Exec(`ALTER TABLE routes ADD COLUMN source_chat_id INTEGER NOT NULL DEFAULT 0`)
	}

	// Backfill known_users from messages
	s.db.Exec(`
		INSERT OR IGNORE INTO known_users (chat_id, user_id, username, first_seen)
		SELECT chat_id, from_id, from_user, datetime(MIN(date)/1000, 'unixepoch')
		FROM messages WHERE from_id != 0
		GROUP BY chat_id, from_id
	`)

	// Create llm_config table
	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS llm_config (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			api_url TEXT NOT NULL DEFAULT '',
			api_key TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT 'gpt-4o-mini',
			system_prompt TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 0
		)
	`)
	if err != nil {
		return err
	}

	// Add long_poll_enabled column to bots if missing
	var hasLongPoll int
	s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('bots') WHERE name='long_poll_enabled'`).Scan(&hasLongPoll)
	if hasLongPoll == 0 {
		s.db.Exec(`ALTER TABLE bots ADD COLUMN long_poll_enabled INTEGER NOT NULL DEFAULT 0`)
	}

	// Add description column to bots if missing
	var hasDescription int
	s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('bots') WHERE name='description'`).Scan(&hasDescription)
	if hasDescription == 0 {
		s.db.Exec(`ALTER TABLE bots ADD COLUMN description TEXT NOT NULL DEFAULT ''`)
	}

	// Add bot_id column to messages if missing (migrate PK to include bot_id)
	var hasMsgBotID int
	s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name='bot_id'`).Scan(&hasMsgBotID)
	if hasMsgBotID == 0 {
		log.Println("[store] migrating messages table: adding bot_id column...")
		s.db.Exec(`CREATE TABLE messages_new (
			id INTEGER NOT NULL,
			bot_id INTEGER NOT NULL DEFAULT 0,
			chat_id INTEGER NOT NULL,
			from_user TEXT NOT NULL DEFAULT '',
			from_id INTEGER NOT NULL DEFAULT 0,
			text TEXT NOT NULL DEFAULT '',
			date INTEGER NOT NULL DEFAULT 0,
			reply_to_id INTEGER NOT NULL DEFAULT 0,
			deleted INTEGER NOT NULL DEFAULT 0,
			media_type TEXT NOT NULL DEFAULT '',
			file_id TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (bot_id, chat_id, id)
		)`)
		// Copy data, deriving bot_id from chats table where possible
		s.db.Exec(`INSERT INTO messages_new (id, bot_id, chat_id, from_user, from_id, text, date, reply_to_id, deleted, media_type, file_id)
			SELECT m.id, COALESCE(c.bot_id, 0), m.chat_id, m.from_user, m.from_id, m.text, m.date, m.reply_to_id, m.deleted, m.media_type, m.file_id
			FROM messages m LEFT JOIN chats c ON m.chat_id = c.id`)
		s.db.Exec(`DROP TABLE messages`)
		s.db.Exec(`ALTER TABLE messages_new RENAME TO messages`)
		s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_date ON messages(date)`)
		s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_from ON messages(chat_id, from_id)`)
		log.Println("[store] messages table migration complete")
	}

	// Create auth tables
	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS auth_users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			display_name TEXT NOT NULL DEFAULT '',
			role TEXT NOT NULL DEFAULT 'user',
			must_change_password INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT '',
			last_login TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS auth_sessions (
			token TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL,
			created_at TEXT NOT NULL,
			expires_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_auth_sessions_user ON auth_sessions(user_id);
		CREATE TABLE IF NOT EXISTS user_bots (
			user_id INTEGER NOT NULL,
			bot_id INTEGER NOT NULL,
			PRIMARY KEY (user_id, bot_id)
		);
		CREATE TABLE IF NOT EXISTS api_keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			key_hash TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			last_used TEXT,
			enabled INTEGER NOT NULL DEFAULT 1
		);
		CREATE INDEX IF NOT EXISTS idx_api_keys_user ON api_keys(user_id)
	`)
	if err != nil {
		return err
	}

	// Create bridge tables
	_, err = s.db.Exec(`
		CREATE TABLE IF NOT EXISTS bridges (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			protocol TEXT NOT NULL DEFAULT 'webhook',
			linked_bot_id INTEGER NOT NULL,
			config TEXT NOT NULL DEFAULT '{}',
			callback_url TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL DEFAULT '',
			last_activity TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS bridge_chat_mappings (
			bridge_id INTEGER NOT NULL,
			external_chat_id TEXT NOT NULL,
			telegram_chat_id INTEGER NOT NULL,
			PRIMARY KEY (bridge_id, external_chat_id)
		);
		CREATE INDEX IF NOT EXISTS idx_bridge_chat_tg ON bridge_chat_mappings(bridge_id, telegram_chat_id);
		CREATE TABLE IF NOT EXISTS bridge_msg_mappings (
			bridge_id INTEGER NOT NULL,
			external_msg_id TEXT NOT NULL,
			telegram_msg_id INTEGER NOT NULL,
			telegram_chat_id INTEGER NOT NULL,
			PRIMARY KEY (bridge_id, external_msg_id)
		);
		CREATE INDEX IF NOT EXISTS idx_bridge_msg_tg ON bridge_msg_mappings(bridge_id, telegram_msg_id)
	`)
	if err != nil {
		return err
	}

	// Create default admin if no users exist
	var userCount int
	s.db.QueryRow(`SELECT COUNT(*) FROM auth_users`).Scan(&userCount)
	if userCount == 0 {
		hash, err := HashPassword("admin")
		if err != nil {
			return err
		}
		s.db.Exec(`INSERT INTO auth_users (username, password_hash, display_name, role, must_change_password, created_at)
			VALUES ('admin', ?, 'Administrator', 'admin', 1, ?)`,
			hash, time.Now().Format(time.RFC3339))
	}

	return nil
}

// Bot config methods

func (s *Store) RegisterCLIBot(token, username string) (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT id FROM bots WHERE token=?`, token).Scan(&id)
	if err == nil {
		s.db.Exec(`UPDATE bots SET bot_username=?, manage_enabled=1 WHERE id=?`, username, id)
		return id, nil
	}
	res, err := s.db.Exec(`INSERT INTO bots (name, token, bot_username, manage_enabled, source) VALUES (?, ?, ?, 1, 'cli')`,
		username, token, username)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) MigrateLegacyChats(botID int64) {
	s.db.Exec(`UPDATE chats SET bot_id=? WHERE bot_id=0`, botID)
}

func (s *Store) AddBotConfig(b BotConfig) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO bots (name, token, bot_username, manage_enabled, proxy_enabled, backend_url, secret_token, polling_timeout, long_poll_enabled, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'web')
	`, b.Name, b.Token, b.BotUsername, b.ManageEnabled, b.ProxyEnabled, b.BackendURL, b.SecretToken, b.PollingTimeout, b.LongPollEnabled)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateBotConfig(b BotConfig) error {
	_, err := s.db.Exec(`
		UPDATE bots SET name=?, token=?, bot_username=?, manage_enabled=?, proxy_enabled=?, backend_url=?, secret_token=?, polling_timeout=?, long_poll_enabled=?
		WHERE id=?
	`, b.Name, b.Token, b.BotUsername, b.ManageEnabled, b.ProxyEnabled, b.BackendURL, b.SecretToken, b.PollingTimeout, b.LongPollEnabled, b.ID)
	return err
}

func (s *Store) DeleteBotConfig(id int64) error {
	_, err := s.db.Exec(`DELETE FROM bots WHERE id=?`, id)
	return err
}

func (s *Store) GetBotConfigs() ([]BotConfig, error) {
	rows, err := s.db.Query(`SELECT id, name, token, bot_username, manage_enabled, proxy_enabled, backend_url, secret_token, polling_timeout, offset_id, last_error, last_activity, updates_forwarded, source, backend_status, backend_checked_at, long_poll_enabled FROM bots ORDER BY source DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bots []BotConfig
	for rows.Next() {
		var b BotConfig
		if err := rows.Scan(&b.ID, &b.Name, &b.Token, &b.BotUsername, &b.ManageEnabled, &b.ProxyEnabled, &b.BackendURL, &b.SecretToken, &b.PollingTimeout, &b.Offset, &b.LastError, &b.LastActivity, &b.UpdatesForwarded, &b.Source, &b.BackendStatus, &b.BackendCheckedAt, &b.LongPollEnabled); err != nil {
			return nil, err
		}
		bots = append(bots, b)
	}
	return bots, nil
}

func (s *Store) GetBotConfig(id int64) (*BotConfig, error) {
	var b BotConfig
	err := s.db.QueryRow(`SELECT id, name, token, bot_username, manage_enabled, proxy_enabled, backend_url, secret_token, polling_timeout, offset_id, last_error, last_activity, updates_forwarded, source, backend_status, backend_checked_at, long_poll_enabled FROM bots WHERE id=?`, id).
		Scan(&b.ID, &b.Name, &b.Token, &b.BotUsername, &b.ManageEnabled, &b.ProxyEnabled, &b.BackendURL, &b.SecretToken, &b.PollingTimeout, &b.Offset, &b.LastError, &b.LastActivity, &b.UpdatesForwarded, &b.Source, &b.BackendStatus, &b.BackendCheckedAt, &b.LongPollEnabled)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *Store) GetBotConfigByToken(token string) (*BotConfig, error) {
	var b BotConfig
	err := s.db.QueryRow(`SELECT id, name, token, bot_username, manage_enabled, proxy_enabled, backend_url, secret_token, polling_timeout, offset_id, last_error, last_activity, updates_forwarded, source, backend_status, backend_checked_at, long_poll_enabled FROM bots WHERE token=?`, token).
		Scan(&b.ID, &b.Name, &b.Token, &b.BotUsername, &b.ManageEnabled, &b.ProxyEnabled, &b.BackendURL, &b.SecretToken, &b.PollingTimeout, &b.Offset, &b.LastError, &b.LastActivity, &b.UpdatesForwarded, &b.Source, &b.BackendStatus, &b.BackendCheckedAt, &b.LongPollEnabled)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *Store) UpdateBotOffset(id int64, offset int64) {
	s.db.Exec(`UPDATE bots SET offset_id=? WHERE id=?`, offset, id)
}

func (s *Store) UpdateBotStatus(id int64, lastError string, lastActivity string) {
	if lastError != "" {
		s.db.Exec(`UPDATE bots SET last_error=? WHERE id=?`, lastError, id)
	}
	if lastActivity != "" {
		s.db.Exec(`UPDATE bots SET last_activity=?, last_error='' WHERE id=?`, lastActivity, id)
	}
}

func (s *Store) IncrementBotForwarded(id int64) {
	s.db.Exec(`UPDATE bots SET updates_forwarded = updates_forwarded + 1 WHERE id=?`, id)
}

func (s *Store) UpdateBackendHealth(id int64, status string, checkedAt string) {
	s.db.Exec(`UPDATE bots SET backend_status=?, backend_checked_at=? WHERE id=?`, status, checkedAt, id)
}

// Chat methods (now with botID)

func (s *Store) UpsertChat(botID int64, c Chat) error {
	_, err := s.db.Exec(`
		INSERT INTO chats (bot_id, id, type, title, username, member_count, description, is_admin, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(bot_id, id) DO UPDATE SET
			type=excluded.type, title=excluded.title, username=excluded.username,
			member_count=excluded.member_count, description=excluded.description,
			is_admin=excluded.is_admin, updated_at=excluded.updated_at
	`, botID, c.ID, c.Type, c.Title, c.Username, c.MemberCount, c.Description, c.IsAdmin, c.UpdatedAt)
	return err
}

func (s *Store) GetChats(botID int64) ([]Chat, error) {
	rows, err := s.db.Query(`
		SELECT c.id, c.type, c.title, c.username, c.member_count, c.description, c.is_admin, c.updated_at,
			COALESCE(m.text, ''), COALESCE(m.from_user, ''), COALESCE(m.date, 0)
		FROM chats c
		LEFT JOIN messages m ON m.chat_id = c.id AND m.id = (
			SELECT id FROM messages WHERE chat_id = c.id ORDER BY id DESC LIMIT 1
		)
		WHERE c.bot_id=?
		ORDER BY CASE WHEN m.date IS NOT NULL THEN m.date ELSE 0 END DESC, c.title
	`, botID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var c Chat
		if err := rows.Scan(&c.ID, &c.Type, &c.Title, &c.Username, &c.MemberCount, &c.Description, &c.IsAdmin, &c.UpdatedAt,
			&c.LastMsgText, &c.LastMsgFrom, &c.LastMsgDate); err != nil {
			return nil, err
		}
		chats = append(chats, c)
	}
	return chats, nil
}

func (s *Store) DeleteChat(botID int64, chatID int64) error {
	_, err := s.db.Exec(`DELETE FROM chats WHERE bot_id=? AND id=?`, botID, chatID)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM messages WHERE chat_id = ?`, chatID)
	return err
}

// Message methods (unchanged - keyed by chat_id which is globally unique)

func (s *Store) SaveMessage(m Message) error {
	res, err := s.db.Exec(`
		INSERT OR IGNORE INTO messages (id, bot_id, chat_id, from_user, from_id, text, date, reply_to_id, media_type, file_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, m.ID, m.BotID, m.ChatID, m.FromUser, m.FromID, m.Text, m.Date, m.ReplyToID, m.MediaType, m.FileID)
	if err == nil {
		if rows, _ := res.RowsAffected(); rows > 0 {
			m.DateStr = time.UnixMilli(m.Date).Format("2006-01-02 15:04:05")
			s.notifySubscribers(m)
		}
	}
	return err
}

func (s *Store) GetMessages(botID, chatID int64, limit, offset int) ([]Message, error) {
	rows, err := s.db.Query(`
		SELECT id, bot_id, chat_id, from_user, from_id, text, date, reply_to_id, deleted, media_type, file_id
		FROM messages WHERE bot_id = ? AND chat_id = ? ORDER BY id DESC LIMIT ? OFFSET ?
	`, botID, chatID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.BotID, &m.ChatID, &m.FromUser, &m.FromID, &m.Text, &m.Date, &m.ReplyToID, &m.Deleted, &m.MediaType, &m.FileID); err != nil {
			return nil, err
		}
		m.DateStr = time.UnixMilli(m.Date).Format("2006-01-02 15:04:05")
		msgs = append(msgs, m)
	}
	return msgs, nil
}

func (s *Store) GetMessage(botID, chatID int64, messageID int) (*Message, error) {
	var m Message
	err := s.db.QueryRow(`
		SELECT id, bot_id, chat_id, from_user, from_id, text, date, reply_to_id, deleted, media_type, file_id
		FROM messages WHERE bot_id = ? AND chat_id = ? AND id = ?
	`, botID, chatID, messageID).Scan(&m.ID, &m.BotID, &m.ChatID, &m.FromUser, &m.FromID, &m.Text, &m.Date, &m.ReplyToID, &m.Deleted, &m.MediaType, &m.FileID)
	if err != nil {
		return nil, err
	}
	m.DateStr = time.UnixMilli(m.Date).Format("2006-01-02 15:04:05")
	return &m, nil
}

func (s *Store) GetChatStats(botID, chatID int64) (*ChatStats, error) {
	stats := &ChatStats{ChatID: chatID}
	s.db.QueryRow(`SELECT title FROM chats WHERE bot_id = ? AND id = ?`, botID, chatID).Scan(&stats.Title)
	s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE bot_id = ? AND chat_id = ?`, botID, chatID).Scan(&stats.TotalMessages)

	todayStart := time.Now().Truncate(24 * time.Hour).UnixMilli()
	s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE bot_id = ? AND chat_id = ? AND date >= ?`, botID, chatID, todayStart).Scan(&stats.TodayMessages)

	weekAgo := time.Now().Add(-7 * 24 * time.Hour).UnixMilli()
	s.db.QueryRow(`SELECT COUNT(DISTINCT from_id) FROM messages WHERE bot_id = ? AND chat_id = ? AND date >= ? AND from_id != 0`, botID, chatID, weekAgo).Scan(&stats.ActiveUsers)

	rows, err := s.db.Query(`
		SELECT from_id, from_user, COUNT(*) as cnt
		FROM messages WHERE bot_id = ? AND chat_id = ? AND from_id != 0
		GROUP BY from_id ORDER BY cnt DESC LIMIT 10
	`, botID, chatID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var u UserActivity
			rows.Scan(&u.UserID, &u.Username, &u.Count)
			stats.TopUsers = append(stats.TopUsers, u)
		}
	}

	rows2, err := s.db.Query(`
		SELECT CAST(strftime('%H', date/1000, 'unixepoch', 'localtime') AS INTEGER) as hour, COUNT(*) as cnt
		FROM messages WHERE bot_id = ? AND chat_id = ? AND date >= ?
		GROUP BY hour ORDER BY hour
	`, botID, chatID, weekAgo)
	if err == nil {
		defer rows2.Close()
		hourMap := make(map[int]int)
		for rows2.Next() {
			var h, c int
			rows2.Scan(&h, &c)
			hourMap[h] = c
		}
		for h := 0; h < 24; h++ {
			stats.HourlyStats = append(stats.HourlyStats, HourlyStat{Hour: h, Count: hourMap[h]})
		}
	}

	return stats, nil
}

func (s *Store) SearchMessages(botID, chatID int64, query string, limit int) ([]Message, error) {
	rows, err := s.db.Query(`
		SELECT id, bot_id, chat_id, from_user, from_id, text, date, reply_to_id, deleted, media_type, file_id
		FROM messages WHERE bot_id = ? AND chat_id = ? AND text LIKE ?
		ORDER BY id DESC LIMIT ?
	`, botID, chatID, "%"+query+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		rows.Scan(&m.ID, &m.BotID, &m.ChatID, &m.FromUser, &m.FromID, &m.Text, &m.Date, &m.ReplyToID, &m.Deleted, &m.MediaType, &m.FileID)
		m.DateStr = time.UnixMilli(m.Date).Format("2006-01-02 15:04:05")
		msgs = append(msgs, m)
	}
	return msgs, nil
}

func (s *Store) MarkMessageDeleted(botID, chatID int64, messageID int) error {
	_, err := s.db.Exec(`UPDATE messages SET deleted = 1 WHERE bot_id = ? AND chat_id = ? AND id = ?`, botID, chatID, messageID)
	return err
}

// Admin log

func (s *Store) LogAdminAction(l AdminLog) error {
	_, err := s.db.Exec(`
		INSERT INTO admin_log (chat_id, action, actor_name, target_id, target_name, details, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, l.ChatID, l.Action, l.ActorName, l.TargetID, l.TargetName, l.Details, l.CreatedAt)
	return err
}

func (s *Store) GetAdminLog(chatID int64, limit, offset int) ([]AdminLog, error) {
	rows, err := s.db.Query(`
		SELECT id, chat_id, action, actor_name, target_id, target_name, details, created_at
		FROM admin_log WHERE chat_id = ? ORDER BY id DESC LIMIT ? OFFSET ?
	`, chatID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []AdminLog
	for rows.Next() {
		var l AdminLog
		if err := rows.Scan(&l.ID, &l.ChatID, &l.Action, &l.ActorName, &l.TargetID, &l.TargetName, &l.Details, &l.CreatedAt); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, nil
}

// User tags

func (s *Store) AddUserTag(t UserTag) error {
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO user_tags (chat_id, user_id, username, tag, color)
		VALUES (?, ?, ?, ?, ?)
	`, t.ChatID, t.UserID, t.Username, t.Tag, t.Color)
	return err
}

func (s *Store) RemoveUserTag(id int64) error {
	_, err := s.db.Exec(`DELETE FROM user_tags WHERE id = ?`, id)
	return err
}

func (s *Store) GetUserTags(chatID int64) ([]UserTag, error) {
	rows, err := s.db.Query(`
		SELECT id, chat_id, user_id, username, tag, color
		FROM user_tags WHERE chat_id = ? ORDER BY username, tag
	`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tags []UserTag
	for rows.Next() {
		var t UserTag
		if err := rows.Scan(&t.ID, &t.ChatID, &t.UserID, &t.Username, &t.Tag, &t.Color); err != nil {
			return nil, err
		}
		tags = append(tags, t)
	}
	return tags, nil
}

func (s *Store) GetUserTagsByUser(chatID int64, userID int64) ([]UserTag, error) {
	rows, err := s.db.Query(`
		SELECT id, chat_id, user_id, username, tag, color
		FROM user_tags WHERE chat_id = ? AND user_id = ? ORDER BY tag
	`, chatID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tags []UserTag
	for rows.Next() {
		var t UserTag
		if err := rows.Scan(&t.ID, &t.ChatID, &t.UserID, &t.Username, &t.Tag, &t.Color); err != nil {
			return nil, err
		}
		tags = append(tags, t)
	}
	return tags, nil
}

func (s *Store) TrackUser(chatID int64, userID int64, username string) {
	if userID == 0 {
		return
	}
	s.db.Exec(`
		INSERT INTO known_users (chat_id, user_id, username, first_seen)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(chat_id, user_id) DO UPDATE SET username=excluded.username
	`, chatID, userID, username, time.Now().Format(time.RFC3339))
}

func (s *Store) GetChatUsers(chatID int64, search string, limit, offset int) ([]ChatUser, error) {
	q := `
		SELECT ku.user_id, ku.username,
			COALESCE(m.cnt, 0) as message_count,
			COALESCE(m.last, '') as last_seen
		FROM known_users ku
		LEFT JOIN (
			SELECT from_id, COUNT(*) as cnt,
				datetime(MAX(date), 'unixepoch', 'localtime') as last
			FROM messages WHERE chat_id = ?
			GROUP BY from_id
		) m ON ku.user_id = m.from_id
		WHERE ku.chat_id = ?
	`
	args := []interface{}{chatID, chatID}
	if search != "" {
		q += ` AND ku.username LIKE ?`
		args = append(args, "%"+search+"%")
	}
	q += ` ORDER BY message_count DESC, ku.username LIMIT ? OFFSET ?`
	args = append(args, limit, offset)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []ChatUser
	for rows.Next() {
		var u ChatUser
		if err := rows.Scan(&u.UserID, &u.Username, &u.MessageCount, &u.LastSeen); err != nil {
			return nil, err
		}
		users = append(users, u)
	}

	for i := range users {
		tags, err := s.GetUserTagsByUser(chatID, users[i].UserID)
		if err == nil {
			users[i].Tags = tags
		}
		if users[i].Tags == nil {
			users[i].Tags = []UserTag{}
		}
	}

	return users, nil
}

// Route methods

func (s *Store) AddRoute(r Route) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO routes (source_bot_id, target_bot_id, source_chat_id, condition_type, condition_value, action, target_chat_id, enabled, description, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, r.SourceBotID, r.TargetBotID, r.SourceChatID, r.ConditionType, r.ConditionValue, r.Action, r.TargetChatID, r.Enabled, r.Description, r.CreatedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateRoute(r Route) error {
	_, err := s.db.Exec(`
		UPDATE routes SET target_bot_id=?, source_chat_id=?, condition_type=?, condition_value=?, action=?, target_chat_id=?, enabled=?, description=?
		WHERE id=?
	`, r.TargetBotID, r.SourceChatID, r.ConditionType, r.ConditionValue, r.Action, r.TargetChatID, r.Enabled, r.Description, r.ID)
	return err
}

func (s *Store) DeleteRoute(id int64) error {
	_, err := s.db.Exec(`DELETE FROM routes WHERE id=?`, id)
	return err
}

func (s *Store) GetRoutes(sourceBotID int64) ([]Route, error) {
	rows, err := s.db.Query(`
		SELECT id, source_bot_id, target_bot_id, source_chat_id, condition_type, condition_value, action, target_chat_id, enabled, description, created_at
		FROM routes WHERE source_bot_id=? ORDER BY id
	`, sourceBotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var routes []Route
	for rows.Next() {
		var r Route
		if err := rows.Scan(&r.ID, &r.SourceBotID, &r.TargetBotID, &r.SourceChatID, &r.ConditionType, &r.ConditionValue, &r.Action, &r.TargetChatID, &r.Enabled, &r.Description, &r.CreatedAt); err != nil {
			return nil, err
		}
		routes = append(routes, r)
	}
	return routes, nil
}

func (s *Store) GetAllEnabledRoutes() ([]Route, error) {
	rows, err := s.db.Query(`
		SELECT id, source_bot_id, target_bot_id, source_chat_id, condition_type, condition_value, action, target_chat_id, enabled, description, created_at
		FROM routes WHERE enabled=1 ORDER BY source_bot_id, id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var routes []Route
	for rows.Next() {
		var r Route
		if err := rows.Scan(&r.ID, &r.SourceBotID, &r.TargetBotID, &r.SourceChatID, &r.ConditionType, &r.ConditionValue, &r.Action, &r.TargetChatID, &r.Enabled, &r.Description, &r.CreatedAt); err != nil {
			return nil, err
		}
		routes = append(routes, r)
	}
	return routes, nil
}

// Route mapping methods (Source-NAT tracking)

func (s *Store) SaveRouteMapping(m RouteMapping) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO route_mappings (route_id, source_bot_id, source_chat_id, source_msg_id, target_bot_id, target_chat_id, target_msg_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, m.RouteID, m.SourceBotID, m.SourceChatID, m.SourceMsgID, m.TargetBotID, m.TargetChatID, m.TargetMsgID, m.CreatedAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FindReverseMapping looks up a mapping by target bot+chat to find the source bot+chat for reverse routing.
// It finds the most recent mapping for this target bot+chat combination.
func (s *Store) FindReverseMapping(targetBotID, targetChatID int64) (*RouteMapping, error) {
	var m RouteMapping
	err := s.db.QueryRow(`
		SELECT id, route_id, source_bot_id, source_chat_id, source_msg_id, target_bot_id, target_chat_id, target_msg_id, created_at
		FROM route_mappings WHERE target_bot_id=? AND target_chat_id=?
		ORDER BY id DESC LIMIT 1
	`, targetBotID, targetChatID).Scan(&m.ID, &m.RouteID, &m.SourceBotID, &m.SourceChatID, &m.SourceMsgID, &m.TargetBotID, &m.TargetChatID, &m.TargetMsgID, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// FindReverseMappingByReply finds a mapping where the target message matches the reply_to_message_id
func (s *Store) FindReverseMappingByReply(targetBotID, targetChatID int64, targetMsgID int) (*RouteMapping, error) {
	var m RouteMapping
	err := s.db.QueryRow(`
		SELECT id, route_id, source_bot_id, source_chat_id, source_msg_id, target_bot_id, target_chat_id, target_msg_id, created_at
		FROM route_mappings WHERE target_bot_id=? AND target_chat_id=? AND target_msg_id=?
		ORDER BY id DESC LIMIT 1
	`, targetBotID, targetChatID, targetMsgID).Scan(&m.ID, &m.RouteID, &m.SourceBotID, &m.SourceChatID, &m.SourceMsgID, &m.TargetBotID, &m.TargetChatID, &m.TargetMsgID, &m.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// CleanOldRouteMappings removes mappings older than the given duration
func (s *Store) CleanOldRouteMappings(olderThan string) {
	s.db.Exec(`DELETE FROM route_mappings WHERE created_at < ?`, olderThan)
}

// Auth user methods

func (s *Store) CreateUser(username, passwordHash, displayName, role string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO auth_users (username, password_hash, display_name, role, created_at)
		VALUES (?, ?, ?, ?, ?)`, username, passwordHash, displayName, role, time.Now().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) GetUserByUsername(username string) (*AuthUser, error) {
	var u AuthUser
	var mustChange int
	err := s.db.QueryRow(`SELECT id, username, password_hash, display_name, role, must_change_password, created_at, last_login
		FROM auth_users WHERE username=?`, username).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.DisplayName, &u.Role, &mustChange, &u.CreatedAt, &u.LastLogin)
	if err != nil {
		return nil, err
	}
	u.MustChangePassword = mustChange != 0
	return &u, nil
}

func (s *Store) GetUserByID(id int64) (*AuthUser, error) {
	var u AuthUser
	var mustChange int
	err := s.db.QueryRow(`SELECT id, username, password_hash, display_name, role, must_change_password, created_at, last_login
		FROM auth_users WHERE id=?`, id).Scan(&u.ID, &u.Username, &u.PasswordHash, &u.DisplayName, &u.Role, &mustChange, &u.CreatedAt, &u.LastLogin)
	if err != nil {
		return nil, err
	}
	u.MustChangePassword = mustChange != 0
	return &u, nil
}

func (s *Store) GetAllUsers() ([]AuthUser, error) {
	rows, err := s.db.Query(`SELECT id, username, password_hash, display_name, role, must_change_password, created_at, last_login
		FROM auth_users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []AuthUser
	for rows.Next() {
		var u AuthUser
		var mustChange int
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.DisplayName, &u.Role, &mustChange, &u.CreatedAt, &u.LastLogin); err != nil {
			return nil, err
		}
		u.MustChangePassword = mustChange != 0
		users = append(users, u)
	}
	return users, nil
}

func (s *Store) UpdateUser(id int64, displayName, role string) error {
	_, err := s.db.Exec(`UPDATE auth_users SET display_name=?, role=? WHERE id=?`, displayName, role, id)
	return err
}

func (s *Store) UpdateUserPassword(id int64, passwordHash string) error {
	_, err := s.db.Exec(`UPDATE auth_users SET password_hash=?, must_change_password=0 WHERE id=?`, passwordHash, id)
	return err
}

func (s *Store) DeleteUser(id int64) error {
	s.db.Exec(`DELETE FROM auth_sessions WHERE user_id=?`, id)
	s.db.Exec(`DELETE FROM user_bots WHERE user_id=?`, id)
	_, err := s.db.Exec(`DELETE FROM auth_users WHERE id=?`, id)
	return err
}

func (s *Store) UpdateUserLastLogin(id int64) {
	s.db.Exec(`UPDATE auth_users SET last_login=? WHERE id=?`, time.Now().Format(time.RFC3339), id)
}

// Session methods

func (s *Store) CreateSession(token string, userID int64, expiresAt time.Time) error {
	_, err := s.db.Exec(`INSERT INTO auth_sessions (token, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		token, userID, time.Now().Format(time.RFC3339), expiresAt.Format(time.RFC3339))
	return err
}

func (s *Store) GetUserBySession(token string) (*AuthUser, error) {
	var userID int64
	var expiresAt string
	err := s.db.QueryRow(`SELECT user_id, expires_at FROM auth_sessions WHERE token=?`, token).Scan(&userID, &expiresAt)
	if err != nil {
		return nil, err
	}
	exp, err := time.Parse(time.RFC3339, expiresAt)
	if err != nil || time.Now().After(exp) {
		s.db.Exec(`DELETE FROM auth_sessions WHERE token=?`, token)
		return nil, nil
	}
	return s.GetUserByID(userID)
}

func (s *Store) DeleteSession(token string) {
	s.db.Exec(`DELETE FROM auth_sessions WHERE token=?`, token)
}

func (s *Store) DeleteUserSessions(userID int64) {
	s.db.Exec(`DELETE FROM auth_sessions WHERE user_id=?`, userID)
}

func (s *Store) CleanExpiredSessions() {
	s.db.Exec(`DELETE FROM auth_sessions WHERE expires_at < ?`, time.Now().Format(time.RFC3339))
}

// User-Bot access methods

func (s *Store) AssignBotToUser(userID, botID int64) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO user_bots (user_id, bot_id) VALUES (?, ?)`, userID, botID)
	return err
}

func (s *Store) RevokeBotFromUser(userID, botID int64) error {
	_, err := s.db.Exec(`DELETE FROM user_bots WHERE user_id=? AND bot_id=?`, userID, botID)
	return err
}

func (s *Store) GetUserBotIDs(userID int64) ([]int64, error) {
	rows, err := s.db.Query(`SELECT bot_id FROM user_bots WHERE user_id=?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids, nil
}

func (s *Store) GetBotUserIDs(botID int64) ([]int64, error) {
	rows, err := s.db.Query(`SELECT user_id FROM user_bots WHERE bot_id=?`, botID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids, nil
}

func (s *Store) UserHasBotAccess(userID, botID int64) bool {
	var count int
	s.db.QueryRow(`SELECT COUNT(*) FROM user_bots WHERE user_id=? AND bot_id=?`, userID, botID).Scan(&count)
	return count > 0
}

func (s *Store) GetBotConfigsForUser(userID int64) ([]BotConfig, error) {
	rows, err := s.db.Query(`
		SELECT b.id, b.name, b.token, b.bot_username, b.manage_enabled, b.proxy_enabled, b.backend_url, b.secret_token, b.polling_timeout, b.offset_id, b.last_error, b.last_activity, b.updates_forwarded, b.source, b.backend_status, b.backend_checked_at
		FROM bots b
		INNER JOIN user_bots ub ON b.id = ub.bot_id
		WHERE ub.user_id = ?
		ORDER BY b.source DESC, b.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bots []BotConfig
	for rows.Next() {
		var b BotConfig
		if err := rows.Scan(&b.ID, &b.Name, &b.Token, &b.BotUsername, &b.ManageEnabled, &b.ProxyEnabled, &b.BackendURL, &b.SecretToken, &b.PollingTimeout, &b.Offset, &b.LastError, &b.LastActivity, &b.UpdatesForwarded, &b.Source, &b.BackendStatus, &b.BackendCheckedAt); err != nil {
			return nil, err
		}
		bots = append(bots, b)
	}
	return bots, nil
}

// API key methods

type APIKey struct {
	ID        int64  `json:"id"`
	UserID    int64  `json:"user_id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	LastUsed  string `json:"last_used"`
	Enabled   bool   `json:"enabled"`
}

func (s *Store) CreateAPIKey(userID int64, keyHash, name string) (int64, error) {
	result, err := s.db.Exec(`INSERT INTO api_keys (user_id, key_hash, name, created_at, enabled) VALUES (?, ?, ?, ?, 1)`,
		userID, keyHash, name, time.Now().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (s *Store) GetAPIKeys(userID int64) ([]APIKey, error) {
	rows, err := s.db.Query(`SELECT id, user_id, name, created_at, COALESCE(last_used,''), enabled FROM api_keys WHERE user_id=? ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []APIKey
	for rows.Next() {
		var k APIKey
		var enabled int
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.CreatedAt, &k.LastUsed, &enabled); err != nil {
			return nil, err
		}
		k.Enabled = enabled != 0
		keys = append(keys, k)
	}
	return keys, nil
}

func (s *Store) GetAllAPIKeys() ([]APIKey, error) {
	rows, err := s.db.Query(`SELECT id, user_id, name, created_at, COALESCE(last_used,''), enabled FROM api_keys ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []APIKey
	for rows.Next() {
		var k APIKey
		var enabled int
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.CreatedAt, &k.LastUsed, &enabled); err != nil {
			return nil, err
		}
		k.Enabled = enabled != 0
		keys = append(keys, k)
	}
	return keys, nil
}

func (s *Store) DeleteAPIKey(id int64) error {
	_, err := s.db.Exec(`DELETE FROM api_keys WHERE id=?`, id)
	return err
}

func (s *Store) ToggleAPIKey(id int64, enabled bool) error {
	val := 0
	if enabled {
		val = 1
	}
	_, err := s.db.Exec(`UPDATE api_keys SET enabled=? WHERE id=?`, val, id)
	return err
}

func (s *Store) GetUserByAPIKey(keyHash string) (*AuthUser, error) {
	var userID int64
	var enabled int
	err := s.db.QueryRow(`SELECT user_id, enabled FROM api_keys WHERE key_hash=?`, keyHash).Scan(&userID, &enabled)
	if err != nil {
		return nil, err
	}
	if enabled == 0 {
		return nil, nil
	}
	s.db.Exec(`UPDATE api_keys SET last_used=? WHERE key_hash=?`, time.Now().Format(time.RFC3339), keyHash)
	return s.GetUserByID(userID)
}

func (s *Store) Close() error {
	return s.db.Close()
}

// LLM config methods

func (s *Store) GetLLMConfig() (*LLMConfig, error) {
	var cfg LLMConfig
	var enabled int
	err := s.db.QueryRow(`SELECT id, api_url, api_key, model, system_prompt, enabled FROM llm_config ORDER BY id LIMIT 1`).
		Scan(&cfg.ID, &cfg.APIURL, &cfg.APIKey, &cfg.Model, &cfg.SystemPrompt, &enabled)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return nil, nil
		}
		return nil, err
	}
	cfg.Enabled = enabled != 0
	return &cfg, nil
}

func (s *Store) SaveLLMConfig(cfg LLMConfig) error {
	var existing int64
	s.db.QueryRow(`SELECT id FROM llm_config ORDER BY id LIMIT 1`).Scan(&existing)
	if existing == 0 {
		_, err := s.db.Exec(`INSERT INTO llm_config (api_url, api_key, model, system_prompt, enabled) VALUES (?, ?, ?, ?, ?)`,
			cfg.APIURL, cfg.APIKey, cfg.Model, cfg.SystemPrompt, cfg.Enabled)
		return err
	}
	_, err := s.db.Exec(`UPDATE llm_config SET api_url=?, api_key=?, model=?, system_prompt=?, enabled=? WHERE id=?`,
		cfg.APIURL, cfg.APIKey, cfg.Model, cfg.SystemPrompt, cfg.Enabled, existing)
	return err
}

// Bot description methods (separate from BotConfig to avoid struct conflicts)

func (s *Store) UpdateBotDescription(botID int64, description string) error {
	_, err := s.db.Exec(`UPDATE bots SET description=? WHERE id=?`, description, botID)
	return err
}

func (s *Store) GetBotDescription(botID int64) (string, error) {
	var desc string
	err := s.db.QueryRow(`SELECT description FROM bots WHERE id=?`, botID).Scan(&desc)
	if err != nil {
		return "", err
	}
	return desc, nil
}

// Bridge methods

func (s *Store) GetBridges() ([]BridgeConfig, error) {
	rows, err := s.db.Query(`SELECT id, name, protocol, linked_bot_id, config, callback_url, enabled, created_at, last_activity, last_error FROM bridges ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []BridgeConfig
	for rows.Next() {
		var b BridgeConfig
		if err := rows.Scan(&b.ID, &b.Name, &b.Protocol, &b.LinkedBotID, &b.Config, &b.CallbackURL, &b.Enabled, &b.CreatedAt, &b.LastActivity, &b.LastError); err != nil {
			return nil, err
		}
		result = append(result, b)
	}
	return result, nil
}

func (s *Store) GetBridge(id int64) (*BridgeConfig, error) {
	var b BridgeConfig
	err := s.db.QueryRow(`SELECT id, name, protocol, linked_bot_id, config, callback_url, enabled, created_at, last_activity, last_error FROM bridges WHERE id=?`, id).
		Scan(&b.ID, &b.Name, &b.Protocol, &b.LinkedBotID, &b.Config, &b.CallbackURL, &b.Enabled, &b.CreatedAt, &b.LastActivity, &b.LastError)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *Store) GetBridgesForBot(botID int64) ([]BridgeConfig, error) {
	rows, err := s.db.Query(`SELECT id, name, protocol, linked_bot_id, config, callback_url, enabled, created_at, last_activity, last_error FROM bridges WHERE linked_bot_id=? ORDER BY id`, botID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []BridgeConfig
	for rows.Next() {
		var b BridgeConfig
		if err := rows.Scan(&b.ID, &b.Name, &b.Protocol, &b.LinkedBotID, &b.Config, &b.CallbackURL, &b.Enabled, &b.CreatedAt, &b.LastActivity, &b.LastError); err != nil {
			return nil, err
		}
		result = append(result, b)
	}
	return result, nil
}

func (s *Store) AddBridge(b BridgeConfig) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO bridges (name, protocol, linked_bot_id, config, callback_url, enabled, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		b.Name, b.Protocol, b.LinkedBotID, b.Config, b.CallbackURL, b.Enabled, time.Now().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateBridge(b BridgeConfig) error {
	_, err := s.db.Exec(`UPDATE bridges SET name=?, protocol=?, linked_bot_id=?, config=?, callback_url=?, enabled=? WHERE id=?`,
		b.Name, b.Protocol, b.LinkedBotID, b.Config, b.CallbackURL, b.Enabled, b.ID)
	return err
}

func (s *Store) DeleteBridge(id int64) error {
	_, err := s.db.Exec(`DELETE FROM bridges WHERE id=?`, id)
	if err != nil {
		return err
	}
	s.db.Exec(`DELETE FROM bridge_chat_mappings WHERE bridge_id=?`, id)
	s.db.Exec(`DELETE FROM bridge_msg_mappings WHERE bridge_id=?`, id)
	return nil
}

func (s *Store) UpdateBridgeActivity(bridgeID int64, lastError string) {
	if lastError != "" {
		s.db.Exec(`UPDATE bridges SET last_activity=?, last_error=? WHERE id=?`, time.Now().Format(time.RFC3339), lastError, bridgeID)
	} else {
		s.db.Exec(`UPDATE bridges SET last_activity=?, last_error='' WHERE id=?`, time.Now().Format(time.RFC3339), bridgeID)
	}
}

// Bridge chat mappings

func (s *Store) SaveBridgeChatMapping(bridgeID int64, externalChatID string, telegramChatID int64) {
	s.db.Exec(`INSERT OR REPLACE INTO bridge_chat_mappings (bridge_id, external_chat_id, telegram_chat_id) VALUES (?, ?, ?)`,
		bridgeID, externalChatID, telegramChatID)
}

func (s *Store) GetBridgeChatMapping(bridgeID int64, externalChatID string) (int64, error) {
	var tgChatID int64
	err := s.db.QueryRow(`SELECT telegram_chat_id FROM bridge_chat_mappings WHERE bridge_id=? AND external_chat_id=?`,
		bridgeID, externalChatID).Scan(&tgChatID)
	return tgChatID, err
}

func (s *Store) GetBridgeChatMappingReverse(bridgeID int64, telegramChatID int64) (string, error) {
	var extChatID string
	err := s.db.QueryRow(`SELECT external_chat_id FROM bridge_chat_mappings WHERE bridge_id=? AND telegram_chat_id=?`,
		bridgeID, telegramChatID).Scan(&extChatID)
	return extChatID, err
}

// Bridge message mappings

func (s *Store) SaveBridgeMsgMapping(bridgeID int64, externalMsgID string, telegramMsgID int, telegramChatID int64) {
	s.db.Exec(`INSERT OR REPLACE INTO bridge_msg_mappings (bridge_id, external_msg_id, telegram_msg_id, telegram_chat_id) VALUES (?, ?, ?, ?)`,
		bridgeID, externalMsgID, telegramMsgID, telegramChatID)
}

func (s *Store) GetBridgeMsgMapping(bridgeID int64, externalMsgID string) (*BridgeMsgMapping, error) {
	var m BridgeMsgMapping
	err := s.db.QueryRow(`SELECT bridge_id, external_msg_id, telegram_msg_id, telegram_chat_id FROM bridge_msg_mappings WHERE bridge_id=? AND external_msg_id=?`,
		bridgeID, externalMsgID).Scan(&m.BridgeID, &m.ExternalMsgID, &m.TelegramMsgID, &m.TelegramChatID)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (s *Store) GetBridgeMsgMappingReverse(bridgeID int64, telegramMsgID int) (string, error) {
	var extMsgID string
	err := s.db.QueryRow(`SELECT external_msg_id FROM bridge_msg_mappings WHERE bridge_id=? AND telegram_msg_id=?`,
		bridgeID, telegramMsgID).Scan(&extMsgID)
	return extMsgID, err
}
