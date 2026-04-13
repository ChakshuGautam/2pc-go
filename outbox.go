package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// OutboxRow is a row in the outbox table.
type OutboxRow struct {
	ID          uuid.UUID
	Topic       string
	Key         string
	Payload     json.RawMessage
	PublishedAt *time.Time
	CreatedAt   time.Time
}

// Enqueue inserts an outbox row inside the caller's transaction.
// Because it shares the same tx as the business write, the two
// are committed (or rolled back) atomically — this is the core
// guarantee of the transactional outbox pattern.
func Enqueue(ctx context.Context, tx *sql.Tx, topic, key string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO outbox (id, topic, key, payload)
		VALUES ($1, $2, $3, $4)`,
		uuid.New(), topic, key, data,
	)
	return err
}

// ListUnpublished fetches up to `limit` outbox rows that haven't
// been published yet, ordered oldest-first.
func ListUnpublished(ctx context.Context, db *sql.DB, limit int) ([]OutboxRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, topic, key, payload, created_at
		FROM outbox
		WHERE published_at IS NULL
		ORDER BY created_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []OutboxRow
	for rows.Next() {
		var r OutboxRow
		if err := rows.Scan(&r.ID, &r.Topic, &r.Key, &r.Payload, &r.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// MarkPublished sets published_at on a row. If this fails, the
// worker will re-publish the row on the next tick — consumers
// must be idempotent.
func MarkPublished(ctx context.Context, db *sql.DB, id uuid.UUID) error {
	_, err := db.ExecContext(ctx, `
		UPDATE outbox SET published_at = now() WHERE id = $1`, id)
	return err
}
