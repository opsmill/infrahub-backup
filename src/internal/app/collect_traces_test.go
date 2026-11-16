package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func TestValidateTraceOptions(t *testing.T) {
	defaults := func(in TraceOptionsInput) TraceOptionsInput {
		if in.Since == "" {
			in.Since = "2h"
		}
		if in.MinDuration == "" {
			in.MinDuration = "1s"
		}
		if in.MaxSizeMB == 0 {
			in.MaxSizeMB = 100
		}
		return in
	}
	given := func(names ...string) map[string]bool {
		m := map[string]bool{}
		for _, name := range names {
			m[name] = true
		}
		return m
	}

	tests := []struct {
		name    string
		in      TraceOptionsInput
		wantErr string
		check   func(t *testing.T, opts TraceOptions)
	}{
		{
			name: "not requested and nothing given",
			in:   TraceOptionsInput{},
			check: func(t *testing.T, opts TraceOptions) {
				if opts.Include {
					t.Error("Include = true")
				}
			},
		},
		{name: "option without include", in: TraceOptionsInput{Given: given("traces-since")}, wantErr: "--traces-since requires --include-traces"},
		{name: "tempo url without include", in: TraceOptionsInput{Given: given("traces-tempo-url")}, wantErr: "--traces-tempo-url requires --include-traces"},
		{
			name: "defaults",
			in:   TraceOptionsInput{Include: true},
			check: func(t *testing.T, opts TraceOptions) {
				if opts.Since != 2*time.Hour || opts.MinDuration != time.Second || opts.MaxSizeMB != 100 {
					t.Errorf("defaults = %+v", opts)
				}
			},
		},
		{name: "since with start", in: TraceOptionsInput{Include: true, Given: given("traces-since", "traces-start"), Start: "2026-11-04T14:00:00Z"}, wantErr: "--traces-since cannot be combined with --traces-start or --traces-end"},
		{name: "start alone", in: TraceOptionsInput{Include: true, Start: "2026-11-04T14:00:00Z"}, wantErr: "--traces-start and --traces-end must be given together"},
		{name: "end alone", in: TraceOptionsInput{Include: true, End: "2026-11-04T14:00:00Z"}, wantErr: "--traces-start and --traces-end must be given together"},
		{name: "start not before end", in: TraceOptionsInput{Include: true, Start: "2026-11-04T15:00:00Z", End: "2026-11-04T14:00:00Z"}, wantErr: "--traces-start must be before --traces-end"},
		{name: "bad start", in: TraceOptionsInput{Include: true, Start: "yesterday", End: "2026-11-04T14:00:00Z"}, wantErr: `invalid --traces-start "yesterday": use RFC 3339, for example 2026-11-04T14:00:00Z`},
		{name: "bad min duration", in: TraceOptionsInput{Include: true, MinDuration: "abc"}, wantErr: `invalid --traces-min-duration "abc": use a duration such as 1s or 500ms`},
		{name: "bad since", in: TraceOptionsInput{Include: true, Since: "0s"}, wantErr: `invalid --traces-since "0s"`},
		{name: "zero size", in: TraceOptionsInput{Include: true, MaxSizeMB: -1}, wantErr: "--traces-max-size must be a positive number of megabytes"},
		{name: "bad tempo url", in: TraceOptionsInput{Include: true, TempoURL: "tempo:3200"}, wantErr: `invalid --traces-tempo-url "tempo:3200": use an http or https URL`},
		{name: "same start and end", in: TraceOptionsInput{Include: true, Start: "2026-11-04T14:00:00Z", End: "2026-11-04T15:00:00+01:00"}, wantErr: "--traces-start must be before --traces-end"},
		{
			name: "absolute window and tempo url",
			in:   TraceOptionsInput{Include: true, Start: "2026-11-04T14:00:00Z", End: "2026-11-04T16:00:00+01:00", TempoURL: "https://tempo.example/"},
			check: func(t *testing.T, opts TraceOptions) {
				w := opts.window(time.Now())
				if w.Start.Format(time.RFC3339) != "2026-11-04T14:00:00Z" || w.End.Format(time.RFC3339) != "2026-11-04T15:00:00Z" {
					t.Errorf("window = %+v", w)
				}
				if opts.TempoURL != "https://tempo.example" {
					t.Errorf("TempoURL = %q", opts.TempoURL)
				}
			},
		},
		{
			name: "min duration zero is allowed",
			in:   TraceOptionsInput{Include: true, MinDuration: "0s"},
			check: func(t *testing.T, opts TraceOptions) {
				if opts.MinDuration != 0 {
					t.Errorf("MinDuration = %s", opts.MinDuration)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.in
			if in.Include {
				in = defaults(in)
			}
			opts, err := ValidateTraceOptions(in)
			if tt.wantErr != "" {
				if err == nil || !strings.HasPrefix(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("error = %v", err)
			}
			if tt.check != nil {
				tt.check(t, opts)
			}
		})
	}
}

func TestTraceOptionsWindow(t *testing.T) {
	now := time.Date(2026, 11, 4, 15, 30, 0, 500, time.UTC)
	w := TraceOptions{Since: 2 * time.Hour}.window(now)
	if w.End != now.Truncate(time.Second) || w.Start != w.End.Add(-2*time.Hour) {
		t.Errorf("window = %+v", w)
	}
}

func TestFormatTraceDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                       "0s",
		time.Second:             "1s",
		500 * time.Millisecond:  "500ms",
		2 * time.Hour:           "2h",
		90 * time.Second:        "90s",
		1500 * time.Microsecond: "1500us",
	} {
		if got := formatTraceDuration(d); got != want {
			t.Errorf("formatTraceDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestReadTraceTokens(t *testing.T) {
	env := func(values map[string]string) func(string) (string, bool) {
		return func(key string) (string, bool) {
			v, ok := values[key]
			return v, ok
		}
	}

	t.Run("variables set, no prompt", func(t *testing.T) {
		opts := TraceOptions{Include: true, TempoURL: "https://tempo.example"}
		err := ReadTraceTokens(&opts, TraceTokenSources{
			LookupEnv:    env(map[string]string{tracesTempoTokenEnv: "tempo-secret", tracesAPITokenEnv: " api-secret\n"}),
			IsTerminal:   func() bool { return true },
			ReadPassword: func(string) (string, error) { t.Fatal("prompted"); return "", nil },
		})
		if err != nil || opts.TempoToken != "tempo-secret" || opts.APIToken != "api-secret" {
			t.Errorf("opts = %+v, err = %v", opts, err)
		}
	})

	t.Run("terminal prompts, tempo token only with a tempo url", func(t *testing.T) {
		var prompts []string
		opts := TraceOptions{Include: true}
		err := ReadTraceTokens(&opts, TraceTokenSources{
			LookupEnv:  env(nil),
			IsTerminal: func() bool { return true },
			ReadPassword: func(prompt string) (string, error) {
				prompts = append(prompts, prompt)
				return "typed", nil
			},
		})
		if err != nil || opts.APIToken != "typed" || opts.TempoToken != "" {
			t.Errorf("opts = %+v, err = %v", opts, err)
		}
		if len(prompts) != 1 || !strings.Contains(prompts[0], "super_admin") {
			t.Errorf("prompts = %q", prompts)
		}
	})

	t.Run("prompt hides the user information of the url", func(t *testing.T) {
		var prompts []string
		opts := TraceOptions{Include: true, TempoURL: "https://user:pass@tempo.example"}
		_ = ReadTraceTokens(&opts, TraceTokenSources{
			LookupEnv:  env(nil),
			IsTerminal: func() bool { return true },
			ReadPassword: func(prompt string) (string, error) {
				prompts = append(prompts, prompt)
				return "", nil
			},
		})
		if len(prompts) != 2 || strings.Contains(prompts[0], "pass") || !strings.Contains(prompts[0], "https://tempo.example") {
			t.Errorf("prompts = %q", prompts)
		}
		if opts.TempoToken != "" || opts.APIToken != "" {
			t.Errorf("empty answers gave tokens: %+v", opts)
		}
	})

	t.Run("no terminal, no prompt", func(t *testing.T) {
		opts := TraceOptions{Include: true, TempoURL: "https://tempo.example"}
		err := ReadTraceTokens(&opts, TraceTokenSources{
			LookupEnv:    env(nil),
			IsTerminal:   func() bool { return false },
			ReadPassword: func(string) (string, error) { t.Fatal("prompted"); return "", nil },
		})
		if err != nil || opts.APIToken != "" || opts.TempoToken != "" {
			t.Errorf("opts = %+v, err = %v", opts, err)
		}
	})

	t.Run("not requested, nothing read", func(t *testing.T) {
		opts := TraceOptions{}
		err := ReadTraceTokens(&opts, TraceTokenSources{
			LookupEnv: func(string) (string, bool) { t.Fatal("read env"); return "", false },
		})
		if err != nil {
			t.Error(err)
		}
	})
}

// fakeTempo is an httptest Tempo that serves the recorded test data, or
// scripted answers.
type fakeTempo struct {
	mu       sync.Mutex
	searches []map[string]string
	auth     []string
	fetched  []string
	// search answers a search; nil serves search_response.json.
	search func(query map[string]string) (int, string)
	// fetch answers a trace fetch; nil serves trace_raw.json for every ID.
	fetch func(id string) (int, string)
}

func newFakeTempo(t *testing.T, ft *fakeTempo) *httptest.Server {
	t.Helper()
	searchBody := string(readTraceTestdata(t, "search_response.json"))
	traceBody := string(readTraceTestdata(t, "trace_raw.json"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ft.mu.Lock()
		ft.auth = append(ft.auth, r.Header.Get("Authorization"))
		ft.mu.Unlock()
		switch {
		case r.URL.Path == "/api/search":
			query := map[string]string{}
			for key := range r.URL.Query() {
				query[key] = r.URL.Query().Get(key)
			}
			ft.mu.Lock()
			ft.searches = append(ft.searches, query)
			ft.mu.Unlock()
			status, body := http.StatusOK, searchBody
			if ft.search != nil {
				status, body = ft.search(query)
			}
			w.WriteHeader(status)
			fmt.Fprint(w, body)
		case strings.HasPrefix(r.URL.Path, "/api/v2/traces/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/v2/traces/")
			if r.Header.Get("Accept") != "application/json" {
				w.WriteHeader(http.StatusNotAcceptable)
				return
			}
			ft.mu.Lock()
			ft.fetched = append(ft.fetched, id)
			ft.mu.Unlock()
			status, body := http.StatusOK, traceBody
			if ft.fetch != nil {
				status, body = ft.fetch(id)
			}
			w.WriteHeader(status)
			fmt.Fprint(w, body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// fakeTracesBackend adds the Tempo locator and the stdin exec to the fake
// collect backend.
type fakeTracesBackend struct {
	*fakeCollectBackend
	locate  func(ctx context.Context) (string, func(), error)
	stdinFn func(service string, command []string, stdin []byte) (string, string, error)
	execs   [][]string
	stopped bool
}

func (f *fakeTracesBackend) LocateTempo(ctx context.Context) (string, func(), error) {
	if f.locate == nil {
		return "", nil, errTempoNotFound
	}
	url, stop, err := f.locate(ctx)
	if stop == nil {
		stop = func() {}
	}
	return url, func() { f.stopped = true; stop() }, err
}

func (f *fakeTracesBackend) ExecStdinContext(ctx context.Context, timeout time.Duration, service string, command []string, stdin []byte) (string, string, error) {
	f.execs = append(f.execs, append([]string{service}, command...))
	if f.stdinFn == nil {
		return "", "", errors.New("container not running")
	}
	return f.stdinFn(service, command, stdin)
}

var (
	_ tempoLocator = (*fakeTracesBackend)(nil)
	_ stdinExecer  = (*fakeTracesBackend)(nil)
)

func newTracesContext(t *testing.T, backend EnvironmentBackend, opts TraceOptions) *collectContext {
	t.Helper()
	if opts.MaxSizeMB == 0 {
		opts.MaxSizeMB = 100
	}
	if opts.Since == 0 && opts.Start.IsZero() {
		opts.Since = 2 * time.Hour
	}
	if opts.MinDuration == 0 {
		opts.MinDuration = time.Second
	}
	opts.Include = true
	return &collectContext{
		ctx:       context.Background(),
		backend:   backend,
		opts:      CollectOptions{Traces: opts},
		bundleDir: t.TempDir(),
	}
}

func stackTempo(server *httptest.Server) *fakeTracesBackend {
	return &fakeTracesBackend{
		fakeCollectBackend: newFakeCollectBackend(),
		locate: func(context.Context) (string, func(), error) {
			return server.URL, nil, nil
		},
	}
}

func tracesDetails(t *testing.T, cc *collectContext) *traceCollectionDetails {
	t.Helper()
	details, ok := cc.details.(*traceCollectionDetails)
	if !ok {
		t.Fatalf("details = %#v, want *traceCollectionDetails", cc.details)
	}
	return details
}

func writtenTraces(t *testing.T, cc *collectContext) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(cc.bundleDir, tracesBundleDir))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

// captureLogs sends logrus output to a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previousOut, previousLevel := logrus.StandardLogger().Out, logrus.GetLevel()
	logrus.SetOutput(&buf)
	logrus.SetLevel(logrus.DebugLevel)
	t.Cleanup(func() {
		logrus.SetOutput(previousOut)
		logrus.SetLevel(previousLevel)
	})
	return &buf
}

func TestCollectTracesWritesSlowestFirst(t *testing.T) {
	ft := &fakeTempo{}
	server := newFakeTempo(t, ft)
	backend := stackTempo(server)
	cc := newTracesContext(t, backend, TraceOptions{})

	before := time.Now().UTC()
	if err := collectTraces(cc); err != nil {
		t.Fatalf("collectTraces() error = %v", err)
	}

	// Search parameters (research R3).
	if len(ft.searches) != 1 {
		t.Fatalf("searches = %d, want 1", len(ft.searches))
	}
	query := ft.searches[0]
	if query["q"] != "{ traceDuration > 1s }" || query["limit"] != "10000" {
		t.Errorf("search query = %v", query)
	}
	var start, end int64
	fmt.Sscan(query["start"], &start)
	fmt.Sscan(query["end"], &end)
	if end-start != int64((2*time.Hour)/time.Second) || end < before.Unix()-1 {
		t.Errorf("search window = %d..%d", start, end)
	}

	// Slowest first: flow-run (12 s), POST /graphql (3 s), task-run (1.5 s).
	// The 31-character ID of the search is padded to 32.
	want := []string{
		"5b0c30fdad6ed0e4637277ab4a9f1554",
		"0f459bd9861636b0ddf4ccb23154e120",
		"9949940536b9e55f8c62cd48c077580f",
	}
	if strings.Join(ft.fetched, ",") != strings.Join(want, ",") {
		t.Errorf("fetched = %v, want %v", ft.fetched, want)
	}
	files := writtenTraces(t, cc)
	wantFiles := []string{"0001-" + want[0] + ".json", "0002-" + want[1] + ".json", "0003-" + want[2] + ".json"}
	if strings.Join(files, ",") != strings.Join(wantFiles, ",") {
		t.Errorf("files = %v, want %v", files, wantFiles)
	}

	// The files are filtered.
	data, _ := os.ReadFile(filepath.Join(cc.bundleDir, tracesBundleDir, files[0]))
	if strings.Contains(string(data), "203.0.113.42") || !strings.Contains(string(data), "resourceSpans") {
		t.Errorf("written trace is not filtered: %s", data)
	}

	details := tracesDetails(t, cc)
	if details.Backend != server.URL || details.TracesFound != 3 || details.TracesWritten != 3 || details.TracesLeftOut != 0 ||
		details.MinDuration != "1s" || details.MinDurationUsed != "1s" || details.MaxSizeMB != 100 ||
		details.SearchLimitReached || details.TimeLimitReached || details.SampleRatio != nil {
		t.Errorf("details = %+v", details)
	}
	if !backend.stopped {
		t.Error("the Tempo locator was not stopped")
	}
	if len(backend.execs) != 0 {
		t.Errorf("exec without an API token: %v", backend.execs)
	}
}

func TestCollectTracesOutcomes(t *testing.T) {
	t.Run("tempo not found is skipped", func(t *testing.T) {
		backend := &fakeTracesBackend{fakeCollectBackend: newFakeCollectBackend()}
		cc := newTracesContext(t, backend, TraceOptions{})
		err := collectTraces(cc)
		var skip *collectSkipError
		if !errors.As(err, &skip) || !strings.Contains(skip.reason, "--traces-tempo-url") {
			t.Fatalf("error = %v, want a skip that names --traces-tempo-url", err)
		}
		if details := tracesDetails(t, cc); details.MaxSizeMB != 100 || details.Window.End.IsZero() {
			t.Errorf("details = %+v", details)
		}
	})

	t.Run("backend without locator is skipped", func(t *testing.T) {
		cc := newTracesContext(t, newFakeCollectBackend(), TraceOptions{})
		var skip *collectSkipError
		if err := collectTraces(cc); !errors.As(err, &skip) {
			t.Fatalf("error = %v, want skip", err)
		}
	})

	t.Run("kubernetes permission missing is skipped", func(t *testing.T) {
		backend := &fakeTracesBackend{
			fakeCollectBackend: newFakeCollectBackend(),
			locate: func(context.Context) (string, func(), error) {
				return "", nil, &tempoPermissionError{action: "forward a port to the Tempo pod", grant: "create on pods/portforward"}
			},
		}
		cc := newTracesContext(t, backend, TraceOptions{})
		var skip *collectSkipError
		err := collectTraces(cc)
		if !errors.As(err, &skip) || !strings.Contains(skip.reason, "pods/portforward") || !strings.Contains(skip.reason, "--traces-tempo-url") {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("tempo that does not answer is skipped", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		url := server.URL
		server.Close()
		backend := &fakeTracesBackend{
			fakeCollectBackend: newFakeCollectBackend(),
			locate:             func(context.Context) (string, func(), error) { return url, nil, nil },
		}
		cc := newTracesContext(t, backend, TraceOptions{})
		var skip *collectSkipError
		if err := collectTraces(cc); !errors.As(err, &skip) {
			t.Fatalf("error = %v, want skip", err)
		}
	})

	t.Run("search rejected is failed", func(t *testing.T) {
		ft := &fakeTempo{search: func(map[string]string) (int, string) { return http.StatusBadRequest, "invalid TraceQL query" }}
		cc := newTracesContext(t, stackTempo(newFakeTempo(t, ft)), TraceOptions{})
		err := collectTraces(cc)
		var skip *collectSkipError
		if err == nil || errors.As(err, &skip) || !strings.Contains(err.Error(), "invalid TraceQL query") {
			t.Fatalf("error = %v, want a failure with the answer", err)
		}
		if details := tracesDetails(t, cc); details.TracesWritten != 0 {
			t.Errorf("details = %+v", details)
		}
	})

	t.Run("no trace is success", func(t *testing.T) {
		ft := &fakeTempo{search: func(map[string]string) (int, string) { return http.StatusOK, `{"traces":[]}` }}
		cc := newTracesContext(t, stackTempo(newFakeTempo(t, ft)), TraceOptions{})
		if err := collectTraces(cc); err != nil {
			t.Fatal(err)
		}
		if files := writtenTraces(t, cc); files != nil {
			t.Errorf("files = %v, want no traces folder", files)
		}
		if details := tracesDetails(t, cc); details.TracesFound != 0 || details.TracesWritten != 0 {
			t.Errorf("details = %+v", details)
		}
	})

	t.Run("tempo stops answering after one trace is a partial failure", func(t *testing.T) {
		calls := 0
		ft := &fakeTempo{}
		raw := string(readTraceTestdata(t, "trace_raw.json"))
		ft.fetch = func(string) (int, string) {
			calls++
			if calls > 1 {
				return http.StatusInternalServerError, "ingester down"
			}
			return http.StatusOK, raw
		}
		cc := newTracesContext(t, stackTempo(newFakeTempo(t, ft)), TraceOptions{})
		err := collectTraces(cc)
		if err == nil || !strings.HasPrefix(err.Error(), "partial") {
			t.Fatalf("error = %v, want a partial failure", err)
		}
		details := tracesDetails(t, cc)
		if details.TracesWritten != 1 || details.TracesLeftOut != 2 || len(writtenTraces(t, cc)) != 1 {
			t.Errorf("details = %+v", details)
		}
	})
}

func TestCollectTracesSearchLimit(t *testing.T) {
	previous := tracesSearchLimit
	tracesSearchLimit = 2
	t.Cleanup(func() { tracesSearchLimit = previous })

	ft := &fakeTempo{search: func(query map[string]string) (int, string) {
		if query["q"] == "{ traceDuration > 1s }" {
			// Two results: the search limit.
			return http.StatusOK, `{"traces":[{"traceID":"a","durationMs":1500},{"traceID":"b","durationMs":3000}]}`
		}
		return http.StatusOK, `{"traces":[{"traceID":"5b0c30fdad6ed0e4637277ab4a9f1554","durationMs":12000}]}`
	}}
	cc := newTracesContext(t, stackTempo(newFakeTempo(t, ft)), TraceOptions{})
	if err := collectTraces(cc); err != nil {
		t.Fatal(err)
	}
	if len(ft.searches) != 2 || ft.searches[1]["q"] != "{ traceDuration > 2s }" {
		t.Errorf("searches = %v", ft.searches)
	}
	details := tracesDetails(t, cc)
	if !details.SearchLimitReached || details.MinDurationUsed != "2s" || details.TracesFound != 1 {
		t.Errorf("details = %+v", details)
	}
}

func TestCollectTracesSizeLimit(t *testing.T) {
	allow, _ := loadTraceAttributes()
	filtered, err := filterTrace(readTraceTestdata(t, "trace_raw.json"), allow)
	if err != nil {
		t.Fatal(err)
	}
	size := len(filtered)

	t.Run("stops at the first trace that does not fit", func(t *testing.T) {
		// Each trace of the fake Tempo has the same size, so a limit of two
		// and a half traces keeps the two slowest.
		tracesBytesPerMBForTest(t, size*5/2)
		ft := &fakeTempo{}
		cc := newTracesContext(t, stackTempo(newFakeTempo(t, ft)), TraceOptions{MaxSizeMB: 1})
		if err := collectTraces(cc); err != nil {
			t.Fatal(err)
		}
		details := tracesDetails(t, cc)
		if details.TracesWritten != 2 || details.TracesLeftOut != 1 || details.BytesWritten != int64(2*size) {
			t.Errorf("details = %+v", details)
		}
		files := writtenTraces(t, cc)
		if len(files) != 2 || !strings.HasPrefix(files[0], "0001-5b0c30fd") || !strings.HasPrefix(files[1], "0002-0f459bd9") {
			t.Errorf("files = %v, want the two slowest", files)
		}
		// A third fetch was made and its trace left out; no later trace was
		// fetched.
		if len(ft.fetched) != 3 {
			t.Errorf("fetched = %v", ft.fetched)
		}
	})

	t.Run("limit smaller than the slowest trace writes nothing", func(t *testing.T) {
		tracesBytesPerMBForTest(t, size-1)
		logs := captureLogs(t)
		cc := newTracesContext(t, stackTempo(newFakeTempo(t, &fakeTempo{})), TraceOptions{MaxSizeMB: 1})
		if err := collectTraces(cc); err != nil {
			t.Fatal(err)
		}
		details := tracesDetails(t, cc)
		if details.TracesWritten != 0 || details.TracesLeftOut != 3 || writtenTraces(t, cc) != nil {
			t.Errorf("details = %+v", details)
		}
		if !strings.Contains(logs.String(), "smaller than the slowest trace") {
			t.Errorf("logs = %s", logs)
		}
	})
}

// tracesBytesPerMBForTest makes one "megabyte" of --traces-max-size equal to
// bytes for the test.
func tracesBytesPerMBForTest(t *testing.T, bytes int) {
	t.Helper()
	previous := tracesMegabyte
	tracesMegabyte = int64(bytes)
	t.Cleanup(func() { tracesMegabyte = previous })
}

func TestCollectTracesTimeLimit(t *testing.T) {
	now := time.Date(2026, 11, 4, 15, 0, 0, 0, time.UTC)
	previousClock, previousLimit := tracesClock, tracesTimeLimit
	tracesTimeLimit = time.Minute
	t.Cleanup(func() { tracesClock, tracesTimeLimit = previousClock, previousLimit })

	// Each call of the clock moves it by 25 seconds: the start, then one
	// check per trace. The third check is past the one-minute limit.
	calls := 0
	tracesClock = func() time.Time {
		calls++
		return now.Add(time.Duration(calls-1) * 25 * time.Second)
	}

	logs := captureLogs(t)
	cc := newTracesContext(t, stackTempo(newFakeTempo(t, &fakeTempo{})), TraceOptions{})
	if err := collectTraces(cc); err != nil {
		t.Fatalf("error = %v, want success", err)
	}
	details := tracesDetails(t, cc)
	if !details.TimeLimitReached || details.TracesWritten != 2 || details.TracesLeftOut != 1 {
		t.Errorf("details = %+v", details)
	}
	if !strings.Contains(logs.String(), "Stopped fetching traces after 1m; wrote the 2 slowest of 3") {
		t.Errorf("logs = %s", logs)
	}
}

func TestCollectTracesCustomTempo(t *testing.T) {
	t.Run("bearer token, no locator, redacted backend", func(t *testing.T) {
		logs := captureLogs(t)
		ft := &fakeTempo{}
		server := newFakeTempo(t, ft)
		backend := &fakeTracesBackend{
			fakeCollectBackend: newFakeCollectBackend(),
			locate: func(context.Context) (string, func(), error) {
				t.Fatal("the locator was called with --traces-tempo-url")
				return "", nil, nil
			},
		}
		withUser := strings.Replace(server.URL, "http://", "http://user:pw@", 1)
		cc := newTracesContext(t, backend, TraceOptions{TempoURL: withUser, TempoToken: "tempo-secret-token"})
		if err := collectTraces(cc); err != nil {
			t.Fatal(err)
		}
		for _, header := range ft.auth {
			if header != "Bearer tempo-secret-token" {
				t.Errorf("Authorization = %q", header)
			}
		}
		details := tracesDetails(t, cc)
		if details.Backend != server.URL {
			t.Errorf("backend = %q, want %q", details.Backend, server.URL)
		}
		encoded, _ := json.Marshal(details)
		for _, secret := range []string{"tempo-secret-token", "pw"} {
			if strings.Contains(string(encoded), secret) || strings.Contains(logs.String(), secret) {
				t.Errorf("%q is in the details or the logs", secret)
			}
		}
	})

	t.Run("rejected token names the variable", func(t *testing.T) {
		ft := &fakeTempo{search: func(map[string]string) (int, string) { return http.StatusUnauthorized, "unauthorized" }}
		server := newFakeTempo(t, ft)
		cc := newTracesContext(t, &fakeTracesBackend{fakeCollectBackend: newFakeCollectBackend()}, TraceOptions{TempoURL: server.URL, TempoToken: "wrong"})
		err := collectTraces(cc)
		var skip *collectSkipError
		if err == nil || errors.As(err, &skip) || !strings.Contains(err.Error(), "rejected") || !strings.Contains(err.Error(), tracesTempoTokenEnv) {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("stack tempo gets no token", func(t *testing.T) {
		ft := &fakeTempo{}
		cc := newTracesContext(t, stackTempo(newFakeTempo(t, ft)), TraceOptions{TempoToken: "should-not-be-sent"})
		if err := collectTraces(cc); err != nil {
			t.Fatal(err)
		}
		for _, header := range ft.auth {
			if header != "" {
				t.Errorf("Authorization = %q, want none", header)
			}
		}
	})
}

func TestCollectTracesSampleRatio(t *testing.T) {
	enabled := string(readTraceTestdata(t, "sample_ratio_enabled.json"))
	disabled := string(readTraceTestdata(t, "sample_ratio_disabled.json"))
	answer := func(status int, body string) string {
		encoded, _ := json.Marshal(map[string]any{"status": status, "body": body})
		return string(encoded)
	}
	const token = "api-secret-token-123"

	tests := []struct {
		name     string
		token    string
		stdinFn  func(service string, command []string, stdin []byte) (string, string, error)
		wantLog  string
		wantExec bool
		check    func(t *testing.T, ratio *sampleRatio)
	}{
		{name: "no token", wantLog: "no API token given (set INFRAHUB_TRACES_API_TOKEN)"},
		{
			name: "enabled", token: token, wantExec: true,
			stdinFn: func(string, []string, []byte) (string, string, error) { return answer(200, enabled), "", nil },
			wantLog: "Recorded the trace sampling ratio: 1 in force, default 0.1",
			check: func(t *testing.T, ratio *sampleRatio) {
				if ratio == nil || !ratio.Enabled || *ratio.RatioInForce != 1 || *ratio.DefaultRatio != 0.1 ||
					*ratio.TemporaryRatio != 1 || *ratio.TemporaryRatioExpiresAt != "2026-11-04T16:00:00Z" {
					t.Errorf("ratio = %+v", ratio)
				}
			},
		},
		{
			name: "tracing off", token: token, wantExec: true,
			stdinFn: func(string, []string, []byte) (string, string, error) { return answer(200, disabled), "", nil },
			wantLog: "tracing is off",
			check: func(t *testing.T, ratio *sampleRatio) {
				if ratio == nil || ratio.Enabled || ratio.RatioInForce != nil || ratio.DefaultRatio != nil {
					t.Errorf("ratio = %+v", ratio)
				}
			},
		},
		{
			name: "401", token: token, wantExec: true,
			stdinFn: func(string, []string, []byte) (string, string, error) { return answer(401, "{}"), "", nil },
			wantLog: "the API token was rejected",
		},
		{
			name: "403", token: token, wantExec: true,
			stdinFn: func(string, []string, []byte) (string, string, error) { return answer(403, "{}"), "", nil },
			wantLog: "lacks the super_admin permission",
		},
		{
			name: "404", token: token, wantExec: true,
			stdinFn: func(string, []string, []byte) (string, string, error) { return answer(404, "{}"), "", nil },
			wantLog: "added in 1.12",
		},
		{
			name: "api not answering", token: token, wantExec: true,
			wantLog: "the Infrahub API did not answer",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logs := captureLogs(t)
			var gotStdin []byte
			backend := stackTempo(newFakeTempo(t, &fakeTempo{}))
			if tt.stdinFn != nil {
				backend.stdinFn = func(service string, command []string, stdin []byte) (string, string, error) {
					gotStdin = stdin
					return tt.stdinFn(service, command, stdin)
				}
			}
			cc := newTracesContext(t, backend, TraceOptions{APIToken: tt.token})
			if err := collectTraces(cc); err != nil {
				t.Fatalf("error = %v; the ratio must not change the status", err)
			}
			if !strings.Contains(logs.String(), tt.wantLog) {
				t.Errorf("logs do not contain %q:\n%s", tt.wantLog, logs)
			}
			if (len(backend.execs) > 0) != tt.wantExec {
				t.Errorf("execs = %v", backend.execs)
			}
			for _, exec := range backend.execs {
				if exec[0] != "infrahub-server" {
					t.Errorf("exec in %q", exec[0])
				}
				if strings.Contains(strings.Join(exec, " "), token) {
					t.Errorf("the token is in the command: %v", exec)
				}
			}
			if tt.stdinFn != nil && !strings.Contains(string(gotStdin), token) {
				t.Errorf("stdin = %q, want the token", gotStdin)
			}
			details := tracesDetails(t, cc)
			encoded, _ := json.Marshal(details)
			if strings.Contains(string(encoded), token) || strings.Contains(logs.String(), token) {
				t.Error("the token is in the details or the logs")
			}
			if tt.check != nil {
				tt.check(t, details.SampleRatio)
			} else if details.SampleRatio != nil {
				t.Errorf("sample_ratio = %+v, want none", details.SampleRatio)
			}
		})
	}
}

// TestCollectTracesManifestEntry runs the traces collector through the
// orchestrator and checks that the details reach bundle_information.json for
// a skipped run too.
func TestCollectTracesManifestEntry(t *testing.T) {
	backend := &fakeTracesBackend{fakeCollectBackend: newFakeCollectBackend()}
	opts := CollectOptions{OutputDir: t.TempDir(), LogLines: 10, Traces: TraceOptions{Include: true, Since: time.Hour, MinDuration: time.Second, MaxSizeMB: 5}}
	iops := &InfrahubOps{}
	if err := iops.runCollectPlan(backend, opts, []collector{tracesCollector(), metricsCollector()}); err != nil {
		t.Fatal(err)
	}
	archives, _ := filepath.Glob(filepath.Join(opts.OutputDir, "support_bundle_*.tar.gz"))
	if len(archives) != 1 {
		t.Fatalf("archives = %v", archives)
	}
	manifest, _ := extractBundleManifest(t, archives[0])
	if len(manifest.Collectors) != 2 {
		t.Fatalf("collectors = %+v", manifest.Collectors)
	}
	traces := manifest.Collectors[0]
	if traces.Name != "traces" || traces.Status != collectorStatusSkipped {
		t.Errorf("traces entry = %+v", traces)
	}
	details, ok := traces.Details.(map[string]any)
	if !ok || details["max_size_mb"] != float64(5) || details["min_duration"] != "1s" {
		t.Errorf("details = %#v", traces.Details)
	}
	if manifest.Collectors[1].Details != nil {
		t.Errorf("details leaked into the next entry: %+v", manifest.Collectors[1])
	}
}

func TestNormalizeTraceID(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want string
		ok   bool
	}{
		{in: "5B0C30FDAD6ED0E4637277AB4A9F1554", want: "5b0c30fdad6ed0e4637277ab4a9f1554", ok: true},
		{in: "f459bd9861636b0ddf4ccb23154e120", want: "0f459bd9861636b0ddf4ccb23154e120", ok: true},
		{in: "../../etc/passwd", ok: false},
		{in: "abc/def", ok: false},
		{in: "", ok: false},
		{in: strings.Repeat("a", 33), ok: false},
	} {
		got, ok := normalizeTraceID(tt.in)
		if ok != tt.ok || got != tt.want {
			t.Errorf("normalizeTraceID(%q) = %q, %v, want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestCollectTracesSkipsInvalidTraceID(t *testing.T) {
	ft := &fakeTempo{search: func(map[string]string) (int, string) {
		return http.StatusOK, `{"traces":[{"traceID":"../../outside","durationMs":9000},{"traceID":"5b0c30fdad6ed0e4637277ab4a9f1554","durationMs":3000}]}`
	}}
	cc := newTracesContext(t, stackTempo(newFakeTempo(t, ft)), TraceOptions{})
	if err := collectTraces(cc); err != nil {
		t.Fatal(err)
	}
	if len(ft.fetched) != 1 || ft.fetched[0] != "5b0c30fdad6ed0e4637277ab4a9f1554" {
		t.Errorf("fetched = %v, want only the valid ID", ft.fetched)
	}
	files := writtenTraces(t, cc)
	if len(files) != 1 || files[0] != "0002-5b0c30fdad6ed0e4637277ab4a9f1554.json" {
		t.Errorf("files = %v", files)
	}
	details := tracesDetails(t, cc)
	if details.TracesFound != 2 || details.TracesWritten != 1 || details.TracesLeftOut != 1 {
		t.Errorf("details = %+v", details)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(cc.bundleDir), "outside.json")); err == nil {
		t.Error("a file was written outside the bundle")
	}
}

func TestCollectTracesRequestTimeout(t *testing.T) {
	previous := tracesRequestTimeout
	tracesRequestTimeout = 100 * time.Millisecond
	t.Cleanup(func() { tracesRequestTimeout = previous })

	slow := func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}

	t.Run("search timeout is skipped", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(slow))
		t.Cleanup(server.Close)
		cc := newTracesContext(t, stackTempo(server), TraceOptions{})
		err := collectTraces(cc)
		var skip *collectSkipError
		if !errors.As(err, &skip) || !strings.Contains(skip.reason, "did not answer within 100ms") || !strings.Contains(skip.reason, "--traces-tempo-url") {
			t.Fatalf("error = %v, want a skip that names the time limit", err)
		}
	})

	t.Run("fetch timeout is a failure", func(t *testing.T) {
		searchBody := string(readTraceTestdata(t, "search_response.json"))
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/api/search" {
				fmt.Fprint(w, searchBody)
				return
			}
			slow(w, r)
		}))
		t.Cleanup(server.Close)
		cc := newTracesContext(t, stackTempo(server), TraceOptions{})
		err := collectTraces(cc)
		var skip *collectSkipError
		if err == nil || errors.As(err, &skip) || !strings.Contains(err.Error(), "timed out after") {
			t.Fatalf("error = %v, want a failure", err)
		}
	})
}

// A Tempo without /api/v2/traces (2.5.0 in the infrahub-observability chart)
// answers 404; the client then reads /api/traces/<id> and keeps using it.
func TestTempoClientFetchFallsBackToLegacyEndpoint(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/api/v2/") {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"batches":[]}`))
	}))
	defer server.Close()

	client := newTempoClient(server.URL, "")
	for _, id := range []string{"a1", "b2"} {
		body, err := client.fetch(context.Background(), id)
		if err != nil || string(body) != `{"batches":[]}` {
			t.Fatalf("fetch(%s) = %q, %v", id, body, err)
		}
	}
	want := []string{"/api/v2/traces/a1", "/api/traces/a1", "/api/traces/b2"}
	if strings.Join(paths, " ") != strings.Join(want, " ") {
		t.Errorf("paths = %v, want %v", paths, want)
	}
}

// When neither endpoint has the trace, fetch returns the error of the
// /api/v2/traces request and does not switch endpoints.
func TestTempoClientFetchMissingTrace(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	client := newTempoClient(server.URL, "")
	_, err := client.fetch(context.Background(), "a1")
	var status *tempoStatusError
	if !errors.As(err, &status) || status.status != http.StatusNotFound {
		t.Fatalf("error = %v, want HTTP 404", err)
	}
	if client.legacyFetch {
		t.Error("the client switched to /api/traces although it has no trace either")
	}
}

// When /api/v2/traces answers 404 and /api/traces fails for another reason,
// fetch returns the error of /api/traces, not the 404.
func TestTempoClientFetchLegacyError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v2/") {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "storage unavailable", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := newTempoClient(server.URL, "")
	_, err := client.fetch(context.Background(), "a1")
	var status *tempoStatusError
	if !errors.As(err, &status) || status.status != http.StatusInternalServerError {
		t.Fatalf("error = %v, want the HTTP 500 of /api/traces", err)
	}
	if client.legacyFetch {
		t.Error("the client switched to /api/traces although the request failed")
	}
}
