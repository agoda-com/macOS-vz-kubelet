package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/trace"
	"github.com/virtual-kubelet/virtual-kubelet/trace/opentelemetry"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.7.0"
)

func initTracerProvider(ctx context.Context, service string, sampler sdktrace.Sampler, attributes ...attribute.KeyValue) error {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return nil
	}

	// Configure the OTLP exporter to send traces to
	options := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(endpoint),
	}
	if os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true" {
		options = append(options, otlptracegrpc.WithInsecure())
	}
	client := otlptracegrpc.NewClient(options...)

	exporter, err := otlptrace.New(ctx, client)
	if err != nil {
		return err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			// the service name used to display traces in backends
			semconv.ServiceNameKey.String(service),
			attribute.String("exporter", "otlp"),
		),
		resource.WithAttributes(attributes...),
	)
	if err != nil {
		return err
	}

	// Create and set the trace provider
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))

	return nil
}

// nolint: ireturn
func determineSampler(rate string) (sdktrace.Sampler, error) {
	switch strings.ToLower(rate) {
	case "", "always":
		return sdktrace.AlwaysSample(), nil
	case "never":
		return sdktrace.NeverSample(), nil
	default:
		rateFloat, err := strconv.ParseFloat(rate, 64)
		if err != nil {
			return nil, errdefs.AsInvalidInput(fmt.Errorf("parsing sample rate: %w", err))
		}
		if rateFloat < 0 || rateFloat > 100 {
			return nil, errdefs.AsInvalidInput(fmt.Errorf("sample rate must be between 0 and 100"))
		}
		return sdktrace.TraceIDRatioBased(rateFloat / 100), nil
	}
}

func configureTracing(ctx context.Context, service string, rate string, attributes ...attribute.KeyValue) error {
	sampler, err := determineSampler(rate)
	if err != nil {
		return fmt.Errorf("determining sampler: %w", err)
	}

	if err := initTracerProvider(ctx, service, sampler, attributes...); err != nil {
		return fmt.Errorf("initializing tracer provider: %w", err)
	}

	trace.T = opentelemetry.Adapter{}

	return nil
}
