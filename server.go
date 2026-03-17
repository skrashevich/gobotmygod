package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"log"
	"mime"
	"path"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	webp "github.com/skrashevich/go-webp"
)

//go:embed templates
var templateFS embed.FS

type Server struct {
	store          *Store
	proxy          *ProxyManager
	bridge         *BridgeManager
	mu             sync.RWMutex
	bots           map[int64]*Bot // botID -> Bot (for Telegram API calls)
	webhookPath    string
	webhookHandler http.HandlerFunc
	demoMode       bool
	logBuf         *LogBuffer
}

func NewServer(store *Store, proxy *ProxyManager) *Server {
	return &Server{
		store: store,
		proxy: proxy,
		bots:  make(map[int64]*Bot),
	}
}

func (s *Server) SetBridgeManager(bm *BridgeManager) {
	s.bridge = bm
}

func (s *Server) RegisterBot(botID int64, bot *Bot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bots[botID] = bot
}

func (s *Server) getBot(botID int64) *Bot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bots[botID]
}

// getBotFromRequest extracts bot_id from query params and returns the Bot instance
func (s *Server) getBotFromRequest(r *http.Request) (*Bot, int64, error) {
	botID, err := strconv.ParseInt(r.URL.Query().Get("bot_id"), 10, 64)
	if err != nil {
		return nil, 0, fmt.Errorf("invalid bot_id")
	}
	bot := s.getBot(botID)
	if bot == nil {
		// Try to get from proxy manager (for non-CLI managed bots)
		bot = s.proxy.GetManagedBot(botID)
	}
	if bot == nil {
		return nil, botID, fmt.Errorf("bot not found or not a management bot")
	}
	return bot, botID, nil
}

func (s *Server) SetWebhookHandler(path string, handler http.HandlerFunc) {
	s.webhookPath = path
	s.webhookHandler = handler
}

// BuildMux creates the HTTP multiplexer with all routes registered.
func (s *Server) BuildMux() *http.ServeMux {
	mux := http.NewServeMux()

	if s.webhookPath != "" && s.webhookHandler != nil {
		mux.HandleFunc(s.webhookPath, s.webhookHandler)
		log.Printf("Webhook endpoint registered at %s", s.webhookPath)
	}

	// Telegram API proxy — no auth (backends use this)
	mux.HandleFunc("/tgapi/", s.handleTelegramAPIProxy)

	// Health check — no auth
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]string{"status": "ok"}
		if s.demoMode {
			resp["mode"] = "demo"
		}
		writeJSON(w, resp)
	})

	// Auth endpoints — no auth required
	mux.HandleFunc("/api/auth/login", s.handleLogin)
	mux.HandleFunc("/api/auth/logout", s.handleLogout)

	// Auth endpoints — auth required
	mux.HandleFunc("/api/auth/me", s.authMiddleware(s.handleMe))
	mux.HandleFunc("/api/auth/change-password", s.authMiddleware(s.handleChangePassword))

	// User management — admin only
	mux.HandleFunc("/api/auth/users", s.adminOnly(s.handleUserList))
	mux.HandleFunc("/api/auth/users/add", s.adminOnly(s.handleUserAdd))
	mux.HandleFunc("/api/auth/users/update", s.adminOnly(s.handleUserUpdate))
	mux.HandleFunc("/api/auth/users/delete", s.adminOnly(s.handleUserDelete))
	mux.HandleFunc("/api/auth/users/reset-password", s.adminOnly(s.handleUserResetPassword))
	mux.HandleFunc("/api/auth/users/bots", s.adminOnly(s.handleUserBots))
	mux.HandleFunc("/api/auth/users/bots/assign", s.adminOnly(s.handleAssignBot))
	mux.HandleFunc("/api/auth/users/bots/revoke", s.adminOnly(s.handleRevokeBot))

	// API keys — admin only
	mux.HandleFunc("/api/auth/api-keys", s.adminOnly(s.handleListAPIKeys))
	mux.HandleFunc("/api/auth/api-keys/create", s.adminOnly(s.handleCreateAPIKey))
	mux.HandleFunc("/api/auth/api-keys/delete", s.adminOnly(s.handleDeleteAPIKey))
	mux.HandleFunc("/api/auth/api-keys/toggle", s.adminOnly(s.handleToggleAPIKey))

	// SPA — no auth (SPA handles it client-side)
	mux.HandleFunc("/", s.handleIndex)

	// Bot management — auth required
	mux.HandleFunc("/api/bots", s.authMiddleware(s.handleBotList))
	mux.HandleFunc("/api/bots/add", s.adminOnly(s.handleBotAdd))
	mux.HandleFunc("/api/bots/update", s.adminOnly(s.handleBotUpdate))
	mux.HandleFunc("/api/bots/delete", s.adminOnly(s.handleBotDelete))
	mux.HandleFunc("/api/bots/validate", s.adminOnly(s.handleBotValidate))
	mux.HandleFunc("/api/bots/health", s.authMiddleware(s.handleBotHealth))

	// Chat management
	mux.HandleFunc("/api/chats", s.authMiddleware(s.handleChats))
	mux.HandleFunc("/api/chats/refresh", s.authMiddleware(s.handleRefreshChat))
	mux.HandleFunc("/api/chats/delete", s.authMiddleware(s.handleDeleteChat))

	// Messages
	mux.HandleFunc("/api/messages", s.authMiddleware(s.handleMessages))
	mux.HandleFunc("/api/messages/search", s.authMiddleware(s.handleSearchMessages))
	mux.HandleFunc("/api/messages/send", s.authMiddleware(s.handleSendMessage))
	mux.HandleFunc("/api/messages/pin", s.authMiddleware(s.handlePinMessage))
	mux.HandleFunc("/api/messages/unpin", s.authMiddleware(s.handleUnpinMessage))
	mux.HandleFunc("/api/messages/delete", s.authMiddleware(s.handleDeleteMessage))

	// Stats
	mux.HandleFunc("/api/stats", s.authMiddleware(s.handleStats))

	// Users (Telegram users in chats, not auth users)
	mux.HandleFunc("/api/users/list", s.authMiddleware(s.handleListUsers))
	mux.HandleFunc("/api/users/ban", s.authMiddleware(s.handleBanUser))
	mux.HandleFunc("/api/users/unban", s.authMiddleware(s.handleUnbanUser))

	// Admins (Telegram chat admins)
	mux.HandleFunc("/api/admins", s.authMiddleware(s.handleGetAdmins))
	mux.HandleFunc("/api/admins/promote", s.authMiddleware(s.handlePromoteAdmin))
	mux.HandleFunc("/api/admins/demote", s.authMiddleware(s.handleDemoteAdmin))
	mux.HandleFunc("/api/admins/title", s.authMiddleware(s.handleSetAdminTitle))

	// Admin log
	mux.HandleFunc("/api/adminlog", s.authMiddleware(s.handleAdminLog))

	// Routes
	mux.HandleFunc("/api/routes", s.authMiddleware(s.handleGetRoutes))
	mux.HandleFunc("/api/routes/add", s.adminOnly(s.handleAddRoute))
	mux.HandleFunc("/api/routes/update", s.adminOnly(s.handleUpdateRoute))
	mux.HandleFunc("/api/routes/delete", s.adminOnly(s.handleDeleteRoute))

	// Bridges — admin only for management, no auth for incoming webhook
	mux.HandleFunc("/api/bridges", s.adminOnly(s.handleBridgeList))
	mux.HandleFunc("/api/bridges/add", s.adminOnly(s.handleBridgeAdd))
	mux.HandleFunc("/api/bridges/update", s.adminOnly(s.handleBridgeUpdate))
	mux.HandleFunc("/api/bridges/delete", s.adminOnly(s.handleBridgeDelete))
	mux.HandleFunc("/bridge/", s.handleBridgeIncoming) // no auth — external services POST here

	// LLM routing config — admin only
	mux.HandleFunc("/api/llm-config", s.authMiddleware(s.handleGetLLMConfig))
	mux.HandleFunc("/api/llm-config/save", s.adminOnly(s.handleSaveLLMConfig))
	mux.HandleFunc("/api/bots/description", s.authMiddleware(s.handleBotDescription))

	// Long polling for updates — auth required
	mux.HandleFunc("/api/updates/poll", s.authMiddleware(s.handleUpdatesPoll))

	// Message stream (SSE) — auth required
	mux.HandleFunc("/api/messages/stream", s.authMiddleware(s.handleMessageStream))

	// Application logs — admin only
	mux.HandleFunc("/api/logs", s.adminOnly(s.handleLogs))
	mux.HandleFunc("/api/logs/stream", s.adminOnly(s.handleLogStream))

	// Media proxy — auth required
	mux.HandleFunc("/api/media", s.authMiddleware(s.handleMediaProxy))

	// User tags
	mux.HandleFunc("/api/tags", s.authMiddleware(s.handleGetTags))
	mux.HandleFunc("/api/tags/add", s.authMiddleware(s.handleAddTag))
	mux.HandleFunc("/api/tags/remove", s.authMiddleware(s.handleRemoveTag))
	mux.HandleFunc("/api/tags/user", s.authMiddleware(s.handleGetUserTags))

	return mux
}

