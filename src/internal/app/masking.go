package app

import (
	"regexp"
	"strings"
)

// maskedValue replaces sensitive values in env/config dumps before they are
// written into a troubleshooting bundle (research R5, FR-008).
const maskedValue = "********"

// sensitiveKeySubstrings is the normative key-name match list (research R5):
// a key is sensitive when it contains any of these substrings,
// case-insensitively.
var sensitiveKeySubstrings = []string{"password", "secret", "token", "key"}

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
