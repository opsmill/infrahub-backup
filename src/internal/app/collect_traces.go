package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// tracesBundleDir is the bundle directory for the opt-in traces collector.
const tracesBundleDir = "traces"

// Defaults of the trace options (contracts/cli.md of INFP-744).
const (
	defaultTracesSince       = 2 * time.Hour
	defaultTracesMinDuration = time.Second
	defaultTracesMaxSizeMB   = 100
)

// tracesMaxSearchRounds bounds the number of searches when each one returns
// the search limit, so a misbehaving backend cannot keep the collector in a
// loop.
const tracesMaxSearchRounds = 20

// Environment variables of the two tokens. They have no option, so they never
// appear on a command line, and they are read with os.LookupEnv rather than
// through viper, so they never appear in viper's debug output.
const (
	tracesTempoTokenEnv = "INFRAHUB_TRACES_TEMPO_TOKEN"
	tracesAPITokenEnv   = "INFRAHUB_TRACES_API_TOKEN"
)

// Package variables that tests replace: the search limit sent to Tempo, the
// clock, and the time limit of the fetch loop.
var (
	tracesSearchLimit = 10000
	tracesClock       = time.Now
	tracesTimeLimit   = collectTransferTimeout
	// tracesMegabyte converts --traces-max-size to bytes.
	tracesMegabyte int64 = 1024 * 1024
	// tracesRequestTimeout limits each Tempo request.
	tracesRequestTimeout = collectExecTimeout
)

// TraceOptions are the validated options of the traces collector.
type TraceOptions struct {
	Include     bool
	Since       time.Duration
	Start       time.Time
	End         time.Time
	MinDuration time.Duration
	MaxSizeMB   int
	TempoURL    string
	// TempoToken and APIToken are secrets: they are never written to the
	// bundle or logged.
	TempoToken string
	APIToken   string
}

// hasAbsoluteWindow reports whether --traces-start and --traces-end were given.
func (o TraceOptions) hasAbsoluteWindow() bool {
	return !o.Start.IsZero() && !o.End.IsZero()
}

// window resolves the time window sent to Tempo: the absolute window when it
// was given, otherwise [now - since, now].
func (o TraceOptions) window(now time.Time) traceWindow {
	if o.hasAbsoluteWindow() {
		return traceWindow{Start: o.Start.UTC(), End: o.End.UTC()}
	}
	since := o.Since
	if since <= 0 {
		since = defaultTracesSince
	}
	now = now.UTC().Truncate(time.Second)
	return traceWindow{Start: now.Add(-since), End: now}
}

// TraceOptionsInput is the raw input of the trace options, as the create
// command reads it from flags and environment variables.
type TraceOptionsInput struct {
	Include     bool
	Since       string
	Start       string
	End         string
	MinDuration string
	MaxSizeMB   int
	TempoURL    string
	// Given holds the names of the trace options that the operator set
	// explicitly (flag or environment variable), without the leading dashes.
	Given map[string]bool
}

// traceOptionNames lists the trace options that require --include-traces, in
// the order the validation reports them.
var traceOptionNames = []string{
	"traces-since",
	"traces-start",
	"traces-end",
	"traces-min-duration",
	"traces-max-size",
	"traces-tempo-url",
}

// TraceOptionNames returns the names of the options that require
// --include-traces, for the entry point to check which ones were given.
func TraceOptionNames() []string {
	return append([]string(nil), traceOptionNames...)
}