func (s *Server) Start(addr string) error {
	mux := s.BuildMux()
	log.Printf("Web interface at http://%s", addr)
	return http.ListenAndServe(addr, mux)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data, err := templateFS.ReadFile("templates/index.html")
	if err != nil {
		http.Error(w, "Template not found", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// Bot management handlers

// handleBotList returns the list of bots accessible to the current user.
// @Summary List bots
// @Description Returns all bots for admin users, or only assigned bots for regular users. Each entry includes a running status flag.
// @Tags bots
// @Produce json
// @Success 200 {array} BotConfig
// @Failure 500 {object} map[string]string
// @Router /api/bots [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleBotList(w http.ResponseWriter, r *http.Request) {
	user := getAuthUser(r)
	var bots []BotConfig
	var err error
	if user.Role == "admin" {
		bots, err = s.store.GetBotConfigs()
	} else {
		bots, err = s.store.GetBotConfigsForUser(user.ID)
	}
	if err != nil {
		writeError(w, err)
		return
	}
	if bots == nil {
		bots = []BotConfig{}
	}
	type BotStatus struct {
		BotConfig
		Running      bool  `json:"running"`
		BotTelegramID int64 `json:"bot_telegram_id,omitempty"`
	}
	var result []BotStatus
	for _, b := range bots {
		bs := BotStatus{BotConfig: b, Running: s.proxy.IsRunning(b.ID)}
		if mb := s.proxy.GetManagedBot(b.ID); mb != nil {
			bs.BotTelegramID = mb.GetSelfID()
		}
		result = append(result, bs)
	}
	if result == nil {
		result = []BotStatus{}
	}
	writeJSON(w, result)
}

// handleBotAdd registers a new bot in the database and starts it.
// @Summary Add bot
// @Description Creates a new bot configuration. Deletes any existing webhook before starting polling. Admin only.
// @Tags bots
// @Accept json
// @Produce json
// @Param bot body BotConfig true "Bot configuration"
// @Success 200 {object} map[string]interface{}
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/bots/add [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleBotAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req BotConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	if req.PollingTimeout <= 0 {
		req.PollingTimeout = 30
	}

	// Validate token and auto-populate bot_username
	username, err := s.proxy.ValidateToken(req.Token)
	if err != nil {
		writeError(w, fmt.Errorf("invalid bot token: %w", err))
		return
	}
	req.BotUsername = username

	// Delete webhook before starting polling
	if req.ManageEnabled || req.ProxyEnabled {
		if err := s.proxy.DeleteWebhook(req.Token); err != nil {
			writeError(w, err)
			return
		}
	}

	id, err := s.store.AddBotConfig(req)
	if err != nil {
		writeError(w, err)
		return
	}

	// Create managed Bot instance if needed
	if req.ManageEnabled {
		managedBot, err := NewBot(req.Token, s.store, id)
		if err != nil {
			writeError(w, err)
			return
		}
		if s.bridge != nil {
			s.bridge.InstallHookOnBot(managedBot)
		}
		s.proxy.RegisterManagedBot(id, managedBot)
	}

	if req.ManageEnabled || req.ProxyEnabled {
		s.proxy.RestartBot(id)
	}
	writeJSON(w, map[string]interface{}{"status": "ok", "id": id})
}

// handleBotUpdate updates an existing bot configuration and restarts it.
// @Summary Update bot
// @Description Updates bot settings. Preserves the source field from the existing record. Admin only.
// @Tags bots
// @Accept json
// @Produce json
// @Param bot body BotConfig true "Bot configuration (must include id)"
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/bots/update [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleBotUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req BotConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	if req.PollingTimeout <= 0 {
		req.PollingTimeout = 30
	}

	// Preserve the source field
	existing, err := s.store.GetBotConfig(req.ID)
	if err != nil {
		writeError(w, err)
		return
	}
	req.Source = existing.Source

	// Re-validate token and update bot_username if token changed
	if req.Token != existing.Token || req.BotUsername == "" {
		username, err := s.proxy.ValidateToken(req.Token)
		if err != nil {
			writeError(w, fmt.Errorf("invalid bot token: %w", err))
			return
		}
		req.BotUsername = username
	}

	if err := s.store.UpdateBotConfig(req); err != nil {
		writeError(w, err)
		return
	}

	// Restart via proxy manager (works for all bots)
	s.proxy.RestartBot(req.ID)
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleBotDelete stops and removes a bot from the database.
// @Summary Delete bot
// @Description Stops the bot, unregisters it from the proxy manager, and deletes its DB record. Admin only.
// @Tags bots
// @Produce json
// @Param id query int true "Bot ID"
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/bots/delete [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleBotDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)

	s.proxy.stopBot(id)
	s.proxy.UnregisterManagedBot(id)
	if err := s.store.DeleteBotConfig(id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleBotValidate validates a Telegram bot token and returns the bot username.
// @Summary Validate bot token
// @Description Calls Telegram API to verify the token and returns the associated bot username. Admin only.
// @Tags bots
// @Produce json
// @Param token query string true "Telegram bot token"
// @Success 200 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/bots/validate [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleBotValidate(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	username, err := s.proxy.ValidateToken(token)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"username": username})
}

// handleBotHealth checks and stores the backend health status for a bot.
// @Summary Check bot backend health
// @Description Performs a health check against the bot's configured backend URL and stores the result.
// @Tags bots
// @Produce json
// @Param id query int true "Bot ID"
// @Success 200 {object} map[string]interface{}
// @Failure 500 {object} map[string]interface{}
// @Router /api/bots/health [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleBotHealth(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	status, err := s.proxy.CheckAndStoreHealth(id)
	if err != nil {
		writeJSON(w, map[string]interface{}{"status": status, "error": err.Error(), "checked_at": time.Now().Format(time.RFC3339)})
		return
	}
	writeJSON(w, map[string]interface{}{"status": status, "checked_at": time.Now().Format(time.RFC3339)})
}

// Chat handlers

// handleChats returns all chats tracked by a given bot.
// @Summary List chats
// @Description Returns all chats stored in the database for the specified bot.
// @Tags chats
// @Produce json
// @Param bot_id query int true "Bot ID"
// @Success 200 {array} Chat
// @Failure 500 {object} map[string]string
// @Router /api/chats [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleChats(w http.ResponseWriter, r *http.Request) {
	botID, _ := strconv.ParseInt(r.URL.Query().Get("bot_id"), 10, 64)
	chats, err := s.store.GetChats(botID)
	if err != nil {
		writeError(w, err)
		return
	}
	if chats == nil {
		chats = []Chat{}
	}
	writeJSON(w, chats)
}

// handleRefreshChat fetches fresh chat metadata from Telegram and updates the database.
// @Summary Refresh chat info
// @Description Calls Telegram API to refresh the chat's title, member count, and other metadata.
// @Tags chats
// @Produce json
// @Param bot_id query int true "Bot ID"
// @Param chat_id query int true "Chat ID"
// @Success 200 {object} Chat
// @Failure 500 {object} map[string]string
// @Router /api/chats/refresh [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleRefreshChat(w http.ResponseWriter, r *http.Request) {
	bot, _, err := s.getBotFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	chatID, err := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	if err != nil {
		writeError(w, err)
		return
	}
	chat, err := bot.RefreshChat(chatID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, chat)
}

// handleDeleteChat removes a chat and all its messages from the database.
// @Summary Delete chat
// @Description Deletes the chat record (and associated messages) from the local database. Does not leave the Telegram chat.
// @Tags chats
// @Produce json
// @Param bot_id query int true "Bot ID"
// @Param chat_id query int true "Chat ID"
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/chats/delete [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleDeleteChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	botID, _ := strconv.ParseInt(r.URL.Query().Get("bot_id"), 10, 64)
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	if err := s.store.DeleteChat(botID, chatID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// Message handlers

// handleMessages returns paginated messages for a chat.
// @Summary List messages
// @Description Returns stored messages for the given chat with pagination support (default limit 50).
// @Tags messages
// @Produce json
// @Param chat_id query int true "Chat ID"
// @Param limit query int false "Max number of messages to return (default 50)"
// @Param offset query int false "Number of messages to skip"
// @Success 200 {array} Message
// @Failure 500 {object} map[string]string
// @Router /api/messages [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	botID, _ := strconv.ParseInt(r.URL.Query().Get("bot_id"), 10, 64)
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit == 0 {
		limit = 50
	}
	msgs, err := s.store.GetMessages(botID, chatID, limit, offset)
	if err != nil {
		writeError(w, err)
		return
	}
	if msgs == nil {
		msgs = []Message{}
	}
	writeJSON(w, msgs)
}

// handleSearchMessages performs a full-text search over messages in a chat.
// @Summary Search messages
// @Description Searches stored messages in the given chat by text query. Returns up to 50 results.
// @Tags messages
// @Produce json
// @Param chat_id query int true "Chat ID"
// @Param q query string true "Search query"
// @Success 200 {array} Message
// @Failure 500 {object} map[string]string
// @Router /api/messages/search [get]
// @Security CookieAuth || BearerAuth
// handleMessageStream sends new messages via Server-Sent Events
func (s *Server) handleMessageStream(w http.ResponseWriter, r *http.Request) {
	botID, _ := strconv.ParseInt(r.URL.Query().Get("bot_id"), 10, 64)
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	if chatID == 0 {
		http.Error(w, "chat_id required", 400)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	ch := s.store.Subscribe()
	defer s.store.Unsubscribe(ch)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-ch:
			if msg.BotID != botID || msg.ChatID != chatID {
				continue
			}
			data, err := json.Marshal(msg)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// handleUpdatesPoll serves raw Telegram updates via long polling (pull mode).
// Compatible with Telegram Bot API getUpdates response format.
// @Summary Long poll for updates
// @Description Returns raw Telegram updates for a bot using long polling. Response format is compatible with Telegram getUpdates.
// @Tags updates
// @Produce json
// @Param bot_id query int true "Bot ID"
// @Param offset query int false "Return updates with update_id >= offset"
// @Param limit query int false "Max updates to return (default 100, max 100)"
// @Param timeout query int false "Long polling timeout in seconds (default 0, max 60)"
// @Success 200 {object} map[string]interface{} "Telegram-compatible response: {ok: true, result: [...]}"
// @Failure 400 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Router /api/updates/poll [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleUpdatesPoll(w http.ResponseWriter, r *http.Request) {
	botID, err := strconv.ParseInt(r.URL.Query().Get("bot_id"), 10, 64)
	if err != nil || botID == 0 {
		w.WriteHeader(400)
		writeJSON(w, map[string]interface{}{"ok": false, "description": "bot_id is required"})
		return
	}

	if !s.checkBotAccess(r, botID) {
		w.WriteHeader(403)
		writeJSON(w, map[string]interface{}{"ok": false, "description": "Forbidden"})
		return
	}

	botCfg, err := s.store.GetBotConfig(botID)
	if err != nil {
		w.WriteHeader(404)
		writeJSON(w, map[string]interface{}{"ok": false, "description": "bot not found"})
		return
	}
	if !botCfg.LongPollEnabled {
		w.WriteHeader(400)
		writeJSON(w, map[string]interface{}{"ok": false, "description": "long polling is not enabled for this bot"})
		return
	}

	s.handleLongPollGetUpdates(w, r, botCfg)
}

// longPollResponse formats updates in Telegram-compatible getUpdates response format.
func longPollResponse(updates []QueuedUpdate) map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(updates))
	for _, u := range updates {
		result = append(result, u.Data)
	}
	return map[string]interface{}{
		"ok":     true,
		"result": result,
	}
}

