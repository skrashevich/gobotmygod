package main

import (
	"log"
)

// seedDemoData populates a fresh database with demo user and bots.
// Called once on startup in demo mode if no bots exist yet.
func seedDemoData(store *Store) {
	// Check if data already exists
	bots, _ := store.GetBotConfigs()
	if len(bots) > 0 {
		log.Printf("[demo] Database already has %d bot(s), skipping seed", len(bots))
		return
	}

	log.Printf("[demo] Seeding demo data...")

	// Update default admin to demo:demo with no password change
	hash, err := HashPassword("demo")
	if err != nil {
		log.Printf("[demo] Failed to hash password: %v", err)
		return
	}
	if _, err := store.db.Exec(`UPDATE auth_users SET username='demo', password_hash=?, display_name='Demo User', must_change_password=0 WHERE id=1`, hash); err != nil {
		log.Printf("[demo] Failed to update admin user: %v", err)
		return
	}

	// Seed demo bots
	demoBots := []BotConfig{
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

	for _, b := range demoBots {
		if _, err := store.AddBotConfig(b); err != nil {
			log.Printf("[demo] Failed to add bot %s: %v", b.Name, err)
		}
	}

	log.Printf("[demo] Seeded %d demo bots", len(demoBots))
}