// ValidateTraceOptions checks the trace options before any collector runs and
// returns the validated options. The error messages are part of the command
// contract.
func ValidateTraceOptions(in TraceOptionsInput) (TraceOptions, error) {
	if !in.Include {
		for _, name := range traceOptionNames {
			if in.Given[name] {
				return TraceOptions{}, fmt.Errorf("--%s requires --include-traces", name)
			}
		}
		return TraceOptions{}, nil
	}

	opts := TraceOptions{Include: true}

	startGiven := in.Given["traces-start"] || in.Start != ""
	endGiven := in.Given["traces-end"] || in.End != ""
	if in.Given["traces-since"] && (startGiven || endGiven) {
		return TraceOptions{}, errors.New("--traces-since cannot be combined with --traces-start or --traces-end")
	}
	if startGiven != endGiven {
		return TraceOptions{}, errors.New("--traces-start and --traces-end must be given together")
	}

	if startGiven {
		start, err := parseTraceTime("traces-start", in.Start)
		if err != nil {
			return TraceOptions{}, err
		}
		end, err := parseTraceTime("traces-end", in.End)
		if err != nil {
			return TraceOptions{}, err
		}
		if !start.Before(end) {
			return TraceOptions{}, errors.New("--traces-start must be before --traces-end")
		}
		opts.Start, opts.End = start, end
	} else {
		opts.Since = defaultTracesSince
		if in.Since != "" {
			since, err := time.ParseDuration(in.Since)
			if err != nil || since <= 0 {
				return TraceOptions{}, fmt.Errorf("invalid --traces-since %q: use a duration such as 30m or 2h", in.Since)
			}
			opts.Since = since
		}
	}

	opts.MinDuration = defaultTracesMinDuration
	if in.MinDuration != "" {
		minDuration, err := time.ParseDuration(in.MinDuration)
		if err != nil || minDuration < 0 {
			return TraceOptions{}, fmt.Errorf("invalid --traces-min-duration %q: use a duration such as 1s or 500ms", in.MinDuration)
		}
		opts.MinDuration = minDuration
	}

	if in.MaxSizeMB <= 0 {
		return TraceOptions{}, errors.New("--traces-max-size must be a positive number of megabytes")
	}
	opts.MaxSizeMB = in.MaxSizeMB

	if in.TempoURL != "" {
		parsed, err := url.Parse(in.TempoURL)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return TraceOptions{}, fmt.Errorf("invalid --traces-tempo-url %q: use an http or https URL", in.TempoURL)
		}
		opts.TempoURL = strings.TrimRight(in.TempoURL, "/")
	}

	return opts, nil
}

// parseTraceTime parses an RFC 3339 time of an absolute window.
func parseTraceTime(option, value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid --%s %q: use RFC 3339, for example 2026-11-04T14:00:00Z", option, value)
	}
	return parsed, nil
}

// TraceTokenSources lets ReadTraceTokens read the environment and the terminal
// without depending on the real process, so tests can replace them.
type TraceTokenSources struct {
	LookupEnv    func(string) (string, bool)
	IsTerminal   func() bool
	ReadPassword func(prompt string) (string, error)
}

// ReadTraceTokens fills the two tokens of the options from their environment
// variables, or asks for them when a variable is not set and standard input is
// a terminal. The prompts run before any collector, so no collector waits for
// input. An empty answer means no token. Without a terminal, nothing is asked.
func ReadTraceTokens(opts *TraceOptions, src TraceTokenSources) error {
	if !opts.Include {
		return nil
	}

	read := func(env, prompt string) (string, error) {
		if value, ok := src.LookupEnv(env); ok {
			return strings.TrimSpace(value), nil
		}
		if src.IsTerminal == nil || !src.IsTerminal() || src.ReadPassword == nil {
			return "", nil
		}
		value, err := src.ReadPassword(prompt)
		if err != nil {
			return "", fmt.Errorf("failed to read %s: %w", env, err)
		}
		return strings.TrimSpace(value), nil
	}

	if opts.TempoURL != "" {
		token, err := read(tracesTempoTokenEnv, fmt.Sprintf("Token for %s (leave empty for none): ", redactURL(opts.TempoURL)))
		if err != nil {
			return err
		}
		opts.TempoToken = token
	}

	token, err := read(tracesAPITokenEnv, "Infrahub API token with super_admin, used only to read the trace sampling ratio (leave empty to skip): ")
	if err != nil {
		return err
	}
	opts.APIToken = token
	return nil
}

