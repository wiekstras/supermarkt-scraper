package ahclient

import (
	"context"
	"fmt"
)

// ─── Types ────────────────────────────────────────────────────────────────────

// Receipt is a summary of a single in-store purchase (kassabon).
type Receipt struct {
	TransactionID string  `json:"transactionId"`
	Date          string  `json:"date"`
	TotalAmount   float64 `json:"totalAmount"`
	// Items, Discounts and Payments are only populated via GetReceiptDetail.
	Items     []ReceiptItem     `json:"items,omitempty"`
	Discounts []ReceiptDiscount `json:"discounts,omitempty"`
	Payments  []ReceiptPayment  `json:"payments,omitempty"`
}

// ReceiptItem is a single line item on a receipt.
type ReceiptItem struct {
	ProductID   int     `json:"productId,omitempty"`
	Description string  `json:"description"`
	Quantity    int     `json:"quantity"`
	UnitPrice   float64 `json:"unitPrice,omitempty"`
	Amount      float64 `json:"amount"`
}

// ReceiptDiscount is a bonus/promotion discount line.
type ReceiptDiscount struct {
	Name   string  `json:"name"`
	Amount float64 `json:"amount"`
}

// ReceiptPayment is a payment method used on a receipt.
type ReceiptPayment struct {
	Method string  `json:"method"`
	Amount float64 `json:"amount"`
}

// ─── GraphQL queries ──────────────────────────────────────────────────────────

const fetchPosReceiptsQuery = `query FetchPosReceipts($offset: Int!, $limit: Int!) {
	posReceiptsPage(pagination: {offset: $offset, limit: $limit}) {
		posReceipts {
			id
			dateTime
			totalAmount {
				amount
			}
		}
	}
}`

const fetchPosReceiptDetailsQuery = `query FetchReceipt($id: String!) {
	posReceiptDetails(id: $id) {
		id
		memberId
		products {
			id
			quantity
			name
			price {
				amount
			}
			amount {
				amount
			}
		}
		discounts {
			name
			amount {
				amount
			}
		}
		payments {
			method
			amount {
				amount
			}
		}
	}
}`

// ─── Internal response types ──────────────────────────────────────────────────

type posReceiptsResponse struct {
	PosReceiptsPage struct {
		PosReceipts []struct {
			ID          string `json:"id"`
			DateTime    string `json:"dateTime"`
			TotalAmount struct {
				Amount float64 `json:"amount"`
			} `json:"totalAmount"`
		} `json:"posReceipts"`
	} `json:"posReceiptsPage"`
}

type posReceiptDetailsResponse struct {
	PosReceiptDetails struct {
		ID       string `json:"id"`
		Products []struct {
			ID       int    `json:"id"`
			Quantity int    `json:"quantity"`
			Name     string `json:"name"`
			Price    *struct {
				Amount float64 `json:"amount"`
			} `json:"price"`
			Amount struct {
				Amount float64 `json:"amount"`
			} `json:"amount"`
		} `json:"products"`
		Discounts []struct {
			Name   string `json:"name"`
			Amount struct {
				Amount float64 `json:"amount"`
			} `json:"amount"`
		} `json:"discounts"`
		Payments []struct {
			Method string `json:"method"`
			Amount struct {
				Amount float64 `json:"amount"`
			} `json:"amount"`
		} `json:"payments"`
	} `json:"posReceiptDetails"`
}

// ─── Methods ──────────────────────────────────────────────────────────────────

// GetReceipts retrieves the list of kassabonnen for the authenticated user.
// Requires an authenticated token — call Login() first if you haven't.
// Returns up to 100 receipts (most recent first).
func (c *Client) GetReceipts(ctx context.Context) ([]Receipt, error) {
	vars := map[string]any{
		"offset": 0,
		"limit":  100,
	}
	var resp posReceiptsResponse
	if err := c.DoGraphQL(ctx, fetchPosReceiptsQuery, vars, &resp); err != nil {
		return nil, fmt.Errorf("GetReceipts: %w", err)
	}

	src := resp.PosReceiptsPage.PosReceipts
	out := make([]Receipt, 0, len(src))
	for _, r := range src {
		out = append(out, Receipt{
			TransactionID: r.ID,
			Date:          r.DateTime,
			TotalAmount:   r.TotalAmount.Amount,
		})
	}
	return out, nil
}

// GetReceiptDetail retrieves the full line-item detail for a single kassabon by ID.
// Requires an authenticated token.
func (c *Client) GetReceiptDetail(ctx context.Context, id string) (*Receipt, error) {
	vars := map[string]any{"id": id}
	var resp posReceiptDetailsResponse
	if err := c.DoGraphQL(ctx, fetchPosReceiptDetailsQuery, vars, &resp); err != nil {
		return nil, fmt.Errorf("GetReceiptDetail(%q): %w", id, err)
	}

	d := resp.PosReceiptDetails

	items := make([]ReceiptItem, 0, len(d.Products))
	for _, p := range d.Products {
		var unitPrice float64
		if p.Price != nil {
			unitPrice = p.Price.Amount
		}
		items = append(items, ReceiptItem{
			ProductID:   p.ID,
			Description: p.Name,
			Quantity:    p.Quantity,
			UnitPrice:   unitPrice,
			Amount:      p.Amount.Amount,
		})
	}

	discounts := make([]ReceiptDiscount, 0, len(d.Discounts))
	for _, disc := range d.Discounts {
		discounts = append(discounts, ReceiptDiscount{
			Name:   disc.Name,
			Amount: disc.Amount.Amount,
		})
	}

	payments := make([]ReceiptPayment, 0, len(d.Payments))
	for _, pay := range d.Payments {
		payments = append(payments, ReceiptPayment{
			Method: pay.Method,
			Amount: pay.Amount.Amount,
		})
	}

	return &Receipt{
		TransactionID: d.ID,
		Items:         items,
		Discounts:     discounts,
		Payments:      payments,
	}, nil
}
