package telemetry

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/Jerannn24/Tugas-Besar-WBD.git/executioncontract"
)

type metricKind uint8

const (
	gaugeKind metricKind = iota
	counterKind
	histogramKind
)

type metricSample struct {
	value   float64
	buckets []uint64
	count   uint64
	sum     float64
}
type metricDescriptor struct {
	kind    metricKind
	labels  []executioncontract.MetricLabel
	buckets []float64
}

type Registry struct {
	mu          sync.RWMutex
	samples     map[string]*metricSample
	descriptors map[string]metricDescriptor
}

func NewRegistry() *Registry {
	r := &Registry{samples: make(map[string]*metricSample), descriptors: make(map[string]metricDescriptor)}
	r.descriptors[string(executioncontract.GaugeJobs)] = metricDescriptor{kind: gaugeKind, labels: []executioncontract.MetricLabel{executioncontract.LabelStatus}}
	r.descriptors[string(executioncontract.GaugePoolConnections)] = metricDescriptor{kind: gaugeKind, labels: []executioncontract.MetricLabel{executioncontract.LabelState}}
	r.descriptors[string(executioncontract.CounterRetries)] = metricDescriptor{kind: counterKind, labels: []executioncontract.MetricLabel{executioncontract.LabelReason}}
	r.descriptors[string(executioncontract.CounterBatches)] = metricDescriptor{kind: counterKind, labels: []executioncontract.MetricLabel{executioncontract.LabelDirection}}
	r.descriptors[string(executioncontract.CounterRows)] = metricDescriptor{kind: counterKind, labels: []executioncontract.MetricLabel{executioncontract.LabelDirection}}
	r.descriptors[string(executioncontract.HistogramQueueWait)] = metricDescriptor{kind: histogramKind, labels: []executioncontract.MetricLabel{executioncontract.LabelMode}, buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}}
	r.descriptors[string(executioncontract.HistogramLockWait)] = metricDescriptor{kind: histogramKind, labels: []executioncontract.MetricLabel{executioncontract.LabelMode}, buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5}}
	r.descriptors[string(executioncontract.HistogramDuration)] = metricDescriptor{kind: histogramKind, labels: []executioncontract.MetricLabel{executioncontract.LabelAction, executioncontract.LabelResult}, buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300}}
	r.descriptors[string(executioncontract.HistogramPoolWait)] = metricDescriptor{kind: histogramKind, labels: []executioncontract.MetricLabel{executioncontract.LabelResult}, buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5}}
	return r
}

func (r *Registry) Set(_ context.Context, name executioncontract.Gauge, value float64, labels executioncontract.MetricLabels) {
	r.update(string(name), gaugeKind, value, labels)
}
func (r *Registry) Add(_ context.Context, name executioncontract.Counter, value float64, labels executioncontract.MetricLabels) {
	r.update(string(name), counterKind, value, labels)
}
func (r *Registry) Observe(_ context.Context, name executioncontract.Histogram, value float64, labels executioncontract.MetricLabels) {
	r.update(string(name), histogramKind, value, labels)
}

func (r *Registry) update(name string, kind metricKind, value float64, labels executioncontract.MetricLabels) {
	if r == nil {
		return
	}
	desc, ok := r.descriptors[name]
	if !ok || desc.kind != kind {
		return
	}
	key, normalized, ok := metricKey(name, desc, labels)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	sample := r.samples[key]
	if sample == nil {
		sample = &metricSample{}
		if kind == histogramKind {
			sample.buckets = make([]uint64, len(desc.buckets))
		}
		r.samples[key] = sample
	}
	switch kind {
	case gaugeKind:
		sample.value = value
	case counterKind:
		if value >= 0 {
			sample.value += value
		}
	case histogramKind:
		if value >= 0 {
			sample.count++
			sample.sum += value
			for i, b := range desc.buckets {
				if value <= b {
					sample.buckets[i]++
				}
			}
		}
	}
	_ = normalized
}

func metricKey(name string, desc metricDescriptor, labels executioncontract.MetricLabels) (string, map[string]string, bool) {
	if len(labels) != len(desc.labels) {
		return "", nil, false
	}
	values := make(map[string]string, len(desc.labels))
	var parts []string
	for _, label := range desc.labels {
		value, ok := labels[label]
		if !ok || !validMetricValue(label, value) {
			return "", nil, false
		}
		values[string(label)] = string(value)
		parts = append(parts, string(label)+`="`+string(value)+`"`)
	}
	return name + "{" + strings.Join(parts, ",") + "}", values, true
}

func validMetricValue(label executioncontract.MetricLabel, value executioncontract.MetricValue) bool {
	switch label {
	case executioncontract.LabelStatus:
		return value == "queued" || value == "running" || value == "retry_wait" || value == "succeeded" || value == "failed" || value == "partial" || value == "partially_reverted" || value == "unknown"
	case executioncontract.LabelReason:
		return value == "transient" || value == "timeout" || value == "worker_lost" || value == "database_busy" || value == "unknown_outcome"
	case executioncontract.LabelMode:
		return value == executioncontract.ValueStandard || value == executioncontract.ValueBatch
	case executioncontract.LabelAction:
		return value == "apply" || value == "rollback" || value == "forward" || value == "resume" || value == "compensate"
	case executioncontract.LabelResult:
		return value == executioncontract.ValueSuccess || value == executioncontract.ValueFailure
	case executioncontract.LabelDirection:
		return value == executioncontract.ValueUp || value == executioncontract.ValueDown
	case executioncontract.LabelState:
		return value == executioncontract.ValueActive || value == executioncontract.ValueIdle
	default:
		return false
	}
}

func (r *Registry) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write(r.render())
}

func (r *Registry) render() []byte {
	var out strings.Builder
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.descriptors))
	for name := range r.descriptors {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		desc := r.descriptors[name]
		typ := "gauge"
		if desc.kind == counterKind {
			typ = "counter"
		}
		if desc.kind == histogramKind {
			typ = "histogram"
		}
		fmt.Fprintf(&out, "# TYPE %s %s\n", name, typ)
		keys := make([]string, 0)
		for key := range r.samples {
			if strings.HasPrefix(key, name+"{") {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for _, key := range keys {
			sample := r.samples[key]
			switch desc.kind {
			case gaugeKind, counterKind:
				fmt.Fprintf(&out, "%s %g\n", key, sample.value)
			case histogramKind:
				labels := strings.TrimSuffix(strings.TrimPrefix(key, name+"{"), "}")
				for i, bucket := range desc.buckets {
					suffix := labels
					if suffix != "" {
						suffix += ","
					}
					fmt.Fprintf(&out, "%s_bucket{%s le=\"%g\"} %d\n", name, suffix, bucket, sample.buckets[i])
				}
				suffix := labels
				if suffix != "" {
					suffix += ","
				}
				fmt.Fprintf(&out, "%s_bucket{%s le=\"+Inf\"} %d\n", name, suffix, sample.count)
				fmt.Fprintf(&out, "%s_sum{%s} %g\n", name, labels, sample.sum)
				fmt.Fprintf(&out, "%s_count{%s} %d\n", name, labels, sample.count)
			}
		}
	}
	return []byte(out.String())
}

var _ executioncontract.Telemetry = (*Registry)(nil)
