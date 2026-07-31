package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/config"
	"llm-gateway/internal/proxy"
	"llm-gateway/internal/registry"
	"llm-gateway/internal/store"
)

// Deps bundles everything the dashboard handlers need.
type Deps struct {
	Store    *store.Store
	Reg      *registry.Registry
	Proxy    *proxy.Proxy
	Auth     *auth.Dashboard
	Env      *config.Env
}

// Mount registers all /dashboard/* routes on mux.
func Mount(mux *http.ServeMux, d *Deps) {
	dash := d.Auth

	api := &apiHandlers{d: d}

	withAuth := dash.Middleware

	// Auth endpoints.
	mux.HandleFunc("POST /dashboard/api/login", dash.LoginHandler)
	mux.HandleFunc("POST /dashboard/api/logout", dash.LogoutHandler)

	// Domain APIs.
	mux.Handle("GET /dashboard/api/providers", withAuth(http.HandlerFunc(api.listProviders)))
	mux.Handle("POST /dashboard/api/providers", withAuth(http.HandlerFunc(api.createProvider)))
	mux.Handle("GET /dashboard/api/providers/{id}", withAuth(http.HandlerFunc(api.getProvider)))
	mux.Handle("PUT /dashboard/api/providers/{id}", withAuth(http.HandlerFunc(api.updateProvider)))
	mux.Handle("DELETE /dashboard/api/providers/{id}", withAuth(http.HandlerFunc(api.deleteProvider)))
	mux.Handle("POST /dashboard/api/providers/{id}/test", withAuth(http.HandlerFunc(api.testProvider)))
	mux.Handle("POST /dashboard/api/models/list", withAuth(http.HandlerFunc(api.listUpstreamModels)))

	mux.Handle("GET /dashboard/api/combos", withAuth(http.HandlerFunc(api.listCombos)))
	mux.Handle("POST /dashboard/api/combos", withAuth(http.HandlerFunc(api.createCombo)))
	mux.Handle("PUT /dashboard/api/combos/{id}", withAuth(http.HandlerFunc(api.updateCombo)))
	mux.Handle("DELETE /dashboard/api/combos/{id}", withAuth(http.HandlerFunc(api.deleteCombo)))
	mux.Handle("POST /dashboard/api/combos/{id}/test", withAuth(http.HandlerFunc(api.testCombo)))

	mux.Handle("GET /dashboard/api/logs", withAuth(http.HandlerFunc(api.listLogs)))
	mux.Handle("GET /dashboard/api/logs/chart", withAuth(http.HandlerFunc(api.logsChart)))

	mux.Handle("GET /dashboard/api/settings", withAuth(http.HandlerFunc(api.getSettings)))
	mux.Handle("PUT /dashboard/api/settings", withAuth(http.HandlerFunc(api.updateSettings)))

	mux.Handle("GET /dashboard/api/export", withAuth(http.HandlerFunc(api.export)))
	mux.Handle("POST /dashboard/api/import", withAuth(http.HandlerFunc(api.importSQL)))

	mux.Handle("GET /dashboard/api/health", withAuth(http.HandlerFunc(api.health)))
	mux.Handle("GET /dashboard/api/overview", withAuth(http.HandlerFunc(api.overview)))
	mux.Handle("GET /dashboard/api/endpoint", withAuth(http.HandlerFunc(api.endpointInfo)))

	// Static SPA + login page.
	mux.HandleFunc("GET /dashboard/login", serveLogin)
	mux.Handle("GET /dashboard/", withAuth(http.HandlerFunc(serveSPA)))
}

type apiHandlers struct{ d *Deps }

// --- helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func (api *apiHandlers) reload() {
	if err := api.d.Reg.Reload(api.d.Store); err != nil {
		slog.Warn("registry reload after mutation failed", "err", err)
	}
}

// --- providers ---