// traceWindow is the time window of one traces collection.
type traceWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// sampleRatio is the trace sampling ratio that the Infrahub API reports.
type sampleRatio struct {
	Enabled                 bool     `json:"enabled"`
	DefaultRatio            *float64 `json:"default_ratio"`
	RatioInForce            *float64 `json:"ratio_in_force"`
	TemporaryRatio          *float64 `json:"temporary_ratio"`
	TemporaryRatioExpiresAt *string  `json:"temporary_ratio_expires_at"`
}

// traceCollectionDetails is the details object of the traces entry of
// bundle_information.json (contracts/bundle.md of INFP-744).
type traceCollectionDetails struct {
	Backend            string       `json:"backend,omitempty"`
	Window             traceWindow  `json:"window"`
	MinDuration        string       `json:"min_duration"`
	MinDurationUsed    string       `json:"min_duration_used,omitempty"`
	MaxSizeMB          int          `json:"max_size_mb"`
	TracesFound        int          `json:"traces_found"`
	TracesWritten      int          `json:"traces_written"`
	TracesLeftOut      int          `json:"traces_left_out"`
	BytesWritten       int64        `json:"bytes_written"`
	SearchLimitReached bool         `json:"search_limit_reached"`
	TimeLimitReached   bool         `json:"time_limit_reached"`
	SampleRatio        *sampleRatio `json:"sample_ratio,omitempty"`
}

// formatTraceDuration renders a duration with its largest exact unit ("1s",
// "500ms", "2h"), a form that TraceQL accepts and people read easily. Go's
// own String() gives "2h0m0s", which TraceQL may reject.
func formatTraceDuration(d time.Duration) string {
	units := []struct {
		size time.Duration
		name string
	}{
		{time.Hour, "h"},
		{time.Minute, "m"},
		{time.Second, "s"},
		{time.Millisecond, "ms"},
		{time.Microsecond, "us"},
	}
	if d == 0 {
		return "0s"
	}
	for _, unit := range units {
		if d%unit.size == 0 {
			return strconv.FormatInt(int64(d/unit.size), 10) + unit.name
		}
	}
	return strconv.FormatInt(int64(d), 10) + "ns"
}

// redactURL removes user information from a URL before it is recorded or
// logged. A URL that cannot be parsed is replaced by a placeholder, because
// it could still hold credentials.
func redactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "<invalid URL>"
	}
	parsed.User = nil
	return parsed.String()
}

// errTempoNotFound reports that the deployment has no Tempo the collector can
// reach.
var errTempoNotFound = errors.New("no Tempo found in the deployment")

// tempoPermissionError reports that the Kubernetes role of the operator lacks a
// permission the collector needs to reach Tempo.
type tempoPermissionError struct {
	action string // what the role cannot do, e.g. "list services"
	grant  string // what to grant, e.g. "list on services"
}

func (e *tempoPermissionError) Error() string {
	return fmt.Sprintf("the role cannot %s; grant %s or use --traces-tempo-url", e.action, e.grant)
}

// tempoLocator is an optional collect-only backend capability: find the
// deployment's Tempo and return a base URL that the host can reach, with a
// function that releases what was started for it (a port-forward). It is
// deliberately not part of EnvironmentBackend, which the backup tool shares.
type tempoLocator interface {
	LocateTempo(ctx context.Context) (baseURL string, stop func(), err error)
}

// stdinExecer is an optional collect-only backend capability: a time-limited
// exec in a service container that writes stdin to the command and returns its
// stdout and stderr apart. The sampling ratio uses it to pass the API token
// without putting it on a command line.
type stdinExecer interface {
	ExecStdinContext(ctx context.Context, timeout time.Duration, service string, command []string, stdin []byte) (string, string, error)
}

// tempoStatusError is an HTTP error answer of Tempo.
type tempoStatusError struct {
	status int
	body   string
}

