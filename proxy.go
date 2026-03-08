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

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// ProxyManager manages polling and forwarding for all non-CLI bots
type ProxyManager struct {
	store       *Store
	mu          sync.Mutex
	runners     map[int64]*proxyRunner
	managedBots map[int64]*Bot // botID -> Bot instance for management processing
	client      *http.Client
}

type proxyRunner struct {
	cancel context.CancelFunc
	botID  int64
}

func NewProxyManager(store *Store) *ProxyManager {
	return &ProxyManager{
		store:       store,
		runners:     make(map[int64]*proxyRunner),
		managedBots: make(map[int64]*Bot),
		client:      &http.Client{Timeout: 120 * time.Second},
	}
}

// RegisterManagedBot registers a Bot instance for management processing
func (pm *ProxyManager) RegisterManagedBot(botID int64, bot *Bot) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	pm.managedBots[botID] = bot
	log.Printf("[proxy] RegisterManagedBot: botID=%d", botID)
}

// UnregisterManagedBot removes a Bot instance
func (pm *ProxyManager) UnregisterManagedBot(botID int64) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	delete(pm.managedBots, botID)
	log.Printf("[proxy] UnregisterManagedBot: botID=%d", botID)
}

// Start launches goroutines for all active non-CLI bots
func (pm *ProxyManager) Start() {
	bots, err := pm.store.GetBotConfigs()
	if err != nil {
		log.Printf("[proxy] Start: failed to load bots: %v", err)
		return
	}
	log.Printf("[proxy] Start: loaded %d bot configs", len(bots))
	for _, bot := range bots {
		log.Printf("[proxy] Start: bot id=%d name=%q source=%s manage=%v proxy=%v backend=%q",
			bot.ID, bot.Name, bot.Source, bot.ManageEnabled, bot.ProxyEnabled, bot.BackendURL)
		if bot.Source == "cli" {
			log.Printf("[proxy] Start: skipping CLI bot id=%d", bot.ID)
			continue
		}
		if bot.ManageEnabled || bot.ProxyEnabled {
			log.Printf("[proxy] Start: starting bot id=%d", bot.ID)
			pm.startBot(bot.ID)
		} else {
			log.Printf("[proxy] Start: bot id=%d has no manage/proxy enabled, skipping", bot.ID)
		}
	}
}

func (pm *ProxyManager) startBot(botID int64) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if r, ok := pm.runners[botID]; ok {
		log.Printf("[proxy] startBot: cancelling existing runner for botID=%d", botID)
		r.cancel()
		delete(pm.runners, botID)
	}

	ctx, cancel := context.WithCancel(context.Background())
	pm.runners[botID] = &proxyRunner{cancel: cancel, botID: botID}
	log.Printf("[proxy] startBot: launched pollLoop for botID=%d", botID)

	go pm.pollLoop(ctx, botID)
}

func (pm *ProxyManager) stopBot(botID int64) {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	if r, ok := pm.runners[botID]; ok {
		log.Printf("[proxy] stopBot: stopping botID=%d", botID)
		r.cancel()
		delete(pm.runners, botID)
	}
}

func (pm *ProxyManager) StopAll() {
	pm.mu.Lock()
	defer pm.mu.Unlock()

	for id, r := range pm.runners {
		r.cancel()
		delete(pm.runners, id)
	}
	log.Printf("[proxy] StopAll: all runners stopped")
}

func (pm *ProxyManager) IsRunning(botID int64) bool {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	_, ok := pm.runners[botID]
	return ok
}

