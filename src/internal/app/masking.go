package app

import (
	"encoding/json"
	"regexp"
	"strings"
)

// maskedValue replaces sensitive values in env/config dumps before they are
// written into a troubleshooting bundle (research R5, FR-008).
const maskedValue = "********"

// sensitiveKeySubstrings is the key-name match list: a key is sensitive when
// it contains any of these substrings, case-insensitively. The normative
// minimum is password|secret|token|key (research R5, FR-008); "pass" widens
// the match to subsume "password" and catch credential keys such as Redis
// "requirepass" and RabbitMQ "default_pass" — over-masking is safe, leaking
// is not.
var sensitiveKeySubstrings = []string{"pass", "secret", "token", "key"}

// isSensitiveKey reports whether a key name refers to a sensitive value,
// using a case-insensitive substring match on the normative token list.
func isSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, substring := range sensitiveKeySubstrings {
		if strings.Contains(lower, substring) {
			return true
		}
	}
	return false
}

// maskEnvOutput masks values in KEY=VALUE dumps (one pair per line), such as
// the output of `env` inside a container. Lines without an equals sign and
// values of non-sensitive keys pass through unchanged. Only the key part
// (before the first '=') is matched, so sensitive substrings inside values do
// not trigger masking.
func maskEnvOutput(input string) string {
	lines := strings.Split(input, "\n")
	for i, line := range lines {
		idx := strings.Index(line, "=")
		if idx < 0 {
			continue
		}
		key := line[:idx]
		if isSensitiveKey(key) {
			lines[i] = key + "=" + maskedValue
		}
	}
	return strings.Join(lines, "\n")
}

// maskConfigPairs masks values in alternating key/value line dumps, the
// format produced by `redis-cli CONFIG GET '*'` in non-interactive mode:
// even lines carry the parameter name, odd lines carry its value.
func maskConfigPairs(input string) string {
	lines := strings.Split(input, "\n")
	for i := 0; i+1 < len(lines); i += 2 {
		if isSensitiveKey(strings.TrimSpace(lines[i])) {
			lines[i+1] = maskedValue
		}
	}
	return strings.Join(lines, "\n")
}

// erlangTuplePattern matches flat two-element Erlang tuples such as
// {default_pass,<<"guest">>} in `rabbitmqctl environment` / `rabbitmqctl
// status` output. Nested tuples/lists as values are not matched; scalar
// credentials in the RabbitMQ configuration are flat tuples.
var erlangTuplePattern = regexp.MustCompile(`\{\s*'?([A-Za-z0-9_.]+)'?\s*,([^{}\[\]]*)\}`)

// maskErlangConfig masks values of sensitive keys in Erlang proplist-style
// dumps (`rabbitmqctl environment` and `rabbitmqctl status`).
func maskErlangConfig(input string) string {
	return erlangTuplePattern.ReplaceAllStringFunc(input, func(match string) string {
		submatch := erlangTuplePattern.FindStringSubmatch(match)
		if submatch == nil || !isSensitiveKey(submatch[1]) {
			return match
		}
		return "{" + submatch[1] + "," + maskedValue + "}"
	})
}

// maskJSON masks values of sensitive keys anywhere in a JSON document, such
// as the server API configuration dump (research R5). Input that is not valid
// JSON is returned unchanged so callers can store error responses as-is and
// keep failures observable.
func maskJSON(input string) string {
	var document any
	if err := json.Unmarshal([]byte(input), &document); err != nil {
		return input
	}
	masked, err := json.MarshalIndent(maskJSONValue(document), "", "    ")
	if err != nil {
		return input
	}
	return string(masked)
}

// maskJSONValue recursively masks sensitive-keyed values in a decoded JSON
// tree. The whole value of a sensitive key is replaced, whatever its type.
func maskJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			if isSensitiveKey(key) {
				typed[key] = maskedValue
				continue
			}
			typed[key] = maskJSONValue(nested)
		}
		return typed
	case []any:
		for i, item := range typed {
			typed[i] = maskJSONValue(item)
		}
		return typed
	default:
		return value
	}
}
