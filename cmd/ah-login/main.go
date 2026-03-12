// cmd/ah-login is a one-time CLI tool that authenticates with AH via the
// browser OAuth flow and saves the tokens in the PostgreSQL database.
//
// Run this once on the server (or locally while pointing to the production DB)
// to enable kassabonnen access:
//
//	DATABASE_URL=... go run ./cmd/ah-login
//
// The tokens are stored in the `api_tokens` table and automatically refreshed
// by the main service. Re-run only when the refresh token expires (~30 days
// of inactivity).
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"

	"github.com/wiekstras/supermarkt-scraper/ahclient"
	"github.com/wiekstras/supermarkt-scraper/db"
)

func main() {
	// DB init
	if err := db.Init(); err != nil {
		log.Fatalf("DB init mislukt: %v\nZorg dat DATABASE_URL ingesteld is.", err)
	}

	ts := db.NewTokenStore()
	client := ahclient.New(ts)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	fmt.Println("=== AH Login ===")
	fmt.Println("Je browser wordt geopend voor de AH inlogpagina.")
	fmt.Println("Na inloggen worden de tokens automatisch opgeslagen in de database.")
	fmt.Println()

	if err := client.Login(ctx); err != nil {
		log.Fatalf("Login mislukt: %v", err)
	}

	fmt.Println()
	fmt.Println("✓ Login geslaagd! Tokens opgeslagen in de database.")
	fmt.Println("  De supermarkt-scraper service heeft nu toegang tot jouw kassabonnen.")
}