// RestartBot restarts a bot after config changes. Creates/removes managed Bot instance as needed.
func (pm *ProxyManager) RestartBot(botID int64) error {
	bot, err := pm.store.GetBotConfig(botID)
	if err != nil {
		log.Printf("[proxy] RestartBot: failed to get config for botID=%d: %v", botID, err)
		return err
	}

	log.Printf("[proxy] RestartBot: botID=%d source=%s manage=%v proxy=%v backend=%q",
		botID, bot.Source, bot.ManageEnabled, bot.ProxyEnabled, bot.BackendURL)

	pm.stopBot(botID)

	if bot.Source == "cli" {
		log.Printf("[proxy] RestartBot: skipping CLI bot id=%d", botID)
		return nil
	}

	active := bot.ManageEnabled || bot.ProxyEnabled

	// Create or remove managed Bot instance
	if bot.ManageEnabled {
		pm.mu.Lock()
		_, hasManagedBot := pm.managedBots[botID]
		pm.mu.Unlock()
		if !hasManagedBot {
			log.Printf("[proxy] RestartBot: creating managed Bot instance for botID=%d", botID)
			managedBot, err := NewBot(bot.Token, pm.store, botID)
			if err != nil {
				log.Printf("[proxy] RestartBot: failed to create bot instance for botID=%d: %v", botID, err)
				return fmt.Errorf("failed to create bot instance: %w", err)
			}
			pm.RegisterManagedBot(botID, managedBot)
		} else {
			log.Printf("[proxy] RestartBot: managed Bot instance already exists for botID=%d", botID)
		}
	} else {
		pm.UnregisterManagedBot(botID)
	}

	if active {
		log.Printf("[proxy] RestartBot: starting bot id=%d", botID)
		pm.startBot(botID)
	} else {
		log.Printf("[proxy] RestartBot: bot id=%d not active (manage=%v proxy=%v)", botID, bot.ManageEnabled, bot.ProxyEnabled)
	}
	return nil
}