// handleLongPollGetUpdates serves updates from UpdateQueue for /tgapi/bot{TOKEN}/getUpdates.
// No auth required — the bot token in the URL is the authorization (same as Telegram API).
func (s *Server) handleLongPollGetUpdates(w http.ResponseWriter, r *http.Request, botCfg *BotConfig) {
	// Parse params from query string (GET) or form body (POST) — Telegram supports both
	var offsetStr, limitStr, timeoutStr string
	if r.Method == http.MethodPost {
		// Try JSON body first, then form values
		if r.Header.Get("Content-Type") == "application/json" {
			var body map[string]interface{}
			if json.NewDecoder(r.Body).Decode(&body) == nil {
				if v, ok := body["offset"]; ok {
					offsetStr = fmt.Sprintf("%v", v)
				}
				if v, ok := body["limit"]; ok {
					limitStr = fmt.Sprintf("%v", v)
				}
				if v, ok := body["timeout"]; ok {
					timeoutStr = fmt.Sprintf("%v", v)
				}
			}
		} else {
			r.ParseForm()
			offsetStr = r.FormValue("offset")
			limitStr = r.FormValue("limit")
			timeoutStr = r.FormValue("timeout")
		}
	}
	// Query params take precedence (or fallback for GET)
	if v := r.URL.Query().Get("offset"); v != "" {
		offsetStr = v
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		limitStr = v
	}
	if v := r.URL.Query().Get("timeout"); v != "" {
		timeoutStr = v
	}

	offset, _ := strconv.ParseInt(offsetStr, 10, 64)
	limit, _ := strconv.Atoi(limitStr)
	timeout, _ := strconv.Atoi(timeoutStr)

	if limit <= 0 || limit > 100 {
		limit = 100
	}
	if timeout < 0 {
		timeout = 0
	}
	if timeout > 60 {
		timeout = 60
	}

	queue := s.proxy.GetOrCreateUpdateQueue(botCfg.ID)

	updates := queue.Get(offset, limit)
	if len(updates) > 0 || timeout == 0 {
		writeJSON(w, longPollResponse(updates))
		return
	}

	ctx := r.Context()
	timer := time.NewTimer(time.Duration(timeout) * time.Second)
	defer timer.Stop()
	notify := queue.Wait(ctx)

	select {
	case <-ctx.Done():
		writeJSON(w, longPollResponse(nil))
	case <-timer.C:
		updates = queue.Get(offset, limit)
		writeJSON(w, longPollResponse(updates))
	case <-notify:
		time.Sleep(50 * time.Millisecond)
		updates = queue.Get(offset, limit)
		writeJSON(w, longPollResponse(updates))
	}
}

func (s *Server) handleSearchMessages(w http.ResponseWriter, r *http.Request) {
	botID, _ := strconv.ParseInt(r.URL.Query().Get("bot_id"), 10, 64)
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	query := r.URL.Query().Get("q")
	msgs, err := s.store.SearchMessages(botID, chatID, query, 50)
	if err != nil {
		writeError(w, err)
		return
	}
	if msgs == nil {
		msgs = []Message{}
	}
	writeJSON(w, msgs)
}

