package ahclient

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
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

// StartLoginProxy start een tijdelijke reverse proxy naar login.ah.nl en geeft
// de login URL terug zonder een browser te openen. Wanneer de gebruiker zijn
// browser naar loginURL stuurt, de login doorloopt en de OAuth callback
// ontvangt, worden de tokens opgeslagen in de TokenStore en wordt returnURL
// aangeroepen (als HTTP redirect in de proxy).
//
// returnURL is de URL waar de proxy naartoe redirect na een succesvolle login,
// bijv. "https://voordeeleter.nl/profiel?ah_login=success".
//
// Stop() beëindigt de proxy server (roep altijd aan, ook na succes).
// De done channel ontvangt één keer een fout (of nil bij succes).
func (c *Client) StartLoginProxy(returnURL string) (loginURL string, done <-chan error, stop func(), err error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, nil, fmt.Errorf("StartLoginProxy: lokale server starten mislukt: %w", err)
	}

	localOrigin := fmt.Sprintf("http://%s", listener.Addr().String())
	doneCh := make(chan error, 1)

	loginBaseURL := "https://login.ah.nl"
	target, parseErr := url.Parse(loginBaseURL)
	if parseErr != nil {
		listener.Close()
		return "", nil, nil, fmt.Errorf("StartLoginProxy: ongeldige login URL: %w", parseErr)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			doneCh <- fmt.Errorf("lege authorization code ontvangen")
			http.Redirect(w, r, returnURL+"?ah_login=error&reden=geen_code", http.StatusFound)
			return
		}
		// Tokens inwisselen in de achtergrond; redirect de browser nu al
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := c.exchangeCode(ctx, code); err != nil {
				doneCh <- err
				return
			}
			doneCh <- nil
		}()
		http.Redirect(w, r, returnURL+"?ah_login=success", http.StatusFound)
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

	stopFn := func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx) //nolint:errcheck
	}

	proxyLoginURL := fmt.Sprintf(
		"%s/login?client_id=%s&response_type=code&redirect_uri=appie://login-exit",
		localOrigin, ClientID,
	)
	return proxyLoginURL, doneCh, stopFn, nil
}

// Login voert de volledige browser-login flow uit voor AH.
// Het start een lokale reverse proxy naar login.ah.nl, opent de browser van
// de gebruiker, wacht op de authorization code callback en wisselt die in
// voor access + refresh tokens. Tokens worden opgeslagen in de TokenStore.
//
// Annuleer de context om de login flow af te breken.
//
// Deze methode is alleen nodig voor endpoints die een ingelogde gebruiker
// vereisen (kassabonnen, persoonlijke data). Voor product search en koopjes
// volstaat een anonymous token.
func (c *Client) Login(ctx context.Context) error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("login: lokale server starten mislukt: %w", err)
	}

	localOrigin := fmt.Sprintf("http://%s", listener.Addr().String())
	codeCh := make(chan string, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		codeCh <- code
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, loginSuccessPage) //nolint:errcheck
	})

	loginBaseURL := "https://login.ah.nl"
	target, err := url.Parse(loginBaseURL)
	if err != nil {
		listener.Close()
		return fmt.Errorf("login: ongeldige login URL: %w", err)
	}

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
	fmt.Printf("\n[AH Login] Open deze URL in je browser als hij niet automatisch opent:\n%s\n\n", loginURL)
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

// exchangeCode wisselt een authorization code in voor tokens en slaat ze op.
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

	// Sla zowel access als refresh token op in de store
	if c.tokenStore != nil {
		_ = c.tokenStore.SaveToken(ctx, "ah_access_token", tok.AccessToken, exp)
		if tok.RefreshToken != "" {
			// Refresh tokens zijn doorgaans langer geldig (~30 dagen)
			_ = c.tokenStore.SaveToken(ctx, "ah_refresh_token", tok.RefreshToken,
				time.Now().Add(30*24*time.Hour))
		}
	}
	return nil
}

// IsAuthenticated returns true if an access token is present (anonymous or logged in).
func (c *Client) IsAuthenticated() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.accessToken != ""
}

// ─── Login reverse-proxy helpers ──────────────────────────────────────────────

func rewriteLoginResponse(resp *http.Response, localOrigin, targetHost string) error {
	// Intercept server-side redirects naar appie://
	if loc := resp.Header.Get("Location"); loc != "" {
		if strings.HasPrefix(loc, "appie://") {
			u, err := url.Parse(loc)
			if err != nil {
				return fmt.Errorf("ongeldige appie URL %q: %w", loc, err)
			}
			resp.Header.Set("Location",
				fmt.Sprintf("%s/callback?%s", localOrigin, u.RawQuery))
			return nil
		}
		if strings.Contains(loc, targetHost) {
			resp.Header.Set("Location",
				strings.ReplaceAll(loc, "https://"+targetHost, localOrigin))
		}
	}

	// Verwijder security headers die de proxy zouden blokkeren
	resp.Header.Del("Content-Security-Policy")
	resp.Header.Del("Strict-Transport-Security")
	resp.Header.Del("X-Frame-Options")

	// Herschrijf cookies: verwijder Secure/SameSite/Domain zodat ze werken
	// over plain HTTP op 127.0.0.1
	if cookies := resp.Header.Values("Set-Cookie"); len(cookies) > 0 {
		resp.Header.Del("Set-Cookie")
		for _, cookie := range cookies {
			resp.Header.Add("Set-Cookie", sanitizeCookie(cookie))
		}
	}

	// Herschrijf alleen text response bodies
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

	body = bytes.ReplaceAll(body, []byte("appie://login-exit"), []byte(localOrigin+"/callback"))
	body = bytes.ReplaceAll(body, []byte("https://"+targetHost), []byte(localOrigin))

	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Content-Encoding")

	return nil
}

// sanitizeCookie strips Secure, SameSite en Domain attributen zodat de cookie
// werkt over plain HTTP op localhost.
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
