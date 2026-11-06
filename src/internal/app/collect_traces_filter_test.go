package app

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readTraceTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "traces", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLoadTraceAttributes(t *testing.T) {
	allow, err := loadTraceAttributes()
	if err != nil {
		t.Fatalf("loadTraceAttributes() error = %v", err)
	}
	if len(allow.Resource) == 0 || len(allow.BySpanName) == 0 || len(allow.BySpanKind) == 0 || len(allow.Removed) == 0 {
		t.Fatalf("allow-list has an empty section: %+v", allow)
	}

	// A key must not be both kept and removed.
	kept := map[string]bool{}
	for _, key := range allow.Resource {
		kept[key] = true
	}
	for _, keys := range allow.BySpanName {
		for _, key := range keys {
			kept[key] = true
		}
	}
	for _, keys := range allow.BySpanKind {
		for _, key := range keys {
			kept[key] = true
		}
	}
	for _, key := range allow.Removed {
		if kept[key] {
			t.Errorf("key %q is both kept and removed", key)
		}
	}
}

func TestFilterTrace(t *testing.T) {
	allow, err := loadTraceAttributes()
	if err != nil {
		t.Fatal(err)
	}
	raw := readTraceTestdata(t, "trace_raw.json")

	filtered, err := filterTrace(raw, allow)
	if err != nil {
		t.Fatalf("filterTrace() error = %v", err)
	}

	// No removed value appears anywhere in the output (SC-002).
	for _, secret := range []string{
		"MATCH",                // Cypher text (query)
		"secret-device",        // Cypher text, exception text, status message
		"203.0.113.42",         // client IP address
		"token=abc",            // URL and target
		"curl/8.4.0",           // user agent
		"alice@example.com",    // exception message
		"exception",            // event name and status message
		"customer-host-secret", // extra resource attribute
		"q-123",                // query_id
		"infrahub.test",        // instrumentation scope
	} {
		if strings.Contains(string(filtered), secret) {
			t.Errorf("filtered trace contains %q:\n%s", secret, filtered)
		}
	}

	var out otlpTrace
	if err := json.Unmarshal(filtered, &out); err != nil {
		t.Fatalf("filtered trace is not OTLP JSON: %v", err)
	}
	if len(out.ResourceSpans) != 1 {
		t.Fatalf("resourceSpans = %d, want 1", len(out.ResourceSpans))
	}
	rs := out.ResourceSpans[0]
	if got := attributeKeys(rs.Resource.Attributes); !sameKeys(got, []string{"service.name", "service.version", "worker.id"}) {
		t.Errorf("resource attributes = %v", got)
	}

	var rawTrace struct {
		Trace otlpTrace `json:"trace"`
	}
	if err := json.Unmarshal(raw, &rawTrace); err != nil {
		t.Fatal(err)
	}
	rawSpans := map[string]otlpSpan{}
	for _, scope := range rawTrace.Trace.ResourceSpans[0].ScopeSpans {
		for _, span := range scope.Spans {
			rawSpans[span.Name] = span
		}
	}

	wantAttrs := map[string][]string{
		"POST /graphql":                  {"http.method", "http.route", "http.status_code"},
		"execute_db_query_with_metadata": {"query_name", "rows"},
		"execute_graphql":                {"type", "operation", "name", "branch"},
	}
	spans := rs.ScopeSpans[0].Spans
	if len(spans) != len(wantAttrs) {
		t.Fatalf("spans = %d, want %d", len(spans), len(wantAttrs))
	}
	for _, span := range spans {
		want, ok := wantAttrs[span.Name]
		if !ok {
			t.Errorf("unexpected span %q", span.Name)
			continue
		}
		if got := attributeKeys(span.Attributes); !sameKeys(got, want) {
			t.Errorf("span %q attributes = %v, want %v", span.Name, got, want)
		}

		// The IDs are lowercase hex and decode to the same bytes as the
		// base64 values of Tempo.
		original := rawSpans[span.Name]
		for _, pair := range [][2]string{
			{span.TraceID, original.TraceID},
			{span.SpanID, original.SpanID},
			{span.ParentSpanID, original.ParentSpanID},
		} {
			assertSameID(t, span.Name, pair[0], pair[1])
		}
		if span.StartTimeUnixNano == nil || span.EndTimeUnixNano == nil || len(span.Kind) == 0 {
			t.Errorf("span %q lost its kind or times", span.Name)
		}
	}

	// The error status code stays; its message is removed.
	if !strings.Contains(string(filtered), "STATUS_CODE_ERROR") {
		t.Error("filtered trace lost the error status code")
	}
}