type providerPayload struct {
	ID              string   `json:"id"`
	Display         string   `json:"display"`
	BaseURL         string   `json:"base_url"`
	AuthKey         string   `json:"auth_key"`
	Model           string   `json:"model"`
	Weight          int      `json:"weight"`
	Tags            []string `json:"tags"`
	Enabled         bool     `json:"enabled"`
	ResponsesNative bool     `json:"responses_native"`
}

func (pp providerPayload) toConfig() config.Provider {
	return config.Provider{
		ID: pp.ID, Display: pp.Display, BaseURL: pp.BaseURL, AuthKey: pp.AuthKey,
		Model: pp.Model, Weight: pp.Weight, Tags: pp.Tags, Enabled: pp.Enabled,
		ResponsesNative: pp.ResponsesNative,
	}
}

func (api *apiHandlers) listProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, api.d.Reg.ListAllProviders())
}

func (api *apiHandlers) getProvider(w http.ResponseWriter, r *http.Request) {
	p := api.d.Reg.GetProvider(r.PathValue("id"))
	if p == nil {
		writeErr(w, 404, "not found")
		return
	}
	writeJSON(w, 200, p)
}

func (api *apiHandlers) createProvider(w http.ResponseWriter, r *http.Request) {
	var p providerPayload
	if err := decodeJSON(r, &p); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	if p.ID == "" || p.BaseURL == "" || p.Model == "" {
		writeErr(w, 400, "id, base_url and model are required")
		return
	}
	if p.Weight < 1 {
		p.Weight = 1
	}
	if err := api.d.Store.UpsertProvider(p.toConfig()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	api.reload()
	writeJSON(w, 201, p)
}

func (api *apiHandlers) updateProvider(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if api.d.Reg.GetProvider(id) == nil {
		writeErr(w, 404, "not found")
		return
	}
	var p providerPayload
	if err := decodeJSON(r, &p); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	p.ID = id
	if p.Weight < 1 {
		p.Weight = 1
	}
	if err := api.d.Store.UpsertProvider(p.toConfig()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	api.reload()
	writeJSON(w, 200, p)
}

func (api *apiHandlers) deleteProvider(w http.ResponseWriter, r *http.Request) {
	if err := api.d.Store.DeleteProvider(r.PathValue("id")); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	api.reload()
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// testProvider sends a minimal chat-completions through the proxy pipeline.
func (api *apiHandlers) testProvider(w http.ResponseWriter, r *http.Request) {
	p := api.d.Reg.GetProvider(r.PathValue("id"))
	if p == nil {
		writeErr(w, 404, "not found")
		return
	}
	body := `{"model":"` + p.ID + `","messages":[{"role":"user","content":"ping"}],"max_tokens":1,"stream":false}`
	res := api.runTest(body, registry.EndpointChatCompletions)
	writeJSON(w, 200, res)
}

func (api *apiHandlers) testCombo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if api.d.Reg.GetCombo(id) == nil {
		writeErr(w, 404, "not found")
		return
	}
	body := `{"model":"` + id + `","messages":[{"role":"user","content":"ping"}],"max_tokens":1,"stream":false}`
	res := api.runTest(body, registry.EndpointChatCompletions)
	writeJSON(w, 200, res)
}

type testResult struct {
	Status       int    `json:"status"`
	LatencyMs    int64  `json:"latency_ms"`
	ProviderUsed string `json:"provider_used,omitempty"`
	Error        string `json:"error,omitempty"`
}

func (api *apiHandlers) runTest(body string, endpoint string) testResult {
	req, err := http.NewRequest("POST", "http://internal"+proxyPathFor(endpoint), strings.NewReader(body))
	if err != nil {
		return testResult{Error: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	rec := newCapture()
	start := time.Now()
	api.d.Proxy.ServeHTTP(rec, req, endpoint)
	lat := time.Since(start).Milliseconds()
	res := testResult{Status: rec.status(), LatencyMs: lat, Error: rec.errorString()}
	// Extract provider_used from the log row just written — simpler: derive from response header
	// We can't easily know which upstream served, so skip for provider test; for combo test we log it async.
	return res
}

// listUpstreamModels queries {base_url}/models (joining without double /v1) and
// returns the model IDs so the SPA can populate a dropdown in Add Provider.
func (api *apiHandlers) listUpstreamModels(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BaseURL string `json:"base_url"`
		AuthKey string `json:"auth_key"`
	}
	if err := decodeJSON(r, &req); err != nil || req.BaseURL == "" {
		writeErr(w, 400, "base_url required")
		return
	}
	// Reuse the same version-aware join as the proxy so ".../v1" is not duplicated.
	url := proxy.BuildUpstreamURL(req.BaseURL, "/v1/models")
	httpReq, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.AuthKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.AuthKey)
	}
	httpReq.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		writeErr(w, 502, "upstream unreachable: "+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		writeErr(w, 502, fmt.Sprintf("upstream returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
		return
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err := json.Unmarshal(body, &parsed); err != nil {
		writeErr(w, 502, "upstream did not return a models list: "+err.Error())
		return
	}
	ids := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	writeJSON(w, 200, map[string]any{"models": ids})
}

func proxyPathFor(endpoint string) string {
	switch endpoint {
	case registry.EndpointResponses:
		return "/v1/responses"
	case registry.EndpointCompletions:
		return "/v1/completions"
	default:
		return "/v1/chat/completions"
	}
}

// capture implements http.ResponseWriter for in-memory test calls.
type capture struct {
	header http.Header
	code   int
	buf    []byte
}

func newCapture() *capture { return &capture{header: http.Header{}, code: 200} }
func (c *capture) Header() http.Header { return c.header }
func (c *capture) WriteHeader(code int) { c.code = code }
func (c *capture) Write(b []byte) (int, error) { c.buf = append(c.buf, b...); return len(b), nil }
func (c *capture) status() int { return c.code }
func (c *capture) errorString() string {
	if c.code < 400 {
		return ""
	}
	var e struct {
		Error struct{ Message string `json:"message"` } `json:"error"`
	}
	if json.Unmarshal(c.buf, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return string(c.buf)
}

// Flush exists so capture satisfies http.Flusher if needed.
func (c *capture) Flush() {}

// --- combos ---

type comboPayload struct {
	ID          string   `json:"id"`
	DisplayName string   `json:"display_name"`
	Rotation    string   `json:"rotation"`
	Members     []string `json:"members"`
	Enabled     bool     `json:"enabled"`
}

func (cp comboPayload) toConfig() config.Combo {
	return config.Combo{
		ID: cp.ID, DisplayName: cp.DisplayName, Rotation: config.RotationPolicy(cp.Rotation),
		Members: cp.Members, Enabled: cp.Enabled,
	}
}

func (api *apiHandlers) listCombos(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, api.d.Reg.ListAllCombos())
}

func (api *apiHandlers) createCombo(w http.ResponseWriter, r *http.Request) {
	var c comboPayload
	if err := decodeJSON(r, &c); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	if c.ID == "" {
		writeErr(w, 400, "id required")
		return
	}
	if c.Rotation == "" {
		c.Rotation = string(config.RoundRobin)
	}
	if err := api.d.Store.UpsertCombo(c.toConfig()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	api.reload()
	writeJSON(w, 201, c)
}

func (api *apiHandlers) updateCombo(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if api.d.Reg.GetCombo(id) == nil {
		writeErr(w, 404, "not found")
		return
	}
	var c comboPayload
	if err := decodeJSON(r, &c); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	c.ID = id
	if err := api.d.Store.UpsertCombo(c.toConfig()); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	api.reload()
	writeJSON(w, 200, c)
}

func (api *apiHandlers) deleteCombo(w http.ResponseWriter, r *http.Request) {
	if err := api.d.Store.DeleteCombo(r.PathValue("id")); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	api.reload()
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// --- logs ---

func (api *apiHandlers) listLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := config.LogFilter{
		ProviderID: q.Get("provider"),
		Endpoint:   q.Get("endpoint"),
		Limit:      atoi(q.Get("limit"), 50),
		Offset:     atoi(q.Get("offset"), 0),
	}
	if q.Get("errors_only") == "1" || q.Get("errors_only") == "true" {
		f.ErrorsOnly = true
	}
	if v := q.Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.Since = t.Unix()
		}
	}
	if v := q.Get("until"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			f.Until = t.Unix()
		}
	}
	entries, err := api.d.Store.QueryLogs(f)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	total, _ := api.d.Store.CountLogs(f)
	writeJSON(w, 200, map[string]any{"items": entries, "total": total, "limit": f.Limit, "offset": f.Offset})
}

func (api *apiHandlers) logsChart(w http.ResponseWriter, r *http.Request) {
	hours := atoi(r.URL.Query().Get("hours"), 24)
	data, err := api.d.Store.RequestsPerHour(hours)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, data)
}

func atoi(s string, fallback int) int {
	if s == "" {
		return fallback
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return fallback
	}
	return n
}

// --- settings ---

func (api *apiHandlers) getSettings(w http.ResponseWriter, r *http.Request) {
	all, err := api.d.Store.AllSettings()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// Ensure defaults present for the UI.
	for _, k := range []string{"health.cooldown", "health.error_codes", "log.retention_days"} {
		if _, ok := all[k]; !ok {
			all[k] = ""
		}
	}
	// Mask the API key display; we surface only a derived hint.
	all["_gateway_api_key_masked"] = mask(api.d.Env.APIKey)
	writeJSON(w, 200, all)
}

func mask(s string) string {
	if len(s) <= 8 {
		return strings.Repeat("•", len(s))
	}
	return s[:4] + strings.Repeat("•", len(s)-8) + s[len(s)-4:]
}

// endpointInfo returns the full public-facing v1 base URL for this gateway
// (scheme + host from the request) and the real API key, so the SPA can offer
// one-click copy. Gated by session auth — anyone reaching it is already an admin.
func (api *apiHandlers) endpointInfo(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = fwd
	}
	base := scheme + "://" + host + "/v1"
	writeJSON(w, 200, map[string]string{
		"base_url": base,
		"api_key":  api.d.Env.APIKey,
	})
}

