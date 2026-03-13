// Package poiesz implements the Poiesz supermarket scraper.
// Poiesz exposes a public REST API at apiv2.poiesz-supermarkten.nl — no auth needed.
package poiesz

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wiekstras/supermarkt-scraper/scraper"
)

const (
	baseURL     = "https://apiv2.poiesz-supermarkten.nl/api/v2.0"
	imageBase   = "https://images.poiesz-supermarkten.nl/artikelen/"
)

// ─── Store implementation ─────────────────────────────────────────────────────

type PoieszStore struct {
	client *http.Client
}

func New() *PoieszStore {
	return &PoieszStore{
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

func (s *PoieszStore) Name() string { return "poiesz" }

func (s *PoieszStore) Zoek(ctx context.Context, query string) ([]scraper.PrijsResultaat, error) {
	u := fmt.Sprintf("%s/products/search?search=%s&from=0", baseURL, url.QueryEscape(query))
	var resp poieszSearchResponse
	if err := s.fetchJSON(ctx, u, &resp); err != nil {
		return nil, fmt.Errorf("poiesz zoek: %w", err)
	}
	resultaten := make([]scraper.PrijsResultaat, 0, len(resp.Products))
	for i, p := range resp.Products {
		if i >= 12 {
			break
		}
		resultaten = append(resultaten, p.toPrijsResultaat())
	}
	return resultaten, nil
}

func (s *PoieszStore) HaalDealsOp(ctx context.Context) ([]scraper.Deal, error) {
	var resp poieszOffersResponse
	if err := s.fetchJSON(ctx, baseURL+"/offers/", &resp); err != nil {
		return nil, fmt.Errorf("poiesz deals: %w", err)
	}
	var deals []scraper.Deal
	seen := make(map[string]bool)
	for _, cat := range resp.Categories {
		for _, offer := range cat.Offers {
			id := fmt.Sprintf("poiesz-%d", offer.ID)
			if seen[id] {
				continue
			}
			seen[id] = true
			deals = append(deals, offer.toDeal(cat.Name, resp.ValidUntil))
		}
	}
	return deals, nil
}

// ─── HTTP helpers ─────────────────────────────────────────────────────────────

func (s *PoieszStore) fetchJSON(ctx context.Context, rawURL string, dst interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Accept", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}

// ─── API response types ───────────────────────────────────────────────────────

type poieszSearchResponse struct {
	Products []poieszProduct `json:"products"`
}

type poieszOffersResponse struct {
	ValidFrom  string          `json:"validFrom"`
	ValidUntil string          `json:"validUntil"`
	Categories []poieszCategory `json:"categories"`
}

type poieszCategory struct {
	ID     int            `json:"id"`
	Name   string         `json:"name"`
	Offers []poieszOffer  `json:"offers"`
}

type poieszProduct struct {
	ID                 int     `json:"id"`
	Name               string  `json:"name"`
	BrandName          string  `json:"brandName"`
	Image              string  `json:"image"`
	Price              float64 `json:"price"`              // euros (API returns euros, not cents)
	StrikeThroughPrice float64 `json:"strikeThroughPrice"` // euros
	Promotion          bool    `json:"promotion"`
	PromotionLabel     string  `json:"promotionLabel"`
	VolumeCe           float64 `json:"volumeCe"` // API returns floats
	WeightCe           float64 `json:"weightCe"`
	UnitID             string  `json:"unitId"`
	PackageDescription string  `json:"packageDescription"`
}

type poieszOffer struct {
	ID                        int            `json:"id"`
	CommercialTextLine1       string         `json:"commercialTextLine1"`
	CommercialTextLine2       string         `json:"commercialTextLine2"`
	CommercialTextDetailsLine1 string        `json:"commercialTextDetailsLine1"`
	OfferTypeLine1            string         `json:"offerTypeLine1"`
	OldPriceLow               float64        `json:"oldPriceLow"`   // euros
	OldPriceHigh              float64        `json:"oldPriceHigh"`  // euros
	NewPriceLow               float64        `json:"newPriceLow"`   // euros
	NewPriceHigh              float64        `json:"newPriceHigh"`  // euros
	ImageFile                 string         `json:"imageFile"`
	ValidUntil                string         `json:"validUntil"`
	OverviewProducts          []poieszProduct `json:"overviewProducts"`
}

// ─── Conversion helpers ───────────────────────────────────────────────────────

func imageURL(img string, id int) string {
	if strings.HasPrefix(img, "http") {
		return img
	}
	if img != "" {
		return imageBase + img
	}
	if id > 0 {
		return fmt.Sprintf("%s%d.png", imageBase, id)
	}
	return ""
}

func formatEenheid(p *poieszProduct) string {
	if p.PackageDescription != "" {
		return p.PackageDescription
	}
	unit := strings.ToUpper(p.UnitID)
	switch unit {
	case "ML":
		if p.VolumeCe >= 1000 {
			return fmt.Sprintf("%.0f liter", float64(p.VolumeCe)/1000)
		}
		return fmt.Sprintf("%.0f ml", p.VolumeCe)
	case "GR", "G":
		if p.WeightCe >= 1000 {
			return fmt.Sprintf("%.0f kg", p.WeightCe/1000)
		}
		w := p.WeightCe
		if w == 0 {
			w = p.VolumeCe
		}
		if w > 0 {
			return fmt.Sprintf("%.0f g", w)
		}
	}
	return ""
}

func (p *poieszProduct) toPrijsResultaat() scraper.PrijsResultaat {
	prijs := p.Price // already in euros
	var actiePrijs *float64
	inAanbieding := false
	normalPrijs := prijs
	if p.StrikeThroughPrice > 0 && p.StrikeThroughPrice > p.Price {
		normalPrijs = p.StrikeThroughPrice
		actiePrijs = &prijs
		inAanbieding = true
	}
	eenheid := formatEenheid(p)
	ef := normalPrijs
	if actiePrijs != nil {
		ef = *actiePrijs
	}
	per100, soort := scraper.BerekeningPrijsPer100(ef, eenheid)
	return scraper.PrijsResultaat{
		Supermarkt:        "poiesz",
		ProductNaam:       p.Name,
		Prijs:             normalPrijs,
		InAanbieding:      inAanbieding,
		ActiePrijs:        actiePrijs,
		BonusTekst:        p.PromotionLabel,
		Eenheid:           eenheid,
		AfbeeldingUrl:     imageURL(p.Image, p.ID),
		ProductUrl:        fmt.Sprintf("https://www.poiesz-supermarkten.nl/aanbiedingen?product=%d", p.ID),
		PrijsPer100:       per100,
		PrijsEenheidSoort: soort,
	}
}

func (o *poieszOffer) toDeal(catName string, defaultValidUntil string) scraper.Deal {
	actiePrijs := o.NewPriceLow // already euros
	if actiePrijs == 0 {
		actiePrijs = o.NewPriceHigh
	}
	originalPrijs := o.OldPriceLow
	if originalPrijs == 0 {
		originalPrijs = o.OldPriceHigh
	}
	if originalPrijs == 0 {
		originalPrijs = actiePrijs
	}

	kortingPercent := 0
	if originalPrijs > 0 && actiePrijs > 0 && actiePrijs < originalPrijs {
		kortingPercent = int(((originalPrijs - actiePrijs) / originalPrijs) * 100)
	}

	naam := o.CommercialTextLine1
	if o.CommercialTextLine2 != "" {
		naam += " " + o.CommercialTextLine2
	}
	naam = strings.TrimSpace(naam)

	bonusTekst := o.OfferTypeLine1
	beschrijving := naam
	if bonusTekst != "" {
		beschrijving += " — " + bonusTekst
	}

	eenheid := o.CommercialTextDetailsLine1
	if eenheid == "" && len(o.OverviewProducts) > 0 {
		eenheid = formatEenheid(&o.OverviewProducts[0])
	}

	geldigTot := o.ValidUntil
	if geldigTot == "" {
		geldigTot = defaultValidUntil
	}
	if geldigTot == "" {
		now := time.Now()
		daysUntilSunday := (7 - int(now.Weekday())) % 7
		if daysUntilSunday == 0 {
			daysUntilSunday = 7
		}
		geldigTot = now.AddDate(0, 0, daysUntilSunday).Format("2006-01-02")
	}

	// o.ImageFile is a Windows PSD path (e.g. "files\0\6\5\120711.psd") —
	// never a usable web URL. Use the first product image from overviewProducts
	// instead, which contains a proper HTTPS URL or a relative filename.
	img := ""
	for _, p := range o.OverviewProducts {
		if candidate := imageURL(p.Image, p.ID); candidate != "" {
			img = candidate
			break
		}
	}

	categorie := scraper.MapCategorie(naam)
	if categorie == "overig" {
		categorie = scraper.MapCategorie(catName)
	}

	return scraper.Deal{
		ID:             fmt.Sprintf("poiesz-%d", o.ID),
		Supermarkt:     "poiesz",
		Naam:           naam,
		Beschrijving:   beschrijving,
		OriginalPrijs:  originalPrijs,
		ActiePrijs:     actiePrijs,
		KortingPercent: kortingPercent,
		Eenheid:        eenheid,
		Categorie:      categorie,
		AfbeeldingUrl:  img,
		GeldigTot:      geldigTot,
	}
}