func TestFilterTraceSpanWithoutAllowedAttribute(t *testing.T) {
	allow, _ := loadTraceAttributes()
	answer := `{"trace":{"resourceSpans":[{"scopeSpans":[{"spans":[{
		"traceId":"AAAAAAAAAAAAAAAAAAAAAQ==","spanId":"AAAAAAAAAAE=","name":"other",
		"kind":1,"startTimeUnixNano":"1","endTimeUnixNano":"2",
		"status":{"code":"STATUS_CODE_OK","message":"secret"},
		"attributes":[{"key":"name","value":{"stringValue":"secret"}}],
		"events":[{"name":"secret"}],"links":[{"traceId":"x"}]}]}]}]}}`
	filtered, err := filterTrace([]byte(answer), allow)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(filtered), "secret") {
		t.Errorf("filtered span kept a removed field: %s", filtered)
	}
	for _, want := range []string{`"name":"other"`, `"kind":1`, `"traceId":"00000000000000000000000000000001"`, `"spanId":"0000000000000001"`, "STATUS_CODE_OK"} {
		if !strings.Contains(string(filtered), want) {
			t.Errorf("filtered span lacks %s: %s", want, filtered)
		}
	}
}

func TestFilterTraceRejectsInvalidID(t *testing.T) {
	allow, _ := loadTraceAttributes()
	answer := `{"trace":{"resourceSpans":[{"scopeSpans":[{"spans":[{"traceId":"not base64!","spanId":"AAAAAAAAAAE=","name":"x"}]}]}]}}`
	if _, err := filterTrace([]byte(answer), allow); err == nil {
		t.Error("filterTrace() accepted an invalid trace ID")
	}
	if _, err := filterTrace([]byte(`{"metrics":{}}`), allow); err == nil {
		t.Error("filterTrace() accepted an answer without a trace")
	}
}

func TestSpanKindName(t *testing.T) {
	for raw, want := range map[string]string{
		`"SPAN_KIND_SERVER"`: "SPAN_KIND_SERVER",
		`2`:                  "SPAN_KIND_SERVER",
		`"2"`:                "SPAN_KIND_SERVER",
		``:                   "",
	} {
		if got := spanKindName(json.RawMessage(raw)); got != want {
			t.Errorf("spanKindName(%s) = %q, want %q", raw, got, want)
		}
	}
}

func assertSameID(t *testing.T, span, hexID, base64ID string) {
	t.Helper()
	if base64ID == "" {
		if hexID != "" {
			t.Errorf("span %q has ID %q, want none", span, hexID)
		}
		return
	}
	if strings.ToLower(hexID) != hexID {
		t.Errorf("span %q ID %q is not lowercase", span, hexID)
	}
	got, err := hex.DecodeString(hexID)
	if err != nil {
		t.Errorf("span %q ID %q is not hex: %v", span, hexID, err)
		return
	}
	want, _ := base64.StdEncoding.DecodeString(base64ID)
	if string(got) != string(want) {
		t.Errorf("span %q ID %q does not match %q", span, hexID, base64ID)
	}
}

func attributeKeys(attrs []otlpAttribute) []string {
	keys := make([]string, 0, len(attrs))
	for _, attr := range attrs {
		keys = append(keys, attr.Key)
	}
	return keys
}

func sameKeys(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]bool{}
	for _, key := range got {
		seen[key] = true
	}
	for _, key := range want {
		if !seen[key] {
			return false
		}
	}
	return true
}

func TestBase64IDToHexLength(t *testing.T) {
	traceID := base64.StdEncoding.EncodeToString(make([]byte, 16))
	spanID := base64.StdEncoding.EncodeToString(make([]byte, 8))
	if _, err := base64IDToHex(traceID, otlpTraceIDBytes); err != nil {
		t.Errorf("16-byte trace ID rejected: %v", err)
	}
	if _, err := base64IDToHex(spanID, otlpSpanIDBytes); err != nil {
		t.Errorf("8-byte span ID rejected: %v", err)
	}
	if _, err := base64IDToHex(spanID, otlpTraceIDBytes); err == nil {
		t.Error("8-byte trace ID accepted")
	}
	if _, err := base64IDToHex(traceID, otlpSpanIDBytes); err == nil {
		t.Error("16-byte span ID accepted")
	}
	if got, err := base64IDToHex("", otlpSpanIDBytes); err != nil || got != "" {
		t.Errorf("empty ID = %q, %v", got, err)
	}
}