func (api *apiHandlers) updateSettings(w http.ResponseWriter, r *http.Request) {
	var m map[string]string
	if err := decodeJSON(r, &m); err != nil {
		writeErr(w, 400, "invalid JSON")
		return
	}
	for k, v := range m {
		if err := api.d.Store.SetSetting(k, v); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	api.reload()
	writeJSON(w, 200, map[string]string{"status": "saved"})
}

// --- export/import ---

func (api *apiHandlers) export(w http.ResponseWriter, r *http.Request) {
	out, err := api.d.Store.ExportSQL()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/sql")
	w.Header().Set("Content-Disposition", "attachment; filename=gateway-export-"+time.Now().Format("2006-01-02")+".sql")
	_, _ = w.Write([]byte(out))
}

func (api *apiHandlers) importSQL(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		writeErr(w, 400, "could not read body")
		return
	}
	if err := api.d.Store.ImportSQL(string(body)); err != nil {
		if store.IsValidation(err) {
			writeErr(w, 400, err.Error())
		} else {
			writeErr(w, 500, err.Error())
		}
		return
	}
	api.reload()
	writeJSON(w, 200, map[string]string{"status": "imported"})
}

// --- health / overview ---

func (api *apiHandlers) health(w http.ResponseWriter, r *http.Request) {
	snaps := api.d.Reg.Health().Snapshot()
	// Enrich with display names.
	byID := map[string]*config.Provider{}
	for _, p := range api.d.Reg.ListAllProviders() {
		byID[p.ID] = p
	}
	type row struct {
		config.HealthSnapshot
		Display string `json:"display"`
		Enabled bool   `json:"enabled"`
	}
	out := []row{}
	// Include providers with no health record too.
	seen := map[string]bool{}
	for _, s := range snaps {
		seen[s.ProviderID] = true
		rw := row{HealthSnapshot: s}
		if p, ok := byID[s.ProviderID]; ok {
			rw.Display = p.Display
			rw.Enabled = p.Enabled
		}
		out = append(out, rw)
	}
	for _, p := range api.d.Reg.ListAllProviders() {
		if !seen[p.ID] {
			out = append(out, row{
				HealthSnapshot: config.HealthSnapshot{ProviderID: p.ID, Available: true},
				Display:        p.Display,
				Enabled:        p.Enabled,
			})
		}
	}
	writeJSON(w, 200, out)
}