// handleSendMessage sends a text message to a Telegram chat via the specified bot.
// @Summary Send message
// @Description Sends a text message (optionally as a reply) to the given chat using the specified bot.
// @Tags messages
// @Accept json
// @Produce json
// @Param body body object true "Send request" SchemaExample({"bot_id":1,"chat_id":-1001234567890,"text":"Hello","reply_to_message_id":0})
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/messages/send [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		BotID            int64  `json:"bot_id"`
		ChatID           int64  `json:"chat_id"`
		Text             string `json:"text"`
		ReplyToMessageID int    `json:"reply_to_message_id,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	bot := s.resolveBot(req.BotID)
	if bot == nil {
		writeError(w, fmt.Errorf("bot not found"))
		return
	}
	if req.ReplyToMessageID > 0 {
		if _, err := bot.SendMessageReply(req.ChatID, req.Text, req.ReplyToMessageID); err != nil {
			writeError(w, err)
			return
		}
	} else {
		if err := bot.SendMessage(req.ChatID, req.Text); err != nil {
			writeError(w, err)
			return
		}
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handlePinMessage pins a message in a Telegram chat.
// @Summary Pin message
// @Description Pins the specified message in the chat and logs the action to the admin log.
// @Tags messages
// @Produce json
// @Param bot_id query int true "Bot ID"
// @Param chat_id query int true "Chat ID"
// @Param message_id query int true "Message ID to pin"
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/messages/pin [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handlePinMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	bot, _, err := s.getBotFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	msgID, _ := strconv.Atoi(r.URL.Query().Get("message_id"))
	if err := bot.PinMessage(chatID, msgID); err != nil {
		writeError(w, err)
		return
	}
	s.store.LogAdminAction(AdminLog{
		ChatID: chatID, Action: "pin_message", ActorName: bot.GetBotName(),
		Details: "Message ID: " + strconv.Itoa(msgID), CreatedAt: time.Now().Format(time.RFC3339),
	})
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleUnpinMessage unpins a message in a Telegram chat.
// @Summary Unpin message
// @Description Unpins the specified message in the chat.
// @Tags messages
// @Produce json
// @Param bot_id query int true "Bot ID"
// @Param chat_id query int true "Chat ID"
// @Param message_id query int true "Message ID to unpin"
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/messages/unpin [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleUnpinMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	bot, _, err := s.getBotFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	msgID, _ := strconv.Atoi(r.URL.Query().Get("message_id"))
	if err := bot.UnpinMessage(chatID, msgID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleDeleteMessage deletes a message from Telegram and marks it deleted in the database.
// @Summary Delete message
// @Description Deletes the specified message from the Telegram chat and marks it as deleted in the local store. Logs the action.
// @Tags messages
// @Produce json
// @Param bot_id query int true "Bot ID"
// @Param chat_id query int true "Chat ID"
// @Param message_id query int true "Message ID to delete"
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/messages/delete [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	bot, botID, err := s.getBotFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	msgID, _ := strconv.Atoi(r.URL.Query().Get("message_id"))
	if err := bot.DeleteMessage(chatID, msgID); err != nil {
		writeError(w, err)
		return
	}
	s.store.MarkMessageDeleted(botID, chatID, msgID)
	s.store.LogAdminAction(AdminLog{
		ChatID: chatID, Action: "delete_message", ActorName: bot.GetBotName(),
		Details: "Message ID: " + strconv.Itoa(msgID), CreatedAt: time.Now().Format(time.RFC3339),
	})
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleStats returns message statistics for a chat.
// @Summary Get chat statistics
// @Description Returns total messages, today's messages, active users, top users, and hourly stats for the specified chat.
// @Tags stats
// @Produce json
// @Param chat_id query int true "Chat ID"
// @Success 200 {object} ChatStats
// @Failure 500 {object} map[string]string
// @Router /api/stats [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	botID, _ := strconv.ParseInt(r.URL.Query().Get("bot_id"), 10, 64)
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	stats, err := s.store.GetChatStats(botID, chatID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, stats)
}

// handleListUsers returns users seen in a chat with optional search and pagination.
// @Summary List chat users
// @Description Returns Telegram users who have sent messages in the given chat. Supports text search and pagination.
// @Tags users
// @Produce json
// @Param chat_id query int true "Chat ID"
// @Param q query string false "Search query (matches username)"
// @Param limit query int false "Max results (default 50)"
// @Param offset query int false "Number of results to skip"
// @Success 200 {array} ChatUser
// @Failure 500 {object} map[string]string
// @Router /api/users/list [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	search := r.URL.Query().Get("q")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit == 0 {
		limit = 50
	}
	users, err := s.store.GetChatUsers(chatID, search, limit, offset)
	if err != nil {
		writeError(w, err)
		return
	}
	if users == nil {
		users = []ChatUser{}
	}
	writeJSON(w, users)
}

// handleBanUser bans a user from a Telegram chat.
// @Summary Ban user
// @Description Bans the specified user from the chat via Telegram API and logs the action.
// @Tags users
// @Produce json
// @Param bot_id query int true "Bot ID"
// @Param chat_id query int true "Chat ID"
// @Param user_id query int true "Telegram user ID to ban"
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/users/ban [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleBanUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	bot, _, err := s.getBotFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	userID, _ := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	if err := bot.BanUser(chatID, userID); err != nil {
		writeError(w, err)
		return
	}
	s.store.LogAdminAction(AdminLog{
		ChatID: chatID, Action: "ban_user", ActorName: bot.GetBotName(),
		TargetID: userID, CreatedAt: time.Now().Format(time.RFC3339),
	})
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleUnbanUser unbans a user from a Telegram chat.
// @Summary Unban user
// @Description Removes the ban for the specified user in the chat via Telegram API and logs the action.
// @Tags users
// @Produce json
// @Param bot_id query int true "Bot ID"
// @Param chat_id query int true "Chat ID"
// @Param user_id query int true "Telegram user ID to unban"
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/users/unban [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleUnbanUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	bot, _, err := s.getBotFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	userID, _ := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	if err := bot.UnbanUser(chatID, userID); err != nil {
		writeError(w, err)
		return
	}
	s.store.LogAdminAction(AdminLog{
		ChatID: chatID, Action: "unban_user", ActorName: bot.GetBotName(),
		TargetID: userID, CreatedAt: time.Now().Format(time.RFC3339),
	})
	writeJSON(w, map[string]string{"status": "ok"})
}

// Admin handlers

// handleGetAdmins returns the list of administrators for a Telegram chat.
// @Summary Get chat admins
// @Description Fetches the current admin list for the specified chat from Telegram API.
// @Tags admins
// @Produce json
// @Param bot_id query int true "Bot ID"
// @Param chat_id query int true "Chat ID"
// @Success 200 {array} AdminInfo
// @Failure 500 {object} map[string]string
// @Router /api/admins [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleGetAdmins(w http.ResponseWriter, r *http.Request) {
	bot, _, err := s.getBotFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	admins, err := bot.GetAdmins(chatID)
	if err != nil {
		writeError(w, err)
		return
	}
	if admins == nil {
		admins = []AdminInfo{}
	}
	writeJSON(w, admins)
}

// handlePromoteAdmin promotes a user to admin with specified permissions.
// @Summary Promote admin
// @Description Grants admin rights to a user in the specified chat with the given permission set. Logs the action.
// @Tags admins
// @Accept json
// @Produce json
// @Param body body object true "Promote request" SchemaExample({"bot_id":1,"chat_id":-1001234567890,"user_id":123456,"perms":{}})
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/admins/promote [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handlePromoteAdmin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		BotID  int64     `json:"bot_id"`
		ChatID int64     `json:"chat_id"`
		UserID int64     `json:"user_id"`
		Perms  AdminInfo `json:"perms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	bot := s.resolveBot(req.BotID)
	if bot == nil {
		writeError(w, fmt.Errorf("bot not found"))
		return
	}
	if err := bot.PromoteAdmin(req.ChatID, req.UserID, req.Perms); err != nil {
		writeError(w, err)
		return
	}
	s.store.LogAdminAction(AdminLog{
		ChatID: req.ChatID, Action: "promote_admin", ActorName: bot.GetBotName(),
		TargetID: req.UserID, Details: "Promoted via web UI", CreatedAt: time.Now().Format(time.RFC3339),
	})
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleDemoteAdmin removes admin rights from a user in a Telegram chat.
// @Summary Demote admin
// @Description Revokes admin status from the specified user. Logs the action.
// @Tags admins
// @Produce json
// @Param bot_id query int true "Bot ID"
// @Param chat_id query int true "Chat ID"
// @Param user_id query int true "Telegram user ID to demote"
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/admins/demote [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleDemoteAdmin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	bot, _, err := s.getBotFromRequest(r)
	if err != nil {
		writeError(w, err)
		return
	}
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	userID, _ := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	if err := bot.DemoteAdmin(chatID, userID); err != nil {
		writeError(w, err)
		return
	}
	s.store.LogAdminAction(AdminLog{
		ChatID: chatID, Action: "demote_admin", ActorName: bot.GetBotName(),
		TargetID: userID, Details: "Demoted via web UI", CreatedAt: time.Now().Format(time.RFC3339),
	})
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleSetAdminTitle sets a custom title for an admin in a Telegram chat.
// @Summary Set admin title
// @Description Sets the custom title (badge) displayed next to an admin's name in the chat. Logs the action.
// @Tags admins
// @Accept json
// @Produce json
// @Param body body object true "Set title request" SchemaExample({"bot_id":1,"chat_id":-1001234567890,"user_id":123456,"title":"Moderator"})
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/admins/title [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleSetAdminTitle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		BotID  int64  `json:"bot_id"`
		ChatID int64  `json:"chat_id"`
		UserID int64  `json:"user_id"`
		Title  string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	bot := s.resolveBot(req.BotID)
	if bot == nil {
		writeError(w, fmt.Errorf("bot not found"))
		return
	}
	if err := bot.SetAdminTitle(req.ChatID, req.UserID, req.Title); err != nil {
		writeError(w, err)
		return
	}
	s.store.LogAdminAction(AdminLog{
		ChatID: req.ChatID, Action: "set_admin_title", ActorName: bot.GetBotName(),
		TargetID: req.UserID, Details: "Title: " + req.Title, CreatedAt: time.Now().Format(time.RFC3339),
	})
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleAdminLog returns paginated admin action log entries for a chat.
// @Summary Get admin log
// @Description Returns the history of admin actions (ban, unban, pin, delete, promote, etc.) for the given chat.
// @Tags admins
// @Produce json
// @Param chat_id query int true "Chat ID"
// @Param limit query int false "Max results (default 50)"
// @Param offset query int false "Number of results to skip"
// @Success 200 {array} AdminLog
// @Failure 500 {object} map[string]string
// @Router /api/adminlog [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleAdminLog(w http.ResponseWriter, r *http.Request) {
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit == 0 {
		limit = 50
	}
	logs, err := s.store.GetAdminLog(chatID, limit, offset)
	if err != nil {
		writeError(w, err)
		return
	}
	if logs == nil {
		logs = []AdminLog{}
	}
	writeJSON(w, logs)
}

// Tag handlers

// handleGetTags returns all user tags for a chat.
// @Summary Get tags
// @Description Returns all user tags assigned within the given chat.
// @Tags tags
// @Produce json
// @Param chat_id query int true "Chat ID"
// @Success 200 {array} UserTag
// @Failure 500 {object} map[string]string
// @Router /api/tags [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleGetTags(w http.ResponseWriter, r *http.Request) {
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	tags, err := s.store.GetUserTags(chatID)
	if err != nil {
		writeError(w, err)
		return
	}
	if tags == nil {
		tags = []UserTag{}
	}
	writeJSON(w, tags)
}

// handleAddTag adds a tag to a user in a chat.
// @Summary Add user tag
// @Description Assigns a colored tag label to a Telegram user within a chat. Logs the action.
// @Tags tags
// @Accept json
// @Produce json
// @Param body body object true "Tag request" SchemaExample({"bot_id":1,"chat_id":-1001234567890,"user_id":123456,"username":"user","tag":"VIP","color":"#ff0000"})
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/tags/add [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleAddTag(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		BotID int64 `json:"bot_id"`
		UserTag
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.AddUserTag(req.UserTag); err != nil {
		writeError(w, err)
		return
	}
	actorName := "Web UI"
	if bot := s.resolveBot(req.BotID); bot != nil {
		actorName = bot.GetBotName()
	}
	s.store.LogAdminAction(AdminLog{
		ChatID: req.ChatID, Action: "add_tag", ActorName: actorName,
		TargetID: req.UserID, TargetName: req.Username,
		Details: "Tag: " + req.Tag, CreatedAt: time.Now().Format(time.RFC3339),
	})
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleRemoveTag removes a tag by its ID.
// @Summary Remove user tag
// @Description Deletes a specific user tag by its database ID.
// @Tags tags
// @Produce json
// @Param id query int true "Tag ID"
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/tags/remove [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleRemoveTag(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	tagID, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err := s.store.RemoveUserTag(tagID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleGetUserTags returns all tags assigned to a specific user in a chat.
// @Summary Get tags for a user
// @Description Returns tags for the given user within the specified chat.
// @Tags tags
// @Produce json
// @Param chat_id query int true "Chat ID"
// @Param user_id query int true "Telegram user ID"
// @Success 200 {array} UserTag
// @Failure 500 {object} map[string]string
// @Router /api/tags/user [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleGetUserTags(w http.ResponseWriter, r *http.Request) {
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	userID, _ := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	tags, err := s.store.GetUserTagsByUser(chatID, userID)
	if err != nil {
		writeError(w, err)
		return
	}
	if tags == nil {
		tags = []UserTag{}
	}
	writeJSON(w, tags)
}

// Route handlers

// handleGetRoutes returns all routing rules for a bot.
// @Summary List routes
// @Description Returns all message routing rules associated with the specified source bot.
// @Tags routes
// @Produce json
// @Param bot_id query int true "Source bot ID"
// @Success 200 {array} Route
// @Failure 500 {object} map[string]string
// @Router /api/routes [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleGetRoutes(w http.ResponseWriter, r *http.Request) {
	botID, _ := strconv.ParseInt(r.URL.Query().Get("bot_id"), 10, 64)
	routes, err := s.store.GetRoutes(botID)
	if err != nil {
		writeError(w, err)
		return
	}
	if routes == nil {
		routes = []Route{}
	}
	writeJSON(w, routes)
}

// handleAddRoute creates a new message routing rule.
// @Summary Add route
// @Description Creates a routing rule that forwards or copies messages matching conditions from one bot/chat to another. Admin only.
// @Tags routes
// @Accept json
// @Produce json
// @Param route body Route true "Route definition"
// @Success 200 {object} map[string]interface{}
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/routes/add [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleAddRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req Route
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	req.CreatedAt = time.Now().Format(time.RFC3339)
	id, err := s.store.AddRoute(req)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]interface{}{"status": "ok", "id": id})
}

// handleUpdateRoute updates an existing routing rule.
// @Summary Update route
// @Description Updates a routing rule by ID. Admin only.
// @Tags routes
// @Accept json
// @Produce json
// @Param route body Route true "Route definition (must include id)"
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/routes/update [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleUpdateRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req Route
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.UpdateRoute(req); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleDeleteRoute deletes a routing rule by ID.
// @Summary Delete route
// @Description Removes the specified routing rule. Admin only.
// @Tags routes
// @Produce json
// @Param id query int true "Route ID"
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/routes/delete [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleDeleteRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err := s.store.DeleteRoute(id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// Bridge handlers

// handleBridgeList returns all bridges, optionally filtered by bot_id.
// @Summary List bridges
// @Description Returns all protocol bridges. Filter by bot_id query param to get bridges for a specific bot.
// @Tags bridges
// @Produce json
// @Param bot_id query int false "Filter by linked bot ID"
// @Success 200 {array} BridgeConfig
// @Router /api/bridges [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleBridgeList(w http.ResponseWriter, r *http.Request) {
	botIDStr := r.URL.Query().Get("bot_id")
	if botIDStr != "" {
		botID, _ := strconv.ParseInt(botIDStr, 10, 64)
		bridges, err := s.store.GetBridgesForBot(botID)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, bridges)
		return
	}
	bridges, err := s.store.GetBridges()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, bridges)
}

// handleBridgeAdd creates a new bridge.
// @Summary Add bridge
// @Description Creates a new protocol bridge linked to a Telegram bot. Admin only.
// @Tags bridges
// @Accept json
// @Produce json
// @Param bridge body BridgeConfig true "Bridge configuration"
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Router /api/bridges/add [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleBridgeAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req BridgeConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	if req.Name == "" || req.LinkedBotID == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"name and linked_bot_id required"}`))
		return
	}
	if req.Protocol == "" {
		req.Protocol = "webhook"
	}
	id, err := s.store.AddBridge(req)
	if err != nil {
		writeError(w, err)
		return
	}
	if s.bridge != nil {
		s.bridge.Reload(id)
	}
	writeJSON(w, map[string]interface{}{"status": "ok", "id": id})
}

// handleBridgeUpdate updates an existing bridge.
// @Summary Update bridge
// @Description Updates bridge settings. Admin only.
// @Tags bridges
// @Accept json
// @Produce json
// @Param bridge body BridgeConfig true "Bridge configuration (must include id)"
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Router /api/bridges/update [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleBridgeUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req BridgeConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.UpdateBridge(req); err != nil {
		writeError(w, err)
		return
	}
	if s.bridge != nil {
		s.bridge.Reload(req.ID)
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleBridgeDelete removes a bridge and its mappings.
// @Summary Delete bridge
// @Description Deletes a bridge and all its chat/message mappings. Admin only.
// @Tags bridges
// @Produce json
// @Param id query int true "Bridge ID"
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Router /api/bridges/delete [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleBridgeDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if s.bridge != nil {
		s.bridge.Remove(id)
	}
	if err := s.store.DeleteBridge(id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleBridgeIncoming receives messages from external protocol bridges.
// @Summary Bridge incoming webhook
// @Description Receives a message from an external protocol and injects it as a Telegram update. URL format: /bridge/{id}/incoming. No authentication — bridges use the URL as a secret. For Slack bridges, handles Events API payloads (including url_verification challenge) and verifies request signatures. For webhook bridges, expects BridgeIncomingMessage JSON.
// @Tags bridges
// @Accept json
// @Produce json
// @Param id path int true "Bridge ID"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Failure 401 {object} map[string]string "Invalid Slack signature"
// @Failure 404 {object} map[string]string
// @Router /bridge/{id}/incoming [post]
func (s *Server) handleBridgeIncoming(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}

	// Parse bridge ID from URL: /bridge/{id}/incoming
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 || parts[2] != "incoming" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		w.Write([]byte(`{"error":"invalid bridge URL, use /bridge/{id}/incoming"}`))
		return
	}
	bridgeID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid bridge ID"}`))
		return
	}

	if s.bridge == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		w.Write([]byte(`{"error":"bridge manager not initialized"}`))
		return
	}

	// Read body once (needed for Slack signature verification)
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"failed to read body"}`))
		return
	}

	// Check if this is a Slack bridge — route to Slack handler
	cfg := s.bridge.GetBridge(bridgeID)
	if cfg != nil && isSlackBridge(cfg) {
		respBody, contentType, status, err := s.bridge.HandleSlackEvent(bridgeID, r.Header, body)
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		w.Write(respBody)
		return
	}

	// Generic webhook bridge — parse as BridgeIncomingMessage
	var msg BridgeIncomingMessage
	if err := json.Unmarshal(body, &msg); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"invalid JSON"}`))
		return
	}

	if err := s.bridge.HandleIncoming(bridgeID, msg); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, map[string]string{"status": "ok"})
}

