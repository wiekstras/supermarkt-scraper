// Package ah implements the Albert Heijn store scraper using the AH mobile API.
// Instead of HTML scraping, we use the same API endpoints as the official Appie
// app — this is far more reliable and gives richer product data.
//
// Authentication:
//   - Product search and deal scraping use an anonymous token (no login needed).
//   - Receipts and member data require a full login; see ahclient.Login().
package ah

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wiekstras/supermarkt-scraper/ahclient"
	"github.com/wiekstras/supermarkt-scraper/scraper"
)

// foodCategoryIDs are the AH taxonomy IDs for the main food categories used
// when scraping all current bonus/deal products. These IDs match the Appie app.
var foodCategoryIDs = []int{
	988, 989, 990, 991, 992, 993, 994, 995, 996, 997,
	998, 999, 1000, 1001, 1002, 1003, 1004, 1005, 1006,
}

// AHStore implements scraper.Store using the AH mobile API.
type AHStore struct {
	client *ahclient.Client
}

// New creates an AH store. The token store must already be initialised via
// db.Init() since it is injected here. Pass nil for a memory-only token store
// (tokens lost on restart).
func New(ts ahclient.TokenStore) *AHStore {
	return &AHStore{
		client: ahclient.New(ts),
	}
}

func (s *AHStore) Name() string { return "ah" }

// ─── Zoek ─────────────────────────────────────────────────────────────────────

func (s *AHStore) Zoek(ctx context.Context, query string) ([]scraper.PrijsResultaat, error) {
	products, err := s.client.SearchProducts(ctx, query, false, 30)
	if err != nil {
		return nil, fmt.Errorf("ah.Zoek: %w", err)
	}
	if len(products) == 0 {
		return nil, fmt.Errorf("ah.Zoek: geen resultaten voor %q", query)
	}
	out := make([]scraper.PrijsResultaat, 0, len(products))
	for i := range products {
		out = append(out, productToPrijsResultaat(&products[i]))
	}
	return out, nil
}

// ─── Deals ────────────────────────────────────────────────────────────────────

func (s *AHStore) HaalDealsOp(ctx context.Context) ([]scraper.Deal, error) {
	seen := make(map[int]bool)
	var deals []scraper.Deal

	for _, catID := range foodCategoryIDs {
		products, err := s.client.SearchBonusByCategory(ctx, catID)
		if err != nil {
			// Log en ga door — één mislukte categorie mag de rest niet blokkeren
			continue
		}
		for i := range products {
			p := &products[i]
			if seen[p.WebshopID] {
				continue
			}
			if !p.IsBonus && !p.IsBonusPrice {
				continue
			}
			seen[p.WebshopID] = true
			deals = append(deals, productToDeal(p))
		}
	}

	if len(deals) == 0 {
		return nil, fmt.Errorf("ah.HaalDealsOp: geen bonus deals gevonden")
	}
	return deals, nil
}

// ─── Mapping helpers ──────────────────────────────────────────────────────────

func productToPrijsResultaat(p *ahclient.Product) scraper.PrijsResultaat {
	normalPrijs, actiePrijs, bonusTekst, inAanbieding := extractPrices(p)
	ef := normalPrijs
	if actiePrijs != nil {
		ef = *actiePrijs
	}
	per100, soort := scraper.BerekeningPrijsPer100(ef, p.SalesUnitSize)
	return scraper.PrijsResultaat{
		Supermarkt:        "ah",
		ProductNaam:       p.Title,
		Prijs:             normalPrijs,
		InAanbieding:      inAanbieding,
		ActiePrijs:        actiePrijs,
		BonusTekst:        bonusTekst,
		Eenheid:           p.SalesUnitSize,
		AfbeeldingUrl:     p.BestImage(),
		ProductUrl:        fmt.Sprintf("https://www.ah.nl/producten/product/wi%d", p.WebshopID),
		PrijsPer100:       per100,
		PrijsEenheidSoort: soort,
	}
}

func productToDeal(p *ahclient.Product) scraper.Deal {
	normalPrijs, actiePrijsPtr, bonusTekst, _ := extractPrices(p)
	actiePrijs := normalPrijs
	if actiePrijsPtr != nil {
		actiePrijs = *actiePrijsPtr
	}
	kortingPercent := 0
	if normalPrijs > 0 && actiePrijs < normalPrijs {
		kortingPercent = int(((normalPrijs - actiePrijs) / normalPrijs) * 100)
	}
	categorie := scraper.MapCategorie(p.Title)
	beschrijving := p.Title
	if bonusTekst != "" {
		beschrijving += " — " + bonusTekst
	}
	// Geldig tot aanstaande zondag
	now := time.Now()
	daysUntilSunday := (7 - int(now.Weekday())) % 7
	if daysUntilSunday == 0 {
		daysUntilSunday = 7
	}
	geldigTot := now.AddDate(0, 0, daysUntilSunday).Format("2006-01-02")

	return scraper.Deal{
		ID:             fmt.Sprintf("ah-%d", p.WebshopID),
		Supermarkt:     "ah",
		Naam:           p.Title,
		Beschrijving:   beschrijving,
		OriginalPrijs:  normalPrijs,
		ActiePrijs:     actiePrijs,
		KortingPercent: kortingPercent,
		Eenheid:        p.SalesUnitSize,
		Categorie:      categorie,
		AfbeeldingUrl:  p.BestImage(),
		GeldigTot:      geldigTot,
	}
}

// extractPrices returns (normalPrijs, actiePrijs, bonusTekst, inAanbieding)
// from a mobile API Product. Mirrors the logic from the old website scraper.
func extractPrices(p *ahclient.Product) (float64, *float64, string, bool) {
	currentPrice := 0.0
	if p.CurrentPrice != nil {
		currentPrice = *p.CurrentPrice
	}
	priceBeforeBonus := 0.0
	if p.PriceBeforeBonus != nil {
		priceBeforeBonus = *p.PriceBeforeBonus
	}

	inAanbieding := (p.IsBonus || p.IsBonusPrice) &&
		currentPrice > 0 && priceBeforeBonus > 0 && currentPrice < priceBeforeBonus

	var bonusTekst string
	if len(p.DiscountLabels) > 0 {
		lbl := p.DiscountLabels[0]
		bonusTekst = strings.TrimSpace(lbl.DefaultDescription)
	}
	if bonusTekst == "" && p.BonusMechanism != "" {
		bonusTekst = p.BonusMechanism
	}

	if inAanbieding {
		v := currentPrice
		return priceBeforeBonus, &v, bonusTekst, true
	}
	// Geen korting: geef de huidige prijs terug
	if currentPrice == 0 {
		currentPrice = priceBeforeBonus
	}
	return currentPrice, nil, bonusTekst, false
}
