// Package jumbo implements the Jumbo supermarket scraper.
// Jumbo uses Nuxt.js SSR which embeds all page data in a
// <script id="__NUXT_DATA__"> tag as a flat array of values.
// Objects are represented as {key: index} maps pointing into the array,
// requiring recursive resolution.
package jumbo

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/wiekstras/supermarkt-scraper/scraper"
)

// ─── Store implementation ─────────────────────────────────────────────────────

type JumboStore struct {
	client *http.Client
}

func New() *JumboStore {
	return &JumboStore{
		client: &http.Client{Timeout: 15 * time.Second},
	}
}

func (s *JumboStore) Name() string { return "jumbo" }

func (s *JumboStore) Zoek(ctx context.Context, query string) ([]scraper.PrijsResultaat, error) {
	u := fmt.Sprintf("https://www.jumbo.com/producten/?searchType=keyword&searchTerms=%s", url.QueryEscape(query))
	html, err := s.fetchHTML(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("jumbo zoek fetch: %w", err)
	}
	raw, err := extractNuxtData(html)
	if err != nil {
		return nil, fmt.Errorf("jumbo zoek nuxt: %w", err)
	}
	producten := parseNuxtProducts(raw)
	if len(producten) == 0 {
		return nil, fmt.Errorf("jumbo zoek: geen producten gevonden voor %q", query)
	}
	resultaten := make([]scraper.PrijsResultaat, 0, len(producten))
	for _, p := range producten {
		if p.Title == "" {
			continue
		}
		resultaten = append(resultaten, p.toPrijsResultaat())
	}
	return resultaten, nil
}

func (s *JumboStore) HaalDealsOp(ctx context.Context) ([]scraper.Deal, error) {
	html, err := s.fetchHTML(ctx, "https://www.jumbo.com/aanbiedingen/nu")
	if err != nil {
		return nil, fmt.Errorf("jumbo deals fetch: %w", err)
	}
	raw, err := extractNuxtData(html)
	if err != nil {
		return nil, fmt.Errorf("jumbo deals nuxt: %w", err)
	}
	promos := parseNuxtPromos(raw)
	deals := make([]scraper.Deal, 0, len(promos))
	for _, pr := range promos {
		if pr.Title == "" {
			continue
		}
		deals = append(deals, pr.toDeal())
	}
	return deals, nil
}

// ─── HTTP fetch ───────────────────────────────────────────────────────────────

func (s *JumboStore) fetchHTML(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "nl-NL,nl;q=0.9")

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

// ─── Nuxt data extraction ─────────────────────────────────────────────────────

var nuxtDataRe = regexp.MustCompile(`<script[^>]*id="__NUXT_DATA__"[^>]*>([\s\S]*?)</script>`)

// nuxtRaw is the flat array Nuxt uses to represent all page data.
type nuxtRaw = []interface{}

func extractNuxtData(html string) (nuxtRaw, error) {
	m := nuxtDataRe.FindStringSubmatch(html)
	if m == nil {
		return nil, fmt.Errorf("__NUXT_DATA__ niet gevonden")
	}
	var raw nuxtRaw
	if err := json.Unmarshal([]byte(m[1]), &raw); err != nil {
		return nil, fmt.Errorf("nuxt data parse: %w", err)
	}
	return raw, nil
}

// nuxtResolve recursively resolves an index in the Nuxt flat array.
func nuxtResolve(raw nuxtRaw, idx int, depth int) interface{} {
	if depth > 12 || idx < 0 || idx >= len(raw) {
		return nil
	}
	v := raw[idx]
	switch val := v.(type) {
	case nil, string, bool:
		return val
	case float64:
		return val
	case []interface{}:
		// Reactive wrapper: [tagIndex, dataIndex]
		if len(val) == 2 {
			if t0, ok := val[0].(float64); ok {
				if t1, ok := val[1].(float64); ok {
					tag := raw[int(t0)]
					if tag == "Reactive" || tag == "ShallowReactive" {
						return nuxtResolve(raw, int(t1), depth+1)
					}
				}
			}
		}
		out := make([]interface{}, len(val))
		for i, ptr := range val {
			if f, ok := ptr.(float64); ok {
				out[i] = nuxtResolve(raw, int(f), depth+1)
			} else {
				out[i] = ptr
			}
		}
		return out
	case map[string]interface{}:
		out := make(map[string]interface{}, len(val))
		for k, ptr := range val {
			if f, ok := ptr.(float64); ok {
				out[k] = nuxtResolve(raw, int(f), depth+1)
			} else {
				out[k] = ptr
			}
		}
		return out
	}
	return v
}

// ─── Product types (search results) ──────────────────────────────────────────

type jumboProduct struct {
	ID       string
	Title    string
	Subtitle string
	Image    string
	Prices   struct {
		Price      float64 // cents
		PromoPrice float64 // cents (0 if no promo)
	}
	PromotionText string
}

func parseNuxtProducts(raw nuxtRaw) []jumboProduct {
	var results []jumboProduct
	for i := 0; i < len(raw); i++ {
		v, ok := raw[i].(map[string]interface{})
		if !ok {
			continue
		}
		// Product dicts must have title, prices, id keys
		_, hasTitle := v["title"]
		_, hasPrices := v["prices"]
		_, hasID := v["id"]
		if !hasTitle || !hasPrices || !hasID {
			continue
		}
		resolved, ok := nuxtResolve(raw, i, 0).(map[string]interface{})
		if !ok {
			continue
		}
		p := extractJumboProduct(resolved)
		if p.Title == "" {
			continue
		}
		results = append(results, p)
		if len(results) >= 12 {
			break
		}
	}
	return results
}

