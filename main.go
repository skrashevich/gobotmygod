package main

import (
	"flag"
	"log"
	"os"
)

// @title BotMux API
// @version 1.0
// @description Multi-bot Telegram manager with proxying, routing and LLM-based message dispatch.
// @contact.name BotMux
// @license.name MIT
// @host localhost:8080
// @BasePath /
// @securityDefinitions.apikey CookieAuth
// @in cookie
// @name botmux_session
func main() {
	token := flag.String("token", "", "Telegram bot token (optional if bots already exist in DB)")
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dbPath := flag.String("db", "botdata.db", "SQLite database path")
	webhookURL := flag.String("webhook", "", "Set webhook URL for the CLI bot (requires -token)")
	flag.Parse()

	if *token == "" {
		*token = os.Getenv("TELEGRAM_BOT_TOKEN")
	}

	store, err := NewStore(*dbPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer store.Close()

	proxy := NewProxyManager(store)
	server := NewServer(store, proxy)

	// Register CLI bot if token is provided
	if *token != "" {
		cliBot, err := NewBot(*token, store, 0)
		if err != nil {
			log.Fatalf("Failed to create bot: %v", err)
		}

		botID, err := store.RegisterCLIBot(*token, cliBot.GetBotInfo())
		if err != nil {
			log.Fatalf("Failed to register CLI bot: %v", err)
		}
		cliBot.botID = botID

		store.MigrateLegacyChats(botID)
		proxy.RegisterManagedBot(botID, cliBot)
		server.RegisterBot(botID, cliBot)

		if *webhookURL != "" {
			if err := cliBot.SetWebhook(*webhookURL); err != nil {
				log.Fatalf("Failed to set webhook: %v", err)
			}
			proxy.SetWebhookMode(botID)
			server.SetWebhookHandler("/tghook", proxy.WebhookHandler(botID))
			log.Printf("CLI bot [%d] @%s: webhook mode at %s", botID, cliBot.GetBotInfo(), *webhookURL)
		} else {
			if err := proxy.DeleteWebhook(*token); err != nil {
				log.Printf("Warning: could not delete webhook: %v", err)
			}
			log.Printf("CLI bot [%d] @%s: polling mode", botID, cliBot.GetBotInfo())
		}
	} else {
		bots, _ := store.GetBotConfigs()
		if len(bots) == 0 {
			log.Fatal("No token provided and no bots in database. Use -token flag or TELEGRAM_BOT_TOKEN env var to add the first bot, or add one via the web UI.")
		}
		log.Printf("No token provided, using %d bot(s) from database", len(bots))
	}

	// Start ProxyManager for ALL bots
	proxy.Start()
	defer proxy.StopAll()

	if err := server.Start(*addr); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
