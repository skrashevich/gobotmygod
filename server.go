package main

import (
	"embed"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"
)

//go:embed templates
var templateFS embed.FS

type Server struct {
	bot            *Bot
	store          *Store
	proxy          *ProxyManager
	webhookPath    string
	webhookHandler http.HandlerFunc
}

func NewServer(bot *Bot, store *Store, proxy *ProxyManager) *Server {
	return &Server{bot: bot, store: store, proxy: proxy}
}

func (s *Server) SetWebhookHandler(path string, handler http.HandlerFunc) {
	s.webhookPath = path
	s.webhookHandler = handler
}

func (s *Server) Start(addr string) error {
	mux := http.NewServeMux()

	// Webhook endpoint (must be before "/" catch-all)
	if s.webhookPath != "" && s.webhookHandler != nil {
		mux.HandleFunc(s.webhookPath, s.webhookHandler)
		log.Printf("Webhook endpoint registered at %s", s.webhookPath)
	}

	// Serve frontend
	mux.HandleFunc("/", s.handleIndex)

	// API routes
	mux.HandleFunc("/api/bot", s.handleBotInfo)
	mux.HandleFunc("/api/chats", s.handleChats)
	mux.HandleFunc("/api/chats/refresh", s.handleRefreshChat)
	mux.HandleFunc("/api/chats/delete", s.handleDeleteChat)
	mux.HandleFunc("/api/messages", s.handleMessages)
	mux.HandleFunc("/api/messages/search", s.handleSearchMessages)
	mux.HandleFunc("/api/messages/send", s.handleSendMessage)
	mux.HandleFunc("/api/messages/pin", s.handlePinMessage)
	mux.HandleFunc("/api/messages/unpin", s.handleUnpinMessage)
	mux.HandleFunc("/api/messages/delete", s.handleDeleteMessage)
	mux.HandleFunc("/api/stats", s.handleStats)
	mux.HandleFunc("/api/users/list", s.handleListUsers)
	mux.HandleFunc("/api/users/ban", s.handleBanUser)
	mux.HandleFunc("/api/users/unban", s.handleUnbanUser)

	// Admin management
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

	// Proxy bots
	mux.HandleFunc("/api/proxy/list", s.handleProxyList)
	mux.HandleFunc("/api/proxy/add", s.handleProxyAdd)
	mux.HandleFunc("/api/proxy/update", s.handleProxyUpdate)
	mux.HandleFunc("/api/proxy/delete", s.handleProxyDelete)
	mux.HandleFunc("/api/proxy/toggle", s.handleProxyToggle)
	mux.HandleFunc("/api/proxy/validate", s.handleProxyValidate)

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

func (s *Server) handleBotInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"username": s.bot.GetBotInfo()})
}

