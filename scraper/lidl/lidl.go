// Package lidl implements the Lidl NL supermarket scraper.
// Lidl uses Nuxt3 SSR which embeds all page data in a
// <script id="__NUXT_DATA__"> tag as a flat JSON array.
// The search page at /q/search?q=<query> contains the full product list.
// The /q/api/search endpoint requires authentication (401) so we parse the HTML.
package lidl

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/wiekstras/supermarkt-scraper/scraper"
)

// ─── Store implementation ─────────────────────────────────────────────────────

type LidlStore struct {
	client *http.Client
}

func New() *LidlStore {
	return &LidlStore{
		client: &http.Client{Timeout: 20 * time.Second},
	}
}

func (s *LidlStore) Name() string { return "lidl" }

func (s *LidlStore) Zoek(ctx context.Context, query string) ([]scraper.PrijsResultaat, error) {
	u := fmt.Sprintf("https://www.lidl.nl/q/search?q=%s", url.QueryEscape(query))
	html, err := s.fetchHTML(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("lidl zoek fetch: %w", err)
	}
	raw, err := extractNuxtData(html)
	if err != nil {
		return nil, fmt.Errorf("lidl zoek nuxt: %w", err)
	}
	producten := parseProducts(raw)
	if len(producten) == 0 {
		return nil, fmt.Errorf("lidl zoek: geen producten gevonden voor %q", query)
	}
	resultaten := make([]scraper.PrijsResultaat, 0, len(producten))
	for _, p := range producten {
		if p.FullTitle == "" {
			continue
		}
		resultaten = append(resultaten, p.toPrijsResultaat())
	}
	return resultaten, nil
}

// HaalDealsOp is not yet implemented for Lidl (no deals page support).
func (s *LidlStore) HaalDealsOp(ctx context.Context) ([]scraper.Deal, error) {
	return nil, nil
}

// ─── HTTP fetch ───────────────────────────────────────────────────────────────

func (s *LidlStore) fetchHTML(ctx context.Context, rawURL string) (string, error) {
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
		return "", fmt.Errorf("HTTP %d voor %s", resp.StatusCode, rawURL)
	}
	body, err := io.ReadAll(resp.Body)
	return string(body), err
}

// ─── Nuxt data extraction ─────────────────────────────────────────────────────

var nuxtDataRe = regexp.MustCompile(`<script[^>]*id="__NUXT_DATA__"[^>]*>([\s\S]*?)</script>`)

type nuxtRaw = []interface{}

func extractNuxtData(html string) (nuxtRaw, error) {
	m := nuxtDataRe.FindStringSubmatch(html)
	if m == nil {
		return nil, fmt.Errorf("__NUXT_DATA__ niet gevonden in Lidl pagina")
	}
	var raw nuxtRaw
	if err := json.Unmarshal([]byte(m[1]), &raw); err != nil {
		return nil, fmt.Errorf("nuxt data parse: %w", err)
	}
	return raw, nil
}

// resolve returns the value at index idx in the flat array.
// Primitive values (string, number, bool) are returned directly.
// Map and list values have their own pointer-valued fields resolved one level deep.
func resolve(raw nuxtRaw, idx int) interface{} {
	if idx < 0 || idx >= len(raw) {
		return nil
	}
	return raw[idx]
}

// resolveStr returns a string value from the flat array, or "".
func resolveStr(raw nuxtRaw, ref interface{}) string {
	switch v := ref.(type) {
	case string:
		return v
	case float64:
		s, _ := resolve(raw, int(v)).(string)
		return s
	}
	return ""
}

// resolveFloat returns a float64 from the flat array, or 0.
func resolveFloat(raw nuxtRaw, ref interface{}) float64 {
	switch v := ref.(type) {
	case float64:
		// Could be a direct value or an index pointing to a float
		if v < float64(len(raw)) {
			if f, ok := raw[int(v)].(float64); ok && int(v) > 100 {
				// Heuristic: if the array value is also a float and index is large, it's a price
				return f
			}
		}
		return v
	}
	return 0
}

// resolveMap resolves an index to a map with all its pointer fields resolved one level.
func resolveMap(raw nuxtRaw, idx int) map[string]interface{} {
	if idx < 0 || idx >= len(raw) {
		return nil
	}
	m, ok := raw[idx].(map[string]interface{})
	if !ok {
		return nil
	}
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		if f, ok := v.(float64); ok {
			out[k] = resolve(raw, int(f))
		} else {
			out[k] = v
		}
	}
	return out
}

// ─── Product types ────────────────────────────────────────────────────────────

type lidlProduct struct {
	FullTitle  string
	Image      string
	URL        string
	Price      float64 // normal price in EUR
	OldPrice   float64 // original price when on sale (0 = not on sale)
	PriceTheme string  // "white_red" = sale item
	Packaging  string  // e.g. "350 g" or "750 ml"
}

