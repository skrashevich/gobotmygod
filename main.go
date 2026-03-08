package main

import (
	"flag"
	"log"
	"os"
)

func main() {
	token := flag.String("token", "", "Telegram bot token")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dbPath := flag.String("db", "botdata.db", "SQLite database path")
	webhookURL := flag.String("webhook", "", "Set webhook URL (e.g. https://example.com/tghook). If empty, uses polling.")
	flag.Parse()

	if *token == "" {
		*token = os.Getenv("TELEGRAM_BOT_TOKEN")
	}
	if *token == "" {
		log.Fatal("Bot token required: use -token flag or TELEGRAM_BOT_TOKEN env var")
	}

	store, err := NewStore(*dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer store.Close()

	proxy := NewProxyManager(store)

	// Register CLI bot in the bots table
	cliBot, err := NewBot(*token, store, 0)
	if err != nil {
		log.Fatalf("Failed to create bot: %v", err)
	}

	botID, err := store.RegisterCLIBot(*token, cliBot.GetBotInfo())
	if err != nil {
		log.Fatalf("Failed to register CLI bot: %v", err)
	}
	cliBot.botID = botID

	// Migrate legacy chats (bot_id=0 -> real botID)
	store.MigrateLegacyChats(botID)

	// Register CLI bot for management processing (same as web bots)
	proxy.RegisterManagedBot(botID, cliBot)

	// Create server
	server := NewServer(store, proxy)
	server.RegisterBot(botID, cliBot)

	if *webhookURL != "" {
		// Webhook mode: set webhook, register handler with proxy support
		if err := cliBot.SetWebhook(*webhookURL); err != nil {
			log.Fatalf("Failed to set webhook: %v", err)
		}
		proxy.SetWebhookMode(botID)
		server.SetWebhookHandler("/tghook", proxy.WebhookHandler(botID))
		log.Printf("CLI bot [%d]: webhook mode at %s", botID, *webhookURL)
	} else {
		// Polling mode: delete webhook, ProxyManager will poll
		if err := proxy.DeleteWebhook(*token); err != nil {
			log.Printf("Warning: could not delete webhook: %v", err)
		}
		log.Printf("CLI bot [%d]: polling mode (managed by ProxyManager)", botID)
	}

	// Start ProxyManager for ALL bots (CLI + web, no difference)
	proxy.Start()
	defer proxy.StopAll()

	if err := server.Start(*addr); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
