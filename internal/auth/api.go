package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// APIKey returns middleware that enforces `Authorization: Bearer <key>`.
func APIKey(key string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := r.Header.Get("Authorization")
			if !strings.HasPrefix(got, "Bearer ") {
				writeOpenAIError(w, http.StatusUnauthorized, "missing or malformed Authorization header", "invalid_request_error")
				return
			}
			token := strings.TrimPrefix(got, "Bearer ")
			if subtle.ConstantTimeCompare([]byte(token), []byte(key)) != 1 {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid API key", "invalid_api_key")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
