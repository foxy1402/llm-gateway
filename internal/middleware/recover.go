package middleware

import (
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recover returns 500 on panic and logs the stack.
func Recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic", "err", rec, "path", r.URL.Path, "stack", string(debug.Stack()))
				// Headers may already be committed; best-effort.
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