func (s *Server) handleChats(w http.ResponseWriter, r *http.Request) {
	chats, err := s.store.GetChats()
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
	chatID, err := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	if err != nil {
		writeError(w, err)
		return
	}
	chat, err := s.bot.RefreshChat(chatID)
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
	chatID, err := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	if err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.DeleteChat(chatID); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

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
		ChatID int64  `json:"chat_id"`
		Text   string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	if err := s.bot.SendMessage(req.ChatID, req.Text); err != nil {
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
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	msgID, _ := strconv.Atoi(r.URL.Query().Get("message_id"))
	if err := s.bot.PinMessage(chatID, msgID); err != nil {
		writeError(w, err)
		return
	}
	s.store.LogAdminAction(AdminLog{
		ChatID: chatID, Action: "pin_message", ActorName: s.bot.GetBotName(),
		Details: "Message ID: " + strconv.Itoa(msgID), CreatedAt: time.Now().Format(time.RFC3339),
	})
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleUnpinMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	msgID, _ := strconv.Atoi(r.URL.Query().Get("message_id"))
	if err := s.bot.UnpinMessage(chatID, msgID); err != nil {
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
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	msgID, _ := strconv.Atoi(r.URL.Query().Get("message_id"))
	if err := s.bot.DeleteMessage(chatID, msgID); err != nil {
		writeError(w, err)
		return
	}
	s.store.MarkMessageDeleted(chatID, msgID)
	s.store.LogAdminAction(AdminLog{
		ChatID: chatID, Action: "delete_message", ActorName: s.bot.GetBotName(),
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
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	userID, _ := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	if err := s.bot.BanUser(chatID, userID); err != nil {
		writeError(w, err)
		return
	}
	s.store.LogAdminAction(AdminLog{
		ChatID: chatID, Action: "ban_user", ActorName: s.bot.GetBotName(),
		TargetID: userID, CreatedAt: time.Now().Format(time.RFC3339),
	})
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleUnbanUser(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	userID, _ := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	if err := s.bot.UnbanUser(chatID, userID); err != nil {
		writeError(w, err)
		return
	}
	s.store.LogAdminAction(AdminLog{
		ChatID: chatID, Action: "unban_user", ActorName: s.bot.GetBotName(),
		TargetID: userID, CreatedAt: time.Now().Format(time.RFC3339),
	})
	writeJSON(w, map[string]string{"status": "ok"})
}

// Admin management handlers

func (s *Server) handleGetAdmins(w http.ResponseWriter, r *http.Request) {
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	admins, err := s.bot.GetAdmins(chatID)
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
		ChatID int64     `json:"chat_id"`
		UserID int64     `json:"user_id"`
		Perms  AdminInfo `json:"perms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	if err := s.bot.PromoteAdmin(req.ChatID, req.UserID, req.Perms); err != nil {
		writeError(w, err)
		return
	}

	s.store.LogAdminAction(AdminLog{
		ChatID:     req.ChatID,
		Action:     "promote_admin",
		ActorName:  s.bot.GetBotName(),
		TargetID:   req.UserID,
		Details:    "Promoted to admin via web UI",
		CreatedAt:  time.Now().Format(time.RFC3339),
	})

	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleDemoteAdmin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	chatID, _ := strconv.ParseInt(r.URL.Query().Get("chat_id"), 10, 64)
	userID, _ := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	if err := s.bot.DemoteAdmin(chatID, userID); err != nil {
		writeError(w, err)
		return
	}

	s.store.LogAdminAction(AdminLog{
		ChatID:    chatID,
		Action:    "demote_admin",
		ActorName: s.bot.GetBotName(),
		TargetID:  userID,
		Details:   "Demoted from admin via web UI",
		CreatedAt: time.Now().Format(time.RFC3339),
	})

	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleSetAdminTitle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		ChatID int64  `json:"chat_id"`
		UserID int64  `json:"user_id"`
		Title  string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	if err := s.bot.SetAdminTitle(req.ChatID, req.UserID, req.Title); err != nil {
		writeError(w, err)
		return
	}

	s.store.LogAdminAction(AdminLog{
		ChatID:    req.ChatID,
		Action:    "set_admin_title",
		ActorName: s.bot.GetBotName(),
		TargetID:  req.UserID,
		Details:   "Title set to: " + req.Title,
		CreatedAt: time.Now().Format(time.RFC3339),
	})

	writeJSON(w, map[string]string{"status": "ok"})
}

// Admin log handler

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

// User tag handlers

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
	var req UserTag
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	if err := s.store.AddUserTag(req); err != nil {
		writeError(w, err)
		return
	}

	s.store.LogAdminAction(AdminLog{
		ChatID:     req.ChatID,
		Action:     "add_tag",
		ActorName:  s.bot.GetBotName(),
		TargetID:   req.UserID,
		TargetName: req.Username,
		Details:    "Tag: " + req.Tag,
		CreatedAt:  time.Now().Format(time.RFC3339),
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

// Proxy handlers

func (s *Server) handleProxyList(w http.ResponseWriter, r *http.Request) {
	bots, err := s.store.GetProxyBots()
	if err != nil {
		writeError(w, err)
		return
	}
	if bots == nil {
		bots = []ProxyBot{}
	}
	// Add running status
	type ProxyBotStatus struct {
		ProxyBot
		Running bool `json:"running"`
	}
	var result []ProxyBotStatus
	for _, b := range bots {
		result = append(result, ProxyBotStatus{
			ProxyBot: b,
			Running:  s.proxy.IsRunning(b.ID),
		})
	}
	writeJSON(w, result)
}

func (s *Server) handleProxyAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req ProxyBot
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	if req.PollingTimeout <= 0 {
		req.PollingTimeout = 30
	}

	// Delete webhook before starting polling
	if err := s.proxy.DeleteWebhook(req.Token); err != nil {
		writeError(w, err)
		return
	}

	id, err := s.store.AddProxyBot(req)
	if err != nil {
		writeError(w, err)
		return
	}
	if req.Enabled {
		s.proxy.RestartBot(id)
	}
	writeJSON(w, map[string]interface{}{"status": "ok", "id": id})
}

func (s *Server) handleProxyUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req ProxyBot
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, err)
		return
	}
	if req.PollingTimeout <= 0 {
		req.PollingTimeout = 30
	}

	// Delete webhook if token changed
	if err := s.proxy.DeleteWebhook(req.Token); err != nil {
		writeError(w, err)
		return
	}

	if err := s.store.UpdateProxyBot(req); err != nil {
		writeError(w, err)
		return
	}
	s.proxy.RestartBot(req.ID)
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleProxyDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	s.proxy.stopBot(id)
	if err := s.store.DeleteProxyBot(id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleProxyToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	bot, err := s.store.GetProxyBot(id)
	if err != nil {
		writeError(w, err)
		return
	}
	bot.Enabled = !bot.Enabled

	// If enabling, delete webhook first
	if bot.Enabled {
		if err := s.proxy.DeleteWebhook(bot.Token); err != nil {
			writeError(w, err)
			return
		}
	}

	if err := s.store.UpdateProxyBot(*bot); err != nil {
		writeError(w, err)
		return
	}
	s.proxy.RestartBot(id)
	writeJSON(w, map[string]interface{}{"status": "ok", "enabled": bot.Enabled})
}

func (s *Server) handleProxyValidate(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	username, err := s.proxy.ValidateToken(token)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, map[string]string{"username": username})
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
