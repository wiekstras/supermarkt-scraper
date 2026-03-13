// supermarkt-scraper: standalone Go microservice for scraping AH, Jumbo, Poiesz, Lidl and Aldi.
// Exposes a REST API and runs a built-in cron scheduler.
package main

import (
	"log"
	"net/http"
	"os"

	"github.com/wiekstras/supermarkt-scraper/api"
	"github.com/wiekstras/supermarkt-scraper/db"
	"github.com/wiekstras/supermarkt-scraper/scheduler"
	"github.com/wiekstras/supermarkt-scraper/scraper"
	"github.com/wiekstras/supermarkt-scraper/scraper/ah"
	"github.com/wiekstras/supermarkt-scraper/scraper/aldi"
	"github.com/wiekstras/supermarkt-scraper/scraper/jumbo"
	"github.com/wiekstras/supermarkt-scraper/scraper/lidl"
	"github.com/wiekstras/supermarkt-scraper/scraper/poiesz"
)

func main() {
	// ── Database ─────────────────────────────────────────────────────────────
	if err := db.Init(); err != nil {
		log.Fatalf("[main] DB init mislukt: %v", err)
	}

	// ── Stores ───────────────────────────────────────────────────────────────
	// AH krijgt een DB-backed token store zodat tokens Railway restarts overleven.
	stores := []scraper.Store{
		ah.New(db.NewTokenStore()),
		jumbo.New(),
		poiesz.New(),
		lidl.New(),
		aldi.New(),
	}
	log.Printf("[main] %d stores geladen: ah, jumbo, poiesz, lidl, aldi", len(stores))

	// ── Scheduler ────────────────────────────────────────────────────────────
	sched := scheduler.New(stores)
	sched.Start()
	defer sched.Stop()

	// ── HTTP server ──────────────────────────────────────────────────────────
	server := api.New(stores, sched)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	log.Printf("[main] Server luistert op %s", addr)
	if err := http.ListenAndServe(addr, server.Handler()); err != nil {
		log.Fatalf("[main] Server gestopt: %v", err)
	}
}
