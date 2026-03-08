package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type Bot struct {
	api   *tgbotapi.BotAPI
	store *Store
}

func NewBot(token string, store *Store) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, err
	}
	log.Printf("Authorized as @%s", api.Self.UserName)
	return &Bot{api: api, store: store}, nil
}

// WebhookStatus represents the current webhook state of the bot
type WebhookStatus struct {
	URL            string // webhook URL if set
	HasWebhook     bool   // whether a webhook is configured
	PendingUpdates int    // number of pending updates
}

// CheckWebhook queries Telegram for current webhook info
func (b *Bot) CheckWebhook() (*WebhookStatus, error) {
	info, err := b.api.GetWebhookInfo()
	if err != nil {
		return nil, fmt.Errorf("getWebhookInfo failed: %w", err)
	}
	return &WebhookStatus{
		URL:            info.URL,
		HasWebhook:     info.URL != "",
		PendingUpdates: info.PendingUpdateCount,
	}, nil
}

// SetWebhook configures a webhook URL on Telegram side
func (b *Bot) SetWebhook(url string) error {
	wh, err := tgbotapi.NewWebhook(url)
	if err != nil {
		return fmt.Errorf("invalid webhook URL: %w", err)
	}
	_, err = b.api.Request(wh)
	if err != nil {
		return fmt.Errorf("setWebhook failed: %w", err)
	}
	log.Printf("Webhook set to %s", url)
	return nil
}

// RemoveWebhook removes the webhook, allowing polling
func (b *Bot) RemoveWebhook() error {
	_, err := b.api.Request(tgbotapi.DeleteWebhookConfig{})
	return err
}

// StartPolling removes any webhook and starts long polling for updates
func (b *Bot) StartPolling() {
	if err := b.RemoveWebhook(); err != nil {
		log.Printf("Warning: could not remove webhook: %v", err)
	}

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	u.AllowedUpdates = []string{"message", "channel_post", "my_chat_member", "chat_member"}

	updates := b.api.GetUpdatesChan(u)

	for update := range updates {
		b.processUpdate(update)
	}
}

// WebhookHandler returns an http.HandlerFunc that accepts updates from Telegram
func (b *Bot) WebhookHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", 405)
			return
		}
		var update tgbotapi.Update
		if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
			http.Error(w, "Bad request", 400)
			return
		}
		b.processUpdate(update)
		w.WriteHeader(200)
	}
}

func (b *Bot) processUpdate(update tgbotapi.Update) {
	if update.Message != nil {
		b.handleMessage(update.Message)
	}
	if update.ChannelPost != nil {
		b.handleChannelPost(update.ChannelPost)
	}
	if update.MyChatMember != nil {
		b.handleMyChatMember(update.MyChatMember)
	}
	if update.ChatMember != nil {
		b.handleChatMember(update.ChatMember)
	}
}

func formatUsername(user *tgbotapi.User) string {
	name := user.FirstName
	if user.LastName != "" {
		name += " " + user.LastName
	}
	if user.UserName != "" {
		name = "@" + user.UserName
	}
	return name
}

func (b *Bot) handleMessage(msg *tgbotapi.Message) {
	// Track the chat
	b.trackChat(msg.Chat)

	// Save message
	fromUser := ""
	var fromID int64
	if msg.From != nil {
		fromUser = formatUsername(msg.From)
		fromID = msg.From.ID
		b.store.TrackUser(msg.Chat.ID, fromID, fromUser)
	}

	// Track new members joining via this message
	if msg.NewChatMembers != nil {
		for i := range msg.NewChatMembers {
			u := &msg.NewChatMembers[i]
			b.store.TrackUser(msg.Chat.ID, u.ID, formatUsername(u))
		}
	}

	replyToID := 0
	if msg.ReplyToMessage != nil {
		replyToID = msg.ReplyToMessage.MessageID
	}

	text := msg.Text
	if text == "" {
		text = msg.Caption
	}

	m := Message{
		ID:        msg.MessageID,
		ChatID:    msg.Chat.ID,
		FromUser:  fromUser,
		FromID:    fromID,
		Text:      text,
		Date:      int64(msg.Date),
		ReplyToID: replyToID,
	}
	if err := b.store.SaveMessage(m); err != nil {
		log.Printf("Error saving message: %v", err)
	}
}

