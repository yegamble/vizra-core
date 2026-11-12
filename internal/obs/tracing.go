package obs

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// spanAttributeAllowlist is every span attribute key that may leave the
// process (VZ-OPS-005). It is an ALLOWLIST: otelhttp also records url.full,
// url.query, url.path, client.address, user_agent.original and request
// headers, and a new otelhttp release may add more; none of those is exported
// unless named here.
var spanAttributeAllowlist = map[string]bool{
	"http.request.method":       true,
	"http.response.status_code": true,
	"http.route":                true,
	"network.protocol.version":  true,
	"url.scheme":                true,
	"server.port":               true,
	"error.type":                true,
}

// DeniedAttribute reports whether a span attribute must not be exported: its
// key is not allowlisted, names a secret, or its value carries a credential
// form Redact recognises.
func DeniedAttribute(key, value string) bool {
	if !spanAttributeAllowlist[key] || secretKeys[strings.ToLower(key)] {
		return true
	}
	return Redact(value) != value
}

// SetupTracing installs a global OTLP/HTTP tracer provider with a batch span
// processor when endpoint is non-empty. endpoint is the OTLP base URL; the
// signal path /v1/traces is appended, as the OTel spec defines for
// OTEL_EXPORTER_OTLP_ENDPOINT. With endpoint empty it changes nothing
// and starts no goroutine. The returned shutdown flushes within ctx's deadline.
func SetupTracing(ctx context.Context, endpoint, serviceName string) (func(context.Context) error, error) {
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(strings.TrimRight(endpoint, "/")+"/v1/traces"))
	if err != nil {
		return nil, err
	}
	res := resource.NewSchemaless(semconv.ServiceName(serviceName))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(filteringExporter{exp}),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// filteringExporter drops every span attribute DeniedAttribute refuses before
// the span reaches the wire.
type filteringExporter struct{ sdktrace.SpanExporter }

func (f filteringExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		out[i] = filteredSpan{s}
	}
	return f.SpanExporter.ExportSpans(ctx, out)
}

type filteredSpan struct{ sdktrace.ReadOnlySpan }

func (s filteredSpan) Attributes() []attribute.KeyValue {
	in := s.ReadOnlySpan.Attributes()
	out := make([]attribute.KeyValue, 0, len(in))
	for _, kv := range in {
		if !DeniedAttribute(string(kv.Key), kv.Value.Emit()) {
			out = append(out, kv)
		}
	}
	return out
}
