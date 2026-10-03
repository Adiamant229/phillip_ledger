package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Adiamant229/phillip_ledger/backend/internal/handler"
	"github.com/Adiamant229/phillip_ledger/backend/internal/repo"
	"github.com/Adiamant229/phillip_ledger/backend/internal/service"
)

func main() {
	dsn := env("DATABASE_URL", "postgres://ledger:ledger@localhost:5432/ledger?sslmode=disable")
	addr := ":" + env("PORT", "8080")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := repo.Open(ctx, dsn)
	if err != nil {
		log.Fatalf("connect to database: %v", err)
	}
	defer store.Close()
	if err := store.Migrate(ctx); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler.New(service.New(store)).Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Printf("ledger API listening on %s", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
