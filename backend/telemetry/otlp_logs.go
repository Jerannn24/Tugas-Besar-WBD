package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type otlpLogRequest struct {
	ResourceLogs []otlpResourceLogs `json:"resourceLogs"`
}

type otlpResourceLogs struct {
	Resource  resource        `json:"resource"`
	ScopeLogs []otlpScopeLogs `json:"scopeLogs"`
}

type otlpScopeLogs struct {
	Scope      map[string]string `json:"scope"`
	LogRecords []otlpLogRecord   `json:"logRecords"`
}

type otlpLogRecord struct {
	TimeUnixNano         string         `json:"timeUnixNano"`
	ObservedTimeUnixNano string         `json:"observedTimeUnixNano"`
	SeverityNumber       int            `json:"severityNumber"`
	SeverityText         string         `json:"severityText"`
	Body                 attributeValue `json:"body"`
	Attributes           []attribute    `json:"attributes,omitempty"`
	TraceID              string         `json:"traceId,omitempty"`
	SpanID               string         `json:"spanId,omitempty"`
}

type attributeValue struct {
	StringValue string `json:"stringValue"`
}

var (
	logQueue     chan otlpLogRecord
	logQueueOnce sync.Once
)

func ExportLogRecord(ctx context.Context, level slog.Level, event string, attrs []slog.Attr) {
	if otlpLogsEndpoint() == "" {
		return
	}
	record := otlpLogRecord{
		TimeUnixNano:         strconv.FormatInt(time.Now().UnixNano(), 10),
		ObservedTimeUnixNano: strconv.FormatInt(time.Now().UnixNano(), 10),
		SeverityNumber:       otlpSeverity(level),
		SeverityText:         level.String(),
		Body:                 attributeValue{StringValue: event},
	}
	if trace, ok := TraceFromContext(ctx); ok {
		record.TraceID = trace.TraceID
		record.SpanID = trace.SpanID
	}
	for _, attr := range attrs {
		if attr.Equal(slog.Attr{}) {
			continue
		}
		record.Attributes = append(record.Attributes, attribute{
			Key:   attr.Key,
			Value: map[string]string{"stringValue": attr.Value.Resolve().String()},
		})
	}
	logQueueOnce.Do(func() {
		logQueue = make(chan otlpLogRecord, 2048)
		go runLogExporter(logQueue)
	})
	select {
	case logQueue <- record:
	default:
		// Drop telemetry under backpressure rather than blocking request work.
	}
}

func otlpSeverity(level slog.Level) int {
	switch {
	case level >= slog.LevelError:
		return 17
	case level >= slog.LevelWarn:
		return 13
	case level >= slog.LevelInfo:
		return 9
	default:
		return 5
	}
}

func otlpLogsEndpoint() string {
	if endpoint := os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"); endpoint != "" {
		return endpoint
	}
	base := strings.TrimRight(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"), "/")
	if base == "" {
		return ""
	}
	return base + "/v1/logs"
}

func runLogExporter(queue <-chan otlpLogRecord) {
	for first := range queue {
		batch := []otlpLogRecord{first}
		timer := time.NewTimer(100 * time.Millisecond)
	collect:
		for len(batch) < 64 {
			select {
			case record := <-queue:
				batch = append(batch, record)
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
		sendLogBatch(batch)
	}
}

func sendLogBatch(records []otlpLogRecord) {
	endpoint := otlpLogsEndpoint()
	if endpoint == "" || len(records) == 0 {
		return
	}
	payload := otlpLogRequest{
		ResourceLogs: []otlpResourceLogs{{
			Resource: resource{Attributes: []attribute{{
				Key:   "service.name",
				Value: map[string]string{"stringValue": envOr("OTEL_SERVICE_NAME", "dbplatform")},
			}}},
			ScopeLogs: []otlpScopeLogs{{
				Scope:      map[string]string{"name": "dbplatform.telemetry"},
				LogRecords: records,
			}},
		}},
	}
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