// LLM config handlers

// handleGetLLMConfig returns the current LLM routing configuration.
// @Summary Get LLM config
// @Description Returns the LLM routing configuration (API URL, model, system prompt, enabled flag). API key is included.
// @Tags llm
// @Produce json
// @Success 200 {object} LLMConfig
// @Router /api/llm-config [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleGetLLMConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.store.GetLLMConfig()
	if err != nil {
		writeJSON(w, LLMConfig{})
		return
	}
	writeJSON(w, cfg)
}

// handleSaveLLMConfig saves the LLM routing configuration.
// @Summary Save LLM config
// @Description Persists the LLM routing configuration. Admin only.
// @Tags llm
// @Accept json
// @Produce json
// @Param config body LLMConfig true "LLM configuration"
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/llm-config/save [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleSaveLLMConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var cfg LLMConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.SaveLLMConfig(cfg); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleBotDescription gets or sets the LLM description for a bot.
// @Summary Get or set bot description
// @Description GET returns the bot's description used for LLM routing. POST updates it.
// @Tags bots
// @Accept json
// @Produce json
// @Param bot_id query int false "Bot ID (GET only)"
// @Param body body object false "Description update (POST only)" SchemaExample({"bot_id":1,"description":"Support bot for channel X"})
// @Success 200 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Router /api/bots/description [get]
// @Router /api/bots/description [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleBotDescription(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req struct {
			BotID       int64  `json:"bot_id"`
			Description string `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, err)
			return
		}
		if err := s.store.UpdateBotDescription(req.BotID, req.Description); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}
	botID, _ := strconv.ParseInt(r.URL.Query().Get("bot_id"), 10, 64)
	desc, err := s.store.GetBotDescription(botID)
	if err != nil {
		writeJSON(w, map[string]string{"description": ""})
		return
	}
	writeJSON(w, map[string]string{"description": desc})
}

// Auth handlers

// handleLogin authenticates a user and creates a session cookie.
// @Summary Login
// @Description Authenticates with username and password. Sets a session cookie on success.
// @Tags auth
// @Accept json
// @Produce json
// @Param credentials body object true "Login credentials" SchemaExample({"username":"admin","password":"secret"})
// @Success 200 {object} map[string]interface{}
// @Failure 401 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/auth/login [post]
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	user, err := s.store.GetUserByUsername(req.Username)
	if err != nil || user == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"invalid credentials"}`))
		return
	}
	if !CheckPassword(user.PasswordHash, req.Password) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"invalid credentials"}`))
		return
	}
	token, err := GenerateSessionToken()
	if err != nil {
		writeError(w, err)
		return
	}
	expiresAt := time.Now().Add(sessionDuration)
	if err := s.store.CreateSession(token, user.ID, expiresAt); err != nil {
		writeError(w, err)
		return
	}
	s.store.UpdateUserLastLogin(user.ID)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, map[string]interface{}{
		"status":               "ok",
		"user":                 user,
		"must_change_password": user.MustChangePassword,
	})
}

// handleLogout destroys the current session and clears the session cookie.
// @Summary Logout
// @Description Invalidates the current session token and clears the session cookie.
// @Tags auth
// @Produce json
// @Success 200 {object} map[string]string
// @Router /api/auth/logout [post]
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		s.store.DeleteSession(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleMe returns the currently authenticated user's profile.
// @Summary Get current user
// @Description Returns the authenticated user's ID, username, display name, role, and other profile fields.
// @Tags auth
// @Produce json
// @Success 200 {object} AuthUser
// @Failure 401 {object} map[string]string
// @Router /api/auth/me [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user := getAuthUser(r)
	if user == nil {
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}
	writeJSON(w, user)
}

// handleChangePassword changes the current user's password.
// @Summary Change password
// @Description Updates the authenticated user's password. If must_change_password is set, the old password check is skipped.
// @Tags auth
// @Accept json
// @Produce json
// @Param body body object true "Password change request" SchemaExample({"old_password":"old","new_password":"new"})
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Failure 401 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/auth/change-password [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	user := getAuthUser(r)
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	if len(req.NewPassword) < 4 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"password too short (min 4 characters)"}`))
		return
	}
	// For must_change_password, skip old password check
	if !user.MustChangePassword {
		dbUser, _ := s.store.GetUserByID(user.ID)
		if dbUser == nil || !CheckPassword(dbUser.PasswordHash, req.OldPassword) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			w.Write([]byte(`{"error":"invalid old password"}`))
			return
		}
	}
	hash, err := HashPassword(req.NewPassword)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.UpdateUserPassword(user.ID, hash); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// User management handlers (admin only)

