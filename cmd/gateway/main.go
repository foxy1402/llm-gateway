package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"llm-gateway/internal/config"
	"llm-gateway/internal/middleware"
	"llm-gateway/internal/proxy"
	"llm-gateway/internal/registry"
	"llm-gateway/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gateway:", err)
		os.Exit(1)
	}
}

func run() error {
	env, err := config.LoadEnv()
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.Level(env.SlogLevel())})))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	st, err := store.Open(ctx, env.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	reg := registry.New()
	if err := reg.Reload(st); err != nil {
		return fmt.Errorf("load registry: %w", err)
	}
	px := proxy.New(reg, st, env.RequestTimeout)
	if len(env.ModelAliases) > 0 {
		px.SetModelAliases(env.ModelAliases)
		slog.Info("model aliases installed", "count", len(env.ModelAliases))
	}
	px.SetMaxBodyBytes(int64(env.MaxRequestBodyMB) << 20)
	px.SetMaxAccountsPerProviderCap(env.MaxAccountAttemptsPerProvider)

	a := &app{env: env, store: st, reg: reg, px: px}

	// Nightly log pruning: respects the log.retention_days setting (default 30).
	stopPrune := startLogPruner(st)

	mux := http.NewServeMux()

	registerRoutes(mux, a)

	handler := middleware.Recover(middleware.Logger(mux))

	srv := &http.Server{
		Addr:              env.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", env.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case sig := <-sigCh:
		slog.Info("shutdown signal", "signal", sig.String())
	case err := <-errCh:
		return err
	}

	stopPrune()

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	return srv.Shutdown(shutdownCtx)
}

// startLogPruner ticks hourly and prunes request logs older than the retention
// setting (default 30 days). Returns a stop function.
func startLogPruner(st *store.Store) func() {
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		prune := func() {
			daysStr, err := st.GetSetting("log.retention_days")
			days := 30
			if err == nil && daysStr != "" {
				if n, err := parsePositiveInt(daysStr); err == nil {
					days = n
				}
			}
			if removed, err := st.PruneLogs(days); err != nil {
				slog.Warn("log prune failed", "err", err)
			} else if removed > 0 {
				slog.Info("pruned old request logs", "removed", removed, "retention_days", days)
			}
		}
		prune() // initial pass
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				prune()
			}
		}
	}()
	return func() { close(stop) }
}

func parsePositiveInt(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 {
		return 0, fmt.Errorf("not a positive int: %q", s)
	}
	return n, nil
}
