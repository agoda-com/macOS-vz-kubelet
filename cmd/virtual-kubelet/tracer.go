package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/trace"
	"github.com/virtual-kubelet/virtual-kubelet/trace/opentelemetry"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.7.0"
)

func initTracerProvider(service string, sampler sdktrace.Sampler, caCertPath string) (*sdktrace.TracerProvider, error) {
	ctx := context.Background()
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return nil, nil
	}

	// Configure the OTLP exporter to send traces to
	options := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(endpoint),
		// otlptracehttp.WithTLSClientConfig(&tls.Config{
		// 	InsecureSkipVerify: true,
		// }),
	}
	if os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true" {
		options = append(options, otlptracehttp.WithInsecure())
	} else {
		options = append(options, otlptracehttp.WithTLSClientConfig(createTLSConfig(caCertPath)))
	}
	client := otlptracehttp.NewClient(options...)

	exporter, err := otlptrace.New(ctx, client)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			// the service name used to display traces in backends
			semconv.ServiceNameKey.String(service),
			attribute.String("exporter", "otlp"),
		),
	)
	if err != nil {
		return nil, err
	}

	// Create and set the trace provider
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	otel.SetTracerProvider(tp)

	return tp, nil
}

// CreateTLSConfig prepares a TLS configuration with the given CA certificate path.
func createTLSConfig(caCertPath string) *tls.Config {
	// Load CA cert
	caCert, err := os.ReadFile(caCertPath)
	if err != nil {
		log.Fatalf("failed to load CA cert: %v", err)
	}

	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	// Create TLS configuration with the CA cert pool.
	tlsConfig := &tls.Config{
		RootCAs:            caCertPool,
		InsecureSkipVerify: true,
	}

	return tlsConfig
}

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

func configureTracing(service string, rate string, caCertPath string) error {
	sampler, err := determineSampler(rate)
	if err != nil {
		return fmt.Errorf("determining sampler: %w", err)
	}

	_, err = initTracerProvider(service, sampler, caCertPath)
	if err != nil {
		return fmt.Errorf("initializing tracer provider: %w", err)
	}

	trace.T = opentelemetry.Adapter{}

	return nil
}