// handleUserList returns all application users with their bot assignments.
// @Summary List app users
// @Description Returns all users registered in the application, each with their assigned bot IDs. Admin only.
// @Tags user-management
// @Produce json
// @Success 200 {array} object
// @Failure 500 {object} map[string]string
// @Router /api/auth/users [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleUserList(w http.ResponseWriter, r *http.Request) {
	users, err := s.store.GetAllUsers()
	if err != nil {
		writeError(w, err)
		return
	}
	if users == nil {
		users = []AuthUser{}
	}
	// Add bot IDs for each user
	type UserWithBots struct {
		AuthUser
		BotIDs []int64 `json:"bot_ids"`
	}
	var result []UserWithBots
	for _, u := range users {
		botIDs, _ := s.store.GetUserBotIDs(u.ID)
		if botIDs == nil {
			botIDs = []int64{}
		}
		result = append(result, UserWithBots{AuthUser: u, BotIDs: botIDs})
	}
	if result == nil {
		result = []UserWithBots{}
	}
	writeJSON(w, result)
}

// handleUserAdd creates a new application user.
// @Summary Add app user
// @Description Creates a new user with username, password, display name, role (admin/user), and optional bot assignments. Admin only.
// @Tags user-management
// @Accept json
// @Produce json
// @Param body body object true "User creation request" SchemaExample({"username":"alice","password":"pass","display_name":"Alice","role":"user","bot_ids":[1,2]})
// @Success 200 {object} map[string]interface{}
// @Failure 400 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/auth/users/add [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleUserAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		Username    string  `json:"username"`
		Password    string  `json:"password"`
		DisplayName string  `json:"display_name"`
		Role        string  `json:"role"`
		BotIDs      []int64 `json:"bot_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	if req.Username == "" || req.Password == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"username and password required"}`))
		return
	}
	if req.Role == "" {
		req.Role = "user"
	}
	if req.Role != "admin" && req.Role != "user" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"role must be admin or user"}`))
		return
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		writeError(w, err)
		return
	}
	id, err := s.store.CreateUser(req.Username, hash, req.DisplayName, req.Role)
	if err != nil {
		writeError(w, err)
		return
	}
	for _, botID := range req.BotIDs {
		s.store.AssignBotToUser(id, botID)
	}
	writeJSON(w, map[string]interface{}{"status": "ok", "id": id})
}

// handleUserUpdate updates an application user's display name, role, and bot assignments.
// @Summary Update app user
// @Description Updates the user's display name, role, and bot assignments. Cannot demote yourself. Admin only.
// @Tags user-management
// @Accept json
// @Produce json
// @Param body body object true "User update request" SchemaExample({"id":2,"display_name":"Alice","role":"user","bot_ids":[1]})
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/auth/users/update [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleUserUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		ID          int64   `json:"id"`
		DisplayName string  `json:"display_name"`
		Role        string  `json:"role"`
		BotIDs      []int64 `json:"bot_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	// Don't allow demoting yourself
	currentUser := getAuthUser(r)
	if currentUser.ID == req.ID && req.Role != "admin" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"cannot demote yourself"}`))
		return
	}
	if err := s.store.UpdateUser(req.ID, req.DisplayName, req.Role); err != nil {
		writeError(w, err)
		return
	}
	// Update bot assignments: remove all, then add new ones
	if req.BotIDs != nil {
		existingBots, _ := s.store.GetUserBotIDs(req.ID)
		for _, botID := range existingBots {
			s.store.RevokeBotFromUser(req.ID, botID)
		}
		for _, botID := range req.BotIDs {
			s.store.AssignBotToUser(req.ID, botID)
		}
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleUserDelete deletes an application user by ID.
// @Summary Delete app user
// @Description Deletes the specified user. Cannot delete yourself. Admin only.
// @Tags user-management
// @Produce json
// @Param id query int true "User ID"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/auth/users/delete [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleUserDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	// Don't allow deleting yourself
	currentUser := getAuthUser(r)
	if currentUser.ID == id {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"cannot delete yourself"}`))
		return
	}
	if err := s.store.DeleteUser(id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleUserResetPassword resets an application user's password and invalidates their sessions.
// @Summary Reset user password
// @Description Sets a new password for the specified user and deletes all their active sessions. Admin only.
// @Tags user-management
// @Accept json
// @Produce json
// @Param body body object true "Reset request" SchemaExample({"id":2,"password":"newpass"})
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/auth/users/reset-password [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleUserResetPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		ID       int64  `json:"id"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.UpdateUserPassword(req.ID, hash); err != nil {
		writeError(w, err)
		return
	}
	s.store.DeleteUserSessions(req.ID)
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleUserBots returns bot IDs assigned to an application user.
// @Summary Get user's bot assignments
// @Description Returns the list of bot IDs assigned to the specified user. Admin only.
// @Tags user-management
// @Produce json
// @Param user_id query int true "User ID"
// @Success 200 {array} integer
// @Failure 500 {object} map[string]string
// @Router /api/auth/users/bots [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleUserBots(w http.ResponseWriter, r *http.Request) {
	userID, _ := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	botIDs, err := s.store.GetUserBotIDs(userID)
	if err != nil {
		writeError(w, err)
		return
	}
	if botIDs == nil {
		botIDs = []int64{}
	}
	writeJSON(w, botIDs)
}

// handleAssignBot grants a user access to a bot.
// @Summary Assign bot to user
// @Description Gives the specified user access to the specified bot. Admin only.
// @Tags user-management
// @Accept json
// @Produce json
// @Param body body object true "Assignment request" SchemaExample({"user_id":2,"bot_id":1})
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/auth/users/bots/assign [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleAssignBot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		UserID int64 `json:"user_id"`
		BotID  int64 `json:"bot_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.AssignBotToUser(req.UserID, req.BotID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// handleRevokeBot removes a user's access to a bot.
// @Summary Revoke bot from user
// @Description Removes the specified user's access to the specified bot. Admin only.
// @Tags user-management
// @Accept json
// @Produce json
// @Param body body object true "Revoke request" SchemaExample({"user_id":2,"bot_id":1})
// @Success 200 {object} map[string]string
// @Failure 405 {string} string "Method not allowed"
// @Failure 500 {object} map[string]string
// @Router /api/auth/users/bots/revoke [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleRevokeBot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		UserID int64 `json:"user_id"`
		BotID  int64 `json:"bot_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.RevokeBotFromUser(req.UserID, req.BotID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// API key management handlers (admin only)

// @Summary List all API keys
// @Description Returns all API keys across all users (admin only)
// @Tags auth
// @Produce json
// @Success 200 {array} APIKey
// @Failure 403 {object} map[string]string
// @Router /api/auth/api-keys [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.GetAllAPIKeys()
	if err != nil {
		writeError(w, err)
		return
	}
	if keys == nil {
		keys = []APIKey{}
	}
	writeJSON(w, keys)
}

// @Summary Create a new API key
// @Description Generates a new API key for the specified user. The raw key is returned only once.
// @Tags auth
// @Accept json
// @Produce json
// @Param body body object true "user_id and name"
// @Success 200 {object} map[string]interface{} "status, id, key"
// @Failure 400 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Router /api/auth/api-keys/create [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		UserID int64  `json:"user_id"`
		Name   string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	if req.UserID == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write([]byte(`{"error":"user_id required"}`))
		return
	}
	rawKey, err := GenerateAPIKey()
	if err != nil {
		writeError(w, err)
		return
	}
	keyHash := HashAPIKey(rawKey)
	id, err := s.store.CreateAPIKey(req.UserID, keyHash, req.Name)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]any{"status": "ok", "id": id, "key": rawKey})
}

// @Summary Delete an API key
// @Description Permanently removes an API key by ID (admin only)
// @Tags auth
// @Produce json
// @Param id query int true "API key ID"
// @Success 200 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Router /api/auth/api-keys/delete [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if err := s.store.DeleteAPIKey(id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// @Summary Toggle API key enabled/disabled
// @Description Enable or disable an API key without deleting it (admin only)
// @Tags auth
// @Accept json
// @Produce json
// @Param body body object true "id and enabled"
// @Success 200 {object} map[string]string
// @Failure 403 {object} map[string]string
// @Router /api/auth/api-keys/toggle [post]
// @Security CookieAuth || BearerAuth
func (s *Server) handleToggleAPIKey(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		ID      int64 `json:"id"`
		Enabled bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.ToggleAPIKey(req.ID, req.Enabled); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// resolveBot finds a Bot instance from either registered bots or proxy manager
func (s *Server) resolveBot(botID int64) *Bot {
	bot := s.getBot(botID)
	if bot != nil {
		return bot
	}
	return s.proxy.GetManagedBot(botID)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(500)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// handleTelegramAPIProxy proxies requests to api.telegram.org and captures sent messages.
// URL format: /tgapi/bot{TOKEN}/{method}
// Backends set their API base URL to http://{addr}/tgapi/ instead of https://api.telegram.org/
func (s *Server) handleTelegramAPIProxy(w http.ResponseWriter, r *http.Request) {
	// Parse path: /tgapi/bot{TOKEN}/{method}
	path := strings.TrimPrefix(r.URL.Path, "/tgapi/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "bot") {
		http.Error(w, "Invalid path. Use /tgapi/bot{TOKEN}/{method}", 400)
		return
	}

	botToken := strings.TrimPrefix(parts[0], "bot")
	method := parts[1]

	// Intercept getUpdates for bots with long_poll_enabled
	if method == "getUpdates" {
		if botCfg, err := s.store.GetBotConfigByToken(botToken); err == nil && botCfg.LongPollEnabled {
			s.handleLongPollGetUpdates(w, r, botCfg)
			return
		}
	}

	// Read request body
	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", 400)
		return
	}

	// If method is empty, try to detect it from query param or request body
	if method == "" {
		if m := r.URL.Query().Get("method"); m != "" {
			method = m
		}
	}
	if method == "" {
		var bodyObj map[string]interface{}
		if json.Unmarshal(reqBody, &bodyObj) == nil {
			if m, ok := bodyObj["method"].(string); ok && m != "" {
				method = m
				delete(bodyObj, "method")
				reqBody, _ = json.Marshal(bodyObj)
			}
		}
	}
	if method == "" {
		method = inferTelegramMethod(reqBody)
	}

	// Log incoming request
	maskedToken := botToken
	if len(maskedToken) > 8 {
		maskedToken = maskedToken[:4] + "..." + maskedToken[len(maskedToken)-4:]
	}
	bodyPreview := string(reqBody)
	if len(bodyPreview) > 512 {
		bodyPreview = bodyPreview[:512] + "..."
	}
	log.Printf("[tgapi-proxy] %s %s bot=%s path=%s body=%s", r.Method, method, maskedToken, r.URL.Path, bodyPreview)

	// Forward to Telegram
	tgURL := fmt.Sprintf("%s/bot%s/%s", telegramAPIURL, botToken, method)
	tgReq, err := http.NewRequestWithContext(r.Context(), r.Method, tgURL, io.NopCloser(strings.NewReader(string(reqBody))))
	if err != nil {
		http.Error(w, "Failed to create request", 500)
		return
	}
	tgReq.Header.Set("Content-Type", r.Header.Get("Content-Type"))

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(tgReq)
	if err != nil {
		log.Printf("[tgapi-proxy] %s FAILED: %v", method, err)
		http.Error(w, fmt.Sprintf("Telegram API error: %v", err), 502)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	// Forward response to the backend
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)

	// Capture sent messages from the response
	if resp.StatusCode == 200 {
		s.captureSentMessage(botToken, method, reqBody, r.Header.Get("Content-Type"), respBody)
	}
}

// handleMediaProxy proxies file downloads from Telegram API with WebP-to-PNG conversion.
// @Summary Proxy media file
// @Description Downloads a file from Telegram by file_id using the specified bot's token. WebP files (stickers) are automatically converted to PNG for browser compatibility.
// @Tags media
// @Produce octet-stream
// @Param bot_id query string false "Bot ID (uses first available bot if omitted)"
// @Param file_id query string true "Telegram file ID"
// @Success 200 {file} binary
// @Failure 400 {string} string "Bad request"
// @Failure 500 {string} string "Internal error"
// @Router /api/media [get]
// @Security CookieAuth || BearerAuth
func (s *Server) handleMediaProxy(w http.ResponseWriter, r *http.Request) {
	botID := r.URL.Query().Get("bot_id")
	fileID := r.URL.Query().Get("file_id")
	if fileID == "" {
		http.Error(w, "file_id required", 400)
		return
	}

	// Find bot token
	var token string
	if botID != "" {
		bots, _ := s.store.GetBotConfigs()
		for _, b := range bots {
			if fmt.Sprintf("%d", b.ID) == botID {
				token = b.Token
				break
			}
		}
	}
	if token == "" {
		// Try CLI bot
		s.mu.RLock()
		for _, bot := range s.bots {
			token = bot.api.Token
			break
		}
		s.mu.RUnlock()
	}
	if token == "" {
		http.Error(w, "no bot available", 400)
		return
	}

	// Step 1: getFile to get file_path
	getFileURL := fmt.Sprintf("%s/bot%s/getFile?file_id=%s", telegramAPIURL, token, fileID)
	resp, err := http.Get(getFileURL)
	if err != nil {
		http.Error(w, "getFile failed", 500)
		return
	}
	defer resp.Body.Close()

	var fileResp struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
			FileSize int64  `json:"file_size"`
		} `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&fileResp); err != nil || !fileResp.OK {
		http.Error(w, "getFile failed", 500)
		return
	}

	// Step 2: Download the file
	downloadURL := fmt.Sprintf("%s/file/bot%s/%s", telegramAPIURL, token, fileResp.Result.FilePath)
	fileResp2, err := http.Get(downloadURL)
	if err != nil {
		http.Error(w, "download failed", 500)
		return
	}
	defer fileResp2.Body.Close()

	// Check if WebP and convert to PNG for browser compatibility (stickers etc.)
	ct := fileResp2.Header.Get("Content-Type")
	isWebP := strings.Contains(ct, "webp") || strings.HasSuffix(fileResp.Result.FilePath, ".webp")
	if isWebP {
		body, err := io.ReadAll(fileResp2.Body)
		if err != nil {
			http.Error(w, "read failed", 500)
			return
		}
		img, err := webp.Decode(bytes.NewReader(body))
		if err != nil {
			// Fallback: serve original WebP
			w.Header().Set("Content-Type", ct)
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.Write(body)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		png.Encode(w, img)
		return
	}

	// Determine correct Content-Type: upstream may return application/octet-stream
	if ct == "" || ct == "application/octet-stream" {
		// Try to infer from file extension
		ext := path.Ext(fileResp.Result.FilePath)
		if mimeType := mime.TypeByExtension(ext); mimeType != "" {
			ct = mimeType
		} else {
			// Sniff from content
			var buf bytes.Buffer
			sniffBytes := make([]byte, 512)
			n, _ := io.ReadAtLeast(fileResp2.Body, sniffBytes, 1)
			if n > 0 {
				ct = http.DetectContentType(sniffBytes[:n])
				buf.Write(sniffBytes[:n])
			}
			w.Header().Set("Content-Type", ct)
			w.Header().Set("Cache-Control", "public, max-age=86400")
			io.Copy(w, io.MultiReader(&buf, fileResp2.Body))
			return
		}
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	io.Copy(w, fileResp2.Body)
}

// inferTelegramMethod tries to guess the Telegram API method from request body fields.
func inferTelegramMethod(body []byte) string {
	var params map[string]interface{}
	if err := json.Unmarshal(body, &params); err != nil {
		return ""
	}
	switch {
	case params["photo"] != nil:
		return "sendPhoto"
	case params["audio"] != nil:
		return "sendAudio"
	case params["document"] != nil:
		return "sendDocument"
	case params["video"] != nil:
		return "sendVideo"
	case params["animation"] != nil:
		return "sendAnimation"
	case params["voice"] != nil:
		return "sendVoice"
	case params["video_note"] != nil:
		return "sendVideoNote"
	case params["sticker"] != nil:
		return "sendSticker"
	case params["latitude"] != nil && params["title"] != nil:
		return "sendVenue"
	case params["latitude"] != nil:
		return "sendLocation"
	case params["phone_number"] != nil:
		return "sendContact"
	case params["question"] != nil:
		return "sendPoll"
	case params["emoji"] != nil && params["text"] == nil:
		return "sendDice"
	case params["message_id"] != nil && params["from_chat_id"] != nil:
		return "forwardMessage"
	case params["text"] != nil && params["message_id"] != nil:
		return "editMessageText"
	case params["text"] != nil:
		return "sendMessage"
	}
	return ""
}

// sendMethods lists Telegram API methods that return a Message in the result
var sendMethods = map[string]bool{
	"sendMessage":    true,
	"sendPhoto":      true,
	"sendAudio":      true,
	"sendDocument":   true,
	"sendVideo":      true,
	"sendAnimation":  true,
	"sendVoice":      true,
	"sendVideoNote":  true,
	"sendSticker":    true,
	"sendLocation":   true,
	"sendVenue":      true,
	"sendContact":    true,
	"sendPoll":       true,
	"sendDice":       true,
	"forwardMessage": true,
	"editMessageText": true,
}

type telegramRequestParams struct {
	ChatID        int64
	FromChatID    int64
	MessageID     int
	Caption       string
	RemoveCaption bool
}

func (s *Server) captureSentMessage(token, method string, reqBody []byte, contentType string, respBody []byte) {
	if method == "copyMessage" {
		s.captureCopiedMessage(token, reqBody, contentType, respBody)
		return
	}

	if !sendMethods[method] {
		return
	}

	var resp struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil || !resp.OK {
		return
	}

	var msg struct {
		MessageID int    `json:"message_id"`
		Chat      struct {
			ID    int64  `json:"id"`
			Type  string `json:"type"`
			Title string `json:"title"`
		} `json:"chat"`
		From struct {
			ID        int64  `json:"id"`
			Username  string `json:"username"`
			FirstName string `json:"first_name"`
			IsBot     bool   `json:"is_bot"`
		} `json:"from"`
		Text    string `json:"text"`
		Caption string `json:"caption"`
		Date    int64  `json:"date"`
		Photo   []struct {
			FileID string `json:"file_id"`
		} `json:"photo"`
		Video     *struct{ FileID string `json:"file_id"` } `json:"video"`
		Animation *struct{ FileID string `json:"file_id"` } `json:"animation"`
		Sticker   *struct{ FileID string `json:"file_id"` } `json:"sticker"`
		Voice     *struct{ FileID string `json:"file_id"` } `json:"voice"`
		Audio     *struct{ FileID string `json:"file_id"` } `json:"audio"`
		Document  *struct{ FileID string `json:"file_id"` } `json:"document"`
		VideoNote *struct{ FileID string `json:"file_id"` } `json:"video_note"`
	}
	if err := json.Unmarshal(resp.Result, &msg); err != nil || msg.MessageID == 0 {
		return
	}

	fromUser := msg.From.FirstName
	if msg.From.Username != "" {
		fromUser = "@" + msg.From.Username
	}

	text := msg.Text
	if text == "" {
		text = msg.Caption
	}

	// Detect media type and file_id from response
	var mediaType, fileID string
	switch {
	case len(msg.Photo) > 0:
		mediaType, fileID = "photo", msg.Photo[len(msg.Photo)-1].FileID
	case msg.Video != nil:
		mediaType, fileID = "video", msg.Video.FileID
	case msg.Animation != nil:
		mediaType, fileID = "animation", msg.Animation.FileID
	case msg.Sticker != nil:
		mediaType, fileID = "sticker", msg.Sticker.FileID
	case msg.Voice != nil:
		mediaType, fileID = "voice", msg.Voice.FileID
	case msg.Audio != nil:
		mediaType, fileID = "audio", msg.Audio.FileID
	case msg.Document != nil:
		mediaType, fileID = "document", msg.Document.FileID
	case msg.VideoNote != nil:
		mediaType, fileID = "video_note", msg.VideoNote.FileID
	}

	// Find bot_id from token
	botCfg := s.findBotByToken(token)
	var msgBotID int64
	if botCfg != nil {
		msgBotID = botCfg.ID
	}

	m := Message{
		ID:        msg.MessageID,
		BotID:     msgBotID,
		ChatID:    msg.Chat.ID,
		FromUser:  fromUser,
		FromID:    msg.From.ID,
		Text:      text,
		Date:      msg.Date * 1000,
		MediaType: mediaType,
		FileID:    fileID,
	}
	if err := s.store.SaveMessage(m); err != nil {
		log.Printf("[tgapi-proxy] Failed to save sent message: %v", err)
	} else {
		log.Printf("[tgapi-proxy] Captured %s: msg_id=%d chat_id=%d from=%s text=%q",
			method, msg.MessageID, msg.Chat.ID, fromUser, truncateStr(text, 80))
	}

	// Also track the chat if we have a bot for this token
	if botCfg != nil {
		s.store.UpsertChat(botCfg.ID, Chat{
			ID:        msg.Chat.ID,
			Type:      msg.Chat.Type,
			Title:     msg.Chat.Title,
			UpdatedAt: time.Now().Format(time.RFC3339),
		})
	}
}

func (s *Server) captureCopiedMessage(token string, reqBody []byte, contentType string, respBody []byte) {
	var resp struct {
		OK     bool `json:"ok"`
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	if err := json.Unmarshal(respBody, &resp); err != nil || !resp.OK || resp.Result.MessageID == 0 {
		return
	}

	params, ok := parseTelegramRequestParams(reqBody, contentType)
	if !ok || params.ChatID == 0 || params.FromChatID == 0 || params.MessageID == 0 {
		return
	}

	botCfg := s.findBotByToken(token)
	var copyBotID int64
	if botCfg != nil {
		copyBotID = botCfg.ID
	}

	sourceMsg, err := s.store.GetMessage(copyBotID, params.FromChatID, params.MessageID)
	if err != nil {
		return
	}

	text := sourceMsg.Text
	if params.RemoveCaption {
		text = ""
	}
	if params.Caption != "" {
		text = params.Caption
	}

	fromUser, fromID := s.findBotSenderByToken(token)
	msg := Message{
		ID:        resp.Result.MessageID,
		BotID:     copyBotID,
		ChatID:    params.ChatID,
		FromUser:  fromUser,
		FromID:    fromID,
		Text:      text,
		Date:      time.Now().UnixMilli(),
		MediaType: sourceMsg.MediaType,
		FileID:    sourceMsg.FileID,
	}
	if err := s.store.SaveMessage(msg); err != nil {
		log.Printf("[tgapi-proxy] Failed to save copied message: %v", err)
		return
	}

	log.Printf("[tgapi-proxy] Captured copyMessage: msg_id=%d chat_id=%d source_chat_id=%d source_msg_id=%d text=%q",
		resp.Result.MessageID, params.ChatID, params.FromChatID, params.MessageID, truncateStr(text, 80))
}

func (s *Server) findBotByToken(token string) *BotConfig {
	bots, err := s.store.GetBotConfigs()
	if err != nil {
		return nil
	}
	for _, b := range bots {
		if b.Token == token {
			return &b
		}
	}
	return nil
}

func (s *Server) findBotSenderByToken(token string) (string, int64) {
	botCfg := s.findBotByToken(token)
	if botCfg == nil {
		return "", 0
	}

	if s.proxy != nil {
		if bot := s.resolveBot(botCfg.ID); bot != nil {
			if bot.api.Self.UserName != "" {
				return "@" + bot.api.Self.UserName, bot.api.Self.ID
			}
			return bot.GetBotName(), bot.api.Self.ID
		}
	}

	if botCfg.BotUsername != "" {
		return "@" + botCfg.BotUsername, 0
	}
	return botCfg.Name, 0
}

func parseTelegramRequestParams(body []byte, contentType string) (telegramRequestParams, bool) {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = contentType
	}

	switch mediaType {
	case "application/json":
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			return telegramRequestParams{}, false
		}

		params := telegramRequestParams{
			ChatID:        rawInt64(raw["chat_id"]),
			FromChatID:    rawInt64(raw["from_chat_id"]),
			MessageID:     int(rawInt64(raw["message_id"])),
			Caption:       rawString(raw["caption"]),
			RemoveCaption: rawBool(raw["remove_caption"]),
		}
		return params, true
	case "application/x-www-form-urlencoded", "":
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return telegramRequestParams{}, false
		}
		params := telegramRequestParams{
			ChatID:        valueInt64(values.Get("chat_id")),
			FromChatID:    valueInt64(values.Get("from_chat_id")),
			MessageID:     int(valueInt64(values.Get("message_id"))),
			Caption:       values.Get("caption"),
			RemoveCaption: valueBool(values.Get("remove_caption")),
		}
		return params, true
	default:
		return telegramRequestParams{}, false
	}
}