func extractJumboProduct(m map[string]interface{}) jumboProduct {
	var p jumboProduct
	p.ID, _ = m["id"].(string)
	p.Title, _ = m["title"].(string)
	p.Subtitle, _ = m["subtitle"].(string)
	p.Image, _ = m["image"].(string)

	if prices, ok := m["prices"].(map[string]interface{}); ok {
		if price, ok := prices["price"].(float64); ok {
			p.Prices.Price = price
		}
		if promo, ok := prices["promoPrice"].(float64); ok {
			p.Prices.PromoPrice = promo
		}
	}
	if promos, ok := m["promotions"].([]interface{}); ok && len(promos) > 0 {
		if promo, ok := promos[0].(map[string]interface{}); ok {
			if tags, ok := promo["tags"].([]interface{}); ok && len(tags) > 0 {
				if tag, ok := tags[0].(map[string]interface{}); ok {
					p.PromotionText, _ = tag["text"].(string)
				}
			}
		}
	}
	return p
}

func (p *jumboProduct) toPrijsResultaat() scraper.PrijsResultaat {
	normalPrijs := p.Prices.Price / 100
	var actiePrijs *float64
	inAanbieding := false
	if p.Prices.PromoPrice > 0 && p.Prices.PromoPrice < p.Prices.Price {
		v := p.Prices.PromoPrice / 100
		actiePrijs = &v
		inAanbieding = true
	}
	ef := normalPrijs
	if actiePrijs != nil {
		ef = *actiePrijs
	}
	per100, soort := scraper.BerekeningPrijsPer100(ef, p.Subtitle)
	productURL := ""
	if p.ID != "" {
		productURL = "https://www.jumbo.com/producten/" + p.ID
	}
	return scraper.PrijsResultaat{
		Supermarkt:        "jumbo",
		ProductNaam:       p.Title,
		Prijs:             normalPrijs,
		InAanbieding:      inAanbieding,
		ActiePrijs:        actiePrijs,
		BonusTekst:        p.PromotionText,
		Eenheid:           p.Subtitle,
		AfbeeldingUrl:     p.Image,
		ProductUrl:        productURL,
		PrijsPer100:       per100,
		PrijsEenheidSoort: soort,
	}
}

// ─── Promotion types (deals page) ────────────────────────────────────────────

type jumboPromo struct {
	ID       string
	Title    string
	Subtitle string
	Image    string
	Tags     []string
	End      string // ISO date
}

func parseNuxtPromos(raw nuxtRaw) []jumboPromo {
	var promos []jumboPromo
	seen := make(map[string]bool)

	for i := 0; i < len(raw); i++ {
		v, ok := raw[i].(map[string]interface{})
		if !ok {
			continue
		}
		// PromotionSection has __typename and promotions
		typePtr, hasType := v["__typename"]
		if !hasType {
			continue
		}
		resolved, ok := nuxtResolve(raw, i, 0).(map[string]interface{})
		if !ok {
			continue
		}
		typeName, _ := typePtr.(string)
		// Look for actual promotion nodes (not section containers)
		if typeName != "" && !strings.Contains(strings.ToLower(typeName), "promo") {
			continue
		}
		id, _ := resolved["id"].(string)
		title, _ := resolved["title"].(string)
		if title == "" || id == "" || seen[id] {
			continue
		}
		seen[id] = true

		var tags []string
		if tagList, ok := resolved["tags"].([]interface{}); ok {
			for _, t := range tagList {
				if tm, ok := t.(map[string]interface{}); ok {
					if txt, ok := tm["text"].(string); ok && txt != "" {
						tags = append(tags, txt)
					}
				}
			}
		}
		img, _ := resolved["image"].(string)
		subtitle, _ := resolved["subtitle"].(string)
		end := ""
		if dur, ok := resolved["durationTexts"].(map[string]interface{}); ok {
			end, _ = dur["shortTitle"].(string)
		}

		promos = append(promos, jumboPromo{
			ID:       id,
			Title:    title,
			Subtitle: subtitle,
			Image:    img,
			Tags:     tags,
			End:      end,
		})
	}
	return promos
}

func (p *jumboPromo) kortingPercent() int {
	for _, tag := range p.Tags {
		lower := strings.ToLower(tag)
		// "50% korting"
		var pct int
		if _, err := fmt.Sscanf(lower, "%d%% korting", &pct); err == nil {
			return pct
		}
		// "1+1 gratis"
		var x, y int
		if _, err := fmt.Sscanf(lower, "%d+%d gratis", &x, &y); err == nil {
			if x+y > 0 {
				return int(float64(y) / float64(x+y) * 100)
			}
		}
		if strings.Contains(lower, "combikorting") {
			return 20
		}
	}
	return 10
}

func (p *jumboPromo) toDeal() scraper.Deal {
	now := time.Now()
	daysUntilSunday := (7 - int(now.Weekday())) % 7
	if daysUntilSunday == 0 {
		daysUntilSunday = 7
	}
	geldigTot := now.AddDate(0, 0, daysUntilSunday).Format("2006-01-02")

	bonusTekst := strings.Join(p.Tags, ", ")
	categorie := scraper.MapCategorie(p.Title)
	beschrijving := p.Title
	if bonusTekst != "" {
		beschrijving += " — " + bonusTekst
	}
	return scraper.Deal{
		ID:             "jumbo-" + p.ID,
		Supermarkt:     "jumbo",
		Naam:           p.Title,
		Beschrijving:   beschrijving,
		OriginalPrijs:  0, // Jumbo deals page doesn't show prices, only discounts
		ActiePrijs:     0,
		KortingPercent: p.kortingPercent(),
		Eenheid:        p.Subtitle,
		Categorie:      categorie,
		AfbeeldingUrl:  p.Image,
		GeldigTot:      geldigTot,
	}
}
