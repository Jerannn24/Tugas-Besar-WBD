
package executioncontract

import "context"

type TraceContext struct {
	Traceparent string
	Tracestate  string
}

type Gauge string
type Counter string
type Histogram string
type MetricLabel string
type MetricValue string

const (
	GaugeJobs            Gauge = "dbplatform_jobs"
	GaugePoolConnections Gauge = "dbplatform_pool_connections"

	CounterRetries  Counter = "dbplatform_job_retries_total"
	CounterBatches  Counter = "dbplatform_batches_committed_total"
	CounterRows     Counter = "dbplatform_rows_committed_total"

	HistogramQueueWait Histogram = "dbplatform_queue_wait_seconds"
	HistogramLockWait  Histogram = "dbplatform_lock_wait_seconds"
	HistogramDuration  Histogram = "dbplatform_migration_duration_seconds"
	HistogramPoolWait  Histogram = "dbplatform_pool_wait_seconds"

	LabelStatus    MetricLabel = "status"
	LabelReason    MetricLabel = "reason"
	LabelMode      MetricLabel = "mode"
	LabelAction    MetricLabel = "action"
	LabelResult    MetricLabel = "result"
	LabelDirection MetricLabel = "direction"
	LabelState     MetricLabel = "state"

	ValueStandard MetricValue = "standard"
	ValueBatch    MetricValue = "batch"
	ValueUp       MetricValue = "up"
	ValueDown     MetricValue = "down"
	ValueActive   MetricValue = "active"
	ValueIdle     MetricValue = "idle"
	ValueSuccess  MetricValue = "success"
	ValueFailure  MetricValue = "failure"
)

type MetricLabels map[MetricLabel]MetricValue

type Telemetry interface {
	Set(
		ctx context.Context,
		metric Gauge,
		value float64,
		labels MetricLabels,
	)

	Add(
		ctx context.Context,
		metric Counter,
		value float64,
		labels MetricLabels,
	)

	Observe(
		ctx context.Context,
		metric Histogram,
		value float64,
		labels MetricLabels,
	)
}