func (b *Bot) handleChannelPost(msg *tgbotapi.Message) {
	b.trackChat(msg.Chat)

	fromUser := "Channel"
	if msg.AuthorSignature != "" {
		fromUser = msg.AuthorSignature
	}

	text := msg.Text
	if text == "" {
		text = msg.Caption
	}

	m := Message{
		ID:       msg.MessageID,
		ChatID:   msg.Chat.ID,
		FromUser: fromUser,
		Text:     text,
		Date:     int64(msg.Date),
	}
	if err := b.store.SaveMessage(m); err != nil {
		log.Printf("Error saving channel post: %v", err)
	}
}

func (b *Bot) handleMyChatMember(update *tgbotapi.ChatMemberUpdated) {
	b.trackChat(&update.Chat)
}

func (b *Bot) handleChatMember(update *tgbotapi.ChatMemberUpdated) {
	user := update.NewChatMember.User
	if user != nil {
		name := formatUsername(user)
		b.store.TrackUser(update.Chat.ID, user.ID, name)
	}
}

func (b *Bot) trackChat(chat *tgbotapi.Chat) {
	isAdmin := false
	memberCount := 0

	// Try to get bot's member info to check admin status
	me, err := b.api.GetChatMember(tgbotapi.GetChatMemberConfig{
		ChatConfigWithUser: tgbotapi.ChatConfigWithUser{
			ChatID: chat.ID,
			UserID: b.api.Self.ID,
		},
	})
	if err == nil {
		isAdmin = me.IsAdministrator() || me.IsCreator()
	}

	// Try to get member count
	count, err := b.api.GetChatMembersCount(tgbotapi.ChatMemberCountConfig{
		ChatConfig: tgbotapi.ChatConfig{ChatID: chat.ID},
	})
	if err == nil {
		memberCount = count
	}

	desc := ""
	// Get full chat info for description
	fullChat, err := b.api.GetChat(tgbotapi.ChatInfoConfig{
		ChatConfig: tgbotapi.ChatConfig{ChatID: chat.ID},
	})
	if err == nil {
		desc = fullChat.Description
	}

	c := Chat{
		ID:          chat.ID,
		Type:        chat.Type,
		Title:       chat.Title,
		Username:    chat.UserName,
		MemberCount: memberCount,
		Description: desc,
		IsAdmin:     isAdmin,
		UpdatedAt:   time.Now().Format(time.RFC3339),
	}

	if err := b.store.UpsertChat(c); err != nil {
		log.Printf("Error upserting chat: %v", err)
	}
}

// RefreshChat refreshes info for a specific chat
func (b *Bot) RefreshChat(chatID int64) (*Chat, error) {
	fullChat, err := b.api.GetChat(tgbotapi.ChatInfoConfig{
		ChatConfig: tgbotapi.ChatConfig{ChatID: chatID},
	})
	if err != nil {
		return nil, err
	}

	isAdmin := false
	me, err := b.api.GetChatMember(tgbotapi.GetChatMemberConfig{
		ChatConfigWithUser: tgbotapi.ChatConfigWithUser{
			ChatID: chatID,
			UserID: b.api.Self.ID,
		},
	})
	if err == nil {
		isAdmin = me.IsAdministrator() || me.IsCreator()
	}

	memberCount := 0
	count, err := b.api.GetChatMembersCount(tgbotapi.ChatMemberCountConfig{
		ChatConfig: tgbotapi.ChatConfig{ChatID: chatID},
	})
	if err == nil {
		memberCount = count
	}

	c := Chat{
		ID:          fullChat.ID,
		Type:        fullChat.Type,
		Title:       fullChat.Title,
		Username:    fullChat.UserName,
		MemberCount: memberCount,
		Description: fullChat.Description,
		IsAdmin:     isAdmin,
		UpdatedAt:   time.Now().Format(time.RFC3339),
	}

	if err := b.store.UpsertChat(c); err != nil {
		return nil, err
	}
	return &c, nil
}

// SendMessage sends a text message to a chat
func (b *Bot) SendMessage(chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	_, err := b.api.Send(msg)
	return err
}

// PinMessage pins a message in a chat
func (b *Bot) PinMessage(chatID int64, messageID int) error {
	pin := tgbotapi.PinChatMessageConfig{
		ChatID:    chatID,
		MessageID: messageID,
	}
	_, err := b.api.Request(pin)
	return err
}

// UnpinMessage unpins a message
func (b *Bot) UnpinMessage(chatID int64, messageID int) error {
	unpin := tgbotapi.UnpinChatMessageConfig{
		ChatID:    chatID,
		MessageID: messageID,
	}
	_, err := b.api.Request(unpin)
	return err
}

