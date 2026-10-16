// Package telemetry sets up OpenTelemetry traces, metrics and logs (docs/observability.md).
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Enabled reports whether an OTLP endpoint is configured. Without one (unit tests, CI),
// nothing is exported and the service only writes its JSON logs.
func Enabled() bool {
	return os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != ""
}

// Setup installs global trace, metric and log providers that export over OTLP. Everything
// comes from the standard OTEL_* environment: endpoint, protocol, service name, resource
// attributes, sampler and export interval. The returned function flushes and stops the
// exporters; call it during shutdown, after the server has drained.
func Setup(ctx context.Context) (shutdown func(context.Context) error, err error) {
	if !Enabled() {
		return func(context.Context) error { return nil }, nil
	}

	res, err := resource.New(ctx, resource.WithFromEnv(), resource.WithTelemetrySDK())
	if err != nil {
		return nil, fmt.Errorf("telemetry resource: %w", err)
	}

	traceExporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("trace exporter: %w", err)
	}
	// Default sampler: parent-based, always on (OTEL_TRACES_SAMPLER can change it).
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
	)

	metricExporter, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("metric exporter: %w", err), tracerProvider.Shutdown(ctx))
	}
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
		sdkmetric.WithResource(res),
	)

	logExporter, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("log exporter: %w", err),
			tracerProvider.Shutdown(ctx), meterProvider.Shutdown(ctx))
	}
	loggerProvider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
		sdklog.WithResource(res),
	)

	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	otel.SetLoggerProvider(loggerProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))

	// Go runtime metrics (memory, goroutines, GC).
	if err := runtime.Start(); err != nil {
		return nil, fmt.Errorf("runtime metrics: %w", err)
	}

	return func(ctx context.Context) error {
		return errors.Join(
			tracerProvider.Shutdown(ctx),
			meterProvider.Shutdown(ctx),
			loggerProvider.Shutdown(ctx),
		)
	}, nil
}
