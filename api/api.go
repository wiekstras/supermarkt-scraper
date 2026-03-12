// Package api implements the HTTP REST API for the supermarkt scraper service.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/wiekstras/supermarkt-scraper/ahclient"
	"github.com/wiekstras/supermarkt-scraper/db"
	"github.com/wiekstras/supermarkt-scraper/scraper"
)


// ScraperTrigger is called by the API to trigger an on-demand deal scrape.
type ScraperTrigger interface {
	ScrapeDeals()
}

// Server holds the router and dependencies.
type Server struct {
	router     *chi.Mux
	stores     map[string]scraper.Store // keyed by store name
	scraper    ScraperTrigger
	ahClient   *ahclient.Client
	apiKey     string
	cronSecret string
}

func New(stores []scraper.Store, scraperTrigger ScraperTrigger) *Server {
	storeMap := make(map[string]scraper.Store, len(stores))
	for _, s := range stores {
		storeMap[s.Name()] = s
	}
	srv := &Server{
		stores:     storeMap,
		scraper:    scraperTrigger,
		ahClient:   ahclient.New(db.NewTokenStore()),
		apiKey:     os.Getenv("API_KEY"),
		cronSecret: os.Getenv("CRON_SECRET"),
	}
	srv.routes()
	return srv
}

func (s *Server) Handler() http.Handler { return s.router }

func (s *Server) routes() {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Public
	r.Get("/health", s.handleHealth)

	// AH login flow — reverse proxy naar login.ah.nl
	// GET  /api/ah/auth/start?return_url=...  → redirect naar proxy login URL
	// /api/ah/login-proxy/*                   → reverse proxy naar login.ah.nl
	// GET  /api/ah/auth/status                → check of AH tokens aanwezig zijn (X-Api-Key vereist)
	r.Get("/api/ah/auth/start", s.handleAHAuthStart)
	r.Mount("/api/ah/login-proxy", s.ahLoginProxyHandler())
	r.Group(func(r chi.Router) {
		r.Use(s.requireAPIKey)
		r.Get("/api/ah/auth/status", s.handleAHAuthStatus)
	})

	// Protected by API key
	r.Group(func(r chi.Router) {
		r.Use(s.requireAPIKey)
		r.Get("/api/zoek", s.handleZoek)
		r.Get("/api/deals", s.handleDeals)
		r.Get("/api/prijshistorie", s.handlePrijsHistorie)
		r.Get("/api/prijshistorie/trend", s.handlePrijsTrend)

		// AH-specific: laatste kansjes per winkel
		// GET /api/koopjes?postcode=1234AB  -> zoekt dichtstbijzijnde store
		// GET /api/koopjes?store_id=1234    -> direct voor bekende store ID
		r.Get("/api/koopjes", s.handleKoopjes)

		// AH kassabonnen (vereist ingelogde AH gebruiker)
		r.Get("/api/kassabonnen", s.handleKassabonnen)
		r.Get("/api/kassabonnen/{id}", s.handleKassabonDetail)
	})

	// Protected by cron secret
	r.Post("/api/scrape/deals", s.handleScrapeDealsTrigger)

	s.router = r
}

// ─── Middleware ───────────────────────────────────────────────────────────────

func (s *Server) requireAPIKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.apiKey == "" {
			// No API key configured — allow all (dev mode)
			next.ServeHTTP(w, r)
			return
		}
		if r.Header.Get("X-Api-Key") != s.apiKey {
			jsonError(w, "Niet geautoriseerd", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ─── Handlers ─────────────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]interface{}{"ok": true, "version": "1.0.0", "time": time.Now()})
}

