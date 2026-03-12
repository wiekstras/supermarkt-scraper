package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/wiekstras/supermarkt-scraper/scraper"
)

// ─── Week helpers ─────────────────────────────────────────────────────────────

// HuidigeWeek returns the ISO week string used as the deals partition key,
// e.g. "2025-W12". Mirrors the getHuidigeWeek() function in Next.js.
func HuidigeWeek() string {
	now := time.Now()
	year, week := now.ISOWeek()
	return fmt.Sprintf("%d-W%02d", year, week)
}

// ─── Deals ────────────────────────────────────────────────────────────────────

// SlaDealsOp upserts a batch of deals for the current week.
// Mirrors the Next.js slaDealsOp() function so both apps share the same table.
func SlaDealsOp(deals []scraper.Deal) error {
	week := HuidigeWeek()
	tx, err := pool.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO deals
			(id, supermarkt, naam, beschrijving, "originalPrijs", "actiePrijs",
			 "kortingPercent", eenheid, categorie, "afbeeldingUrl", "geldigTot", "opgeslagenOp", weeknummer)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NOW(),$12)
		ON CONFLICT (id, weeknummer) DO UPDATE SET
			naam            = EXCLUDED.naam,
			beschrijving    = EXCLUDED.beschrijving,
			"originalPrijs" = EXCLUDED."originalPrijs",
			"actiePrijs"    = EXCLUDED."actiePrijs",
			"kortingPercent"= EXCLUDED."kortingPercent",
			"afbeeldingUrl" = EXCLUDED."afbeeldingUrl",
			"opgeslagenOp"  = NOW()
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, d := range deals {
		img := sql.NullString{String: d.AfbeeldingUrl, Valid: d.AfbeeldingUrl != ""}
		_, err := stmt.Exec(
			d.ID, d.Supermarkt, d.Naam, d.Beschrijving,
			d.OriginalPrijs, d.ActiePrijs, d.KortingPercent,
			d.Eenheid, d.Categorie, img, d.GeldigTot, week,
		)
		if err != nil {
			log.Printf("[DB] Deal upsert fout %s: %v", d.ID, err)
		}
	}
	return tx.Commit()
}

// HaalDealsOp returns deals from the DB for the given week (defaults to current).
func HaalDealsOp(store, week string) ([]scraper.Deal, error) {
	if week == "" {
		week = HuidigeWeek()
	}
	query := `
		SELECT id, supermarkt, naam, beschrijving,
		       "originalPrijs", "actiePrijs", "kortingPercent",
		       eenheid, categorie, COALESCE("afbeeldingUrl",''), "geldigTot"
		FROM deals WHERE weeknummer = $1`
	args := []interface{}{week}

	if store != "" {
		query += " AND supermarkt = $2"
		args = append(args, store)
	}
	query += ` ORDER BY "kortingPercent" DESC`

	rows, err := pool.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var deals []scraper.Deal
	for rows.Next() {
		var d scraper.Deal
		if err := rows.Scan(&d.ID, &d.Supermarkt, &d.Naam, &d.Beschrijving,
			&d.OriginalPrijs, &d.ActiePrijs, &d.KortingPercent,
			&d.Eenheid, &d.Categorie, &d.AfbeeldingUrl, &d.GeldigTot); err != nil {
			return nil, err
		}
		deals = append(deals, d)
	}
	return deals, rows.Err()
}

// ─── Product price history ────────────────────────────────────────────────────

// SlaaPrijsSnapshotOp inserts one row per product into product_prices.
// Called after every scrape (deals and search results).
func SlaaPrijsSnapshotOp(results []scraper.PrijsResultaat, store string) error {
	if len(results) == 0 {
		return nil
	}
	tx, err := pool.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO product_prices
			(store, product_id, naam, eenheid, categorie,
			 prijs, actie_prijs, in_aanbieding, bonus_tekst,
			 afbeelding_url, product_url, gemeten_op)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NOW())
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, r := range results {
		productID := store + "-" + r.ProductNaam // fallback ID when no numeric ID
		if r.ProductUrl != "" {
			productID = store + ":" + r.ProductUrl
		}
		categorie := scraper.MapCategorie(r.ProductNaam)
		var actiePrijs sql.NullFloat64
		if r.ActiePrijs != nil {
			actiePrijs = sql.NullFloat64{Float64: *r.ActiePrijs, Valid: true}
		}
		_, err := stmt.Exec(
			store, productID, r.ProductNaam, r.Eenheid, categorie,
			r.Prijs, actiePrijs, r.InAanbieding, r.BonusTekst,
			r.AfbeeldingUrl, r.ProductUrl,
		)
		if err != nil {
			log.Printf("[DB] PrijsSnapshot fout %s: %v", r.ProductNaam, err)
		}
	}
	return tx.Commit()
}

