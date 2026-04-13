# You're Losing Events: Reliable Event Publishing with Postgres + Kafka in ~200 Lines of Go

If your service writes to Postgres and then publishes to Kafka, you're probably losing events. Not often — maybe once a week, maybe once a month. Enough to never notice in dev, and enough to rot your data in production.

This repo contains a complete, runnable implementation of the **transactional outbox pattern** in ~200 lines of Go. No frameworks, no libraries beyond `database/sql` and `kafka-go`. Clone it, run `docker compose up`, and see it work.

> **Two versions**: The first commit has the core outbox pattern. The second commit adds [OTel trace correlation](#aside-trace-correlation-across-the-async-boundary) across the async boundary. Check the [git log](../../commits/main) to see the diff.

## The Two Broken Patterns

Almost every backend I've reviewed does one of these:

### Pattern 1: Commit, Then Publish

```go
tx.Commit()                          // Data is saved
producer.Publish("order.created")    // Process dies here — event lost forever
```

Your database has the order. Kafka never got the event. Downstream services never learn about it. No retry will fix this because your code already moved on — it committed, returned 201 to the client, and the goroutine exited.

**Failure mode**: Lost events. Downstream systems silently drift out of sync. You discover it weeks later when a customer complains.

### Pattern 2: Publish, Then Commit

```go
producer.Publish("order.created")    // Event is published
tx.Commit()                          // Commit fails — but event is already out there
```

Now Kafka has an event for an order that doesn't exist in your database. Downstream services process it, charge the customer, send a confirmation — for a phantom order.

**Failure mode**: Ghost events. Downstream systems act on data that was never persisted. Worse than lost events because it's actively wrong.

### Why Both Patterns Fail

The problem is fundamental: **you cannot atomically write to two different systems**. Postgres and Kafka are separate. There's always a window between the two writes where a crash, timeout, or network partition will leave them inconsistent.

You need both writes to succeed or both to fail. You need atomicity. You need a transaction that spans the boundary.

## The Fix: Transactional Outbox

The insight is simple: **don't write to two systems. Write to one.**

Instead of publishing to Kafka in your request handler, write the event to an **outbox table** in your Postgres database — in the same transaction as your business data. A background worker polls the outbox and publishes to Kafka separately.

```
┌─────────────────────────────────────┐
│         Single Postgres TX          │
│                                     │
│  INSERT INTO orders (...)           │
│  INSERT INTO outbox (...)  <-- event│
│                                     │
│  COMMIT  <-- both or neither        │
└─────────────────────────────────────┘
            |
            |  (later, async)
            v
┌─────────────────────────────────────┐
│        Background Worker            │
│                                     │
│  SELECT * FROM outbox               │
│    WHERE published_at IS NULL       │
│                                     │
│  -> publish to Kafka                │
│  -> UPDATE outbox SET published_at  │
└─────────────────────────────────────┘
```

Because both writes are in the same database transaction, Postgres guarantees atomicity. If the transaction commits, both the order and the event exist. If it rolls back, neither exists. There is no window for inconsistency.

The worker runs asynchronously — if it crashes, unpublished rows stay in the outbox and get picked up when it restarts. **No events are ever lost.**

## The Implementation

The entire thing is three files plus a schema. Let's walk through each.

### 1. The Schema (18 lines)

```sql
-- schema.sql

CREATE TABLE IF NOT EXISTS orders (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    item       TEXT        NOT NULL,
    qty        INT         NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS outbox (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    topic         TEXT        NOT NULL,
    key           TEXT        NOT NULL,
    payload       JSONB       NOT NULL,
    published_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_outbox_unpublished
    ON outbox (created_at) WHERE published_at IS NULL;
```

The `orders` table is your business data — substitute whatever you have. The `outbox` table is generic: it holds a topic, a routing key, a JSON payload, and a `published_at` timestamp that starts NULL. The partial index on `published_at IS NULL` ensures the worker's poll query stays fast no matter how many published rows accumulate.

### 2. The Outbox Repository (72 lines)

```go
// outbox.go — three functions, three guarantees

func Enqueue(ctx context.Context, tx *sql.Tx, topic, key string, payload any) error
func ListUnpublished(ctx context.Context, db *sql.DB, limit int) ([]OutboxRow, error)
func MarkPublished(ctx context.Context, db *sql.DB, id uuid.UUID) error
```

**`Enqueue`** takes the caller's `*sql.Tx` — not a `*sql.DB`. This is the entire trick. Because it receives the same transaction handle that the caller used for their business write, both writes commit or rollback together. One line of SQL, one critical invariant.

**`ListUnpublished`** fetches rows where `published_at IS NULL`, ordered by `created_at`. The worker calls this every tick.

**`MarkPublished`** sets `published_at = now()`. If this fails after Kafka publish succeeds, the row will be re-published on the next tick. That's fine — **consumers must be idempotent anyway** (Kafka's at-least-once delivery already requires this).

