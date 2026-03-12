// Package db manages the PostgreSQL connection pool and schema migrations.
// It shares the same DATABASE_URL as the Next.js voordeeleter app so both
// services read/write the same tables.
package db

import (
	"database/sql"
	"fmt"
	"log"
	"os"

	_ "github.com/lib/pq"
)

var pool *sql.DB

// Init opens the connection pool and runs auto-migrations.
func Init() error {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is niet ingesteld")
	}
	var err error
	pool, err = sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("db open: %w", err)
	}
	pool.SetMaxOpenConns(10)
	pool.SetMaxIdleConns(5)

	if err = pool.Ping(); err != nil {
		return fmt.Errorf("db ping: %w", err)
	}
	log.Println("[DB] Verbonden met PostgreSQL")

	return migrate()
}

// DB returns the shared connection pool (panics when called before Init).
func DB() *sql.DB {
	if pool == nil {
		panic("db.Init() is nog niet aangeroepen")
	}
	return pool
}

// migrate creates tables that don't exist yet (idempotent).
func migrate() error {
	_, err := pool.Exec(`
		-- product_prices: elke scrape-snapshot voor prijshistorie en trends
		CREATE TABLE IF NOT EXISTS product_prices (
			id             BIGSERIAL PRIMARY KEY,
			store          TEXT NOT NULL,
			product_id     TEXT NOT NULL,
			naam           TEXT NOT NULL,
			eenheid        TEXT,
			categorie      TEXT,
			prijs          REAL NOT NULL,
			actie_prijs    REAL,
			in_aanbieding  BOOLEAN NOT NULL DEFAULT FALSE,
			bonus_tekst    TEXT,
			afbeelding_url TEXT,
			product_url    TEXT,
			gemeten_op     TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS idx_prices_store_id ON product_prices(store, product_id);
		CREATE INDEX IF NOT EXISTS idx_prices_gemeten  ON product_prices(gemeten_op DESC);
		CREATE INDEX IF NOT EXISTS idx_prices_naam     ON product_prices(store, lower(naam));

		-- search_cache: gedeeld met Next.js app (zelfde tabel, zelfde schema)
		CREATE TABLE IF NOT EXISTS search_cache (
			sleutel         TEXT NOT NULL,
			supermarkt      TEXT NOT NULL,
			resultaten_json TEXT NOT NULL,
			gecached_op     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (sleutel, supermarkt)
		);

		-- deals: gedeeld met Next.js app (zelfde tabel, zelfde schema)
		CREATE TABLE IF NOT EXISTS deals (
			id              TEXT NOT NULL,
			supermarkt      TEXT NOT NULL,
			naam            TEXT NOT NULL,
			beschrijving    TEXT NOT NULL,
			"originalPrijs" REAL NOT NULL,
			"actiePrijs"    REAL NOT NULL,
			"kortingPercent" INTEGER NOT NULL,
			eenheid         TEXT NOT NULL,
			categorie       TEXT NOT NULL,
			"afbeeldingUrl" TEXT,
			"geldigTot"     TEXT NOT NULL,
			"opgeslagenOp"  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			weeknummer      TEXT NOT NULL,
			PRIMARY KEY (id, weeknummer)
		);
		CREATE INDEX IF NOT EXISTS idx_deals_week       ON deals(weeknummer);
		CREATE INDEX IF NOT EXISTS idx_deals_supermarkt ON deals(supermarkt);

		-- api_tokens: persistente opslag voor AH mobile API tokens
		CREATE TABLE IF NOT EXISTS api_tokens (
			token_key     TEXT PRIMARY KEY,
			token_value   TEXT NOT NULL,
			expires_at    TIMESTAMPTZ NOT NULL,
			opgeslagen_op TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
	`)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	log.Println("[DB] Schema up-to-date")
	return nil
}
