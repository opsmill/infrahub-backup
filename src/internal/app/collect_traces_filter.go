package app

import (
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// tracesAttributesJSON is the allow-list of the trace attributes kept in the
// bundle. The same file lists the keys removed on purpose, so the end-to-end
// test can fail on a key that is in neither list (FR-007 of INFP-744).
//
//go:embed traces_attributes.json
var tracesAttributesJSON []byte

// traceAttributes is the parsed allow-list.
type traceAttributes struct {
	Resource   []string            `json:"resource"`
	BySpanName map[string][]string `json:"by_span_name"`
	BySpanKind map[string][]string `json:"by_span_kind"`
	Removed    []string            `json:"removed"`
}

var (
	traceAttributesOnce   sync.Once
	traceAttributesValue  *traceAttributes
	traceAttributesLoadEr error
)

// loadTraceAttributes parses the embedded allow-list once.
func loadTraceAttributes() (*traceAttributes, error) {
	traceAttributesOnce.Do(func() {
		var parsed traceAttributes
		if err := json.Unmarshal(tracesAttributesJSON, &parsed); err != nil {
			traceAttributesLoadEr = fmt.Errorf("failed to parse the trace attribute allow-list: %w", err)
			return
		}
		traceAttributesValue = &parsed
	})
	return traceAttributesValue, traceAttributesLoadEr
}

// spanKindNames maps the numeric OTLP span kinds to their enum names, because
// an OTLP JSON document can carry either form.
var spanKindNames = map[int]string{
	0: "SPAN_KIND_UNSPECIFIED",
	1: "SPAN_KIND_INTERNAL",
	2: "SPAN_KIND_SERVER",
	3: "SPAN_KIND_CLIENT",
	4: "SPAN_KIND_PRODUCER",
	5: "SPAN_KIND_CONSUMER",
}

// The otlp* types describe only the fields of an OTLP JSON trace that the
// bundle keeps. Decoding into them drops every other field (events, links,
// status messages, scopes and their attributes), which is how the filter
// removes them.

type otlpAttribute struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

type otlpStatus struct {
	Code json.RawMessage `json:"code,omitempty"`
}

type otlpSpan struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	ParentSpanID      string          `json:"parentSpanId,omitempty"`
	Name              string          `json:"name"`
	Kind              json.RawMessage `json:"kind,omitempty"`
	StartTimeUnixNano json.RawMessage `json:"startTimeUnixNano,omitempty"`
	EndTimeUnixNano   json.RawMessage `json:"endTimeUnixNano,omitempty"`
	Status            *otlpStatus     `json:"status,omitempty"`
	Attributes        []otlpAttribute `json:"attributes,omitempty"`
}

type otlpScopeSpans struct {
	Spans []otlpSpan `json:"spans"`
}

type otlpResource struct {
	Attributes []otlpAttribute `json:"attributes,omitempty"`
}

