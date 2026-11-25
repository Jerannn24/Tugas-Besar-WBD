package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Span struct {
	TraceID      string
	SpanID       string
	ParentSpanID string
	Name         string
	Kind         int
	StartedAt    time.Time
	EndedAt      time.Time
	Attributes   map[string]string
	Failed       bool
}

type otlpRequest struct {
	ResourceSpans []resourceSpans `json:"resourceSpans"`
}
type resourceSpans struct {
	Resource   resource     `json:"resource"`
	ScopeSpans []scopeSpans `json:"scopeSpans"`
}
type resource struct {
	Attributes []attribute `json:"attributes"`
}
type scopeSpans struct {
	Scope map[string]string `json:"scope"`
	Spans []otlpSpan        `json:"spans"`
}
type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []attribute    `json:"attributes,omitempty"`
	Status            map[string]any `json:"status"`
}
type attribute struct {
	Key   string            `json:"key"`
	Value map[string]string `json:"value"`
}

var (
	otlpClient    = &http.Client{Timeout: 1200 * time.Millisecond}
	spanQueue     chan Span
	spanQueueOnce sync.Once
)

func ExportSpan(_ context.Context, span Span) {
	if otlpEndpoint() == "" || span.TraceID == "" || span.SpanID == "" {
		return
	}
	spanQueueOnce.Do(func() {
		spanQueue = make(chan Span, 2048)
		go runSpanExporter(spanQueue)
	})
	select {
	case spanQueue <- span:
	default:
		// Observability must never block application work. A full queue drops
		// spans rather than creating unbounded goroutines or request latency.
	}
}

func otlpEndpoint() string {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	if endpoint == "" {
		base := strings.TrimRight(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"), "/")
		if base != "" {
			endpoint = base + "/v1/traces"
		}
	}
	return endpoint
}

func runSpanExporter(queue <-chan Span) {
	for first := range queue {
		batch := []Span{first}
		timer := time.NewTimer(100 * time.Millisecond)
	collect:
		for len(batch) < 64 {
			select {
			case span := <-queue:
				batch = append(batch, span)
			case <-timer.C:
				break collect
			}
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		sendSpanBatch(batch)
	}
}

func sendSpanBatch(spans []Span) {
	endpoint := otlpEndpoint()
	if endpoint == "" || len(spans) == 0 {
		return
	}
	encoded := make([]otlpSpan, 0, len(spans))
	for _, span := range spans {
		attrs := make([]attribute, 0, len(span.Attributes))
		for key, value := range span.Attributes {
			attrs = append(attrs, attribute{Key: key, Value: map[string]string{"stringValue": value}})
		}
		status := map[string]any{"code": 1}
		if span.Failed {
			status = map[string]any{"code": 2, "message": "operation failed"}
		}
		encoded = append(encoded, otlpSpan{TraceID: span.TraceID, SpanID: span.SpanID, ParentSpanID: span.ParentSpanID, Name: span.Name, Kind: span.Kind, StartTimeUnixNano: itoaNano(span.StartedAt), EndTimeUnixNano: itoaNano(span.EndedAt), Attributes: attrs, Status: status})
	}
	payload := otlpRequest{ResourceSpans: []resourceSpans{{Resource: resource{Attributes: []attribute{{Key: "service.name", Value: map[string]string{"stringValue": envOr("OTEL_SERVICE_NAME", "dbplatform")}}}}, ScopeSpans: []scopeSpans{{Scope: map[string]string{"name": "dbplatform.telemetry"}, Spans: encoded}}}}}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if headers := os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"); headers != "" {
		for _, part := range strings.Split(headers, ",") {
			kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
			if len(kv) == 2 {
				req.Header.Set(kv[0], kv[1])
			}
		}
	}
	resp, err := otlpClient.Do(req)
	if err == nil {
		_ = resp.Body.Close()
	}
}

func itoaNano(t time.Time) string { return strconv.FormatInt(t.UnixNano(), 10) }
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
