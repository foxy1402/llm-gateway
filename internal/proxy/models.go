package proxy

import (
	"encoding/json"
	"net/http"
	"time"

	"llm-gateway/internal/registry"
)

// ModelsHandler responds to GET /v1/models.
func ModelsHandler(reg *registry.Registry) http.HandlerFunc {
	type modelObj struct {
		ID          string `json:"id"`
		Object      string `json:"object"`
		Created     int64  `json:"created"`
		OwnedBy     string `json:"owned_by"`
		Description string `json:"description,omitempty"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
		created := time.Now().Unix()
		out := []modelObj{}
		for _, c := range reg.ListEnabledCombos() {
			mo := modelObj{ID: c.ID, Object: "model", Created: created, OwnedBy: "gateway"}
			if c.DisplayName != "" {
				mo.Description = c.DisplayName
			}
			out = append(out, mo)
		}
		for _, p := range reg.ListEnabledProviders() {
			mo := modelObj{ID: p.ID, Object: "model", Created: created, OwnedBy: "gateway"}
			if p.Display != "" {
				mo.Description = p.Display
			}
			out = append(out, mo)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": out})
	}
}