func (e *tempoStatusError) Error() string {
	line := strings.TrimSpace(strings.SplitN(e.body, "\n", 2)[0])
	if len(line) > 200 {
		line = line[:200]
	}
	if line == "" {
		return fmt.Sprintf("HTTP %d", e.status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.status, line)
}

// tempoClient sends the Tempo HTTP requests from the host.
type tempoClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func newTempoClient(baseURL, token string) *tempoClient {
	return &tempoClient{baseURL: strings.TrimRight(baseURL, "/"), token: token, client: &http.Client{}}
}

// get sends one GET request limited by tracesRequestTimeout and returns the
// body of a 2xx answer.
func (c *tempoClient) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	rctx, cancel := context.WithTimeout(ctx, tracesRequestTimeout)
	defer cancel()

	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		if errors.Is(rctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			return nil, &timeoutError{timeout: tracesRequestTimeout}
		}
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read the answer of %s: %w", path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &tempoStatusError{status: resp.StatusCode, body: string(body)}
	}
	return body, nil
}

// traceSummary is one result of a Tempo search.
type traceSummary struct {
	TraceID     string `json:"traceID"`
	RootService string `json:"rootServiceName"`
	RootName    string `json:"rootTraceName"`
	DurationMs  int64  `json:"durationMs"`
}

// search runs one TraceQL search for the traces of the window longer than
// minDuration.
func (c *tempoClient) search(ctx context.Context, window traceWindow, minDuration time.Duration, limit int) ([]traceSummary, error) {
	query := url.Values{}
	query.Set("q", fmt.Sprintf("{ traceDuration > %s }", formatTraceDuration(minDuration)))
	query.Set("start", strconv.FormatInt(window.Start.Unix(), 10))
	query.Set("end", strconv.FormatInt(window.End.Unix(), 10))
	query.Set("limit", strconv.Itoa(limit))

	body, err := c.get(ctx, "/api/search", query)
	if err != nil {
		return nil, err
	}
	var answer struct {
		Traces []traceSummary `json:"traces"`
	}
	if err := json.Unmarshal(body, &answer); err != nil {
		return nil, fmt.Errorf("failed to parse the Tempo search answer: %w", err)
	}
	return answer.Traces, nil
}

// fetch returns the answer of GET /api/v2/traces/<id>.
func (c *tempoClient) fetch(ctx context.Context, traceID string) ([]byte, error) {
	return c.get(ctx, "/api/v2/traces/"+url.PathEscape(traceID), nil)
}

// searchResult is the outcome of searchSlowTraces.
type searchResult struct {
	traces          []traceSummary
	minDurationUsed time.Duration
	limitReached    bool
}

// searchSlowTraces searches the window and, while a search returns the search
// limit, searches again with the minimum duration doubled: when the limit is
// reached, more traces may exist and the subset Tempo returns is not the
// slowest one. The result is sorted slowest first.
func searchSlowTraces(ctx context.Context, client *tempoClient, window traceWindow, minDuration time.Duration) (searchResult, error) {
	result := searchResult{minDurationUsed: minDuration}
	for round := 0; ; round++ {
		traces, err := client.search(ctx, window, result.minDurationUsed, tracesSearchLimit)
		if err != nil {
			return result, err
		}
		result.traces = traces
		if len(traces) < tracesSearchLimit || round+1 >= tracesMaxSearchRounds {
			break
		}
		result.limitReached = true
		next := result.minDurationUsed * 2
		if next <= 0 {
			next = time.Millisecond
		}
		logrus.Warnf("Tempo returned the search limit of %d traces; searching again for traces longer than %s", tracesSearchLimit, formatTraceDuration(next))
		result.minDurationUsed = next
	}

	sort.SliceStable(result.traces, func(i, j int) bool {
		return result.traces[i].DurationMs > result.traces[j].DurationMs
	})
	return result, nil
}

// normalizeTraceID returns a trace ID of the search as 32 lowercase hex
// characters, and false when the ID is not hex or is too long. Tempo can drop
// the leading zeros of a trace ID. The ID becomes part of a file name, so only
// hex characters are accepted: a backend cannot make the collector write
// outside traces/.
func normalizeTraceID(id string) (string, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" || len(id) > 32 {
		return "", false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", false
		}
	}
	if len(id) < 32 {
		id = strings.Repeat("0", 32-len(id)) + id
	}
	return id, true
}

