package telemetry

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
)

func TestRegistryExportsOnlyAllowlistedLabels(t *testing.T) {
	r := NewRegistry()
	r.Add(context.Background(), executioncontract.CounterRetries, 1, executioncontract.MetricLabels{executioncontract.LabelReason: "timeout"})
	r.Add(context.Background(), executioncontract.CounterRetries, 10, executioncontract.MetricLabels{executioncontract.LabelReason: "job-123"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	body := w.Body.String()
	if !strings.Contains(body, `dbplatform_job_retries_total{reason="timeout"} 1`) {
		t.Fatalf("expected retry metric, got: %s", body)
	}
	if strings.Contains(body, "job-123") {
		t.Fatal("unbounded label value was exported")
	}
}

func TestRegistryExportsHistogram(t *testing.T) {
	r := NewRegistry()
	r.Observe(context.Background(), executioncontract.HistogramDuration, .2, executioncontract.MetricLabels{executioncontract.LabelAction: "apply", executioncontract.LabelResult: "success"})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(w.Body.String(), `dbplatform_migration_duration_seconds_count{action="apply",result="success"} 1`) {
		t.Fatalf("histogram count missing: %s", w.Body.String())
	}
}
