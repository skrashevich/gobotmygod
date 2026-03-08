package main

import (
	"database/sql"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db *sql.DB
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
}

type Message struct {
	ID        int    `json:"id"`
	ChatID    int64  `json:"chat_id"`
	FromUser  string `json:"from_user"`
	FromID    int64  `json:"from_id"`
	Text      string `json:"text"`
	Date      int64  `json:"date"`
	DateStr   string `json:"date_str"`
	ReplyToID int    `json:"reply_to_id,omitempty"`
	Deleted   bool   `json:"deleted"`
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
	ID        int64  `json:"id"`
	ChatID    int64  `json:"chat_id"`
	Action    string `json:"action"`
	ActorName string `json:"actor_name"`
	TargetID  int64  `json:"target_id,omitempty"`
	TargetName string `json:"target_name,omitempty"`
	Details   string `json:"details,omitempty"`
	CreatedAt string `json:"created_at"`
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

type AdminInfo struct {
	UserID           int64  `json:"user_id"`
	Username         string `json:"username"`
	Status           string `json:"status"`
	CustomTitle      string `json:"custom_title"`
	CanDeleteMessages bool  `json:"can_delete_messages"`
	CanRestrictMembers bool `json:"can_restrict_members"`
	CanPromoteMembers bool  `json:"can_promote_members"`
	CanChangeInfo     bool  `json:"can_change_info"`
	CanInviteUsers    bool  `json:"can_invite_users"`
	CanPinMessages    bool  `json:"can_pin_messages"`
	CanManageChat     bool  `json:"can_manage_chat"`
}

func NewStore(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL")
	if err != nil {
		return nil, err
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			id INTEGER PRIMARY KEY,
			type TEXT NOT NULL DEFAULT '',
			title TEXT NOT NULL DEFAULT '',
			username TEXT NOT NULL DEFAULT '',
			member_count INTEGER NOT NULL DEFAULT 0,
			description TEXT NOT NULL DEFAULT '',
			is_admin INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE IF NOT EXISTS messages (
			id INTEGER NOT NULL,
			chat_id INTEGER NOT NULL,
			from_user TEXT NOT NULL DEFAULT '',
			from_id INTEGER NOT NULL DEFAULT 0,
			text TEXT NOT NULL DEFAULT '',
			date INTEGER NOT NULL DEFAULT 0,
			reply_to_id INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (chat_id, id)
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

		CREATE TABLE IF NOT EXISTS proxy_bots (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL DEFAULT '',
			token TEXT NOT NULL DEFAULT '',
			backend_url TEXT NOT NULL DEFAULT '',
			secret_token TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 0,
			polling_timeout INTEGER NOT NULL DEFAULT 30,
			offset_id INTEGER NOT NULL DEFAULT 0,
			bot_username TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT '',
			last_activity TEXT NOT NULL DEFAULT '',
			updates_forwarded INTEGER NOT NULL DEFAULT 0
		);
	`)
	if err != nil {
		return err
	}

	// Add deleted column if missing (migration)
	var colCount int
	s.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name='deleted'`).Scan(&colCount)
	if colCount == 0 {
		if _, err := s.db.Exec(`ALTER TABLE messages ADD COLUMN deleted INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}

	// Backfill known_users from messages
	s.db.Exec(`
		INSERT OR IGNORE INTO known_users (chat_id, user_id, username, first_seen)
		SELECT chat_id, from_id, from_user, datetime(MIN(date), 'unixepoch')
		FROM messages WHERE from_id != 0
		GROUP BY chat_id, from_id
	`)
	return err
}

func (s *Store) UpsertChat(c Chat) error {
	_, err := s.db.Exec(`
		INSERT INTO chats (id, type, title, username, member_count, description, is_admin, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			type=excluded.type, title=excluded.title, username=excluded.username,
			member_count=excluded.member_count, description=excluded.description,
			is_admin=excluded.is_admin, updated_at=excluded.updated_at
	`, c.ID, c.Type, c.Title, c.Username, c.MemberCount, c.Description, c.IsAdmin, c.UpdatedAt)
	return err
}

func (s *Store) GetChats() ([]Chat, error) {
	rows, err := s.db.Query(`SELECT id, type, title, username, member_count, description, is_admin, updated_at FROM chats ORDER BY title`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chats []Chat
	for rows.Next() {
		var c Chat
		if err := rows.Scan(&c.ID, &c.Type, &c.Title, &c.Username, &c.MemberCount, &c.Description, &c.IsAdmin, &c.UpdatedAt); err != nil {
			return nil, err
		}
		chats = append(chats, c)
	}
	return chats, nil
}

func (s *Store) SaveMessage(m Message) error {
	_, err := s.db.Exec(`
		INSERT OR IGNORE INTO messages (id, chat_id, from_user, from_id, text, date, reply_to_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, m.ID, m.ChatID, m.FromUser, m.FromID, m.Text, m.Date, m.ReplyToID)
	return err
}

func (s *Store) GetMessages(chatID int64, limit, offset int) ([]Message, error) {
	rows, err := s.db.Query(`
		SELECT id, chat_id, from_user, from_id, text, date, reply_to_id, deleted
		FROM messages WHERE chat_id = ? ORDER BY date DESC LIMIT ? OFFSET ?
	`, chatID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		if err := rows.Scan(&m.ID, &m.ChatID, &m.FromUser, &m.FromID, &m.Text, &m.Date, &m.ReplyToID, &m.Deleted); err != nil {
			return nil, err
		}
		m.DateStr = time.Unix(m.Date, 0).Format("2006-01-02 15:04:05")
		msgs = append(msgs, m)
	}
	return msgs, nil
}

func (s *Store) GetChatStats(chatID int64) (*ChatStats, error) {
	stats := &ChatStats{ChatID: chatID}

	// Title
	s.db.QueryRow(`SELECT title FROM chats WHERE id = ?`, chatID).Scan(&stats.Title)

	// Total messages
	s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_id = ?`, chatID).Scan(&stats.TotalMessages)

	// Today messages
	todayStart := time.Now().Truncate(24 * time.Hour).Unix()
	s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE chat_id = ? AND date >= ?`, chatID, todayStart).Scan(&stats.TodayMessages)

	// Active users (last 7 days)
	weekAgo := time.Now().Add(-7 * 24 * time.Hour).Unix()
	s.db.QueryRow(`SELECT COUNT(DISTINCT from_id) FROM messages WHERE chat_id = ? AND date >= ? AND from_id != 0`, chatID, weekAgo).Scan(&stats.ActiveUsers)

	// Top users
	rows, err := s.db.Query(`
		SELECT from_id, from_user, COUNT(*) as cnt
		FROM messages WHERE chat_id = ? AND from_id != 0
		GROUP BY from_id ORDER BY cnt DESC LIMIT 10
	`, chatID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var u UserActivity
			rows.Scan(&u.UserID, &u.Username, &u.Count)
			stats.TopUsers = append(stats.TopUsers, u)
		}
	}

	// Hourly distribution (last 7 days)
	rows2, err := s.db.Query(`
		SELECT CAST(strftime('%H', date, 'unixepoch', 'localtime') AS INTEGER) as hour, COUNT(*) as cnt
		FROM messages WHERE chat_id = ? AND date >= ?
		GROUP BY hour ORDER BY hour
	`, chatID, weekAgo)
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

func (s *Store) SearchMessages(chatID int64, query string, limit int) ([]Message, error) {
	rows, err := s.db.Query(`
		SELECT id, chat_id, from_user, from_id, text, date, reply_to_id, deleted
		FROM messages WHERE chat_id = ? AND text LIKE ?
		ORDER BY date DESC LIMIT ?
	`, chatID, "%"+query+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var m Message
		rows.Scan(&m.ID, &m.ChatID, &m.FromUser, &m.FromID, &m.Text, &m.Date, &m.ReplyToID, &m.Deleted)
		m.DateStr = time.Unix(m.Date, 0).Format("2006-01-02 15:04:05")
		msgs = append(msgs, m)
	}
	return msgs, nil
}

func (s *Store) MarkMessageDeleted(chatID int64, messageID int) error {
	_, err := s.db.Exec(`UPDATE messages SET deleted = 1 WHERE chat_id = ? AND id = ?`, chatID, messageID)
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
	// Merge known_users with message stats via LEFT JOIN
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

	// Load tags for each user
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

// Proxy bots

func (s *Store) AddProxyBot(b ProxyBot) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO proxy_bots (name, token, backend_url, secret_token, enabled, polling_timeout, bot_username)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, b.Name, b.Token, b.BackendURL, b.SecretToken, b.Enabled, b.PollingTimeout, b.BotUsername)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateProxyBot(b ProxyBot) error {
	_, err := s.db.Exec(`
		UPDATE proxy_bots SET name=?, token=?, backend_url=?, secret_token=?, enabled=?, polling_timeout=?, bot_username=?
		WHERE id=?
	`, b.Name, b.Token, b.BackendURL, b.SecretToken, b.Enabled, b.PollingTimeout, b.BotUsername, b.ID)
	return err
}

func (s *Store) DeleteProxyBot(id int64) error {
	_, err := s.db.Exec(`DELETE FROM proxy_bots WHERE id=?`, id)
	return err
}

func (s *Store) GetProxyBots() ([]ProxyBot, error) {
	rows, err := s.db.Query(`SELECT id, name, token, backend_url, secret_token, enabled, polling_timeout, offset_id, bot_username, last_error, last_activity, updates_forwarded FROM proxy_bots ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var bots []ProxyBot
	for rows.Next() {
		var b ProxyBot
		if err := rows.Scan(&b.ID, &b.Name, &b.Token, &b.BackendURL, &b.SecretToken, &b.Enabled, &b.PollingTimeout, &b.Offset, &b.BotUsername, &b.LastError, &b.LastActivity, &b.UpdatesForwarded); err != nil {
			return nil, err
		}
		bots = append(bots, b)
	}
	return bots, nil
}

func (s *Store) GetProxyBot(id int64) (*ProxyBot, error) {
	var b ProxyBot
	err := s.db.QueryRow(`SELECT id, name, token, backend_url, secret_token, enabled, polling_timeout, offset_id, bot_username, last_error, last_activity, updates_forwarded FROM proxy_bots WHERE id=?`, id).
		Scan(&b.ID, &b.Name, &b.Token, &b.BackendURL, &b.SecretToken, &b.Enabled, &b.PollingTimeout, &b.Offset, &b.BotUsername, &b.LastError, &b.LastActivity, &b.UpdatesForwarded)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

func (s *Store) UpdateProxyBotOffset(id int64, offset int64) {
	s.db.Exec(`UPDATE proxy_bots SET offset_id=? WHERE id=?`, offset, id)
}

func (s *Store) UpdateProxyBotStatus(id int64, lastError string, lastActivity string) {
	if lastError != "" {
		s.db.Exec(`UPDATE proxy_bots SET last_error=? WHERE id=?`, lastError, id)
	}
	if lastActivity != "" {
		s.db.Exec(`UPDATE proxy_bots SET last_activity=?, last_error='' WHERE id=?`, lastActivity, id)
	}
}

func (s *Store) IncrementProxyBotForwarded(id int64) {
	s.db.Exec(`UPDATE proxy_bots SET updates_forwarded = updates_forwarded + 1 WHERE id=?`, id)
}

func (s *Store) DeleteChat(chatID int64) error {
	_, err := s.db.Exec(`DELETE FROM chats WHERE id = ?`, chatID)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM messages WHERE chat_id = ?`, chatID)
	return err
}

func (s *Store) Close() error {
	return s.db.Close()
}