[See full source](outbox.go)

### 3. The Worker (72 lines)

```go
// worker.go — poll, batch, publish, mark

func RunWorker(ctx context.Context, db *sql.DB, writer *kafka.Writer, interval time.Duration)
```

The worker is a goroutine with a ticker. Every 500ms it:

1. **Polls** the outbox for up to 50 unpublished rows
2. **Builds** Kafka messages from them (topic, key, and a JSON envelope)
3. **Publishes** the batch to Kafka via `writer.WriteMessages`
4. **Marks** each row as published

If step 3 fails, the rows stay unpublished and get retried next tick. If step 4 fails for a row, it gets re-published next tick (idempotent consumers handle this). If the worker crashes entirely, rows accumulate in the outbox and drain when it restarts.

The key insight: **the worker doesn't need to be reliable. The outbox table is the source of truth.** The worker is just a pump that moves data from Postgres to Kafka. If it falls behind, events are delayed but never lost.

[See full source](worker.go)

### 4. The Application (120 lines)

```go
// main.go — wire it up

func main() {
    db, _ := sql.Open("postgres", dsn)
    writer := &kafka.Writer{Addr: kafka.TCP(broker), ...}

    // Start worker in background
    go RunWorker(ctx, db, writer, 500*time.Millisecond)

    // HTTP handler
    http.HandleFunc("POST /orders", func(w http.ResponseWriter, r *http.Request) {
        tx, _ := db.BeginTx(r.Context(), nil)
        defer tx.Rollback()

        // Business write
        tx.ExecContext(r.Context(),
            `INSERT INTO orders (id, item, qty) VALUES ($1, $2, $3)`,
            orderID, req.Item, req.Qty)

        // Outbox write — same transaction
        Enqueue(r.Context(), tx, "orders.events", orderID.String(), payload)

        tx.Commit()
    })
}
```

That's it. The handler creates a transaction, does its business write, calls `Enqueue` with the same `tx`, and commits. Three lines of transactional code give you reliable event publishing.

[See full source](main.go)

## The Guarantees

What does this pattern actually give you?

**No lost events.** If the transaction commits, the outbox row exists. The worker will eventually publish it. If the transaction rolls back, neither the business data nor the outbox row exists. There's no state where you have data without a corresponding event.

**No ghost events.** Events are only published from committed outbox rows. If the business write fails, the outbox row is rolled back too. Downstream systems never see events for data that doesn't exist.

**Crash recovery.** If the service crashes after committing but before publishing, the outbox row is durable in Postgres. When the worker restarts, it picks up where it left off.

**Ordering.** Events are published in `created_at` order (the outbox table's partial index). Within a single service, events are ordered. Cross-service ordering requires additional coordination (but you probably don't need it).

**Idempotent re-delivery.** The worker may publish a row more than once if `MarkPublished` fails after Kafka accepts the message. This is the same guarantee Kafka already provides — at-least-once delivery. Your consumers need to handle duplicates regardless.

## What This Is NOT

