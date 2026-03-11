package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
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

	// Telegram API proxy — backends use this instead of api.telegram.org
	mux.HandleFunc("/tgapi/", s.handleTelegramAPIProxy)

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

	// Routes
	mux.HandleFunc("/api/routes", s.handleGetRoutes)
	mux.HandleFunc("/api/routes/add", s.handleAddRoute)
	mux.HandleFunc("/api/routes/update", s.handleUpdateRoute)
	mux.HandleFunc("/api/routes/delete", s.handleDeleteRoute)

	// LLM routing config
	mux.HandleFunc("/api/llm-config", s.handleGetLLMConfig)
	mux.HandleFunc("/api/llm-config/save", s.handleSaveLLMConfig)
	mux.HandleFunc("/api/bots/description", s.handleBotDescription)

	// Media proxy
	mux.HandleFunc("/api/media", s.handleMediaProxy)

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

// Route handlers

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

// LLM config handlers

func (s *Server) handleGetLLMConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := s.store.GetLLMConfig()
	if err != nil {
		writeJSON(w, LLMConfig{})
		return
	}
	writeJSON(w, cfg)
}

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

	// Read request body
	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Failed to read request body", 400)
		return
	}

	// Forward to Telegram
	tgURL := fmt.Sprintf("https://api.telegram.org/bot%s/%s", botToken, method)
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

// handleMediaProxy proxies file downloads from Telegram API
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
	getFileURL := fmt.Sprintf("https://api.telegram.org/bot%s/getFile?file_id=%s", token, fileID)
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
	downloadURL := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", token, fileResp.Result.FilePath)
	fileResp2, err := http.Get(downloadURL)
	if err != nil {
		http.Error(w, "download failed", 500)
		return
	}
	defer fileResp2.Body.Close()

	// Forward content type and cache
	if ct := fileResp2.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	io.Copy(w, fileResp2.Body)
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

	m := Message{
		ID:        msg.MessageID,
		ChatID:    msg.Chat.ID,
		FromUser:  fromUser,
		FromID:    msg.From.ID,
		Text:      text,
		Date:      msg.Date,
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
	botCfg := s.findBotByToken(token)
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

	sourceMsg, err := s.store.GetMessage(params.FromChatID, params.MessageID)
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
		ChatID:    params.ChatID,
		FromUser:  fromUser,
		FromID:    fromID,
		Text:      text,
		Date:      time.Now().Unix(),
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
