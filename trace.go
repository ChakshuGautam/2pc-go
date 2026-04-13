package main

import (
	"context"
	"encoding/json"

	kafka "github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// extractTraceContext serialises the current OTel context (W3C
// traceparent + tracestate) into JSON suitable for the outbox
// table's trace_context JSONB column.
func extractTraceContext(ctx context.Context) json.RawMessage {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) == 0 {
		return nil
	}
	b, _ := json.Marshal(carrier)
	return b
}

// injectTraceContext restores a saved trace context from JSONB and
// writes it into Kafka message headers so downstream consumers
// continue the same distributed trace.
func injectTraceContext(ctx context.Context, traceJSON *json.RawMessage, msg *kafka.Message) {
	if traceJSON == nil || len(*traceJSON) == 0 {
		return
	}
	carrier := propagation.MapCarrier{}
	if err := json.Unmarshal(*traceJSON, &carrier); err != nil {
		return
	}
	parentCtx := otel.GetTextMapPropagator().Extract(ctx, carrier)
	otel.GetTextMapPropagator().Inject(parentCtx, &messageCarrier{msg: msg})
}

// messageCarrier implements propagation.TextMapCarrier over
// Kafka message headers.
type messageCarrier struct{ msg *kafka.Message }

func (c *messageCarrier) Get(key string) string {
	for _, h := range c.msg.Headers {
		if h.Key == key {
			return string(h.Value)
		}
	}
	return ""
}

func (c *messageCarrier) Set(key, val string) {
	for i, h := range c.msg.Headers {
		if h.Key == key {
			c.msg.Headers[i].Value = []byte(val)
			return
		}
	}
	c.msg.Headers = append(c.msg.Headers, kafka.Header{Key: key, Value: []byte(val)})
}

func (c *messageCarrier) Keys() []string {
	out := make([]string, len(c.msg.Headers))
	for i, h := range c.msg.Headers {
		out[i] = h.Key
	}
	return out
}