// Output messages of the sampling ratio (contracts/cli.md).
const (
	ratioNoToken    = "no API token given (set INFRAHUB_TRACES_API_TOKEN)"
	ratioNoAnswer   = "the Infrahub API did not answer"
	ratioRejected   = "the API token was rejected"
	ratioNoPerm     = "the API token lacks the super_admin permission"
	ratioNoEndpoint = "this Infrahub version has no sampling ratio endpoint (added in 1.12)"
	ratioBadAnswer  = "the Infrahub API answer could not be read"
	sampleRatioPath = "/api/troubleshooting/trace-sample-ratio"
)

// sampleRatioCommand builds the Python command that reads the sampling ratio
// inside infrahub-server. The token is read from standard input, so it is not
// in the process list of the host or of the container. The command prints the
// HTTP status and the body as one JSON object.
func sampleRatioCommand(url string) []string {
	script := strings.Join([]string{
		"import json",
		"import sys",
		"import httpx",
		"token = sys.stdin.read().strip()",
		"resp = httpx.get(sys.argv[1], headers={'X-INFRAHUB-KEY': token}, timeout=30)",
		"sys.stdout.write(json.dumps({'status': resp.status_code, 'body': resp.text}))",
	}, "\n")
	return []string{"python", "-c", script, url}
}

// readSampleRatio reads the trace sampling ratio through the Infrahub API. It
// returns the ratio, or nil and the reason it was not read.
func readSampleRatio(cc *collectContext, token string) (*sampleRatio, string) {
	if token == "" {
		return nil, ratioNoToken
	}
	execer, ok := cc.backend.(stdinExecer)
	if !ok {
		return nil, ratioNoAnswer
	}

	stdout, _, err := execer.ExecStdinContext(cc.ctx, collectExecTimeout, "infrahub-server",
		sampleRatioCommand(infrahubServerAPIFallback+sampleRatioPath), []byte(token+"\n"))
	if err != nil {
		logrus.Debugf("Sampling ratio request failed: %v", err)
		return nil, ratioNoAnswer
	}

	var answer struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal([]byte(lastLine(stdout)), &answer); err != nil {
		return nil, ratioBadAnswer
	}
	switch {
	case answer.Status == http.StatusUnauthorized:
		return nil, ratioRejected
	case answer.Status == http.StatusForbidden:
		return nil, ratioNoPerm
	case answer.Status == http.StatusNotFound:
		return nil, ratioNoEndpoint
	case answer.Status < 200 || answer.Status >= 300:
		return nil, fmt.Sprintf("the Infrahub API answered HTTP %d", answer.Status)
	}

	var api struct {
		Enabled        bool     `json:"enabled"`
		DefaultRatio   *float64 `json:"default_ratio"`
		RatioInForce   *float64 `json:"ratio_in_force"`
		TemporaryRatio *float64 `json:"temporary_ratio"`
		ExpiresAt      *string  `json:"expires_at"`
	}
	if err := json.Unmarshal([]byte(answer.Body), &api); err != nil {
		return nil, ratioBadAnswer
	}
	ratio := &sampleRatio{Enabled: api.Enabled}
	if api.Enabled {
		ratio.DefaultRatio = api.DefaultRatio
		ratio.RatioInForce = api.RatioInForce
		ratio.TemporaryRatio = api.TemporaryRatio
		ratio.TemporaryRatioExpiresAt = api.ExpiresAt
	}
	return ratio, ""
}