// SlaaDealsPrijsSnapshotOp inserts a price snapshot from a deals scrape.
func SlaaDealsPrijsSnapshotOp(deals []scraper.Deal) error {
	if len(deals) == 0 {
		return nil
	}
	results := make([]scraper.PrijsResultaat, 0, len(deals))
	for _, d := range deals {
		var actiePrijs *float64
		if d.ActiePrijs > 0 && d.ActiePrijs < d.OriginalPrijs {
			v := d.ActiePrijs
			actiePrijs = &v
		}
		_, soort := scraper.BerekeningPrijsPer100(d.ActiePrijs, d.Eenheid)
		results = append(results, scraper.PrijsResultaat{
			Supermarkt:        d.Supermarkt,
			ProductNaam:       d.Naam,
			Prijs:             d.OriginalPrijs,
			ActiePrijs:        actiePrijs,
			InAanbieding:      actiePrijs != nil,
			BonusTekst:        d.Beschrijving,
			Eenheid:           d.Eenheid,
			AfbeeldingUrl:     d.AfbeeldingUrl,
			PrijsEenheidSoort: soort,
		})
	}
	return SlaaPrijsSnapshotOp(results, deals[0].Supermarkt)
}

// HaalPrijsHistorieOp returns the price history for a single product.
func HaalPrijsHistorieOp(productID string) ([]PrijsHistorieRij, error) {
	rows, err := pool.Query(`
		SELECT store, product_id, naam, prijs, actie_prijs, in_aanbieding, bonus_tekst, gemeten_op
		FROM product_prices
		WHERE product_id = $1
		ORDER BY gemeten_op DESC
		LIMIT 200
	`, productID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanHistorieRows(rows)
}

// HaalPrijsTrendOp returns price history matched by name (fuzzy) for a store.
func HaalPrijsTrendOp(naam, store string, dagen int) ([]PrijsHistorieRij, error) {
	if dagen <= 0 {
		dagen = 90
	}
	since := time.Now().AddDate(0, 0, -dagen)
	rows, err := pool.Query(`
		SELECT store, product_id, naam, prijs, actie_prijs, in_aanbieding, bonus_tekst, gemeten_op
		FROM product_prices
		WHERE store = $1
		  AND lower(naam) LIKE $2
		  AND gemeten_op >= $3
		ORDER BY gemeten_op DESC
		LIMIT 500
	`, store, "%"+strings.ToLower(naam)+"%", since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanHistorieRows(rows)
}

type PrijsHistorieRij struct {
	Store        string    `json:"store"`
	ProductID    string    `json:"productId"`
	Naam         string    `json:"naam"`
	Prijs        float64   `json:"prijs"`
	ActiePrijs   *float64  `json:"actiePrijs,omitempty"`
	InAanbieding bool      `json:"inAanbieding"`
	BonusTekst   string    `json:"bonusTekst,omitempty"`
	GemetenOp    time.Time `json:"gemetenOp"`
}

func scanHistorieRows(rows *sql.Rows) ([]PrijsHistorieRij, error) {
	var result []PrijsHistorieRij
	for rows.Next() {
		var r PrijsHistorieRij
		var actiePrijs sql.NullFloat64
		var bonusTekst sql.NullString
		if err := rows.Scan(&r.Store, &r.ProductID, &r.Naam, &r.Prijs,
			&actiePrijs, &r.InAanbieding, &bonusTekst, &r.GemetenOp); err != nil {
			return nil, err
		}
		if actiePrijs.Valid {
			r.ActiePrijs = &actiePrijs.Float64
		}
		r.BonusTekst = bonusTekst.String
		result = append(result, r)
	}
	return result, rows.Err()
}

// ─── Search cache ─────────────────────────────────────────────────────────────
// Shared with Next.js — same table, same format.

// SlaZoekResultatenOp caches price comparison results for a query+store.
func SlaZoekResultatenOp(query, store string, resultaten []scraper.PrijsResultaat) error {
	if len(resultaten) == 0 {
		return nil
	}
	data, err := json.Marshal(resultaten)
	if err != nil {
		return err
	}
	_, err = pool.Exec(`
		INSERT INTO search_cache (sleutel, supermarkt, resultaten_json, gecached_op)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (sleutel, supermarkt) DO UPDATE
			SET resultaten_json = EXCLUDED.resultaten_json,
			    gecached_op     = NOW()
	`, strings.ToLower(query), store, string(data))
	return err
}

// HaalZoekResultatenOp retrieves cached results for a query+store (max 7 days old).
func HaalZoekResultatenOp(query, store string) ([]scraper.PrijsResultaat, error) {
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	row := pool.QueryRow(`
		SELECT resultaten_json FROM search_cache
		WHERE sleutel = $1 AND supermarkt = $2 AND gecached_op >= $3
	`, strings.ToLower(query), store, cutoff)

	var raw string
	if err := row.Scan(&raw); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	var resultaten []scraper.PrijsResultaat
	if err := json.Unmarshal([]byte(raw), &resultaten); err != nil {
		return nil, err
	}
	return resultaten, nil
}

