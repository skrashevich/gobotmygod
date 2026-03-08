package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"time"
)

// ProxyBot represents a bot configuration for reverse proxy mode
type ProxyBot struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	Token          string `json:"token"`
	BackendURL     string `json:"backend_url"`
	SecretToken    string `json:"secret_token,omitempty"`
	Enabled        bool   `json:"enabled"`
	PollingTimeout int    `json:"polling_timeout"`
	Offset         int64  `json:"offset"`
	BotUsername     string `json:"bot_username,omitempty"`
	LastError      string `json:"last_error,omitempty"`
	LastActivity   string `json:"last_activity,omitempty"`
	UpdatesForwarded int64 `json:"updates_forwarded"`
}

// ProxyManager manages multiple polling→webhook proxy goroutines
type ProxyManager struct {
	store   *Store
	mu      sync.Mutex
	runners map[int64]*proxyRunner
	client  *http.Client
}

type proxyRunner struct {
	cancel context.CancelFunc
	botID  int64
}

func NewProxyManager(store *Store) *ProxyManager {
	return &ProxyManager{
		store:   store,
		runners: make(map[int64]*proxyRunner),
		client: &http.Client{
			Timeout: 120 * time.Second,
		},
	}
}

// Start launches proxy goroutines for all enabled bots
func (pm *ProxyManager) Start() {
	bots, err := pm.store.GetProxyBots()
	if err != nil {
		log.Printf("Proxy: failed to load bots: %v", err)
		return
	}
	for _, bot := range bots {
		if bot.Enabled {
			pm.startBot(bot.ID)
		}
	}
	if len(bots) > 0 {
		log.Printf("Proxy: loaded %d bot(s)", len(bots))
	}
}

// startBot starts polling for a specific bot
func (pm *ProxyManager) startBot(botID int64) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	// Stop existing runner if any
	if r, ok := pm.runners[botID]; ok {
		r.cancel()
		delete(pm.runners, botID)
	}

	ctx, cancel := context.WithCancel(context.Background())
	pm.runners[botID] = &proxyRunner{cancel: cancel, botID: botID}

	go pm.pollLoop(ctx, botID)
}

// stopBot stops polling for a specific bot
func (pm *ProxyManager) stopBot(botID int64) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if r, ok := pm.runners[botID]; ok {
		r.cancel()
		delete(pm.runners, botID)
	}
}

// StopAll stops all proxy goroutines
func (pm *ProxyManager) StopAll() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for id, r := range pm.runners {
		r.cancel()
		delete(pm.runners, id)
	}
}

// IsRunning checks if a bot is currently running
func (pm *ProxyManager) IsRunning(botID int64) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	_, ok := pm.runners[botID]
	return ok
}

// RestartBot restarts proxy for a bot (used after config changes)
func (pm *ProxyManager) RestartBot(botID int64) error {
	bot, err := pm.store.GetProxyBot(botID)
	if err != nil {
		return err
	}
	pm.stopBot(botID)
	if bot.Enabled {
		pm.startBot(botID)
	}
	return nil
}

func (pm *ProxyManager) pollLoop(ctx context.Context, botID int64) {
	retryDelay := time.Second
	maxRetryDelay := 30 * time.Second

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		bot, err := pm.store.GetProxyBot(botID)
		if err != nil || !bot.Enabled {
			return
		}

		timeout := bot.PollingTimeout
		if timeout <= 0 {
			timeout = 30
		}

		updates, err := pm.getUpdates(ctx, bot.Token, bot.Offset, timeout)
		if err != nil {
			pm.store.UpdateProxyBotStatus(botID, fmt.Sprintf("getUpdates error: %v", err), "")
			log.Printf("Proxy [%s]: getUpdates error: %v", bot.Name, err)

			select {
			case <-ctx.Done():
				return
			case <-time.After(retryDelay):
			}
			retryDelay = min(retryDelay*2, maxRetryDelay)
			continue
		}

		retryDelay = time.Second

		for _, update := range updates {
			select {
			case <-ctx.Done():
				return
			default:
			}

			updateID, ok := update["update_id"].(float64)
			if !ok {
				continue
			}

			err := pm.forwardUpdate(ctx, bot, update)
			if err != nil {
				pm.store.UpdateProxyBotStatus(botID, fmt.Sprintf("forward error: %v", err), "")
				log.Printf("Proxy [%s]: forward error for update %d: %v", bot.Name, int64(updateID), err)

				select {
				case <-ctx.Done():
					return
				case <-time.After(retryDelay):
				}
				retryDelay = min(retryDelay*2, maxRetryDelay)
				continue
			}

			newOffset := int64(updateID) + 1
			pm.store.UpdateProxyBotOffset(botID, newOffset)
			pm.store.UpdateProxyBotStatus(botID, "", time.Now().Format(time.RFC3339))
			pm.store.IncrementProxyBotForwarded(botID)
		}
	}
}

