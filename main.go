package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
	kafka "github.com/segmentio/kafka-go"
)

func main() {
	dsn := env("DATABASE_URL", "postgres://demo:demo@localhost:5432/demo?sslmode=disable")
	broker := env("KAFKA_BROKER", "localhost:29092")

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		slog.Error("db open", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	writer := &kafka.Writer{
		Addr:                   kafka.TCP(broker),
		BatchTimeout:           50 * time.Millisecond,
		WriteTimeout:           10 * time.Second,
		RequiredAcks:           kafka.RequireOne,
		AllowAutoTopicCreation: true,
	}
	defer writer.Close()

	// Start the outbox worker in the background.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go RunWorker(ctx, db, writer, 500*time.Millisecond)

	// A single endpoint: POST /orders creates an order and
	// enqueues an event in the same database transaction.
	http.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Item string `json:"item"`
			Qty  int    `json:"qty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		orderID := uuid.New()

		// --- THE 2PC BOUNDARY ---
		tx, err := db.BeginTx(r.Context(), nil)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer tx.Rollback() //nolint:errcheck

		// Business write.
		_, err = tx.ExecContext(r.Context(),
			`INSERT INTO orders (id, item, qty) VALUES ($1, $2, $3)`,
			orderID, req.Item, req.Qty)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// Outbox write — same transaction, same commit.
		err = Enqueue(r.Context(), tx, "orders.events", orderID.String(), map[string]any{
			"event":    "order.created",
			"order_id": orderID,
			"item":     req.Item,
			"qty":      req.Qty,
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if err := tx.Commit(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// --- END 2PC BOUNDARY ---

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"order_id": orderID})
	})

	slog.Info("listening", "addr", ":8090")
	srv := &http.Server{Addr: ":8090"}
	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			slog.Error("server", "error", err)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	srv.Shutdown(context.Background()) //nolint:errcheck
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func init() {
	// Minimal text handler — keep the demo output clean.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	fmt.Fprintln(os.Stderr) // blank line for readability
}