// parseProducts extracts all products from the Nuxt flat array.
// The structure: raw[5] = search result object with items:[ref, ...]
// Each item → gridbox → data → product fields.
func parseProducts(raw nuxtRaw) []lidlProduct {
	// raw[5] is the search result object
	if len(raw) < 6 {
		return nil
	}
	searchObj, ok := raw[5].(map[string]interface{})
	if !ok {
		return nil
	}
	itemsRefRaw, ok := searchObj["items"]
	if !ok {
		return nil
	}
	itemsIdx, ok := itemsRefRaw.(float64)
	if !ok {
		return nil
	}
	itemsList, ok := raw[int(itemsIdx)].([]interface{})
	if !ok {
		return nil
	}

	results := make([]lidlProduct, 0, len(itemsList))
	for _, itemRefRaw := range itemsList {
		itemIdx, ok := itemRefRaw.(float64)
		if !ok {
			continue
		}
		itemMap, ok := raw[int(itemIdx)].(map[string]interface{})
		if !ok {
			continue
		}
		gridboxRefRaw, ok := itemMap["gridbox"]
		if !ok {
			continue
		}
		gridboxIdx, ok := gridboxRefRaw.(float64)
		if !ok {
			continue
		}
		gridboxMap, ok := raw[int(gridboxIdx)].(map[string]interface{})
		if !ok {
			continue
		}
		dataRefRaw, ok := gridboxMap["data"]
		if !ok {
			continue
		}
		dataIdx, ok := dataRefRaw.(float64)
		if !ok {
			continue
		}
		dataMap, ok := raw[int(dataIdx)].(map[string]interface{})
		if !ok {
			continue
		}

		p := extractProduct(raw, dataMap)
		if p.FullTitle != "" {
			results = append(results, p)
		}
		if len(results) >= 12 {
			break
		}
	}
	return results
}

func extractProduct(raw nuxtRaw, d map[string]interface{}) lidlProduct {
	var p lidlProduct

	// fullTitle
	p.FullTitle = resolveStr(raw, d["fullTitle"])
	if p.FullTitle == "" {
		p.FullTitle = resolveStr(raw, d["title"])
	}

	// canonicalUrl → product URL
	canonical := resolveStr(raw, d["canonicalUrl"])
	if canonical == "" {
		canonical = resolveStr(raw, d["canonicalPath"])
	}
	if canonical != "" {
		p.URL = "https://www.lidl.nl" + canonical
	}

	// image — can be a string (URL) directly or an index
	if imgRef, ok := d["image"]; ok {
		switch v := imgRef.(type) {
		case string:
			p.Image = v
		case float64:
			// The array value at that index is the URL string
			p.Image, _ = raw[int(v)].(string)
		}
	}

	// price object
	if priceRef, ok := d["price"]; ok {
		if priceIdx, ok := priceRef.(float64); ok {
			priceMap, ok := raw[int(priceIdx)].(map[string]interface{})
			if ok {
				// price field — direct float
				if pv, ok := priceMap["price"].(float64); ok {
					p.Price = pv
				}
				if ov, ok := priceMap["oldPrice"].(float64); ok {
					p.OldPrice = ov
				}
				if theme, ok := priceMap["priceTheme"].(float64); ok {
					p.PriceTheme, _ = raw[int(theme)].(string)
				} else if theme, ok := priceMap["priceTheme"].(string); ok {
					p.PriceTheme = theme
				}
			}
		}
	}

	// packaging — resolve from index if needed
	if pkgRef, ok := d["packaging"]; ok {
		switch v := pkgRef.(type) {
		case string:
			p.Packaging = v
		case float64:
			if m, ok := raw[int(v)].(map[string]interface{}); ok {
				// packaging object has a "label" field
				if labelRef, ok := m["label"]; ok {
					switch lv := labelRef.(type) {
					case string:
						p.Packaging = lv
					case float64:
						p.Packaging, _ = raw[int(lv)].(string)
					}
				}
			} else {
				p.Packaging, _ = raw[int(v)].(string)
			}
		}
	}

	return p
}

func (p *lidlProduct) toPrijsResultaat() scraper.PrijsResultaat {
	inAanbieding := p.OldPrice > 0 && p.OldPrice > p.Price
	var actiePrijs *float64
	normalPrijs := p.Price
	if inAanbieding {
		normalPrijs = p.OldPrice
		v := p.Price
		actiePrijs = &v
	}

	bonusTekst := ""
	if inAanbieding {
		bonusTekst = fmt.Sprintf("Was €%.2f", p.OldPrice)
	}

	ef := normalPrijs
	if actiePrijs != nil {
		ef = *actiePrijs
	}
	per100, soort := scraper.BerekeningPrijsPer100(ef, p.Packaging)

	return scraper.PrijsResultaat{
		Supermarkt:        "lidl",
		ProductNaam:       p.FullTitle,
		Prijs:             normalPrijs,
		InAanbieding:      inAanbieding,
		ActiePrijs:        actiePrijs,
		BonusTekst:        bonusTekst,
		Eenheid:           p.Packaging,
		AfbeeldingUrl:     p.Image,
		ProductUrl:        p.URL,
		PrijsPer100:       per100,
		PrijsEenheidSoort: soort,
	}
}
