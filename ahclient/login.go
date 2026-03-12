package ahclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const loginSuccessPage = `<!DOCTYPE html>
<html><head><title>Login geslaagd</title></head>
<body style="font-family:system-ui;max-width:500px;margin:80px auto;text-align:center">
<h1>Login geslaagd!</h1>
<p>Je kunt dit tabblad sluiten.</p>
<script>setTimeout(function(){window.close()},500)</script>
</body></html>`

// LoginProxyHandler returns an http.Handler that acts as a reverse proxy to
// login.ah.nl. Mount it at /api/ah/login-proxy on your main server.
//
// publicBaseURL: externally reachable base of the Go service,
//
//	e.g. "https://supermarkt-scraper-production.up.railway.app"
//
// returnURL: where the browser goes after successful login,
//
//	e.g. "https://voordeeleter.nl/profiel"
// LoginProxyHandler returns an http.Handler that acts as a reverse proxy to
// login.ah.nl — exactly like appie-go's Login() but mounted on the public
// server instead of a local port.
//
// Mount at /api/ah/login-proxy. The browser navigates to:
//
//	{publicBaseURL}/api/ah/login-proxy/login?client_id=appie-ios&response_type=code&redirect_uri=appie://login-exit
func (c *Client) LoginProxyHandler(publicBaseURL, returnURL string) http.Handler {
	loginBaseURL := "https://login.ah.nl"
	target, _ := url.Parse(loginBaseURL)
	// proxyOrigin is what the browser sees — used to rewrite URLs in responses
	proxyOrigin := strings.TrimRight(publicBaseURL, "/") + "/api/ah/login-proxy"

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.Header.Del("Accept-Encoding")
			log.Printf("[AH proxy] >> %s %s", req.Method, req.URL.Path)
		},
		ModifyResponse: func(resp *http.Response) error {
			log.Printf("[AH proxy] << %d %s", resp.StatusCode, resp.Request.URL.Path)
			return rewriteLoginResponse(resp, proxyOrigin, target.Host)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("[AH proxy] fout: %v", err)
			http.Error(w, "proxy fout", http.StatusBadGateway)
		},
	}

	// Use a single HandlerFunc instead of ServeMux to avoid Go's path matching issues.
	// chi r.Mount already strips the /api/ah/login-proxy prefix.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/callback" {
			code := r.URL.Query().Get("code")
			if code == "" {
				http.Redirect(w, r, returnURL+"?ah_login=error&reden=geen_code", http.StatusFound)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := c.exchangeCode(ctx, code); err != nil {
				log.Printf("[AH proxy] exchangeCode mislukt: %v", err)
				http.Redirect(w, r, returnURL+"?ah_login=error&reden=exchange_mislukt", http.StatusFound)
				return
			}
			log.Println("[AH proxy] Login geslaagd, tokens opgeslagen")
			http.Redirect(w, r, returnURL+"?ah_login=success", http.StatusFound)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

// LoginURL returns the URL the browser should visit to start the AH OAuth flow.
// publicBaseURL is the externally reachable base URL of the Go service.
func LoginURL(publicBaseURL string) string {
	base := strings.TrimRight(publicBaseURL, "/")
	return fmt.Sprintf(
		"%s/api/ah/login-proxy/login?client_id=%s&response_type=code&redirect_uri=appie://login-exit",
		base, ClientID,
	)
}

// Login voert de volledige browser-login flow uit via een lokale proxy.
// Alleen voor CLI gebruik (cmd/ah-login). Op Railway gebruik LoginProxyHandler.
func (c *Client) Login(ctx context.Context) error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("login: lokale server starten mislukt: %w", err)
	}

	localOrigin := fmt.Sprintf("http://%s", listener.Addr().String())
	codeCh := make(chan string, 1)

	loginBaseURL := "https://login.ah.nl"
	target, err := url.Parse(loginBaseURL)
	if err != nil {
		listener.Close()
		return fmt.Errorf("login: ongeldige login URL: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		codeCh <- code
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, loginSuccessPage) //nolint:errcheck
	})

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.Header.Del("Accept-Encoding")
		},
		ModifyResponse: func(resp *http.Response) error {
			return rewriteLoginResponse(resp, localOrigin, target.Host)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "proxy fout", http.StatusBadGateway)
		},
	}
	mux.Handle("/", proxy)

	srv := &http.Server{Handler: mux}
	go srv.Serve(listener) //nolint:errcheck
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx) //nolint:errcheck
	}()

	loginURL := fmt.Sprintf(
		"%s/login?client_id=%s&response_type=code&redirect_uri=appie://login-exit",
		localOrigin, ClientID,
	)
	fmt.Printf("\n[AH Login] Open deze URL in je browser:\n%s\n\n", loginURL)
	openBrowser(loginURL)

	select {
	case code := <-codeCh:
		if code == "" {
			return fmt.Errorf("login: lege authorization code ontvangen")
		}
		return c.exchangeCode(ctx, code)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// LoginWithPassword logs in directly with AH username and password.
// No browser or OAuth flow needed — uses the mobile app API directly.
func (c *Client) LoginWithPassword(ctx context.Context, username, password string) error {
	body := map[string]string{
		"clientId": ClientID,
		"username": username,
		"password": password,
	}
	var tok tokenResponse
	if err := c.doRequest(ctx, http.MethodPost, "/mobile-auth/v1/auth/token", body, &tok); err != nil {
		return fmt.Errorf("LoginWithPassword: %w", err)
	}
	exp := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	c.mu.Lock()
	c.accessToken = tok.AccessToken
	c.refreshToken = tok.RefreshToken
	c.expiresAt = exp
	c.mu.Unlock()
	if c.tokenStore != nil {
		_ = c.tokenStore.SaveToken(ctx, "ah_access_token", tok.AccessToken, exp)
		if tok.RefreshToken != "" {
			_ = c.tokenStore.SaveToken(ctx, "ah_refresh_token", tok.RefreshToken,
				time.Now().Add(30*24*time.Hour))
		}
	}
	return nil
}

// ExchangeCode wisselt een authorization code in voor tokens en slaat ze op.
func (c *Client) ExchangeCode(ctx context.Context, code string) error {
	return c.exchangeCode(ctx, code)
}

// exchangeCode is de interne implementatie.
func (c *Client) exchangeCode(ctx context.Context, code string) error {
	body := map[string]string{
		"clientId": ClientID,
		"code":     code,
	}
	var tok tokenResponse
	if err := c.doRequest(ctx, http.MethodPost, "/mobile-auth/v1/auth/token", body, &tok); err != nil {
		return fmt.Errorf("exchangeCode: %w", err)
	}

	exp := time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	c.mu.Lock()
	c.accessToken = tok.AccessToken
	c.refreshToken = tok.RefreshToken
	c.expiresAt = exp
	c.mu.Unlock()

	if c.tokenStore != nil {
		_ = c.tokenStore.SaveToken(ctx, "ah_access_token", tok.AccessToken, exp)
		if tok.RefreshToken != "" {
			_ = c.tokenStore.SaveToken(ctx, "ah_refresh_token", tok.RefreshToken,
				time.Now().Add(30*24*time.Hour))
		}
	}
	return nil
}

// IsAuthenticated returns true if an access token is present.
func (c *Client) IsAuthenticated() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.accessToken != ""
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func rewriteLoginResponse(resp *http.Response, proxyOrigin, targetHost string) error {
	if loc := resp.Header.Get("Location"); loc != "" {
		if strings.HasPrefix(loc, "appie://") {
			u, err := url.Parse(loc)
			if err != nil {
				return fmt.Errorf("ongeldige appie URL %q: %w", loc, err)
			}
			resp.Header.Set("Location",
				fmt.Sprintf("%s/callback?%s", proxyOrigin, u.RawQuery))
			return nil
		}
		if strings.Contains(loc, targetHost) {
			resp.Header.Set("Location",
				strings.ReplaceAll(loc, "https://"+targetHost, proxyOrigin))
		}
	}

	resp.Header.Del("Content-Security-Policy")
	resp.Header.Del("Strict-Transport-Security")
	resp.Header.Del("X-Frame-Options")

	if cookies := resp.Header.Values("Set-Cookie"); len(cookies) > 0 {
		resp.Header.Del("Set-Cookie")
		for _, cookie := range cookies {
			resp.Header.Add("Set-Cookie", sanitizeCookie(cookie))
		}
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/html") &&
		!strings.Contains(ct, "javascript") &&
		!strings.Contains(ct, "json") {
		return nil
	}

	body, err := readResponseBody(resp)
	if err != nil {
		return err
	}

	body = bytes.ReplaceAll(body, []byte("appie://login-exit"), []byte(proxyOrigin+"/callback"))
	body = bytes.ReplaceAll(body, []byte("https://"+targetHost), []byte(proxyOrigin))

	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Content-Encoding")

	return nil
}

func sanitizeCookie(cookie string) string {
	parts := strings.Split(cookie, ";")
	out := parts[:1]
	for _, p := range parts[1:] {
		attr := strings.TrimSpace(p)
		lower := strings.ToLower(attr)
		if lower == "secure" ||
			strings.HasPrefix(lower, "samesite") ||
			strings.HasPrefix(lower, "domain") {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, ";")
}

func readResponseBody(resp *http.Response) ([]byte, error) {
	var reader io.Reader = resp.Body
	if resp.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		reader = gz
	}
	data, err := io.ReadAll(reader)
	resp.Body.Close()
	return data, err
}

func openBrowser(rawURL string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "linux":
		cmd = exec.Command("xdg-open", rawURL)
	default:
		return
	}
	_ = cmd.Start()
}
