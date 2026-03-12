package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// DemoSession holds per-browser-session resources
type DemoSession struct {
	store      *Store
	proxy      *ProxyManager
	bridge     *BridgeManager
	server     *Server
	mux        *http.ServeMux
	token      string // session cookie value
	tempDBPath string
	createdAt  time.Time
	lastUsed   time.Time
}

// DemoManager manages ephemeral demo sessions
type DemoManager struct {
	mu       sync.Mutex
	sessions map[string]*DemoSession
	mainMux  *http.ServeMux // serves SPA for unauthenticated users
}

func NewDemoManager() *DemoManager {
	dm := &DemoManager{
		sessions: make(map[string]*DemoSession),
	}

	// Minimal mux for unauthenticated requests (SPA + login + health)
	dm.mainMux = http.NewServeMux()
	dm.mainMux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"status": "ok", "mode": "demo"})
	})
	dm.mainMux.HandleFunc("/api/auth/login", dm.handleDemoLogin)
	dm.mainMux.HandleFunc("/api/demo/cleanup", dm.handleCleanup)
	dm.mainMux.HandleFunc("/", handleStaticIndex)

	return dm
}

// handleStaticIndex serves the SPA without needing a Server instance
func handleStaticIndex(w http.ResponseWriter, r *http.Request) {
	data, err := templateFS.ReadFile("templates/index.html")
	if err != nil {
		http.Error(w, "Template not found", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

// ServeHTTP routes requests to the correct demo session or the main mux
func (dm *DemoManager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Always handle these paths globally (not per-session)
	switch r.URL.Path {
	case "/api/auth/login", "/api/demo/cleanup", "/api/health":
		dm.mainMux.ServeHTTP(w, r)
		return
	}

	// Check for existing demo session
	cookie, err := r.Cookie(sessionCookieName)
	if err == nil && cookie.Value != "" {
		dm.mu.Lock()
		sess, ok := dm.sessions[cookie.Value]
		if ok {
			sess.lastUsed = time.Now()
		}
		dm.mu.Unlock()
		if ok {
			sess.mux.ServeHTTP(w, r)
			return
		}
	}

	// No valid session — serve SPA (shows login page) or return 401 for API calls
	if len(r.URL.Path) > 4 && r.URL.Path[:5] == "/api/" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"unauthorized"}`))
		return
	}

	dm.mainMux.ServeHTTP(w, r)
}

// handleDemoLogin creates a new demo session on demo:demo credentials
func (dm *DemoManager) handleDemoLogin(w http.ResponseWriter, r *http.Request) {
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

	if req.Username != "demo" || req.Password != "demo" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(401)
		w.Write([]byte(`{"error":"invalid credentials. Use demo:demo"}`))
		return
	}

	sess, err := dm.createSession()
	if err != nil {
		log.Printf("[demo] Failed to create session: %v", err)
		writeError(w, fmt.Errorf("failed to create demo session"))
		return
	}

	expiresAt := time.Now().Add(30 * time.Minute)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sess.token,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})

	writeJSON(w, map[string]interface{}{
		"status": "ok",
		"user": map[string]interface{}{
			"id":           1,
			"username":     "demo",
			"display_name": "Demo User",
			"role":         "admin",
		},
		"must_change_password": false,
	})
}

// createSession sets up a fully isolated demo environment
func (dm *DemoManager) createSession() (*DemoSession, error) {
	// Generate session token
	token, err := GenerateSessionToken()
	if err != nil {
		return nil, fmt.Errorf("generate token: %w", err)
	}

	// Create temp DB
	tmpFile, err := os.CreateTemp("", "botmux-demo-*.db")
	if err != nil {
		return nil, fmt.Errorf("create temp db: %w", err)
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()

	store, err := NewStore(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("open store: %w", err)
	}

	// Seed demo user (replace the default admin created by migrate)
	if err := seedDemoUser(store, token); err != nil {
		store.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("seed user: %w", err)
	}

	// Seed demo bots
	if err := seedDemoBots(store); err != nil {
		store.Close()
		os.Remove(tmpPath)
		return nil, fmt.Errorf("seed bots: %w", err)
	}

	// Create ProxyManager and Server
	proxy := NewProxyManager(store)
	server := NewServer(store, proxy)

	// Create BridgeManager
	bridge := NewBridgeManager(store, proxy)
	bridge.Start()
	server.SetBridgeManager(bridge)

	// Start proxy (polls bots from fake API)
	proxy.Start()
	bridge.InstallHooks()

	// Build the HTTP mux for this session
	mux := server.BuildMux()

	// Add demo cleanup endpoint to session mux
	mux.HandleFunc("/api/demo/cleanup", dm.handleCleanup)

	sess := &DemoSession{
		store:      store,
		proxy:      proxy,
		bridge:     bridge,
		server:     server,
		mux:        mux,
		token:      token,
		tempDBPath: tmpPath,
		createdAt:  time.Now(),
		lastUsed:   time.Now(),
	}

	dm.mu.Lock()
	dm.sessions[token] = sess
	dm.mu.Unlock()

	log.Printf("[demo] Session created: %s...%s (db: %s)", token[:8], token[len(token)-4:], tmpPath)
	return sess, nil
}

// seedDemoUser updates the default admin to demo:demo with no password change required
func seedDemoUser(store *Store, sessionToken string) error {
	hash, err := HashPassword("demo")
	if err != nil {
		return err
	}
	_, err = store.db.Exec(`UPDATE auth_users SET username='demo', password_hash=?, display_name='Demo User', must_change_password=0 WHERE id=1`, hash)
	if err != nil {
		return err
	}
	// Create a session so the cookie works
	return store.CreateSession(sessionToken, 1, time.Now().Add(30*time.Minute))
}

// seedDemoBots inserts pre-configured demo bots
func seedDemoBots(store *Store) error {
	bots := []BotConfig{
		{
			Name:          "Support Bot",
			Token:         "111111111:AAFakeToken_SupportBot_Demo",
			ManageEnabled: true,
		},
		{
			Name:          "News Bot",
			Token:         "222222222:AAFakeToken_NewsBot_Demo",
			ManageEnabled: true,
		},
		{
			Name:          "Moderation Bot",
			Token:         "333333333:AAFakeToken_ModerationBot_Demo",
			ManageEnabled: true,
		},
	}

	for _, b := range bots {
		if _, err := store.AddBotConfig(b); err != nil {
			return fmt.Errorf("add bot %s: %w", b.Name, err)
		}
	}
	return nil
}

// handleCleanup handles explicit session cleanup (called from beforeunload)
func (dm *DemoManager) handleCleanup(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil || cookie.Value == "" {
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}

	dm.destroySession(cookie.Value)

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
	})
	writeJSON(w, map[string]string{"status": "ok"})
}

// destroySession cleans up all resources for a demo session
func (dm *DemoManager) destroySession(token string) {
	dm.mu.Lock()
	sess, ok := dm.sessions[token]
	if ok {
		delete(dm.sessions, token)
	}
	dm.mu.Unlock()

	if !ok {
		return
	}

	log.Printf("[demo] Destroying session %s...%s", token[:8], token[len(token)-4:])
	sess.proxy.StopAll()
	sess.store.Close()
	os.Remove(sess.tempDBPath)
	// Also remove WAL/SHM files
	os.Remove(sess.tempDBPath + "-wal")
	os.Remove(sess.tempDBPath + "-shm")
}

// StartReaper periodically cleans up expired demo sessions
func (dm *DemoManager) StartReaper(maxAge time.Duration) {
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			dm.mu.Lock()
			var expired []string
			for token, sess := range dm.sessions {
				if time.Since(sess.lastUsed) > maxAge {
					expired = append(expired, token)
				}
			}
			dm.mu.Unlock()

			for _, token := range expired {
				log.Printf("[demo] Session expired (inactive > %v)", maxAge)
				dm.destroySession(token)
			}
		}
	}()
}

// StopAll destroys all active demo sessions
func (dm *DemoManager) StopAll() {
	dm.mu.Lock()
	tokens := make([]string, 0, len(dm.sessions))
	for token := range dm.sessions {
		tokens = append(tokens, token)
	}
	dm.mu.Unlock()

	for _, token := range tokens {
		dm.destroySession(token)
	}
}