func rawInt64(value json.RawMessage) int64 {
	var n int64
	if err := json.Unmarshal(value, &n); err == nil {
		return n
	}

	var s string
	if err := json.Unmarshal(value, &s); err == nil {
		return valueInt64(s)
	}
	return 0
}

func rawString(value json.RawMessage) string {
	var s string
	if err := json.Unmarshal(value, &s); err == nil {
		return s
	}
	return ""
}

func rawBool(value json.RawMessage) bool {
	var b bool
	if err := json.Unmarshal(value, &b); err == nil {
		return b
	}

	var s string
	if err := json.Unmarshal(value, &s); err == nil {
		return valueBool(s)
	}
	return false
}

func valueInt64(value string) int64 {
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func valueBool(value string) bool {
	b, err := strconv.ParseBool(value)
	if err != nil {
		return false
	}
	return b
}

func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// handleLogs returns recent application log entries
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if s.logBuf == nil {
		writeJSON(w, []struct{}{})
		return
	}
	n := 200
	if v := r.URL.Query().Get("n"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 && parsed <= 1000 {
			n = parsed
		}
	}
	writeJSON(w, s.logBuf.Recent(n))
}

// handleLogStream streams application logs via SSE
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	if s.logBuf == nil {
		http.Error(w, "logging not available", 500)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", 500)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	ch := s.logBuf.Subscribe()
	defer s.logBuf.Unsubscribe(ch)

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case entry := <-ch:
			data, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}
