package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"price-crawler/services/user-api/internal/config"
	"price-crawler/services/user-api/internal/messaging"
	"price-crawler/services/user-api/internal/observability"
)

func run(ctx context.Context, cfg config.Config, logger *slog.Logger) error {
	broker, err := messaging.NewPublisher(cfg.RabbitMQURL)
	if err != nil {
		return errors.New("user-api RabbitMQ connection failed")
	}
	defer func() {
		if err := broker.Close(); err != nil {
			logger.Warn("RabbitMQ close failed", "error", err)
		}
	}()
	// Business routes are added with their corresponding workflow tasks.
	pool := observability.NewWorkerPool(cfg.WorkerConcurrency)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"alive"}`))
	})
	server := &http.Server{Addr: cfg.HTTPAddr, ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
