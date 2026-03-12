package ahclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// ─── Product search ───────────────────────────────────────────────────────────

type ProductSearchResponse struct {
	Products []Product `json:"products"`
	Page     struct {
		TotalElements int `json:"totalElements"`
	} `json:"page"`
}

type Product struct {
	WebshopID     int     `json:"webshopId"`
	Title         string  `json:"title"`
	Brand         string  `json:"brand"`
	Category      string  `json:"mainCategory"`
	SubCategory   string  `json:"subCategory"`
	SalesUnitSize string  `json:"salesUnitSize"`
	IsBonus       bool    `json:"isBonus"`
	IsBonusPrice  bool    `json:"isBonusPrice"`
	BonusMechanism string `json:"bonusMechanism"`
	CurrentPrice  *float64 `json:"currentPrice"`
	PriceBeforeBonus *float64 `json:"priceBeforeBonus"`
	Images        []ProductImage `json:"images"`
	DiscountLabels []DiscountLabel `json:"discountLabels"`
}

type ProductImage struct {
	Width  int    `json:"width"`
	Height int    `json:"height"`
	URL    string `json:"url"`
}

type DiscountLabel struct {
	Code               string  `json:"code"`
	DefaultDescription string  `json:"defaultDescription"`
	Percentage         float64 `json:"percentage"`
	Count              int     `json:"count"`
	Price              float64 `json:"price"`
}

func (p *Product) BestImage() string {
	// Prefer 400px, fall back to first available
	for _, img := range p.Images {
		if img.Width == 400 {
			return img.URL
		}
	}
	if len(p.Images) > 0 {
		return p.Images[0].URL
	}
	return ""
}

// SearchProducts searches the AH product catalogue.
func (c *Client) SearchProducts(ctx context.Context, query string, bonusOnly bool, size int) ([]Product, error) {
	if size <= 0 {
		size = 12
	}
	u := fmt.Sprintf("/mobile-services/product/search/v2?query=%s&sortOn=RELEVANCE&size=%d",
		url.QueryEscape(query), size)
	if bonusOnly {
		u += "&bonus=BONUS"
	}

	var resp ProductSearchResponse
	if err := c.DoRequest(ctx, http.MethodGet, u, nil, &resp); err != nil {
		return nil, fmt.Errorf("SearchProducts: %w", err)
	}
	return resp.Products, nil
}

// SearchBonusByCategory searches a specific taxonomy category for bonus products.
// Used by HaalDealsOp to scrape all current bonus deals.
func (c *Client) SearchBonusByCategory(ctx context.Context, taxonomyID int) ([]Product, error) {
	u := fmt.Sprintf(
		"/mobile-services/product/search/v2?sortOn=RELEVANCE&bonus=BONUS&size=100&taxonomyId=%d",
		taxonomyID,
	)
	var resp ProductSearchResponse
	if err := c.DoRequest(ctx, http.MethodGet, u, nil, &resp); err != nil {
		return nil, fmt.Errorf("SearchBonusByCategory(%d): %w", taxonomyID, err)
	}
	// Filter strictly to bonus products only
	var bonus []Product
	for _, p := range resp.Products {
		if p.IsBonus || p.IsBonusPrice {
			bonus = append(bonus, p)
		}
	}
	return bonus, nil
}
