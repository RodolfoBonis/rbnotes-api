// Package telemetry centraliza logging estruturado (slog) e OpenTelemetry.
//
// Privacidade: o logger aplica logredact.ReplaceAttr para redigir segredos por chave; ainda assim,
// NUNCA despeje structs de credencial inteiras — dê a elas um LogValue()/String() que redige.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/RodolfoBonis/rbnotes-api/internal/platform/config"
	"github.com/RodolfoBonis/rbnotes-api/internal/platform/logredact"
)

// NewLogger cria um logger JSON estruturado no nível configurado, com redação de segredos.
func NewLogger(cfg config.Telemetry) *slog.Logger {
	level := parseLevel(cfg.LogLevel)
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: logredact.ReplaceAttr,
	})
	return slog.New(handler).With(slog.String("service", cfg.ServiceName))
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Shutdown libera os recursos de telemetria. Seguro chamar mesmo que nada tenha sido inicializado.
type Shutdown func(context.Context) error

// SetupTracing configura o tracer provider OTLP quando habilitado; no-op quando desabilitado.
func SetupTracing(ctx context.Context, cfg config.Telemetry) (Shutdown, error) {
	if !cfg.OTelEnabled {
		return func(context.Context) error { return nil }, nil
	}
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.OTLPEndpoint),
		otlptracegrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("otlp trace exporter: %w", err)
	}
	res, err := newResource(ctx, cfg)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{},
	))
	return tp.Shutdown, nil
}

// SetupMetrics configura o meter provider OTLP quando habilitado; no-op quando desabilitado.
// (As métricas de produto são Prometheus — ver internal/platform/metrics.)
func SetupMetrics(ctx context.Context, cfg config.Telemetry) (Shutdown, error) {
	if !cfg.OTelEnabled {
		return func(context.Context) error { return nil }, nil
	}
	exporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(cfg.OTLPEndpoint),
		otlpmetricgrpc.WithInsecure(),
	)
	if err != nil {
		return nil, fmt.Errorf("otlp metric exporter: %w", err)
	}
	res, err := newResource(ctx, cfg)
	if err != nil {
		return nil, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)
	return mp.Shutdown, nil
}

func newResource(ctx context.Context, cfg config.Telemetry) (*resource.Resource, error) {
	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(cfg.ServiceName)))
	if err != nil {
		return nil, fmt.Errorf("otel resource: %w", err)
	}
	return res, nil
}
