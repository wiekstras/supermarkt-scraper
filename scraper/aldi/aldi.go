// Package aldi implements the Aldi NL supermarket scraper.
//
// Search: Aldi uses Algolia for product search. The public API key is embedded
// in the Next.js JS bundle. We query the Algolia index directly.
//
// Deals: Aldi exposes a public Next.js internal API at /api/offer/nl/current
// that returns all current week deals with prices, images and validity dates.
package aldi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/wiekstras/supermarkt-scraper/scraper"
)

// Algolia credentials — public, embedded in Aldi's JS bundle.
const (
	algoliaAppID  = "2HU29PF6BH"
	algoliaAPIKey = "686cf0c8ddcf740223d420d1115c94c1"
	algoliaIndex  = "an_prd_nl_nl_products"
)

// ─── Store implementation ─────────────────────────────────────────────────────

type AldiStore struct {
	client *http.Client
}

func New() *AldiStore {
	return &AldiStore{
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

func (s *AldiStore) Name() string { return "aldi" }

// ─── Search ───────────────────────────────────────────────────────────────────

// algoliaQuery is the request body for Algolia search.
type algoliaQuery struct {
	Query       string `json:"query"`
	HitsPerPage int    `json:"hitsPerPage"`
}

// algoliaResponse is the Algolia search response.
type algoliaResponse struct {
	Hits []algoliaHit `json:"hits"`
}

// algoliaHit is a single product from Algolia.
type algoliaHit struct {
	ObjectID     string  `json:"objectID"`
	VariantName  string  `json:"variantName"`
	BrandName    string  `json:"brandName"`
	SalesUnit    string  `json:"salesUnit"`
	ProductSlug  string  `json:"productSlug"`
	IsAvailable  bool    `json:"isAvailable"`
	Images       []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"images"`
	CurrentPrice struct {
		PriceValue       *float64 `json:"priceValue"`
		StrikePriceValue *float64 `json:"strikePriceValue"`
	} `json:"currentPrice"`
}

func (h *algoliaHit) primaryImage() string {
	for _, img := range h.Images {
		if img.Type == "primary" {
			return img.URL
		}
	}
	if len(h.Images) > 0 {
		return h.Images[0].URL
	}
	return ""
}

func (s *AldiStore) Zoek(ctx context.Context, query string) ([]scraper.PrijsResultaat, error) {
	body, _ := json.Marshal(algoliaQuery{Query: query, HitsPerPage: 12})

	url := fmt.Sprintf("https://%s-dsn.algolia.net/1/indexes/%s/query", algoliaAppID, algoliaIndex)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("aldi zoek request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Algolia-Application-Id", algoliaAppID)
	req.Header.Set("X-Algolia-API-Key", algoliaAPIKey)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("aldi zoek fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("aldi algolia HTTP %d", resp.StatusCode)
	}

	var result algoliaResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("aldi zoek decode: %w", err)
	}

	resultaten := make([]scraper.PrijsResultaat, 0, len(result.Hits))
	for _, hit := range result.Hits {
		if hit.VariantName == "" || !hit.IsAvailable {
			continue
		}

		// Aldi only shows prices for products that are currently in the weekly deal.
		// For regular products priceValue is null — skip those since we can't compare.
		if hit.CurrentPrice.PriceValue == nil {
			continue
		}

		prijs := *hit.CurrentPrice.PriceValue
		var actiePrijs *float64
		inAanbieding := false
		bonusTekst := ""

		if hit.CurrentPrice.StrikePriceValue != nil && *hit.CurrentPrice.StrikePriceValue > prijs {
			inAanbieding = true
			v := prijs
			actiePrijs = &v
			prijs = *hit.CurrentPrice.StrikePriceValue
			bonusTekst = fmt.Sprintf("Was €%.2f", prijs)
		}

		ef := prijs
		if actiePrijs != nil {
			ef = *actiePrijs
		}
		per100, soort := scraper.BerekeningPrijsPer100(ef, hit.SalesUnit)

		productURL := ""
		if hit.ProductSlug != "" {
			productURL = "https://www.aldi.nl/producten/" + hit.ProductSlug + ".html"
		}

		naam := hit.VariantName
		if hit.BrandName != "" {
			naam = hit.BrandName + " " + naam
		}

		resultaten = append(resultaten, scraper.PrijsResultaat{
			Supermarkt:        "aldi",
			ProductNaam:       naam,
			Prijs:             prijs,
			InAanbieding:      inAanbieding,
			ActiePrijs:        actiePrijs,
			BonusTekst:        bonusTekst,
			Eenheid:           hit.SalesUnit,
			AfbeeldingUrl:     hit.primaryImage(),
			ProductUrl:        productURL,
			PrijsPer100:       per100,
			PrijsEenheidSoort: soort,
		})
	}

	if len(resultaten) == 0 {
		return nil, fmt.Errorf("aldi zoek: geen producten met prijs gevonden voor %q", query)
	}
	return resultaten, nil
}

// ─── Deals ────────────────────────────────────────────────────────────────────

// offerResponse is the response from /api/offer/nl/current.
type offerResponse struct {
	AlgoliaDataMap map[string]offerProduct `json:"algoliaDataMap"`
	Categories     []offerCategory         `json:"categories"`
}

type offerCategory struct {
	Title      string         `json:"title"`
	ShortTitle string         `json:"shortTitle"`
	StartDate  string         `json:"startDate"`
	EndDate    string         `json:"endDate"`
	Content    []offerContent `json:"content"`
}

type offerContent struct {
	Title      string   `json:"title"`
	ProductIDs []string `json:"productIds"`
}

type offerProduct struct {
	ObjectID    string `json:"objectID"`
	Name        string `json:"name"`
	BrandName   string `json:"brandName"`
	SalesUnit   string `json:"salesUnit"`
	ProductSlug string `json:"productSlug"`
	Assets      []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"assets"`
	CurrentPrice offerPrice `json:"currentPrice"`
}

type offerPrice struct {
	PriceValue  *float64 `json:"priceValue"`
	StrikePrice *struct {
		StrikePriceValue float64 `json:"strikePriceValue"`
	} `json:"strikePrice"`
	BasePrice []struct {
		BasePriceValue float64 `json:"basePriceValue"`
		BasePriceScale string  `json:"basePriceScale"`
	} `json:"basePrice"`
	PriceTagLabels *struct {
		PromoText1 string `json:"promoText1"`
	} `json:"priceTagLabels"`
	ValidFrom  *int64 `json:"validFrom"`
	ValidUntil *int64 `json:"validUntil"`
}

func (p *offerProduct) primaryImage() string {
	for _, a := range p.Assets {
		if a.Type == "primary" {
			return a.URL
		}
	}
	if len(p.Assets) > 0 {
		return p.Assets[0].URL
	}
	return ""
}

func (s *AldiStore) HaalDealsOp(ctx context.Context) ([]scraper.Deal, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.aldi.nl/api/offer/nl/current", nil)
	if err != nil {
		return nil, fmt.Errorf("aldi deals request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36")
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("aldi deals fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("aldi deals HTTP %d: %s", resp.StatusCode, string(b)[:min(200, len(b))])
	}

	var offerResp offerResponse
	if err := json.NewDecoder(resp.Body).Decode(&offerResp); err != nil {
		return nil, fmt.Errorf("aldi deals decode: %w", err)
	}

	// Build a set of product IDs that are in the current (first) category window.
	// Later windows (e.g. "Vanaf woensdag") are upcoming deals, not yet valid.
	currentIDs := make(map[string]bool)
	geldigTot := ""
	if len(offerResp.Categories) > 0 {
		cat := offerResp.Categories[0]
		geldigTot = cat.EndDate
		for _, content := range cat.Content {
			for _, id := range content.ProductIDs {
				currentIDs[id] = true
			}
		}
	}

	deals := make([]scraper.Deal, 0, len(currentIDs))
	for id, prod := range offerResp.AlgoliaDataMap {
		if !currentIDs[id] {
			continue
		}
		if prod.CurrentPrice.PriceValue == nil {
			continue
		}

		actiePrijs := *prod.CurrentPrice.PriceValue
		originalPrijs := actiePrijs
		if prod.CurrentPrice.StrikePrice != nil && prod.CurrentPrice.StrikePrice.StrikePriceValue > actiePrijs {
			originalPrijs = prod.CurrentPrice.StrikePrice.StrikePriceValue
		}

		kortingPercent := 0
		if prod.CurrentPrice.PriceTagLabels != nil && prod.CurrentPrice.PriceTagLabels.PromoText1 != "" {
			fmt.Sscanf(prod.CurrentPrice.PriceTagLabels.PromoText1, "-%d%%", &kortingPercent)
		}
		if kortingPercent == 0 && originalPrijs > actiePrijs {
			kortingPercent = int((1 - actiePrijs/originalPrijs) * 100)
		}

		// Validity from the price object (unix timestamps)
		dealGeldigTot := geldigTot
		if prod.CurrentPrice.ValidUntil != nil {
			dealGeldigTot = time.Unix(*prod.CurrentPrice.ValidUntil, 0).Format("2006-01-02")
		}

		naam := prod.Name
		if prod.BrandName != "" {
			naam = prod.BrandName + " " + naam
		}

		categorie := scraper.MapCategorie(naam)
		beschrijving := naam
		if prod.SalesUnit != "" {
			beschrijving += " — " + prod.SalesUnit
		}

		productURL := ""
		if prod.ProductSlug != "" {
			productURL = "https://www.aldi.nl/producten/" + prod.ProductSlug + ".html"
		}

		deals = append(deals, scraper.Deal{
			ID:             "aldi-" + id,
			Supermarkt:     "aldi",
			Naam:           naam,
			Beschrijving:   beschrijving,
			OriginalPrijs:  originalPrijs,
			ActiePrijs:     actiePrijs,
			KortingPercent: kortingPercent,
			Eenheid:        prod.SalesUnit,
			Categorie:      categorie,
			AfbeeldingUrl:  prod.primaryImage(),
			GeldigTot:      dealGeldigTot,
			ProductUrl:     productURL,
		})
	}

	return deals, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