// lastLine returns the last non-empty line of output, where the JSON answer
// of the ratio command is, after any notice a runtime may print first.
func lastLine(output string) string {
	lines := nonEmptyLines(output)
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

// formatRatio renders an optional ratio for the output.
func formatRatio(value *float64) string {
	if value == nil {
		return "none"
	}
	return strconv.FormatFloat(*value, 'g', -1, 64)
}

// tracesCollector adds the traces of slow requests to the bundle when
// --include-traces is given. It reads Tempo from the host, so it works while
// the Infrahub pods are down; only the sampling ratio needs infrahub-server.
func tracesCollector() collector {
	return collector{
		name: "traces",
		skip: func(cc *collectContext) (bool, string) {
			if !cc.opts.Traces.Include {
				return true, "not requested"
			}
			return false, ""
		},
		run: collectTraces,
	}
}

func collectTraces(cc *collectContext) error {
	opts := cc.opts.Traces
	started := tracesClock()
	details := &traceCollectionDetails{
		Window:      opts.window(started),
		MinDuration: formatTraceDuration(opts.MinDuration),
		MaxSizeMB:   opts.MaxSizeMB,
	}
	cc.setDetails(details)

	baseURL, token := opts.TempoURL, opts.TempoToken
	if baseURL == "" {
		token = ""
		locator, ok := cc.backend.(tempoLocator)
		if !ok {
			return &collectSkipError{reason: errTempoNotFound.Error() + "; use --traces-tempo-url"}
		}
		located, stop, err := locator.LocateTempo(cc.ctx)
		if err != nil {
			var permission *tempoPermissionError
			switch {
			case errors.As(err, &permission):
				return &collectSkipError{reason: permission.Error()}
			case errors.Is(err, errTempoNotFound):
				return &collectSkipError{reason: errTempoNotFound.Error() + "; use --traces-tempo-url"}
			}
			return fmt.Errorf("failed to locate Tempo: %w", err)
		}
		if stop != nil {
			defer stop()
		}
		baseURL = located
	}
	details.Backend = redactURL(baseURL)
	client := newTempoClient(baseURL, token)

	logrus.Infof("Collecting traces from %s between %s and %s, longer than %s",
		details.Backend, details.Window.Start.Format(time.RFC3339), details.Window.End.Format(time.RFC3339), details.MinDuration)

	found, err := searchSlowTraces(cc.ctx, client, details.Window, opts.MinDuration)
	details.MinDurationUsed = formatTraceDuration(found.minDurationUsed)
	details.SearchLimitReached = found.limitReached
	if err != nil {
		return tempoError(err, opts.TempoURL != "", details.Backend, "search")
	}
	details.TracesFound = len(found.traces)

	writeErr := writeTraces(cc, client, found.traces, opts, started, details)
	details.TracesLeftOut = details.TracesFound - details.TracesWritten

	// The sampling ratio never changes the status of the collector.
	ratio, reason := readSampleRatio(cc, opts.APIToken)
	if ratio != nil {
		details.SampleRatio = ratio
		if ratio.Enabled {
			logrus.Infof("Recorded the trace sampling ratio: %s in force, default %s", formatRatio(ratio.RatioInForce), formatRatio(ratio.DefaultRatio))
		} else {
			logrus.Warn("The Infrahub API says that tracing is off; the bundle records it")
		}
	} else {
		logrus.Infof("Did not record the trace sampling ratio: %s", reason)
	}

	return writeErr
}

// writeTraces fetches, filters and writes the traces slowest first, until the
// size limit or the time limit. It updates the counts in details.
func writeTraces(cc *collectContext, client *tempoClient, traces []traceSummary, opts TraceOptions, started time.Time, details *traceCollectionDetails) error {
	if len(traces) == 0 {
		logrus.Infof("No trace longer than %s in the window; tracing may be off or the sampling ratio low", details.MinDurationUsed)
		return nil
	}

	allowList, err := loadTraceAttributes()
	if err != nil {
		return err
	}

	maxBytes := int64(opts.MaxSizeMB) * tracesMegabyte
	dir := filepath.Join(cc.bundleDir, tracesBundleDir)
	sizeLimitReached := false

	for i, summary := range traces {
		if err := cc.ctx.Err(); err != nil {
			return err
		}
		if tracesClock().Sub(started) >= tracesTimeLimit {
			details.TimeLimitReached = true
			break
		}

		id, ok := normalizeTraceID(summary.TraceID)
		if !ok {
			logrus.Warnf("Skipping a trace with an invalid ID from the trace backend: %q", summary.TraceID)
			continue
		}
		raw, err := client.fetch(cc.ctx, id)
		if err != nil {
			return partialTracesError(details, tempoError(err, opts.TempoURL != "", details.Backend, "fetch of trace "+id))
		}
		filtered, err := filterTrace(raw, allowList)
		if err != nil {
			return partialTracesError(details, fmt.Errorf("failed to filter trace %s: %w", id, err))
		}

		if details.BytesWritten+int64(len(filtered)) > maxBytes {
			sizeLimitReached = true
			if details.TracesWritten == 0 {
				logrus.Warnf("The size limit of %d MB is smaller than the slowest trace (%s); no trace was written", opts.MaxSizeMB, formatBytes(int64(len(filtered))))
			}
			break
		}

		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create %s directory: %w", tracesBundleDir, err)
		}
		name := fmt.Sprintf("%04d-%s.json", i+1, id)
		if err := os.WriteFile(filepath.Join(dir, name), filtered, 0644); err != nil {
			return partialTracesError(details, fmt.Errorf("failed to write %s: %w", name, err))
		}
		details.TracesWritten++
		details.BytesWritten += int64(len(filtered))
	}

	leftOut := details.TracesFound - details.TracesWritten
	switch {
	case details.TimeLimitReached:
		logrus.Warnf("Stopped fetching traces after %s; wrote the %d slowest of %d", formatTraceDuration(tracesTimeLimit), details.TracesWritten, details.TracesFound)
	case sizeLimitReached:
		logrus.Infof("Wrote %d of %d traces (%s); %d left out by the size limit", details.TracesWritten, details.TracesFound, formatBytes(details.BytesWritten), leftOut)
	default:
		logrus.Infof("Wrote %d of %d traces (%s)", details.TracesWritten, details.TracesFound, formatBytes(details.BytesWritten))
	}
	return nil
}