This is **not** a distributed two-phase commit protocol (2PC in the traditional database sense). There's no prepare/commit across two resource managers. What it *is* is a way to get **atomic write + reliable async publish** using a single database as the coordinator. Some people call this the "transactional outbox" pattern; others call it "2PC" colloquially because it achieves the atomicity guarantee that motivates 2PC in the first place. The distinction matters if you're reading academic papers. It doesn't matter if you're trying to stop losing events.

## Running It

```bash
git clone git@github.com:ChakshuGautam/2pc-go.git
cd 2pc-go

# Start Postgres, Redpanda, and Redpanda Console
docker compose up -d

# Wait for services to be healthy
docker compose ps  # all should show "healthy"

# Run the app
go run .

# In another terminal — create an order
curl -X POST http://localhost:8090/orders \
  -H 'Content-Type: application/json' \
  -d '{"item": "widget", "qty": 3}'

# Check Redpanda Console at http://localhost:8080
# Topic "orders.events" should have the message
```

To see crash recovery in action:

```bash
# Create an order
curl -X POST http://localhost:8090/orders \
  -H 'Content-Type: application/json' \
  -d '{"item": "gadget", "qty": 1}'

# Kill the app immediately (Ctrl+C or kill -9)
# The outbox row is committed but may not be published yet.

# Restart the app
go run .

# The worker picks up the unpublished row and publishes it.
# Check Redpanda Console — the event is there.
```

## Aside: Trace Correlation Across the Async Boundary

> Added in the [second commit](../../commit/HEAD) — check the diff to see exactly what changed.

One problem with the outbox pattern: you lose observability. The HTTP request that created the order finishes and its trace ends. The worker publishes the event seconds later in a completely separate context. In your tracing tool (Jaeger, Tempo, etc.), these show up as two unrelated traces. You can't follow the causal chain from "user placed order" to "Kafka message published" to "downstream service processed it".

The fix is ~70 lines in [`trace.go`](trace.go). Three changes:

1. **Schema**: add a `trace_context JSONB` column to the outbox table
2. **Enqueue**: capture the current OTel context (W3C `traceparent` headers) and store it in the new column
3. **Worker**: restore the saved context and inject it into Kafka message headers before publishing

```
HTTP request (trace: abc123)
  +-- INSERT orders         <-- span
  +-- INSERT outbox         <-- span (stores traceparent: abc123)

... 500ms later ...

Outbox worker
  +-- outbox.publish        <-- span (parent: abc123, from JSONB)
      +-- Kafka headers carry traceparent: abc123

Downstream consumer
  +-- kafka.consume         <-- span (parent: abc123, from headers)
  +-- process order         <-- span
```

One trace, from HTTP request through async publish to downstream processing. The 500ms gap between commit and publish is visible in the trace timeline, which is actually useful — it tells you how far behind the worker is.

This is optional. The core outbox pattern works without it. But if you're running in production, you'll want it.

## Adapting to Your Service

To use this pattern in your own service:

1. **Add the outbox table** — copy `schema.sql`, add it to your migrations
2. **Copy `outbox.go`** — the three functions are generic; nothing to change
3. **Copy `worker.go`** — adjust batch size and poll interval to taste
4. **Copy `trace.go`** — if you use OpenTelemetry (optional)
5. **In your handlers** — pass `tx` to `Enqueue` inside your existing transactions
6. **In `main.go`** — start the worker goroutine and create a `kafka.Writer`

The pattern works with any language and database that supports transactions. The Go code here is a reference implementation, but the idea is the same in Python/SQLAlchemy, Java/Spring, Rust/sqlx, or anything else. The outbox table is just a table. The worker is just a loop.

## Further Reading

- [Transactional Outbox Pattern](https://microservices.io/patterns/data/transactional-outbox.html) — Chris Richardson's canonical description
- [Reliable Microservices Data Exchange with the Outbox Pattern](https://debezium.io/blog/2019/02/19/reliable-microservices-data-exchange-with-the-outbox-pattern/) — Debezium's CDC-based alternative (no polling worker; Postgres WAL instead)
- [Designing Data-Intensive Applications](https://dataintensive.net/) — Martin Kleppmann's book covers exactly-once semantics and the theory behind this
