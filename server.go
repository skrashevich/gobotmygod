package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

//go:embed templates
var templateFS embed.FS

type Server struct {
	store          *Store
	proxy          *ProxyManager
	mu             sync.RWMutex
	bots           map[int64]*Bot // botID -> Bot (for Telegram API calls)
	webhookPath    string
	webhookHandler http.HandlerFunc
}

func NewServer(store *Store, proxy *ProxyManager) *Server {
	return &Server{
		store: store,
		proxy: proxy,
		bots:  make(map[int64]*Bot),
	}
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

func (s *Server) Start(addr string) error {
	mux := http.NewServeMux()

	if s.webhookPath != "" && s.webhookHandler != nil {
		mux.HandleFunc(s.webhookPath, s.webhookHandler)
		log.Printf("Webhook endpoint registered at %s", s.webhookPath)
	}

	mux.HandleFunc("/", s.handleIndex)

	// Bot management
	mux.HandleFunc("/api/bots", s.handleBotList)
	mux.HandleFunc("/api/bots/add", s.handleBotAdd)
	mux.HandleFunc("/api/bots/update", s.handleBotUpdate)
	mux.HandleFunc("/api/bots/delete", s.handleBotDelete)
	mux.HandleFunc("/api/bots/validate", s.handleBotValidate)
	mux.HandleFunc("/api/bots/health", s.handleBotHealth)

	// Chat management (requires bot_id)
	mux.HandleFunc("/api/chats", s.handleChats)
	mux.HandleFunc("/api/chats/refresh", s.handleRefreshChat)
	mux.HandleFunc("/api/chats/delete", s.handleDeleteChat)

	// Messages
	mux.HandleFunc("/api/messages", s.handleMessages)
	mux.HandleFunc("/api/messages/search", s.handleSearchMessages)
	mux.HandleFunc("/api/messages/send", s.handleSendMessage)
	mux.HandleFunc("/api/messages/pin", s.handlePinMessage)
	mux.HandleFunc("/api/messages/unpin", s.handleUnpinMessage)
	mux.HandleFunc("/api/messages/delete", s.handleDeleteMessage)

	// Stats
	mux.HandleFunc("/api/stats", s.handleStats)

	// Users
	mux.HandleFunc("/api/users/list", s.handleListUsers)
	mux.HandleFunc("/api/users/ban", s.handleBanUser)
	mux.HandleFunc("/api/users/unban", s.handleUnbanUser)

	// Admins
	mux.HandleFunc("/api/admins", s.handleGetAdmins)
	mux.HandleFunc("/api/admins/promote", s.handlePromoteAdmin)
	mux.HandleFunc("/api/admins/demote", s.handleDemoteAdmin)
	mux.HandleFunc("/api/admins/title", s.handleSetAdminTitle)

	// Admin log
	mux.HandleFunc("/api/adminlog", s.handleAdminLog)

	// User tags
	mux.HandleFunc("/api/tags", s.handleGetTags)
	mux.HandleFunc("/api/tags/add", s.handleAddTag)
	mux.HandleFunc("/api/tags/remove", s.handleRemoveTag)
	mux.HandleFunc("/api/tags/user", s.handleGetUserTags)

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

func (s *Server) handleBotList(w http.ResponseWriter, r *http.Request) {
	bots, err := s.store.GetBotConfigs()
	if err != nil {
		writeError(w, err)
		return
	}
	if bots == nil {
		bots = []BotConfig{}
	}
	type BotStatus struct {
		BotConfig
		Running bool `json:"running"`
	}
	var result []BotStatus
	for _, b := range bots {
		result = append(result, BotStatus{BotConfig: b, Running: s.proxy.IsRunning(b.ID)})
	}
	if result == nil {
		result = []BotStatus{}
	}
	writeJSON(w, result)
}

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
		s.proxy.RegisterManagedBot(id, managedBot)
	}

	if req.ManageEnabled || req.ProxyEnabled {
		s.proxy.RestartBot(id)
	}
	writeJSON(w, map[string]interface{}{"status": "ok", "id": id})
}

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

	if err := s.store.UpdateBotConfig(req); err != nil {
		writeError(w, err)
		return
	}

	// Restart via proxy manager (works for all bots)
	s.proxy.RestartBot(req.ID)
	writeJSON(w, map[string]string{"status": "ok"})
}

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

func (s *Server) handleBotValidate(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	username, err := s.proxy.ValidateToken(token)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"username": username})
}

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

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if limit == 0 {
		limit = 50
	}
	msgs, err := s.store.GetMessages(chatID, limit, offset)
	if err != nil {
		writeError(w, err)
		return
	}
	if msgs == nil {
		msgs = []Message{}
	}
	writeJSON(w, msgs)
}

func (s *Server) handleSearchMessages(w http.ResponseWriter, r *http.Request) {
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	query := r.URL.Query().Get("q")
	msgs, err := s.store.SearchMessages(chatID, query, 50)
	if err != nil {
		writeError(w, err)
		return
	}
	if msgs == nil {
		msgs = []Message{}
	}
	writeJSON(w, msgs)
}

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		BotID  int64  `json:"bot_id"`
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
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
	if err := bot.SendMessage(req.ChatID, req.Text); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

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

func (s *Server) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
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
	if err := bot.DeleteMessage(chatID, msgID); err != nil {
		writeError(w, err)
		return
	}
	s.store.MarkMessageDeleted(chatID, msgID)
	s.store.LogAdminAction(AdminLog{
		ChatID: chatID, Action: "delete_message", ActorName: bot.GetBotName(),
		Details: "Message ID: " + strconv.Itoa(msgID), CreatedAt: time.Now().Format(time.RFC3339),
	})
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	stats, err := s.store.GetChatStats(chatID)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, stats)
}

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