// handleZoek searches all (or selected) stores concurrently and returns grouped results.
// GET /api/zoek?q=pindakaas[&stores=ah,jumbo]
func (s *Server) handleZoek(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	if len(strings.TrimSpace(query)) < 2 {
		jsonError(w, "Zoekterm te kort (minimaal 2 tekens)", http.StatusBadRequest)
		return
	}

	// Which stores to search
	storeFilter := r.URL.Query().Get("stores")
	var targetStores []scraper.Store
	if storeFilter != "" {
		for _, name := range strings.Split(storeFilter, ",") {
			if st, ok := s.stores[strings.TrimSpace(name)]; ok {
				targetStores = append(targetStores, st)
			}
		}
	}
	if len(targetStores) == 0 {
		for _, st := range s.stores {
			targetStores = append(targetStores, st)
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	type storeResult struct {
		name    string
		results []scraper.PrijsResultaat
		err     error
	}

	ch := make(chan storeResult, len(targetStores))
	var wg sync.WaitGroup

	for _, st := range targetStores {
		wg.Add(1)
		go func(st scraper.Store) {
			defer wg.Done()
			results, err := st.Zoek(ctx, query)
			if err != nil {
				log.Printf("[API/zoek] %s fout: %v — probeer cache", st.Name(), err)
				// Fallback to DB search cache
				cached, cErr := db.HaalZoekResultatenOp(query, st.Name())
				if cErr == nil && len(cached) > 0 {
					log.Printf("[API/zoek] %s cache hit (%d resultaten)", st.Name(), len(cached))
					ch <- storeResult{st.Name(), cached, nil}
					return
				}
			} else {
				// Cache successful results
				go db.SlaZoekResultatenOp(query, st.Name(), results)
				// Also store price snapshots for history
				go db.SlaaPrijsSnapshotOp(results, st.Name())
			}
			ch <- storeResult{st.Name(), results, err}
		}(st)
	}

	go func() {
		wg.Wait()
		close(ch)
	}()

	allResults := make(map[string][]scraper.PrijsResultaat)
	for res := range ch {
		if res.err == nil && len(res.results) > 0 {
			allResults[res.name] = res.results
		}
	}

	jsonOK(w, map[string]interface{}{
		"query":      query,
		"resultaten": allResults,
	})
}

// handleDeals returns deals from the DB.
// GET /api/deals[?store=ah][&week=2025-W12]
func (s *Server) handleDeals(w http.ResponseWriter, r *http.Request) {
	store := r.URL.Query().Get("store")
	week := r.URL.Query().Get("week")
	deals, err := db.HaalDealsOp(store, week)
	if err != nil {
		log.Printf("[API/deals] DB fout: %v", err)
		jsonError(w, "Database fout", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]interface{}{"deals": deals, "week": week, "store": store})
}

// handleScrapeDealsTrigger triggers an on-demand scrape (protected by cron secret).
// POST /api/scrape/deals
func (s *Server) handleScrapeDealsTrigger(w http.ResponseWriter, r *http.Request) {
	if s.cronSecret != "" && r.Header.Get("X-Cron-Secret") != s.cronSecret {
		jsonError(w, "Niet geautoriseerd", http.StatusUnauthorized)
		return
	}
	go s.scraper.ScrapeDeals()
	jsonOK(w, map[string]interface{}{"ok": true, "bericht": "Deal scrape gestart op de achtergrond"})
}

// handlePrijsHistorie returns the price history for a single product ID.
// GET /api/prijshistorie?product_id=ah-123432
func (s *Server) handlePrijsHistorie(w http.ResponseWriter, r *http.Request) {
	productID := r.URL.Query().Get("product_id")
	if productID == "" {
		jsonError(w, "product_id is vereist", http.StatusBadRequest)
		return
	}
	history, err := db.HaalPrijsHistorieOp(productID)
	if err != nil {
		log.Printf("[API/prijshistorie] DB fout: %v", err)
		jsonError(w, "Database fout", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]interface{}{"productId": productID, "historie": history})
}

// handlePrijsTrend returns price history matched by product name for a store.
// GET /api/prijshistorie/trend?naam=cola&store=ah[&dagen=90]
func (s *Server) handlePrijsTrend(w http.ResponseWriter, r *http.Request) {
	naam := r.URL.Query().Get("naam")
	store := r.URL.Query().Get("store")
	if naam == "" || store == "" {
		jsonError(w, "naam en store zijn vereist", http.StatusBadRequest)
		return
	}
	dagenStr := r.URL.Query().Get("dagen")
	dagen := 90
	if dagenStr != "" {
		if d, err := strconv.Atoi(dagenStr); err == nil && d > 0 {
			dagen = d
		}
	}
	history, err := db.HaalPrijsTrendOp(naam, store, dagen)
	if err != nil {
		log.Printf("[API/trend] DB fout: %v", err)
		jsonError(w, "Database fout", http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]interface{}{"naam": naam, "store": store, "historie": history})
}

// handleKoopjes returns laatste kansjes (last-chance markdown items) for an AH store.
//
//	GET /api/koopjes?postcode=1234AB  → zoekt dichtstbijzijnde store automatisch
//	GET /api/koopjes?store_id=1234    → geef koopjes voor een specifieke store
func (s *Server) handleKoopjes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	storeIDStr := r.URL.Query().Get("store_id")
	postcode := r.URL.Query().Get("postcode")

	if storeIDStr == "" && postcode == "" {
		jsonError(w, "Geef store_id of postcode op", http.StatusBadRequest)
		return
	}

	var storeID int
	var storeName string

	if storeIDStr != "" {
		id, err := strconv.Atoi(storeIDStr)
		if err != nil {
			jsonError(w, "store_id moet een getal zijn", http.StatusBadRequest)
			return
		}
		storeID = id
		storeName = fmt.Sprintf("AH store %d", storeID)
	} else {
		// Zoek stores op postcode, pak de dichtstbijzijnde (eerste resultaat)
		stores, err := s.ahClient.SearchStores(ctx, postcode)
		if err != nil {
			log.Printf("[API/koopjes] SearchStores fout: %v", err)
			jsonError(w, "Winkels zoeken mislukt", http.StatusBadGateway)
			return
		}
		if len(stores) == 0 {
			jsonError(w, "Geen AH winkels gevonden voor deze postcode", http.StatusNotFound)
			return
		}
		storeID = stores[0].ID
		storeName = stores[0].Name
		if stores[0].Address.City != "" {
			storeName += " " + stores[0].Address.City
		}
	}

	bargains, err := s.ahClient.GetBargains(ctx, storeID)
	if err != nil {
		log.Printf("[API/koopjes] GetBargains(%d) fout: %v", storeID, err)
		jsonError(w, "Koopjes ophalen mislukt", http.StatusBadGateway)
		return
	}

	jsonOK(w, map[string]interface{}{
		"storeId":   storeID,
		"storeName": storeName,
		"koopjes":   bargains,
		"aantal":    len(bargains),
	})
}

// handleKassabonnen returns the kassabon list for the authenticated AH user.
// Requires a full AH login — the user must have previously authenticated via
// the CLI: cd supermarkt-scraper && go run ./cmd/ah-login
// GET /api/kassabonnen
func (s *Server) handleKassabonnen(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	receipts, err := s.ahClient.GetReceipts(ctx)
	if err != nil {
		log.Printf("[API/kassabonnen] fout: %v", err)
		if err == ahclient.ErrUnauthorized {
			jsonError(w, "Niet ingelogd bij AH — voer ah-login uit op de server", http.StatusUnauthorized)
			return
		}
		jsonError(w, "Kassabonnen ophalen mislukt", http.StatusBadGateway)
		return
	}

	jsonOK(w, map[string]interface{}{
		"kassabonnen": receipts,
		"aantal":      len(receipts),
	})
}

// handleKassabonDetail returns line-item detail for a single kassabon.
// GET /api/kassabonnen/{id}
func (s *Server) handleKassabonDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if id == "" {
		jsonError(w, "kassabon ID ontbreekt", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	receipt, err := s.ahClient.GetReceiptDetail(ctx, id)
	if err != nil {
		log.Printf("[API/kassabonnen/%s] fout: %v", id, err)
		if err == ahclient.ErrUnauthorized {
			jsonError(w, "Niet ingelogd bij AH — voer ah-login uit op de server", http.StatusUnauthorized)
			return
		}
		jsonError(w, "Kassabon ophalen mislukt", http.StatusBadGateway)
		return
	}

	jsonOK(w, receipt)
}

// ─── AH OAuth login flow ──────────────────────────────────────────────────────

// publicURL returns the externally reachable base URL of this service.
func publicURL(r *http.Request) string {
	if u := os.Getenv("PUBLIC_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	scheme := "https"
	if r.TLS == nil && r.Header.Get("X-Forwarded-Proto") != "https" {
		scheme = "http"
	}
	return fmt.Sprintf("%s://%s", scheme, r.Host)
}

// ahLoginProxyHandler mounts a reverse proxy to login.ah.nl at /api/ah/login-proxy.
// The proxy runs on the public HTTPS Railway URL so cookies work correctly.
func (s *Server) ahLoginProxyHandler() http.Handler {
	base := strings.TrimRight(os.Getenv("PUBLIC_URL"), "/")
	if base == "" {
		base = "https://supermarkt-scraper-production.up.railway.app"
	}
	returnURL := strings.TrimRight(os.Getenv("NEXT_PUBLIC_BASE_URL"), "/") + "/profiel"
	if os.Getenv("NEXT_PUBLIC_BASE_URL") == "" {
		returnURL = "http://localhost:3000/profiel"
	}
	return s.ahClient.LoginProxyHandler(base, returnURL)
}

// handleAHAuthStart serveert een HTML-pagina met de AH login in een iframe.
// De browser laadt login.ah.nl direct (geen server-side proxy nodig),
// en we onderscheppen de appie:// callback via de iframe URL.
// GET /api/ah/auth/start?return_url=https://voordeeleter.nl/profiel
func (s *Server) handleAHAuthStart(w http.ResponseWriter, r *http.Request) {
	base := strings.TrimRight(os.Getenv("PUBLIC_URL"), "/")
	if base == "" {
		scheme := "https"
		if r.Header.Get("X-Forwarded-Proto") == "http" {
			scheme = "http"
		}
		base = fmt.Sprintf("%s://%s", scheme, r.Host)
	}
	returnURL := strings.TrimRight(os.Getenv("NEXT_PUBLIC_BASE_URL"), "/") + "/profiel"
	if os.Getenv("NEXT_PUBLIC_BASE_URL") == "" {
		returnURL = "http://localhost:3000/profiel"
	}

	// The callback URL that receives the code after login
	callbackBase := base + "/api/ah/login-proxy/callback"

	// Direct AH login URL — browser navigates to login.ah.nl directly (no proxy needed)
	// redirect_uri=appie://login-exit is required by AH; we intercept it via JS
	ahLoginURL := fmt.Sprintf(
		"https://login.ah.nl/login?client_id=%s&response_type=code&redirect_uri=appie://login-exit",
		ahclient.ClientID,
	)

	log.Printf("[API/ah/auth/start] Serveer login pagina, callbackBase=%s", callbackBase)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, loginPageHTML, ahLoginURL, callbackBase, returnURL)
}

// loginPageHTML is the HTML page that embeds the AH login in an iframe.
// After login, AH redirects to appie://login-exit?code=... — the browser
// can't open that URL, so we detect the navigation failure and extract the code.
const loginPageHTML = `<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Inloggen bij Albert Heijn</title>
<style>
* { margin: 0; padding: 0; box-sizing: border-box; }
body { font-family: system-ui, sans-serif; background: #f5f5f5; }
#frame-container { width: 100%%; height: 100vh; position: relative; }
iframe { width: 100%%; height: 100%%; border: none; }
#status { display: none; position: fixed; top: 0; left: 0; right: 0; bottom: 0;
  background: white; align-items: center; justify-content: center;
  flex-direction: column; gap: 16px; font-size: 18px; }
#status.visible { display: flex; }
.spinner { width: 40px; height: 40px; border: 4px solid #eee;
  border-top-color: #0056b3; border-radius: 50%%; animation: spin 0.8s linear infinite; }
@keyframes spin { to { transform: rotate(360deg); } }
</style>
</head>
<body>
<div id="frame-container">
  <iframe id="ah-frame" src="%s" sandbox="allow-same-origin allow-scripts allow-forms allow-popups allow-top-navigation"></iframe>
</div>
<div id="status">
  <div class="spinner"></div>
  <p>Bezig met inloggen...</p>
</div>
<script>
const CALLBACK_BASE = %q;
const RETURN_URL = %q;

// Poll the iframe's location — when AH redirects to appie://, the iframe
// navigation fails and we can catch it via the error or unload event.
const frame = document.getElementById('ah-frame');
const status = document.getElementById('status');

frame.addEventListener('load', function() {
  try {
    const frameURL = frame.contentWindow.location.href;
    checkForCode(frameURL);
  } catch(e) {
    // Cross-origin — normal during login flow
  }
});

// Also intercept navigation attempts to appie:// by monitoring beforeunload
window.addEventListener('message', function(e) {
  if (e.data && e.data.appieCode) {
    handleCode(e.data.appieCode);
  }
});

// Inject a script into the iframe once it's on the login.ah.nl domain
// to intercept the appie:// redirect before it happens
frame.addEventListener('load', function() {
  try {
    const doc = frame.contentWindow.document;
    const script = doc.createElement('script');
    script.textContent = ` + "`" + `
      // Intercept all link clicks and location changes for appie://
      const origAssign = window.location.assign.bind(window.location);
      const origReplace = window.location.replace.bind(window.location);
      function interceptAppie(url) {
        if (typeof url === 'string' && url.startsWith('appie://')) {
          const u = new URL(url);
          const code = u.searchParams.get('code');
          if (code) {
            window.parent.postMessage({ appieCode: code }, '*');
            return true;
          }
        }
        return false;
      }
      window.location.assign = function(url) {
        if (!interceptAppie(url)) origAssign(url);
      };
      window.location.replace = function(url) {
        if (!interceptAppie(url)) origReplace(url);
      };
      // Override href setter on location
      const origHref = Object.getOwnPropertyDescriptor(window.location, 'href');
    ` + "`" + `;
    doc.head.appendChild(script);
  } catch(e) {
    // Cross-origin, can't inject — use fallback
  }
});

function checkForCode(url) {
  if (url && url.startsWith('appie://')) {
    try {
      const u = new URL(url);
      const code = u.searchParams.get('code');
      if (code) handleCode(code);
    } catch(e) {}
  }
}

function handleCode(code) {
  status.classList.add('visible');
  fetch(CALLBACK_BASE + '?code=' + encodeURIComponent(code))
    .then(r => r.json())
    .then(data => {
      if (data.ok) {
        window.location.href = RETURN_URL + '?ah_login=success';
      } else {
        window.location.href = RETURN_URL + '?ah_login=error&reden=exchange_mislukt';
      }
    })
    .catch(() => {
      window.location.href = RETURN_URL + '?ah_login=error&reden=exchange_mislukt';
    });
}
</script>
</body>
</html>`

// handleAHAuthStatus geeft aan of er een geldig AH access token in de DB staat.
// GET /api/ah/auth/status
func (s *Server) handleAHAuthStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	ts := db.NewTokenStore()
	token, expiresAt, err := ts.LoadToken(ctx, "ah_access_token")
	if err != nil {
		log.Printf("[API/ah/auth/status] DB fout: %v", err)
		jsonError(w, "Status ophalen mislukt", http.StatusInternalServerError)
		return
	}

	linked := token != "" && time.Now().Before(expiresAt.Add(-30*time.Second))
	jsonOK(w, map[string]interface{}{
		"linked":    linked,
		"expiresAt": expiresAt,
	})
}

// ─── Response helpers ─────────────────────────────────────────────────────────

func jsonOK(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
