package telemetry

import (
	"log/slog"
	"os"
	"sync"
)

var (
	defaultOnce        sync.Once
	defaultLogger      *Logger
	defaultRegistry    *Registry
	defaultQueryTracer *QueryTracer
)

func DefaultLogger() *Logger {
	defaultOnce.Do(func() {
		level := slog.LevelInfo
		if os.Getenv("LOG_LEVEL") == "debug" {
			level = slog.LevelDebug
		}
		defaultLogger = NewLogger(os.Stdout, level)
		defaultRegistry = NewRegistry()
		defaultQueryTracer = &QueryTracer{Logger: defaultLogger}
	})
	return defaultLogger
}

func DefaultRegistry() *Registry       { DefaultLogger(); return defaultRegistry }
func DefaultQueryTracer() *QueryTracer { DefaultLogger(); return defaultQueryTracer }