// partialTracesError marks an error after some traces were written as a
// partial collection. The written traces stay in the bundle.
func partialTracesError(details *traceCollectionDetails, err error) error {
	if details.TracesWritten == 0 {
		return err
	}
	return fmt.Errorf("partial traces collection: wrote %d of %d traces: %w", details.TracesWritten, details.TracesFound, err)
}

// tempoError turns a Tempo request error into the collector outcome: a
// backend that does not answer the search, or does not answer it in time, is
// skipped (INFP-744: without a reachable Tempo, the collector is skipped); a
// rejected request names the token variable; any other answer is a failure.
// A fetch that does not answer stays a failure, because the search showed
// that the backend is reachable.
func tempoError(err error, customURL bool, backend, step string) error {
	var status *tempoStatusError
	if errors.As(err, &status) {
		if status.status == http.StatusUnauthorized || status.status == http.StatusForbidden {
			if customURL {
				return fmt.Errorf("the trace backend rejected the %s (HTTP %d); check %s", step, status.status, tracesTempoTokenEnv)
			}
			return fmt.Errorf("the trace backend rejected the %s (HTTP %d)", step, status.status)
		}
		return fmt.Errorf("the trace backend %s failed: %w", step, err)
	}

	if step == "search" {
		var timeout *timeoutError
		if errors.As(err, &timeout) {
			return &collectSkipError{reason: fmt.Sprintf("Tempo at %s did not answer within %s; use --traces-tempo-url", backend, formatTraceDuration(timeout.timeout))}
		}
		var netErr net.Error
		if errors.As(err, &netErr) {
			return &collectSkipError{reason: fmt.Sprintf("Tempo at %s did not answer; use --traces-tempo-url", backend)}
		}
	}
	return fmt.Errorf("the trace backend %s failed: %w", step, err)
}