// getUpdates calls Telegram getUpdates API
func (pm *ProxyManager) getUpdates(ctx context.Context, token string, offset int64, timeout int) ([]map[string]interface{}, error) {
	reqBody, _ := json.Marshal(map[string]interface{}{
		"offset":  offset,
		"timeout": timeout,
		"limit":   100,
	})

	url := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates", token)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	// Use a client with timeout longer than polling timeout
	client := &http.Client{Timeout: time.Duration(timeout+10) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result struct {
		OK          bool                     `json:"ok"`
		Result      []map[string]interface{} `json:"result"`
		Description string                   `json:"description"`
		ErrorCode   int                      `json:"error_code"`
		RetryAfter  int                      `json:"retry_after"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("invalid response: %s", string(body[:min(200, len(body))]))
	}

	if !result.OK {
		if result.RetryAfter > 0 {
			return nil, fmt.Errorf("rate limited, retry after %ds: %s", result.RetryAfter, result.Description)
		}
		return nil, fmt.Errorf("API error %d: %s", result.ErrorCode, result.Description)
	}

	return result.Result, nil
}

// forwardUpdate sends an update to the backend URL as a webhook POST
func (pm *ProxyManager) forwardUpdate(ctx context.Context, bot *ProxyBot, update map[string]interface{}) error {
	data, err := json.Marshal(update)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", bot.BackendURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if bot.SecretToken != "" {
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", bot.SecretToken)
	}

	backendClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := backendClient.Do(req)
	if err != nil {
		return fmt.Errorf("backend request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("backend returned %d: %s", resp.StatusCode, string(respBody[:min(200, len(respBody))]))
	}

	// Handle webhook reply pattern: if backend responds with JSON containing "method",
	// proxy it to Telegram API
	pm.handleWebhookReply(bot.Token, respBody)

	return nil
}

// handleWebhookReply proxies webhook-style replies back to Telegram
func (pm *ProxyManager) handleWebhookReply(token string, body []byte) {
	if len(body) == 0 {
		return
	}

	var reply map[string]interface{}
	if err := json.Unmarshal(body, &reply); err != nil {
		return
	}

	methodRaw, ok := reply["method"]
	if !ok {
		return
	}
	method, ok := methodRaw.(string)
	if !ok || method == "" {
		return
	}

	// Remove "method" key and forward to Telegram
	delete(reply, "method")
	data, err := json.Marshal(reply)
	if err != nil {
		return
	}

	url := fmt.Sprintf("https://api.telegram.org/bot%s/%s", token, method)
	resp, err := pm.client.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		log.Printf("Proxy: webhook reply error: %v", err)
		return
	}
	resp.Body.Close()
}

// ValidateToken checks if a bot token is valid by calling getMe
func (pm *ProxyManager) ValidateToken(token string) (string, error) {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/getMe", token)
	resp, err := pm.client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			Username string `json:"username"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	if !result.OK {
		return "", fmt.Errorf("invalid token: %s", result.Description)
	}
	return result.Result.Username, nil
}

// DeleteWebhook removes webhook for a bot token (required before polling)
func (pm *ProxyManager) DeleteWebhook(token string) error {
	url := fmt.Sprintf("https://api.telegram.org/bot%s/deleteWebhook", token)
	resp, err := pm.client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("deleteWebhook failed: %s", result.Description)
	}
	return nil
}