// DeleteMessage deletes a message
func (b *Bot) DeleteMessage(chatID int64, messageID int) error {
	del := tgbotapi.NewDeleteMessage(chatID, messageID)
	_, err := b.api.Request(del)
	return err
}

// BanUser bans a user from a chat
func (b *Bot) BanUser(chatID int64, userID int64) error {
	ban := tgbotapi.BanChatMemberConfig{
		ChatMemberConfig: tgbotapi.ChatMemberConfig{
			ChatID: chatID,
			UserID: userID,
		},
	}
	_, err := b.api.Request(ban)
	return err
}

// UnbanUser unbans a user from a chat
func (b *Bot) UnbanUser(chatID int64, userID int64) error {
	unban := tgbotapi.UnbanChatMemberConfig{
		ChatMemberConfig: tgbotapi.ChatMemberConfig{
			ChatID: chatID,
			UserID: userID,
		},
		OnlyIfBanned: true,
	}
	_, err := b.api.Request(unban)
	return err
}

// GetAdmins returns list of chat administrators
func (b *Bot) GetAdmins(chatID int64) ([]AdminInfo, error) {
	admins, err := b.api.GetChatAdministrators(tgbotapi.ChatAdministratorsConfig{
		ChatConfig: tgbotapi.ChatConfig{ChatID: chatID},
	})
	if err != nil {
		return nil, err
	}

	var result []AdminInfo
	for _, a := range admins {
		username := ""
		if a.User != nil {
			username = a.User.FirstName
			if a.User.LastName != "" {
				username += " " + a.User.LastName
			}
			if a.User.UserName != "" {
				username = "@" + a.User.UserName
			}
		}
		info := AdminInfo{
			UserID:   a.User.ID,
			Username: username,
			Status:   a.Status,
			CustomTitle: a.CustomTitle,
		}
		if a.CanDeleteMessages {
			info.CanDeleteMessages = true
		}
		if a.CanRestrictMembers {
			info.CanRestrictMembers = true
		}
		if a.CanPromoteMembers {
			info.CanPromoteMembers = true
		}
		if a.CanChangeInfo {
			info.CanChangeInfo = true
		}
		if a.CanInviteUsers {
			info.CanInviteUsers = true
		}
		if a.CanPinMessages {
			info.CanPinMessages = true
		}
		if a.CanManageChat {
			info.CanManageChat = true
		}
		// Creator has all rights
		if a.Status == "creator" {
			info.CanDeleteMessages = true
			info.CanRestrictMembers = true
			info.CanPromoteMembers = true
			info.CanChangeInfo = true
			info.CanInviteUsers = true
			info.CanPinMessages = true
			info.CanManageChat = true
		}
		result = append(result, info)
	}
	return result, nil
}

// PromoteAdmin promotes a user to admin with specified permissions
func (b *Bot) PromoteAdmin(chatID int64, userID int64, perms AdminInfo) error {
	promo := tgbotapi.PromoteChatMemberConfig{
		ChatMemberConfig: tgbotapi.ChatMemberConfig{
			ChatID: chatID,
			UserID: userID,
		},
		CanDeleteMessages:  perms.CanDeleteMessages,
		CanRestrictMembers: perms.CanRestrictMembers,
		CanPromoteMembers:  perms.CanPromoteMembers,
		CanChangeInfo:      perms.CanChangeInfo,
		CanInviteUsers:     perms.CanInviteUsers,
		CanPinMessages:     perms.CanPinMessages,
		CanManageChat:      perms.CanManageChat,
	}
	_, err := b.api.Request(promo)
	return err
}

// DemoteAdmin removes admin rights from a user
func (b *Bot) DemoteAdmin(chatID int64, userID int64) error {
	promo := tgbotapi.PromoteChatMemberConfig{
		ChatMemberConfig: tgbotapi.ChatMemberConfig{
			ChatID: chatID,
			UserID: userID,
		},
	}
	_, err := b.api.Request(promo)
	return err
}

// SetAdminTitle sets custom title for an admin
func (b *Bot) SetAdminTitle(chatID int64, userID int64, title string) error {
	cfg := tgbotapi.SetChatAdministratorCustomTitle{
		ChatMemberConfig: tgbotapi.ChatMemberConfig{
			ChatID: chatID,
			UserID: userID,
		},
		CustomTitle: title,
	}
	_, err := b.api.Request(cfg)
	return err
}

// GetBotInfo returns bot username
func (b *Bot) GetBotInfo() string {
	return b.api.Self.UserName
}

// GetBotName returns display name for logging
func (b *Bot) GetBotName() string {
	return "Bot (@" + b.api.Self.UserName + ")"
}
