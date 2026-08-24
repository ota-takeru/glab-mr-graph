package oteltrace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestNormalizeEndpoint(t *testing.T) {
	tests := map[string]string{
		"":                        DefaultEndpoint,
		"true":                    DefaultEndpoint,
		"http://localhost:4318":   "http://localhost:4318/v1/traces",
		"http://collector/custom": "http://collector/custom",
	}
	for input, want := range tests {
		got, err := normalizeEndpoint(input)
		if err != nil {
			t.Fatalf("normalizeEndpoint(%q): %v", input, err)
		}
		if got != want {
			t.Errorf("normalizeEndpoint(%q) = %q, want %q", input, got, want)
		}
	}
	if _, err := normalizeEndpoint("localhost:4318"); err == nil {
		t.Error("normalizeEndpoint accepted a URL without a scheme")
	}
}

func TestExporterEndpointReturnsNormalizedEndpoint(t *testing.T) {
	exporter, err := New("http://localhost:4318")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := exporter.Endpoint(), "http://localhost:4318/v1/traces"; got != want {
		t.Errorf("Endpoint() = %q, want %q", got, want)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := exporter.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestExporterPreservesParentChildRelationship(t *testing.T) {
	received := make(chan map[string]any, 1)
	exporter, err := New("http://collector.test")
	if err != nil {
		t.Fatal(err)
	}
	exporter.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		received <- body
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	ctx, root := exporter.Start(context.Background(), "GET /api/v1/graph", SpanServer, nil)
	_, child := exporter.Start(ctx, "glab api request", SpanClient, Attributes{"gitlab.operation.name": "search"})
	child.End(nil, Attributes{"process.exit.code": 0})
	root.End(nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := exporter.Close(ctx); err != nil {
		t.Fatal(err)
	}

	payload := <-received
	spans := payloadSpans(t, payload)
	if len(spans) != 2 {
		t.Fatalf("exported spans = %d, want 2", len(spans))
	}
	childSpan, rootSpan := spans[0], spans[1]
	if childSpan["traceId"] != rootSpan["traceId"] {
		t.Errorf("trace IDs differ: child %v, root %v", childSpan["traceId"], rootSpan["traceId"])
	}
	if childSpan["parentSpanId"] != rootSpan["spanId"] {
		t.Errorf("child parentSpanId = %v, root spanId = %v", childSpan["parentSpanId"], rootSpan["spanId"])
	}
	encoded, _ := json.Marshal(payload)
	if strings.Contains(string(encoded), "command_args") || strings.Contains(string(encoded), "owner=orangain") {
		t.Errorf("payload contains private command arguments: %s", encoded)
	}
}

func TestExporterDoesNotExportErrorMessage(t *testing.T) {
	const secret = "private-token-branch-secret-endpoint"
	received := make(chan map[string]any, 1)
	exporter, err := New("http://collector.test")
	if err != nil {
		t.Fatal(err)
	}
	exporter.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		received <- body
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	_, span := exporter.Start(context.Background(), "glab api request", SpanClient, Attributes{"gitlab.operation.name": "detail"})
	span.End(errors.New(secret), nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := exporter.Close(ctx); err != nil {
		t.Fatal(err)
	}

	payload := <-received
	encoded, _ := json.Marshal(payload)
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("exported payload contains the error message: %s", encoded)
	}
	spans := payloadSpans(t, payload)
	if len(spans) != 1 {
		t.Fatalf("exported spans = %d, want 1", len(spans))
	}
	status, ok := spans[0]["status"].(map[string]any)
	if !ok {
		t.Fatalf("status = %#v, want object", spans[0]["status"])
	}
	if got := status["code"]; got != float64(2) {
		t.Errorf("status code = %#v, want 2", got)
	}
	if _, ok := status["message"]; ok {
		t.Error("error status unexpectedly contains a message")
	}
	attrs := spans[0]["attributes"]
	if !strings.Contains(fmt.Sprint(attrs), "error.type") || !strings.Contains(fmt.Sprint(attrs), "_OTHER") {
		t.Errorf("error.type attribute = %#v, want fixed _OTHER", attrs)
	}
}

func payloadSpans(t *testing.T, payload map[string]any) []map[string]any {
	t.Helper()
	resources := payload["resourceSpans"].([]any)
	scopes := resources[0].(map[string]any)["scopeSpans"].([]any)
	rawSpans := scopes[0].(map[string]any)["spans"].([]any)
	spans := make([]map[string]any, 0, len(rawSpans))
	for _, span := range rawSpans {
		spans = append(spans, span.(map[string]any))
	}
	return spans
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