type otlpResourceSpans struct {
	Resource   *otlpResource    `json:"resource,omitempty"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpTrace struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

// filterTrace reads one answer of Tempo's GET /api/v2/traces/<id> and returns
// the OTLP JSON document written to the bundle: only the fields of the
// allow-list, with the trace, span and parent span IDs converted from base64
// to lowercase hex so that an OTLP HTTP receiver accepts the file.
func filterTrace(answer []byte, allow *traceAttributes) ([]byte, error) {
	var wrapper struct {
		Trace *otlpTrace `json:"trace"`
	}
	if err := json.Unmarshal(answer, &wrapper); err != nil {
		return nil, fmt.Errorf("failed to parse the trace: %w", err)
	}
	if wrapper.Trace == nil {
		return nil, errors.New("the answer has no trace")
	}

	out := otlpTrace{ResourceSpans: []otlpResourceSpans{}}
	for _, rs := range wrapper.Trace.ResourceSpans {
		kept := otlpResourceSpans{ScopeSpans: []otlpScopeSpans{}}
		if rs.Resource != nil {
			if attrs := keepAttributes(rs.Resource.Attributes, allow.Resource); len(attrs) > 0 {
				kept.Resource = &otlpResource{Attributes: attrs}
			}
		}

		spans := []otlpSpan{}
		for _, scope := range rs.ScopeSpans {
			for _, span := range scope.Spans {
				filtered, err := filterSpan(span, allow)
				if err != nil {
					return nil, err
				}
				spans = append(spans, filtered)
			}
		}
		// The instrumentation scopes are dropped: all spans of a resource go
		// into one scope without a name or attributes.
		kept.ScopeSpans = append(kept.ScopeSpans, otlpScopeSpans{Spans: spans})
		out.ResourceSpans = append(out.ResourceSpans, kept)
	}

	return json.Marshal(out)
}

// filterSpan keeps the allowed fields of one span.
func filterSpan(span otlpSpan, allow *traceAttributes) (otlpSpan, error) {
	traceID, err := base64IDToHex(span.TraceID, otlpTraceIDBytes)
	if err != nil {
		return otlpSpan{}, fmt.Errorf("invalid traceId %q: %w", span.TraceID, err)
	}
	spanID, err := base64IDToHex(span.SpanID, otlpSpanIDBytes)
	if err != nil {
		return otlpSpan{}, fmt.Errorf("invalid spanId %q: %w", span.SpanID, err)
	}
	parentID, err := base64IDToHex(span.ParentSpanID, otlpSpanIDBytes)
	if err != nil {
		return otlpSpan{}, fmt.Errorf("invalid parentSpanId %q: %w", span.ParentSpanID, err)
	}

	kind := spanKindName(span.Kind)
	keys := append([]string{}, allow.BySpanName[span.Name]...)
	keys = append(keys, allow.BySpanKind[kind]...)

	kept := otlpSpan{
		TraceID:           traceID,
		SpanID:            spanID,
		ParentSpanID:      parentID,
		Name:              span.Name,
		Kind:              span.Kind,
		StartTimeUnixNano: span.StartTimeUnixNano,
		EndTimeUnixNano:   span.EndTimeUnixNano,
		Attributes:        keepAttributes(span.Attributes, keys),
	}
	if span.Status != nil && len(span.Status.Code) > 0 {
		kept.Status = &otlpStatus{Code: span.Status.Code}
	}
	return kept, nil
}

// keepAttributes returns the attributes whose key is in keys, in their order.
func keepAttributes(attrs []otlpAttribute, keys []string) []otlpAttribute {
	if len(keys) == 0 {
		return nil
	}
	allowed := make(map[string]bool, len(keys))
	for _, key := range keys {
		allowed[key] = true
	}
	var kept []otlpAttribute
	for _, attr := range attrs {
		if allowed[attr.Key] {
			kept = append(kept, attr)
		}
	}
	return kept
}

// spanKindName returns the enum name of a span kind given as a name or as a
// number.
func spanKindName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var name string
	if err := json.Unmarshal(raw, &name); err == nil {
		if number, convErr := strconv.Atoi(name); convErr == nil {
			return spanKindNames[number]
		}
		return name
	}
	var number int
	if err := json.Unmarshal(raw, &number); err == nil {
		return spanKindNames[number]
	}
	return ""
}

// OTLP ID sizes: a trace ID has 16 bytes, a span ID 8 bytes.
const (
	otlpTraceIDBytes = 16
	otlpSpanIDBytes  = 8
)

// base64IDToHex converts a base64 trace or span ID, as Tempo returns it, to
// lowercase hex. The decoded ID must have exactly size bytes, so the file
// holds IDs that an OTLP receiver accepts. An empty ID stays empty.
func base64IDToHex(id string, size int) (string, error) {
	if id == "" {
		return "", nil
	}
	decoded, err := base64.StdEncoding.DecodeString(id)
	if err != nil {
		decoded, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(id, "="))
		if err != nil {
			return "", err
		}
	}
	if len(decoded) != size {
		return "", fmt.Errorf("decodes to %d bytes, want %d", len(decoded), size)
	}
	return hex.EncodeToString(decoded), nil
}