func (api *apiHandlers) overview(w http.ResponseWriter, r *http.Request) {
	provs := api.d.Reg.ListAllProviders()
	combos := api.d.Reg.ListAllCombos()
	today, err := api.d.Store.CountLogsToday()
	if err != nil {
		slog.Warn("count today", "err", err)
	}
	enabledProvs := 0
	for _, p := range provs {
		if p.Enabled {
			enabledProvs++
		}
	}
	enabledCombos := 0
	for _, c := range combos {
		if c.Enabled {
			enabledCombos++
		}
	}
	writeJSON(w, 200, map[string]any{
		"providers_total":   len(provs),
		"providers_enabled": enabledProvs,
		"combos_total":      len(combos),
		"combos_enabled":    enabledCombos,
		"requests_today":    today,
	})
}

// --- static ---

// loginHTML is served on the login page.
const loginHTML = `<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>LLM Gateway — Login</title>
<style>body{font-family:system-ui,-apple-system,Segoe UI,Roboto,sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh;margin:0;background:#0b1220;color:#e6edf3}
.card{background:#0f1926;padding:28px;border-radius:10px;width:340px;box-shadow:0 6px 24px rgba(0,0,0,.4)}
h1{font-size:18px;margin:0 0 16px}input{width:100%;padding:10px;border-radius:6px;border:1px solid #22303f;background:#0b1220;color:#e6edf3;box-sizing:border-box}
button{margin-top:12px;width:100%;padding:10px;background:#2563eb;color:#fff;border:0;border-radius:6px;cursor:pointer}
.err{color:#f87171;margin-top:10px;font-size:13px;min-height:1em}</style></head>
<body><form class="card" onsubmit="return doLogin(event)"><h1>LLM Gateway</h1>
<input type="password" id="pw" placeholder="Dashboard password" autofocus autocomplete="current-password">
<button type="submit">Sign in</button><div class="err" id="err"></div></form>
<script>async function doLogin(e){e.preventDefault();const pw=document.getElementById('pw').value;const res=await fetch('/dashboard/api/login',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({password:pw})});if(res.ok){location.href='/dashboard/'}else{document.getElementById('err').textContent='Invalid password'}}</script>
</body></html>`

func serveLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, loginHTML)
}

func serveSPA(w http.ResponseWriter, r *http.Request) {
	assets, err := staticFiles()
	if err != nil {
		http.Error(w, "assets unavailable", 500)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/dashboard")
	if path == "" || path == "/" {
		path = "index.html"
	} else {
		path = strings.TrimPrefix(path, "/")
	}
	b, err := fs.ReadFile(assets, path)
	if err != nil {
		// SPA fallback.
		b, err = fs.ReadFile(assets, "index.html")
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
	}
	switch {
	case strings.HasSuffix(path, ".js"):
		w.Header().Set("Content-Type", "application/javascript")
	case strings.HasSuffix(path, ".css"):
		w.Header().Set("Content-Type", "text/css")
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	_, _ = w.Write(b)
}