func (pm *ProxyManager) pollLoop(ctx context.Context, botID int64) {
	retryDelay := time.Second
	maxRetryDelay := 30 * time.Second
	lastHealthCheck := time.Time{}
	healthCheckInterval := 60 * time.Second
	pollCount := 0

	log.Printf("[proxy] pollLoop STARTED for botID=%d", botID)

	for {
		select {
		case <-ctx.Done():
			log.Printf("[proxy] pollLoop STOPPED (context cancelled) for botID=%d after %d polls", botID, pollCount)
			return
		default:
		}

		bot, err := pm.store.GetBotConfig(botID)
		if err != nil {
			log.Printf("[proxy] pollLoop: failed to load config for botID=%d: %v — exiting", botID, err)
			return
		}
		if !bot.ManageEnabled && !bot.ProxyEnabled {
			log.Printf("[proxy] pollLoop: botID=%d has no manage/proxy enabled — exiting", botID)
			return
		}

		timeout := bot.PollingTimeout
		if timeout <= 0 {
			timeout = 30
		}

		pollCount++
		if pollCount <= 3 || pollCount%10 == 0 {
			log.Printf("[proxy] pollLoop: botID=%d poll #%d (offset=%d, timeout=%ds, proxy=%v, manage=%v, backend=%q)",
				botID, pollCount, bot.Offset, timeout, bot.ProxyEnabled, bot.ManageEnabled, bot.BackendURL)
		}

		updates, err := pm.getUpdates(ctx, bot.Token, bot.Offset, timeout)
		if err != nil {
			pm.store.UpdateBotStatus(botID, fmt.Sprintf("getUpdates error: %v", err), "")
			log.Printf("[proxy] pollLoop: botID=%d getUpdates ERROR: %v", botID, err)

			select {
			case <-ctx.Done():
				return
			case <-time.After(retryDelay):
			}
			retryDelay = min(retryDelay*2, maxRetryDelay)
			continue
		}

		retryDelay = time.Second

		if len(updates) > 0 {
			log.Printf("[proxy] pollLoop: botID=%d received %d updates", botID, len(updates))
		}

		// Periodic backend health check for proxy bots
		if bot.ProxyEnabled && bot.BackendURL != "" && time.Since(lastHealthCheck) >= healthCheckInterval {
			lastHealthCheck = time.Now()
			status, err := pm.CheckAndStoreHealth(botID)
			if err != nil {
				log.Printf("[proxy] pollLoop: botID=%d health check FAILED: %s — %v", botID, status, err)
			} else {
				log.Printf("[proxy] pollLoop: botID=%d health check OK: %s", botID, status)
			}
		}

		for i, update := range updates {
			select {
			case <-ctx.Done():
				return
			default:
			}

			updateID, ok := update["update_id"].(float64)
			if !ok {
				log.Printf("[proxy] pollLoop: botID=%d update[%d] has no valid update_id, skipping", botID, i)
				continue
			}

			// Log update summary
			updateSummary := fmt.Sprintf("update_id=%d", int64(updateID))
			if msg, ok := update["message"].(map[string]interface{}); ok {
				if text, ok := msg["text"].(string); ok {
					if len(text) > 80 {
						text = text[:80] + "..."
					}
					updateSummary += fmt.Sprintf(" message.text=%q", text)
				}
				if from, ok := msg["from"].(map[string]interface{}); ok {
					if uname, ok := from["username"].(string); ok {
						updateSummary += fmt.Sprintf(" from=@%s", uname)
					}
				}
				if chat, ok := msg["chat"].(map[string]interface{}); ok {
					if chatID, ok := chat["id"].(float64); ok {
						updateSummary += fmt.Sprintf(" chat_id=%d", int64(chatID))
					}
				}
			}
			log.Printf("[proxy] pollLoop: botID=%d processing %s", botID, updateSummary)

			// Proxy: forward to backend
			if bot.ProxyEnabled && bot.BackendURL != "" {
				log.Printf("[proxy] forward: botID=%d %s → %s", botID, updateSummary, bot.BackendURL)
				err := pm.forwardUpdate(ctx, bot, update)
				if err != nil {
					pm.store.UpdateBotStatus(botID, fmt.Sprintf("forward error: %v", err), "")
					log.Printf("[proxy] forward: botID=%d FAILED for %s: %v", botID, updateSummary, err)
					// Don't skip management processing on forward failure
				} else {
					log.Printf("[proxy] forward: botID=%d SUCCESS for %s", botID, updateSummary)
					pm.store.IncrementBotForwarded(botID)
				}
			} else if bot.ProxyEnabled {
				log.Printf("[proxy] pollLoop: botID=%d proxy enabled but no backend_url set!", botID)
			}

			// Management: process update for chat/message tracking
			if bot.ManageEnabled {
				log.Printf("[proxy] manage: botID=%d processing %s", botID, updateSummary)
				pm.processForManagement(botID, update)
			}

			newOffset := int64(updateID) + 1
			pm.store.UpdateBotOffset(botID, newOffset)
			pm.store.UpdateBotStatus(botID, "", time.Now().Format(time.RFC3339))
		}
	}
}

func (pm *ProxyManager) processForManagement(botID int64, rawUpdate map[string]interface{}) {
	pm.mu.Lock()
	bot := pm.managedBots[botID]
	pm.mu.Unlock()
	if bot == nil {
		log.Printf("[proxy] processForManagement: botID=%d has no managed Bot instance!", botID)
		return
	}

	data, err := json.Marshal(rawUpdate)
	if err != nil {
		log.Printf("[proxy] processForManagement: botID=%d marshal error: %v", botID, err)
		return
	}
	var update tgbotapi.Update
	if err := json.Unmarshal(data, &update); err != nil {
		log.Printf("[proxy] processForManagement: botID=%d unmarshal to tgbotapi.Update error: %v", botID, err)
		return
	}
	bot.processUpdate(update)
}

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

