package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"price-crawler/services/user-api/internal/config"
	"price-crawler/services/user-api/internal/observability"
)

func checkIssuer(ctx context.Context, issuer, jwksURL string) error {
	base := strings.TrimRight(issuer, "/")
	if jwksURL != "" {
		var ok bool
		base, ok = strings.CutSuffix(jwksURL, "/protocol/openid-connect/certs")
		if !ok {
			return errors.New("identity discovery address is invalid")
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/.well-known/openid-configuration", nil)
	if err != nil {
		return errors.New("identity discovery address is invalid")
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		return errors.New("identity discovery unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return errors.New("identity discovery unavailable")
	}
	var document struct {
		Issuer string `json:"issuer"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&document); err != nil || document.Issuer != strings.TrimRight(issuer, "/") {
		return errors.New("identity discovery issuer mismatch or invalid response")
	}
	return nil
}

func run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	broker := &observability.Broker{URL: cfg.RabbitMQURL, InitialDelay: cfg.ReconnectInitial, MaxDelay: cfg.ReconnectMax}
	brokerCtx, stopBroker := context.WithCancel(ctx)
	defer stopBroker()
	firstReady := make(chan struct{})
	identityReady := make(chan struct{})
	identityDone := make(chan struct{})
	identity := &observability.Dependency{InitialDelay: cfg.ReconnectInitial, MaxDelay: cfg.ReconnectMax, Check: func(probe context.Context) error { return checkIssuer(probe, cfg.OIDCIssuer, cfg.OIDCJWKSURL) }}
	go func() { defer close(identityDone); identity.Run(brokerCtx, identityReady) }()
	brokerDone := make(chan struct{})
	go func() { defer close(brokerDone); broker.Run(brokerCtx, firstReady) }()
	defer func() { stopBroker(); <-brokerDone; <-identityDone }()
	startup, cancelStartup := context.WithTimeout(ctx, cfg.ReadinessTimeout)
	defer cancelStartup()
	if err := broker.WaitReady(startup, firstReady); err != nil {
		return err
	}
	select {
	case <-identityReady:
	case <-startup.Done():
		return fmt.Errorf("user-api identity readiness timeout: %v", identity.LastError())
	}

	// Business consumers are added with their corresponding workflow tasks.
	pool := observability.NewWorkerPool(cfg.WorkerConcurrency)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !broker.Ready() || !identity.Ready() {
			http.Error(w, "required dependency unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"alive"}`))
	})
	server := &http.Server{Addr: cfg.HTTPAddr, ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" && (!broker.Ready() || !identity.Ready()) {
			http.Error(w, "required dependency unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := pool.Run(r.Context(), func(context.Context) error { mux.ServeHTTP(w, r); return nil }); err != nil {
			http.Error(w, "service is shutting down", http.StatusServiceUnavailable)
		}
	})}
	serverErr := make(chan error, 1)
	go func() {
		logger.Info("user-api process started", "addr", cfg.HTTPAddr, "worker_concurrency", cfg.WorkerConcurrency)
		err := server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		serverErr <- err
	}()
	select {
	case err := <-serverErr:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := observability.ShutdownHTTP(shutdown, server); err != nil {
		return fmt.Errorf("user-api HTTP shutdown failed: %w", err)
	}
	return <-serverErr
}

func main() {
	logger := observability.Logger()
	cfg, err := config.Load()
	if err != nil {
		logger.Error("user-api configuration failed", "error", err)
		os.Exit(1)
	}
	ctx, stop := observability.ShutdownContext(context.Background())
	defer stop()
	if err := run(ctx, cfg, logger); err != nil {
		logger.Error("user-api stopped", "error", err)
		os.Exit(1)
	}
}
