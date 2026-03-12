package ahclient

import (
	"context"
	"fmt"
)

// ─── Stores ───────────────────────────────────────────────────────────────────

const storesSearchQuery = `query StoresSearch($filter: StoresFilterInput) {
	storesSearch(filter: $filter, limit: 10) {
		result {
			id
			name
			storeType
			address {
				street
				houseNumber
				houseNumberExtra
				postalCode
				city
			}
		}
	}
}`

type AHStore struct {
	ID        int        `json:"id"`
	Name      string     `json:"name"`
	StoreType string     `json:"storeType"`
	Address   AHAddress  `json:"address"`
}

type AHAddress struct {
	Street           string `json:"street"`
	HouseNumber      string `json:"houseNumber"`
	HouseNumberExtra string `json:"houseNumberExtra,omitempty"`
	PostalCode       string `json:"postalCode"`
	City             string `json:"city"`
}

type storesSearchResponse struct {
	StoresSearch struct {
		Result []AHStore `json:"result"`
	} `json:"storesSearch"`
}

// SearchStores finds AH stores near a postal code.
func (c *Client) SearchStores(ctx context.Context, postalCode string) ([]AHStore, error) {
	vars := map[string]any{
		"filter": map[string]any{"postalCode": postalCode},
	}
	var resp storesSearchResponse
	if err := c.DoGraphQL(ctx, storesSearchQuery, vars, &resp); err != nil {
		return nil, fmt.Errorf("SearchStores: %w", err)
	}
	return resp.StoresSearch.Result, nil
}

// ─── Koopjes / Laatste kansjes ─────────────────────────────────────────────

const bargainItemsQuery = `query BargainItems($storeId: String!) {
	bargainItems(storeId: $storeId) {
		product {
			id
			title
			brand
			salesUnitSize
		}
		categoryTitle
		markdown {
			markdownType
			markdownExpirationDate
			markdownPercentage
		}
		stock
		bargainPrice {
			priceWas
			priceNow
		}
	}
}`

type Bargain struct {
	Product            BargainProduct `json:"product"`
	CategoryTitle      string         `json:"categoryTitle"`
	MarkdownType       string         `json:"markdownType"`
	MarkdownPercentage float64        `json:"markdownPercentage"`
	ExpirationDate     string         `json:"expirationDate"`
	Stock              int            `json:"stock"`
	PriceWas           string         `json:"priceWas"`
	PriceNow           string         `json:"priceNow"`
}

type BargainProduct struct {
	ID       int    `json:"id"`
	Title    string `json:"title"`
	Brand    string `json:"brand"`
	UnitSize string `json:"unitSize"`
}

type bargainItemsResponse struct {
	BargainItems []struct {
		Product struct {
			ID            int    `json:"id"`
			Title         string `json:"title"`
			Brand         string `json:"brand"`
			SalesUnitSize string `json:"salesUnitSize"`
		} `json:"product"`
		CategoryTitle string `json:"categoryTitle"`
		Markdown      struct {
			MarkdownType           string  `json:"markdownType"`
			MarkdownExpirationDate string  `json:"markdownExpirationDate"`
			MarkdownPercentage     float64 `json:"markdownPercentage"`
		} `json:"markdown"`
		Stock        int `json:"stock"`
		BargainPrice struct {
			PriceWas string `json:"priceWas"`
			PriceNow string `json:"priceNow"`
		} `json:"bargainPrice"`
	} `json:"bargainItems"`
}

// GetBargains retrieves laatste kansjes (last-chance discounted products) for a store.
func (c *Client) GetBargains(ctx context.Context, storeID int) ([]Bargain, error) {
	vars := map[string]any{"storeId": fmt.Sprintf("%d", storeID)}
	var resp bargainItemsResponse
	if err := c.DoGraphQL(ctx, bargainItemsQuery, vars, &resp); err != nil {
		return nil, fmt.Errorf("GetBargains(%d): %w", storeID, err)
	}

	bargains := make([]Bargain, 0, len(resp.BargainItems))
	for _, item := range resp.BargainItems {
		bargains = append(bargains, Bargain{
			Product: BargainProduct{
				ID:       item.Product.ID,
				Title:    item.Product.Title,
				Brand:    item.Product.Brand,
				UnitSize: item.Product.SalesUnitSize,
			},
			CategoryTitle:      item.CategoryTitle,
			MarkdownType:       item.Markdown.MarkdownType,
			MarkdownPercentage: item.Markdown.MarkdownPercentage,
			ExpirationDate:     item.Markdown.MarkdownExpirationDate,
			Stock:              item.Stock,
			PriceWas:           item.BargainPrice.PriceWas,
			PriceNow:           item.BargainPrice.PriceNow,
		})
	}
	return bargains, nil
}
