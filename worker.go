package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"time"

	kafka "github.com/segmentio/kafka-go"
)

// RunWorker polls the outbox table and publishes unpublished rows
// to Kafka in batches. It runs until ctx is cancelled.
func RunWorker(ctx context.Context, db *sql.DB, writer *kafka.Writer, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := tick(ctx, db, writer); err != nil {
				slog.Error("outbox tick failed", "error", err)
			}
		}
	}
}

func tick(ctx context.Context, db *sql.DB, writer *kafka.Writer) error {
	rows, err := ListUnpublished(ctx, db, 50)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}

	// Build Kafka messages from outbox rows.
	msgs := make([]kafka.Message, 0, len(rows))
	for _, row := range rows {
		env, _ := json.Marshal(map[string]any{
			"event_id":    row.ID,
			"occurred_at": row.CreatedAt,
			"payload":     json.RawMessage(row.Payload),
		})

		msg := kafka.Message{
			Topic: row.Topic,
			Key:   []byte(row.Key),
			Value: env,
		}

		// Restore OTel trace context from the JSONB column and
		// inject it into Kafka headers (see trace.go).
		injectTraceContext(ctx, row.TraceContext, &msg)

		msgs = append(msgs, msg)
	}

	// Publish the batch. If this fails, rows stay unpublished
	// and will be retried on the next tick.
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := writer.WriteMessages(writeCtx, msgs...); err != nil {
		return err
	}

	// Mark rows as published. If marking fails, the row will be
	// re-published next tick. This is safe because consumers
	// must be idempotent.
	for _, row := range rows {
		if err := MarkPublished(ctx, db, row.ID); err != nil {
			slog.Error("mark published failed (will re-deliver)",
				"id", row.ID, "error", err)
		}
	}
	return nil
}