func (pm *ProxyManager) forwardUpdate(ctx context.Context, bot *BotConfig, update map[string]interface{}) error {
	data, err := json.Marshal(update)
	if err != nil {
		return err
	}

	log.Printf("[proxy] forwardUpdate: POST %s (%d bytes)", bot.BackendURL, len(data))

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

	log.Printf("[proxy] forwardUpdate: backend responded %d (%d bytes): %s",
		resp.StatusCode, len(respBody), truncate(string(respBody), 500))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("backend returned %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	pm.handleWebhookReply(bot.Token, respBody)
	return nil
}

func (pm *ProxyManager) handleWebhookReply(token string, body []byte) {
	if len(body) == 0 {
		log.Printf("[proxy] handleWebhookReply: empty response body, no action")
		return
	}

	var reply map[string]interface{}
	if err := json.Unmarshal(body, &reply); err != nil {
		log.Printf("[proxy] handleWebhookReply: response is not JSON: %s", truncate(string(body), 200))
		return
	}

	methodRaw, ok := reply["method"]
	if !ok {
		log.Printf("[proxy] handleWebhookReply: no 'method' field in response — not a webhook reply (keys: %v)", mapKeys(reply))
		return
	}
	method, ok := methodRaw.(string)
	if !ok || method == "" {
		log.Printf("[proxy] handleWebhookReply: 'method' field is not a valid string: %v", methodRaw)
		return
	}

	delete(reply, "method")
	data, err := json.Marshal(reply)
	if err != nil {
		log.Printf("[proxy] handleWebhookReply: failed to marshal reply params: %v", err)
		return
	}

	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/%s", token, method)
	log.Printf("[proxy] handleWebhookReply: executing method=%s (%d bytes params)", method, len(data))

	resp, err := pm.client.Post(apiURL, "application/json", bytes.NewReader(data))
	if err != nil {
		log.Printf("[proxy] handleWebhookReply: Telegram API call %s FAILED: %v", method, err)
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	log.Printf("[proxy] handleWebhookReply: Telegram API %s responded %d: %s",
		method, resp.StatusCode, truncate(string(respBody), 300))
}

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

func (pm *ProxyManager) DeleteWebhook(token string) error {
	log.Printf("[proxy] DeleteWebhook: removing webhook")
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
	log.Printf("[proxy] DeleteWebhook: success — %s", result.Description)
	return nil
}

// CheckBackendHealth sends a test POST to the backend URL and returns status
func (pm *ProxyManager) CheckBackendHealth(backendURL, secretToken string) (string, error) {
	if backendURL == "" {
		return "no_url", fmt.Errorf("no backend URL configured")
	}

	testPayload := []byte(`{"health_check":true}`)
	req, err := http.NewRequest("POST", backendURL, bytes.NewReader(testPayload))
	if err != nil {
		return "error", fmt.Errorf("invalid URL: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if secretToken != "" {
		req.Header.Set("X-Telegram-Bot-Api-Secret-Token", secretToken)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "unreachable", fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 500 {
		return fmt.Sprintf("ok:%d", resp.StatusCode), nil
	}
	return fmt.Sprintf("error:%d", resp.StatusCode), fmt.Errorf("backend returned %d", resp.StatusCode)
}

// CheckAndStoreHealth runs a health check and stores the result
func (pm *ProxyManager) CheckAndStoreHealth(botID int64) (string, error) {
	bot, err := pm.store.GetBotConfig(botID)
	if err != nil {
		return "", err
	}
	status, checkErr := pm.CheckBackendHealth(bot.BackendURL, bot.SecretToken)
	now := time.Now().Format(time.RFC3339)
	if checkErr != nil {
		pm.store.UpdateBackendHealth(botID, status+": "+checkErr.Error(), now)
		return status, checkErr
	}
	pm.store.UpdateBackendHealth(botID, status, now)
	return status, nil
}

// GetManagedBot returns a managed Bot instance by botID
func (pm *ProxyManager) GetManagedBot(botID int64) *Bot {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	return pm.managedBots[botID]
}

// truncate truncates a string to maxLen
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// mapKeys returns the keys of a map for debugging
func mapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
