package telemetry

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/logger"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
)

var (
	tp *sdktrace.TracerProvider
	lp *log.LoggerProvider
	once sync.Once
)

// Init initializes OpenTelemetry SDK based on the provided configuration.
func Init(ctx context.Context, cfg config.TelemetryConfig) (func(), error) {
	if !cfg.Enabled {
		return func() {}, nil
	}

	var traceExporter *otlptrace.Exporter
	var logExporter log.Exporter
	var err error

	protocol := strings.ToLower(cfg.Protocol)
	if protocol == "" {
		protocol = "http"
	}

	switch protocol {
	case "grpc":
		traceExporter, err = otlptracegrpc.New(ctx,
			otlptracegrpc.WithEndpoint(cfg.Endpoint),
			otlptracegrpc.WithHeaders(cfg.Headers),
			otlptracegrpc.WithInsecure(),
		)
		if err == nil {
			logExporter, err = otlploggrpc.New(ctx,
				otlploggrpc.WithEndpoint(cfg.Endpoint),
				otlploggrpc.WithHeaders(cfg.Headers),
				otlploggrpc.WithInsecure(),
			)
		}
	case "http":
		traceExporter, err = otlptracehttp.New(ctx,
			otlptracehttp.WithEndpoint(cfg.Endpoint),
			otlptracehttp.WithHeaders(cfg.Headers),
			otlptracehttp.WithInsecure(),
		)
		if err == nil {
			logExporter, err = otlploghttp.New(ctx,
				otlploghttp.WithEndpoint(cfg.Endpoint),
				otlploghttp.WithHeaders(cfg.Headers),
				otlploghttp.WithInsecure(),
			)
		}
	default:
		return nil, fmt.Errorf("unsupported telemetry protocol: %s", protocol)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to create exporters: %w", err)
	}

	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = "picoclaw"
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(serviceName),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create resource: %w", err)
	}

	// Trace Provider
	tp = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)

	// Log Provider
	lp = log.NewLoggerProvider(
		log.WithProcessor(log.NewBatchProcessor(logExporter)),
		log.WithResource(res),
	)
	global.SetLoggerProvider(lp)

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.WarnCF("telemetry", "OpenTelemetry export error", map[string]any{"error": err.Error()})
	}))

	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tp.Shutdown(ctx); err != nil {
			logger.ErrorCF("telemetry", "Failed to shutdown TracerProvider", map[string]any{"error": err.Error()})
		}
		if err := lp.Shutdown(ctx); err != nil {
			logger.ErrorCF("telemetry", "Failed to shutdown LoggerProvider", map[string]any{"error": err.Error()})
		}
	}

	logger.InfoCF("telemetry", "OpenTelemetry initialized (Traces + Logs)", map[string]any{
		"protocol": protocol,
		"endpoint": cfg.Endpoint,
		"service":  serviceName,
	})

	return shutdown, nil
}

// GetTracer returns the global tracer for the application.
func GetTracer() trace.Tracer {
	return otel.Tracer("picoclaw")
}

// WithPanicRecovery records a panic in the current span if one occurs.
// Should be used with defer: defer telemetry.WithPanicRecovery(ctx)
func WithPanicRecovery(ctx context.Context) {
	if r := recover(); r != nil {
		span := trace.SpanFromContext(ctx)
		if span.IsRecording() {
			err, ok := r.(error)
			if !ok {
				err = fmt.Errorf("%v", r)
			}
			span.RecordError(err, trace.WithStackTrace(true))
			span.SetStatus(codes.Error, "panic recovered")
		}
		// Re-panic to let the global handler (logger.InitPanic) log it to file
		panic(r)
	}
}
