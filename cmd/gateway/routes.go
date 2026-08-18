package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"llm-gateway/internal/auth"
	"llm-gateway/internal/config"
	"llm-gateway/internal/dashboard"
	"llm-gateway/internal/proxy"
	"llm-gateway/internal/registry"
	"llm-gateway/internal/store"
)

type app struct {
	env   *config.Env
	store *store.Store
	reg   *registry.Registry
	px    *proxy.Proxy
}

// registerRoutes wires all HTTP routes.
func registerRoutes(mux *http.ServeMux, a *app) {
	apiKey := auth.APIKey(a.env.APIKey)

	// Public.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ready := len(a.reg.ListEnabledProviders()) > 0
		status := http.StatusOK
		body := map[string]any{"status": "ready"}
		if !ready {
			status = http.StatusServiceUnavailable
			body = map[string]any{"status": "no providers"}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	})

	// OpenAI-compatible API.
	mux.Handle("POST /v1/chat/completions", apiKey(handleProxy(a, registry.EndpointChatCompletions)))
	mux.Handle("POST /v1/completions", apiKey(handleProxy(a, registry.EndpointCompletions)))
	mux.Handle("POST /v1/responses", apiKey(handleProxy(a, registry.EndpointResponses)))
	mux.Handle("POST /v1/embeddings", apiKey(handleProxy(a, registry.EndpointEmbeddings)))
	mux.Handle("GET /v1/models", apiKey(proxy.ModelsHandler(a.reg)))

	// Admin (API-key).
	mux.Handle("GET /admin/status", apiKey(http.HandlerFunc(a.adminStatus)))
	mux.Handle("POST /admin/reload", apiKey(http.HandlerFunc(a.adminReload)))
	mux.Handle("GET /admin/export", apiKey(http.HandlerFunc(a.adminExport)))
	mux.Handle("POST /admin/import", apiKey(http.HandlerFunc(a.adminImport)))

	// Web dashboard (session cookie auth).
	dashDeps := &dashboard.Deps{
		Store: a.store,
		Reg:   a.reg,
		Proxy: a.px,
		Auth:  auth.NewDashboard(a.env.DashboardPassword, a.env.DashboardSecret),
		Env:   a.env,
	}
	// Strict fail-to-ban on the login endpoint (BAN_MAXFAIL=0 disables).
	if a.env.BanCfg.MaxFail > 0 {
		saved, err := a.store.GetSetting(auth.BanSettingsKey)
		if err != nil {
			slog.Warn("fail-ban state load failed, starting fresh", "err", err)
		}
		dashDeps.FailBan = auth.NewFailBan(a.env.BanCfg, saved, func(state string) error {
			return a.store.SetSetting(auth.BanSettingsKey, state)
		})
	}
	dashboard.Mount(mux, dashDeps)
}

func handleProxy(a *app, endpoint string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.px.ServeHTTP(w, r, endpoint)
	})
}

func (a *app) adminStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"providers": a.reg.ListAllProviders(),
		"combos":    a.reg.ListAllCombos(),
		"health":    a.reg.Health().Snapshot(),
	})
}

func (a *app) adminReload(w http.ResponseWriter, r *http.Request) {
	if err := a.reg.Reload(a.store); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "reloaded"})
}

func (a *app) adminExport(w http.ResponseWriter, r *http.Request) {
	out, err := a.store.ExportSQL()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	filename := "gateway-export-" + time.Now().Format("2006-01-02") + ".sql"
	w.Header().Set("Content-Type", "application/sql")
	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	_, _ = w.Write([]byte(out))
}

func (a *app) adminImport(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		http.Error(w, "could not read body", http.StatusBadRequest)
		return
	}
	if err := a.store.ImportSQL(string(body)); err != nil {
		status := http.StatusInternalServerError
		if store.IsValidation(err) {
			status = http.StatusBadRequest
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if err := a.reg.Reload(a.store); err != nil {
		slog.Warn("reload after import failed", "err", err)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "imported"})
}
