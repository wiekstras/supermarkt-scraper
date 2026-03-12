// Package scraper defines the Store interface and shared types used by all
// supermarket scrapers (AH, Jumbo, Poiesz). Adding a new store means
// implementing the Store interface — the rest of the application (API,
// scheduler) requires no changes.
package scraper

import (
	"context"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// ─── Store interface ──────────────────────────────────────────────────────────

// Store is the common interface every supermarket scraper must implement.
type Store interface {
	// Name returns the store identifier: "ah", "jumbo" or "poiesz".
	Name() string
	// Zoek searches for products matching query (used by the vergelijker endpoint).
	Zoek(ctx context.Context, query string) ([]PrijsResultaat, error)
	// HaalDealsOp returns all current bonus/promotion deals (used by the scheduler).
	HaalDealsOp(ctx context.Context) ([]Deal, error)
}

// ─── Shared types ─────────────────────────────────────────────────────────────

// PrijsResultaat is the per-store search result for a single product,
// matching the shape expected by the Next.js vergelijker frontend.
type PrijsResultaat struct {
	Supermarkt       string   `json:"supermarkt"`
	ProductNaam      string   `json:"productNaam"`
	Prijs            float64  `json:"prijs"`              // normal/list price
	InAanbieding     bool     `json:"inAanbieding"`
	ActiePrijs       *float64 `json:"actiePrijs"`         // sale price (nil = not on sale)
	BonusTekst       string   `json:"bonusTekst,omitempty"`
	Eenheid          string   `json:"eenheid,omitempty"`
	AfbeeldingUrl    string   `json:"afbeeldingUrl,omitempty"`
	ProductUrl       string   `json:"productUrl,omitempty"`
	PrijsPer100      *float64 `json:"prijsPer100,omitempty"`
	PrijsEenheidSoort string  `json:"prijsEenheidSoort,omitempty"` // "g" or "ml"
}

// Deal is a weekly promotion deal, matching the Next.js deals table schema.
type Deal struct {
	ID             string  `json:"id"`
	Supermarkt     string  `json:"supermarkt"`
	Naam           string  `json:"naam"`
	Beschrijving   string  `json:"beschrijving"`
	OriginalPrijs  float64 `json:"originalPrijs"`
	ActiePrijs     float64 `json:"actiePrijs"`
	KortingPercent int     `json:"kortingPercent"`
	Eenheid        string  `json:"eenheid"`
	Categorie      string  `json:"categorie"`
	AfbeeldingUrl  string  `json:"afbeeldingUrl,omitempty"`
	GeldigTot      string  `json:"geldigTot"`
}

// ─── Unit parsing & price-per-100 calculation ─────────────────────────────────

type eenheidParsed struct {
	totaal float64
	soort  string // "g" or "ml"
}

var (
	multiRe  = regexp.MustCompile(`(?i)(\d+(?:[.,]\d+)?)\s*[x×]\s*(\d+(?:[.,]\d+)?)\s*(ml|cl|dl|l|liter|ltr|g|gr|gram|kg)`)
	singleRe = regexp.MustCompile(`(?i)(\d+(?:[.,]\d+)?)\s*(ml|cl|dl|l|liter|ltr|g|gr|gram|kg)`)
)

func parseFloat(s string) float64 {
	s = strings.ReplaceAll(s, ",", ".")
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func convertEenheid(qty float64, unit string) (float64, string, bool) {
	switch strings.ToLower(unit) {
	case "g", "gr", "gram":
		return qty, "g", true
	case "kg":
		return qty * 1000, "g", true
	case "ml":
		return qty, "ml", true
	case "cl":
		return qty * 10, "ml", true
	case "dl":
		return qty * 100, "ml", true
	case "l", "liter", "ltr":
		return qty * 1000, "ml", true
	}
	return 0, "", false
}

func ParseerEenheid(eenheid string) *eenheidParsed {
	s := strings.TrimSpace(eenheid)
	if s == "" {
		return nil
	}
	if m := multiRe.FindStringSubmatch(s); m != nil {
		count := parseFloat(m[1])
		qty := parseFloat(m[2])
		if totaal, soort, ok := convertEenheid(qty*count, m[3]); ok {
			return &eenheidParsed{totaal, soort}
		}
	}
	if m := singleRe.FindStringSubmatch(s); m != nil {
		qty := parseFloat(m[1])
		if totaal, soort, ok := convertEenheid(qty, m[2]); ok {
			return &eenheidParsed{totaal, soort}
		}
	}
	return nil
}

// BerekeningPrijsPer100 returns the price per 100g or 100ml, or nil when
// the unit cannot be parsed.
func BerekeningPrijsPer100(prijs float64, eenheid string) (*float64, string) {
	if prijs <= 0 {
		return nil, ""
	}
	p := ParseerEenheid(eenheid)
	if p == nil || p.totaal <= 0 {
		return nil, ""
	}
	v := math.Round((prijs/p.totaal)*100*100) / 100
	return &v, p.soort
}

// ─── Category mapping ─────────────────────────────────────────────────────────

var categorieRegels = []struct {
	categorie string
	woorden   []string
}{
	{"vlees", []string{"kip", "rund", "vark", "ham", "gehakt", "worst", "schnitzel", "filet", "drumstick", "biefstuk", "spek", "lams", "vlees", "tartaar", "balletje", "ribbelje"}},
	{"vis", []string{"vis", "zalm", "kabeljauw", "tonijn", "garnaal", "zeevruchten", "haring", "schol", "tilapia", "forel"}},
	{"groente", []string{"groente", "sla", "tomaat", "paprika", "komkommer", "broccoli", "prei", "spinazie", "wortel", "aardappel", "courgette", "sperziebonen", "pompoen", "ui", "biet", "bloemkool", "andijvie", "witlof", "knol"}},
	{"fruit", []string{"fruit", "appel", "peer", "druif", "aardbei", "mango", "banaan", "sinaas", "mandarijn", "citroen", "meloen", "kiwi", "kers", "pruim", "ananas", "framboos", "bosbes"}},
	{"zuivel", []string{"kaas", "melk", "yoghurt", "kwark", "boter", "room", "ei", "eieren", "zuivel", "feta", "mozzarella", "slagroom"}},
	{"brood", []string{"brood", "bakker", "croissant", "pistolet", "baguette", "toast", "wrap", "pita"}},
	{"pasta", []string{"pasta", "rijst", "noodle", "couscous", "graan", "meel", "macaroni", "spaghetti", "fusilli", "penne"}},
	{"conserven", []string{"blik", "peulvrucht", "mais", "bonen", "linzen", "soep", "conserv", "ingelegd"}},
	{"dranken", []string{"drank", "sap", "frisdrank", "cola", "bier", "wijn", "koffie", "thee", "water", "limonade", "smoothie", "espresso"}},
	{"snacks", []string{"snack", "chips", "noot", "koek", "chocola", "snoep", "pizza", "milka", "reep", "drop", "popcorn", "cracker"}},
}

// MapCategorie returns a deal category string based on a product name.
func MapCategorie(naam string) string {
	lower := strings.ToLower(naam)
	for _, regel := range categorieRegels {
		for _, woord := range regel.woorden {
			if strings.Contains(lower, woord) {
				return regel.categorie
			}
		}
	}
	return "overig"
}

// IsGeschiktVoorWeekmenu returns true when the category is a cooking ingredient
// (as opposed to a grocery item like drinks or snacks).
func IsGeschiktVoorWeekmenu(categorie string) bool {
	switch categorie {
	case "fruit", "dranken", "snacks", "conserven":
		return false
	}
	return true
}

// EffectievePrijs returns the actual price the customer pays (sale price when
// on promotion, normal price otherwise).
func EffectievePrijs(r PrijsResultaat) float64 {
	if r.InAanbieding && r.ActiePrijs != nil {
		return *r.ActiePrijs
	}
	return r.Prijs
}
