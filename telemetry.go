package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otelglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

func initTelemetry(ctx context.Context, serviceName, endpoint string, insecure bool) (func(context.Context) error, *slog.Logger, error) {
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName(serviceName),
			attribute.String("service.version", "dev"),
		),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create resource: %w", err)
	}

	traceOpts := []otlptracegrpc.Option{}
	logOpts := []otlploggrpc.Option{}
	if endpoint != "" {
		traceOpts = append(traceOpts, otlptracegrpc.WithEndpoint(endpoint))
		logOpts = append(logOpts, otlploggrpc.WithEndpoint(endpoint))
	}
	if insecure {
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
		logOpts = append(logOpts, otlploggrpc.WithInsecure())
	}

	traceExporter, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, nil, fmt.Errorf("create trace exporter: %w", err)
	}
	traceProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(traceProvider)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	logExporter, err := otlploggrpc.New(ctx, logOpts...)
	if err != nil {
		_ = traceProvider.Shutdown(ctx)
		return nil, nil, fmt.Errorf("create log exporter: %w", err)
	}
	logProvider := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
		sdklog.WithResource(res),
	)
	otelglobal.SetLoggerProvider(logProvider)

	logger := slog.New(otelslog.NewHandler("sonos-exporter", otelslog.WithLoggerProvider(logProvider)))
	slog.SetDefault(logger)

	shutdown := func(shutdownCtx context.Context) error {
		var traceErr, logErr error
		logErr = logProvider.Shutdown(shutdownCtx)
		traceErr = traceProvider.Shutdown(shutdownCtx)
		if traceErr != nil || logErr != nil {
			return fmt.Errorf("telemetry shutdown trace=%v logs=%v", traceErr, logErr)
		}
		return nil
	}

	return shutdown, logger, nil
}

func fallbackLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

func withTimeoutContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, 5*time.Second)
}
