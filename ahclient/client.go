// Package ahclient is a Go client for the Albert Heijn mobile API.
// It mirrors the approach from appie-go (github.com/gwillem/appie-go) but is
// integrated with our PostgreSQL token cache so tokens survive service restarts.
//
// Authentication:
//   - Anonymous: call GetAnonymousToken() — no login needed, works for product
//     search, deals, stores and koopjes.
//   - Authenticated: call Login() to open browser OAuth flow — required for
//     receipts and personal data. Tokens are persisted in the DB.
package ahclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	BaseURL       = "https://api.ah.nl"
	GraphQLURL    = BaseURL + "/graphql"
	UserAgent     = "Appie/9.28 (iPhone17,3; iPhone; CPU OS 26_1 like Mac OS X)"
	ClientID      = "appie-ios"
	ClientVersion = "9.28"
)

// TokenStore persists tokens across service restarts.
// Implement this with your DB layer, or use MemoryTokenStore for testing.
type TokenStore interface {
	LoadToken(ctx context.Context, key string) (token string, expiresAt time.Time, err error)
	SaveToken(ctx context.Context, key string, token string, expiresAt time.Time) error
}

// Client is the AH mobile API client. Thread-safe.
type Client struct {
	http       *http.Client
	tokenStore TokenStore

	mu           sync.RWMutex
	accessToken  string
	refreshToken string
	expiresAt    time.Time
}

// New creates a new AH API client with an optional token store.
// If tokenStore is nil, tokens are only held in memory.
func New(tokenStore TokenStore) *Client {
	return &Client{
		http:       &http.Client{Timeout: 15 * time.Second},
		tokenStore: tokenStore,
	}
}

// ─── Token management ─────────────────────────────────────────────────────────

// EnsureToken guarantees a valid access token is available, loading from the
// store or fetching a fresh anonymous token as needed.
func (c *Client) EnsureToken(ctx context.Context) error {
	c.mu.RLock()
	hasToken := c.accessToken != ""
	expired := !c.expiresAt.IsZero() && time.Now().After(c.expiresAt.Add(-5*time.Minute))
	c.mu.RUnlock()

	if hasToken && !expired {
		return nil
	}

	// Try loading from persistent store first
	if c.tokenStore != nil {
		tok, exp, err := c.tokenStore.LoadToken(ctx, "ah_access_token")
		if err == nil && tok != "" && time.Now().Before(exp.Add(-5*time.Minute)) {
			c.mu.Lock()
			c.accessToken = tok
			c.expiresAt = exp
			c.mu.Unlock()
			log.Println("[AH Client] Token uit DB cache geladen")
			return nil
		}
	}

	return c.GetAnonymousToken(ctx)
}

// GetAnonymousToken fetches a fresh anonymous token (no login required).
func (c *Client) GetAnonymousToken(ctx context.Context) error {
	body := map[string]string{"clientId": ClientID}
	var tok tokenResponse
	if err := c.doRequest(ctx, http.MethodPost, "/mobile-auth/v1/auth/token/anonymous", body, &tok); err != nil {
		return fmt.Errorf("anonymous token: %w", err)
	}
	c.setToken(tok)
	log.Printf("[AH Client] Nieuw anonymous token opgehaald (geldig %ds)", tok.ExpiresIn)

	if c.tokenStore != nil {
		exp := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
		_ = c.tokenStore.SaveToken(ctx, "ah_access_token", tok.AccessToken, exp)
	}
	return nil
}

// SetTokens stores a known access + refresh token (e.g. after OAuth login).
func (c *Client) SetTokens(accessToken, refreshToken string, expiresAt time.Time) {
	c.mu.Lock()
	c.accessToken = accessToken
	c.refreshToken = refreshToken
	c.expiresAt = expiresAt
	c.mu.Unlock()
}

// RefreshToken uses the refresh token to get a new access token.
func (c *Client) RefreshToken(ctx context.Context) error {
	c.mu.RLock()
	rt := c.refreshToken
	c.mu.RUnlock()
	if rt == "" {
		return c.GetAnonymousToken(ctx)
	}
	body := map[string]string{"clientId": ClientID, "refreshToken": rt}
	var tok tokenResponse
	if err := c.doRequest(ctx, http.MethodPost, "/mobile-auth/v1/auth/token/refresh", body, &tok); err != nil {
		return fmt.Errorf("refresh token: %w", err)
	}
	c.setToken(tok)
	if c.tokenStore != nil {
		exp := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
		_ = c.tokenStore.SaveToken(ctx, "ah_access_token", tok.AccessToken, exp)
	}
	return nil
}

func (c *Client) setToken(tok tokenResponse) {
	exp := time.Time{}
	if tok.ExpiresIn > 0 {
		exp = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	c.mu.Lock()
	c.accessToken = tok.AccessToken
	c.refreshToken = tok.RefreshToken
	c.expiresAt = exp
	c.mu.Unlock()
}

func (c *Client) currentToken() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.accessToken
}

// ─── HTTP core ────────────────────────────────────────────────────────────────

func (c *Client) doRequest(ctx context.Context, method, path string, body, result any) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, BaseURL+path, bodyReader)
	if err != nil {
		return err
	}

	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("x-client-name", ClientID)
	req.Header.Set("x-client-version", ClientVersion)
	req.Header.Set("x-application", "AHWEBSHOP")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	if tok := c.currentToken(); tok != "" && !strings.HasPrefix(path, "/mobile-auth/") {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("request %s: %w", path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	if resp.StatusCode == 401 {
		return ErrUnauthorized
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("AH API HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	if result != nil && len(respBody) > 0 {
		return json.Unmarshal(respBody, result)
	}
	return nil
}

// DoRequest performs an authenticated request, auto-refreshing the token once on 401.
func (c *Client) DoRequest(ctx context.Context, method, path string, body, result any) error {
	if err := c.EnsureToken(ctx); err != nil {
		return err
	}
	err := c.doRequest(ctx, method, path, body, result)
	if err == ErrUnauthorized {
		// Token expired — get a fresh one and retry once
		log.Println("[AH Client] 401 — token verlopen, nieuw token ophalen")
		if ferr := c.GetAnonymousToken(ctx); ferr != nil {
			return fmt.Errorf("token refresh na 401: %w", ferr)
		}
		return c.doRequest(ctx, method, path, body, result)
	}
	return err
}

// DoGraphQL performs a GraphQL request against the AH API.
func (c *Client) DoGraphQL(ctx context.Context, query string, variables map[string]any, result any) error {
	reqBody := graphQLRequest{Query: query, Variables: variables}
	var resp graphQLResponse
	if err := c.DoRequest(ctx, http.MethodPost, "/graphql", reqBody, &resp); err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		return fmt.Errorf("graphql: %s", resp.Errors[0].Message)
	}
	if result != nil && len(resp.Data) > 0 {
		return json.Unmarshal(resp.Data, result)
	}
	return nil
}

// ─── Errors ───────────────────────────────────────────────────────────────────

var ErrUnauthorized = fmt.Errorf("unauthorized")

// ─── Internal types ───────────────────────────────────────────────────────────

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

type graphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type graphQLResponse struct {
	Data   json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}
