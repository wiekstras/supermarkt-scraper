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

	// AH login flow — simpel login formulier met gebruikersnaam/wachtwoord
	// GET  /api/ah/auth/start?return_url=...  → toon login formulier
	// POST /api/ah/auth/login                 → verwerk login
	// GET  /api/ah/auth/status                → check of AH tokens aanwezig zijn (X-Api-Key vereist)
	r.Get("/api/ah/auth/start", s.handleAHAuthStart)
	r.Post("/api/ah/auth/login", s.handleAHLogin)
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

// handleAHAuthStart toont een simpel login formulier voor AH.
// GET /api/ah/auth/start?return_url=https://voordeeleter.nl/profiel
func (s *Server) handleAHAuthStart(w http.ResponseWriter, r *http.Request) {
	returnURL := r.URL.Query().Get("return_url")
	if returnURL == "" {
		returnURL = os.Getenv("NEXT_PUBLIC_BASE_URL") + "/profiel"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="nl">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Inloggen bij Albert Heijn</title>
<style>
  * { box-sizing: border-box; margin: 0; padding: 0; }
  body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif; background: #f5f5f5; min-height: 100vh; display: flex; align-items: center; justify-content: center; }
  .card { background: white; border-radius: 12px; padding: 32px; width: 100%%; max-width: 400px; box-shadow: 0 2px 16px rgba(0,0,0,0.1); }
  .logo { text-align: center; margin-bottom: 24px; }
  .logo span { font-size: 32px; }
  h1 { font-size: 20px; font-weight: 700; color: #1a1a1a; margin-bottom: 4px; text-align: center; }
  p.sub { font-size: 14px; color: #666; text-align: center; margin-bottom: 24px; }
  label { display: block; font-size: 13px; font-weight: 500; color: #444; margin-bottom: 6px; }
  input { width: 100%%; padding: 10px 14px; border: 1.5px solid #ddd; border-radius: 8px; font-size: 15px; outline: none; transition: border-color 0.2s; }
  input:focus { border-color: #00a346; }
  .field { margin-bottom: 16px; }
  button { width: 100%%; padding: 12px; background: #00a346; color: white; border: none; border-radius: 8px; font-size: 15px; font-weight: 600; cursor: pointer; margin-top: 8px; }
  button:hover { background: #008a3c; }
  .error { background: #fff0f0; border: 1px solid #ffcdd2; color: #c62828; padding: 10px 14px; border-radius: 8px; font-size: 13px; margin-bottom: 16px; display: none; }
  .note { font-size: 12px; color: #999; text-align: center; margin-top: 16px; }
</style>
</head>
<body>
<div class="card">
  <div class="logo"><span>🛒</span></div>
  <h1>Inloggen bij Albert Heijn</h1>
  <p class="sub">Koppel je AH-account aan Voordeeleter</p>
  <div class="error" id="err"></div>
  <form id="form">
    <input type="hidden" name="return_url" value="%s">
    <div class="field">
      <label for="email">E-mailadres</label>
      <input type="email" id="email" name="username" placeholder="jouw@email.nl" required autofocus>
    </div>
    <div class="field">
      <label for="pass">Wachtwoord</label>
      <input type="password" id="pass" name="password" placeholder="••••••••" required>
    </div>
    <button type="submit" id="btn">Inloggen</button>
  </form>
  <p class="note">Je gegevens worden alleen gebruikt om je kassabonnen op te halen.</p>
</div>
<script>
document.getElementById('form').addEventListener('submit', async function(e) {
  e.preventDefault();
  const btn = document.getElementById('btn');
  const err = document.getElementById('err');
  btn.textContent = 'Bezig...';
  btn.disabled = true;
  err.style.display = 'none';
  const data = new FormData(this);
  const res = await fetch('/api/ah/auth/login', { method: 'POST', body: data });
  const json = await res.json();
  if (json.ok) {
    window.location.href = json.redirect;
  } else {
    err.textContent = json.error || 'Inloggen mislukt. Controleer je gegevens.';
    err.style.display = 'block';
    btn.textContent = 'Inloggen';
    btn.disabled = false;
  }
});
</script>
</body>
</html>`, returnURL)
}

// handleAHLogin verwerkt het login formulier.
// POST /api/ah/auth/login  (form: username, password, return_url)
func (s *Server) handleAHLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		jsonError(w, "Ongeldig formulier", http.StatusBadRequest)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	returnURL := r.FormValue("return_url")
	if returnURL == "" {
		returnURL = os.Getenv("NEXT_PUBLIC_BASE_URL") + "/profiel"
	}

	if username == "" || password == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "E-mailadres en wachtwoord zijn verplicht"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	if err := s.ahClient.LoginWithPassword(ctx, username, password); err != nil {
		log.Printf("[API/ah/auth/login] Login mislukt voor %s: %v", username, err)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":    false,
			"error": "Inloggen mislukt. Controleer je e-mailadres en wachtwoord.",
		})
		return
	}

	log.Printf("[API/ah/auth/login] AH account gekoppeld: %s", username)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok":       true,
		"redirect": returnURL + "?ah_login=success",
	})
}

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
