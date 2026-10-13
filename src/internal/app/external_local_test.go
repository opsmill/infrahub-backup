package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// directTestHost is a fake host for direct mode: a namespace file in a
// temporary directory, a PATH lookup that answers from a set, a scratch parent
// of the test's own, and a configurable amount of free space. Children are real
// processes; command records what reached each one.
type directTestHost struct {
	*directModeHost

	mu    sync.Mutex
	argvs [][]string
}

func newDirectTestHost(t *testing.T, namespace string, missing ...string) *directTestHost {
	t.Helper()

	nsFile := filepath.Join(t.TempDir(), "namespace")
	if namespace != "" {
		if err := os.WriteFile(nsFile, []byte(namespace+"\n"), 0o600); err != nil {
			t.Fatalf("writing the namespace file = %v", err)
		}
	}

	absent := map[string]bool{}
	for _, tool := range missing {
		absent[tool] = true
	}

	scratchParent := t.TempDir()
	h := &directTestHost{}
	h.directModeHost = &directModeHost{
		namespaceFile: nsFile,
		lookPath: func(file string) (string, error) {
			if absent[file] {
				return "", exec.ErrNotFound
			}

			return "/usr/bin/" + file, nil
		},
		tempDir:   func() string { return scratchParent },
		freeSpace: func(string) (uint64, error) { return 1 << 40, nil },
		command: func(ctx context.Context, name string, args ...string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, name, args...)
			h.mu.Lock()
			h.argvs = append(h.argvs, append([]string{name}, args...))
			h.mu.Unlock()

			return cmd
		},
	}

	return h
}

// scratchEntries lists what is left under the scratch parent.
func (h *directTestHost) scratchEntries(t *testing.T) []string {
	t.Helper()

	entries, err := os.ReadDir(h.tempDir())
	if err != nil {
		t.Fatalf("listing the scratch parent = %v", err)
	}

	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names
}

// writeFakeTools puts scripts named after the database tools in a directory at
// the front of PATH, alongside a kubectl that records every call and fails.
// Each fake tool appends its argv and the credential variables it sees to two
// logs, so a test can tell where the credentials travelled.
func writeFakeTools(t *testing.T, tools map[string]string) (argvLog, envLog, kubectlLog string) {
	t.Helper()

	bin := t.TempDir()
	logs := t.TempDir()
	argvLog = filepath.Join(logs, "argv")
	envLog = filepath.Join(logs, "env")
	kubectlLog = filepath.Join(logs, "kubectl")

	write := func(name, body string) {
		script := fmt.Sprintf("#!/bin/sh\nprintf '%%s %%s\\n' %q \"$*\" >> %q\nprintf 'PGUSER=%%s PGPASSWORD=%%s NEO4J_USERNAME=%%s NEO4J_PASSWORD=%%s\\n' \"$PGUSER\" \"$PGPASSWORD\" \"$NEO4J_USERNAME\" \"$NEO4J_PASSWORD\" >> %q\n%s\n",
			name, argvLog, envLog, body)
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o755); err != nil {
			t.Fatalf("writing fake %s = %v", name, err)
		}
	}

	for name, body := range tools {
		write(name, body)
	}

	kubectl := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nexit 1\n", kubectlLog)
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(kubectl), 0o755); err != nil {
		t.Fatalf("writing the kubectl stub = %v", err)
	}

	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")

	return argvLog, envLog, kubectlLog
}

func readLog(t *testing.T, path string) string {
	t.Helper()

	content, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reading %s = %v", path, err)
	}

	return string(content)
}

// newDirectModeOps is an InfrahubOps on a Kubernetes backend in namespace
// "infrahub", with credentials for both databases and the given host.
func newDirectModeOps(host *directTestHost) *InfrahubOps {
	iops := NewInfrahubOps()
	iops.backend = newTestKubernetesBackend()
	iops.directModeHost = host.directModeHost
	iops.config.PostgresUsername = "prefect-user"
	iops.config.PostgresPassword = "pg-s3cr3t-value"
	iops.config.Neo4jUsername = "neo4j-user"
	iops.config.Neo4jPassword = "neo4j-s3cr3t-value"

	return iops
}

// TestDetectDirectMode is FR-032's detection: the namespace file must name the
// target namespace and every tool must be on PATH, and each refusal says why.
func TestDetectDirectMode(t *testing.T) {
	tests := []struct {
		name       string
		namespace  string
		missing    []string
		services   []string
		wantDirect bool
		wantReason string
	}{
		{name: "a pod in the target namespace with every tool", namespace: "infrahub", services: []string{serviceNeo4j, serviceTaskManagerDB}, wantDirect: true, wantReason: "target namespace"},
		{name: "no namespace file: not a pod", namespace: "", services: []string{serviceNeo4j}, wantReason: "not running as a Kubernetes pod"},
		{name: "a pod in another namespace", namespace: "monitoring", services: []string{serviceNeo4j}, wantReason: `namespace "monitoring", not in the target namespace "infrahub"`},
		{name: "a missing tool", namespace: "infrahub", missing: []string{"cypher-shell"}, services: []string{serviceNeo4j}, wantReason: "cypher-shell is not found on PATH"},
		{name: "two missing tools", namespace: "infrahub", missing: []string{"pg_dump", "psql"}, services: []string{serviceTaskManagerDB}, wantReason: "pg_dump, psql are not found on PATH"},
		{name: "a tool only the other database needs is not required", namespace: "infrahub", missing: []string{"neo4j-admin"}, services: []string{serviceTaskManagerDB}, wantDirect: true},
		{name: "no database named", namespace: "infrahub", wantReason: "no database"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host := newDirectTestHost(t, tt.namespace, tt.missing...)

			decision := host.detectDirectMode("infrahub", tt.services)
			if decision.direct != tt.wantDirect {
				t.Errorf("direct = %v, want %v (reason %q)", decision.direct, tt.wantDirect, decision.reason)
			}
			if !strings.Contains(decision.reason, tt.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", decision.reason, tt.wantReason)
			}
		})
	}

	t.Run("a scratch parent the shell commands cannot carry", func(t *testing.T) {
		host := newDirectTestHost(t, "infrahub")
		host.tempDir = func() string { return "/tmp/with space" }

		decision := host.detectDirectMode("infrahub", []string{serviceNeo4j})
		if decision.direct || !strings.Contains(decision.reason, "TMPDIR") {
			t.Errorf("decision = %+v, want not direct, naming TMPDIR", decision)
		}
	})
}

// TestLocalWorkloadTranslate maps the Job's scratch mount onto the run's
// directory wherever it is a path of its own, and nowhere else.
func TestLocalWorkloadTranslate(t *testing.T) {
	w := &localWorkload{scratch: "/tmp/xdb-1"}

	tests := map[string]string{
		"/scratch":                              "/tmp/xdb-1",
		"/scratch/infrahubops":                  "/tmp/xdb-1/infrahubops",
		"--temp-path=/scratch/staging":          "--temp-path=/tmp/xdb-1/staging",
		"rm -rf /scratch/a /scratch/b":          "rm -rf /tmp/xdb-1/a /tmp/xdb-1/b",
		"-C /scratch /scratch/x":                "-C /tmp/xdb-1 /tmp/xdb-1/x",
		"base64 -d > '/scratch/ca.pem'":         "base64 -d > '/tmp/xdb-1/ca.pem'",
		"-Djavax.net.ssl.trustStore=/scratch/x": "-Djavax.net.ssl.trustStore=/tmp/xdb-1/x",
		"/data/scratch/x":                       "/data/scratch/x",
		"/data/scratch/scratch":                 "/data/scratch/scratch",
		"/data/scratch/scratch/x":               "/data/scratch/scratch/x",
		"/scratch/scratch":                      "/tmp/xdb-1/scratch",
		"/data/scratch /scratch":                "/data/scratch /tmp/xdb-1",
		"/scratchpad":                           "/scratchpad",
		"abc+/scratch+def":                      "abc+/scratch+def",
		"no path at all":                        "no path at all",
	}

	for in, want := range tests {
		if got := w.translate(in); got != want {
			t.Errorf("translate(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLocalWorkloadEnvironmentDropsWhatClientsReadOnTheirOwn holds the child's
// environment to what the Job's container would have: a PGSERVICE or
// PGPASSFILE in this process's environment would point libpq at a connection
// or a password the run never chose, so neither reaches the child, while the
// image's own tool variables, the credentials and the sequence's variables do.
func TestLocalWorkloadEnvironmentDropsWhatClientsReadOnTheirOwn(t *testing.T) {
	t.Setenv("PGSERVICE", "inherited-service")
	t.Setenv("PGPASSFILE", "/inherited/.pgpass")
	t.Setenv("NEO4J_USERNAME", "inherited-user")
	t.Setenv("JAVA_TOOL_OPTIONS", "-Dinherited=true")
	t.Setenv("NEO4J_HOME", "/opt/neo4j")
	t.Setenv("NEO4J_CONF", "/opt/neo4j/conf")
	t.Setenv("NEO4J_LOGS", "/opt/neo4j/logs")
	t.Setenv("JAVA_HOME", "/usr/lib/jvm/java-21-openjdk")

	w := &localWorkload{
		scratch:     "/tmp/xdb-1",
		credentials: map[string]string{"PGUSER": "prefect-user", "PGPASSWORD": "pg-s3cr3t-value"},
	}
	env := w.environment(map[string]string{"PGSSLMODE": "require", "JAVA_OPTS": "-Dx=/scratch/ts"})

	values := map[string][]string{}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = append(values[key], value)
	}

	for _, key := range []string{"PGSERVICE", "PGPASSFILE", "NEO4J_USERNAME", "JAVA_TOOL_OPTIONS"} {
		if got, ok := values[key]; ok {
			t.Errorf("%s reached the child as %q, want it dropped", key, got)
		}
	}

	want := map[string]string{
		"PATH":       os.Getenv("PATH"),
		"NEO4J_HOME": "/opt/neo4j",
		"NEO4J_CONF": "/opt/neo4j/conf",
		"NEO4J_LOGS": "/opt/neo4j/logs",
		"JAVA_HOME":  "/usr/lib/jvm/java-21-openjdk",
		"PGUSER":     "prefect-user",
		"PGPASSWORD": "pg-s3cr3t-value",
		"PGSSLMODE":  "require",
		"JAVA_OPTS":  "-Dx=/tmp/xdb-1/ts",
	}
	for key, value := range want {
		if got := values[key]; len(got) != 1 || got[0] != value {
			t.Errorf("%s = %q, want exactly [%q]", key, got, value)
		}
	}
}

// TestDirectModeCreatesNothingInTheNamespace is T171: when direct mode is
// taken the gate hands back operations served by this process, and not one
// kubectl call is made — no reaper listing, no Job, no Secret. A development
// build with no --external-db-image is not refused, because no image is run.
func TestDirectModeCreatesNothingInTheNamespace(t *testing.T) {
	withBuildVersion(t, "")
	_, _, kubectlLog := writeFakeTools(t, map[string]string{})

	host := newDirectTestHost(t, "infrahub")
	iops := newDirectModeOps(host)

	ops, err := iops.externalCaptureOps(serviceTaskManagerDB)
	if err != nil {
		t.Fatalf("externalCaptureOps() = %v, want direct-mode operations", err)
	}

	target, release, err := ops.startProbe(serviceTaskManagerDB, 1)
	if err != nil {
		t.Fatalf("startProbe() = %v", err)
	}
	if !strings.HasPrefix(target, localTargetPrefix) {
		t.Errorf("target = %q, want a local target", target)
	}
	release()

	if iops.externalRun != "" || iops.transientStraysReaped {
		t.Errorf("run id %q, reaped %v: want neither — nothing is created, so nothing is labelled or swept", iops.externalRun, iops.transientStraysReaped)
	}

	// Positive control for the log: the stub records when it is called.
	if calls := readLog(t, kubectlLog); calls != "" {
		t.Errorf("kubectl was called in direct mode:\n%s\nwant no call at all", calls)
	}
	if err := exec.Command("kubectl", "version").Run(); err == nil {
		t.Fatal("the kubectl stub exited 0, want it to fail")
	}
	if !strings.Contains(readLog(t, kubectlLog), "version") {
		t.Fatal("the kubectl stub did not record its own call; the assertion above observed nothing")
	}
}

// TestDirectModeOutsideTheNamespaceKeepsTheJobPath is the other half: a run
// that is not a pod in the namespace goes on to the Job path, whose first
// steps are the image refusal (a development build here) — so direct mode was
// not taken.
func TestDirectModeOutsideTheNamespaceKeepsTheJobPath(t *testing.T) {
	withBuildVersion(t, "")
	writeFakeTools(t, map[string]string{})

	host := newDirectTestHost(t, "")
	iops := newDirectModeOps(host)

	_, err := iops.externalCaptureOps(serviceTaskManagerDB)
	if err == nil || !strings.Contains(err.Error(), "--external-db-image") {
		t.Fatalf("externalCaptureOps() = %v, want the Job path's development-build refusal", err)
	}
}

// TestDirectModeIsStillRefusedForCollect is ADR 0003: infrahub-collect is
// turned back before the direct-mode decision, so it captures in neither mode.
func TestDirectModeIsStillRefusedForCollect(t *testing.T) {
	writeFakeTools(t, map[string]string{})

	host := newDirectTestHost(t, "infrahub")
	iops := newDirectModeOps(host)
	iops.forbidExternalCapture()

	if _, err := iops.externalCaptureOps(serviceNeo4j); err == nil || !strings.Contains(err.Error(), "infrahub-collect") {
		t.Fatalf("externalCaptureOps() = %v, want the collect refusal", err)
	}
	if len(iops.directModeDecisions) != 0 {
		t.Errorf("direct mode was decided (%v) for a run that may not capture", iops.directModeDecisions)
	}
}

// TestDirectModeProbeRunsLocallyWithCredentialsInTheEnvironment is T170 and
// FR-014 through the real probe sequence: the PostgreSQL probe runs pg_dump and
// psql as children of this process, they read the credentials from their
// environment, and no argument any child received carries a credential.
func TestDirectModeProbeRunsLocallyWithCredentialsInTheEnvironment(t *testing.T) {
	argvLog, envLog, kubectlLog := writeFakeTools(t, map[string]string{
		"pg_dump": `echo "pg_dump (PostgreSQL) 18.1"`,
		"psql":    `case "$*" in *server_version*) echo "18.1" ;; *) echo "1048576" ;; esac`,
	})

	host := newDirectTestHost(t, "infrahub")
	iops := newDirectModeOps(host)

	ops, err := iops.externalCaptureOps(serviceTaskManagerDB)
	if err != nil {
		t.Fatalf("externalCaptureOps() = %v", err)
	}

	target, release, err := ops.startProbe(serviceTaskManagerDB, 1)
	if err != nil {
		t.Fatalf("startProbe() = %v", err)
	}
	defer release()

	endpoint := &DatabaseEndpoint{Service: serviceTaskManagerDB, Hosts: []HostPort{{Host: "db.example.net", Port: 5432}}, Database: "prefect"}
	facts, err := ops.probe(target, endpoint, externalTLSDecision{Service: serviceTaskManagerDB, Encrypt: true, OptedOut: true})
	if err != nil {
		t.Fatalf("probe() = %v", err)
	}
	if facts.UtilityVersion != "18.1" || facts.ServerVersion != "18.1" || facts.StoreSizeBytes != 1048576 {
		t.Errorf("facts = %+v, want utility 18.1, server 18.1, size 1048576", facts)
	}

	for _, secret := range []string{"pg-s3cr3t-value", "neo4j-s3cr3t-value"} {
		if argv := readLog(t, argvLog); strings.Contains(argv, secret) {
			t.Errorf("a child's argv carried a credential:\n%s", argv)
		}
		for _, argv := range host.argvs {
			if strings.Contains(strings.Join(argv, " "), secret) {
				t.Errorf("argv %q carries a credential", argv)
			}
		}
	}
	if env := readLog(t, envLog); !strings.Contains(env, "PGUSER=prefect-user PGPASSWORD=pg-s3cr3t-value") {
		t.Errorf("the children's environment = %q, want the PostgreSQL credentials there", env)
	}
	if len(host.argvs) < 3 {
		t.Errorf("ran %d children, want the utility version, the server version and the size", len(host.argvs))
	}
	if calls := readLog(t, kubectlLog); calls != "" {
		t.Errorf("kubectl was called:\n%s", calls)
	}
}

// TestDirectModeScratchIsRemovedOnEveryExitPath covers a capture that succeeds
// (artifact copied out, scratch removed on release), one that fails (scratch
// removed on release), one whose scratch does not fit (never left behind), and
// the run's backstop for a workload nobody released.
func TestDirectModeScratchIsRemovedOnEveryExitPath(t *testing.T) {
	writeFakeTools(t, map[string]string{})

	t.Run("success: the artifact is copied out and the scratch removed", func(t *testing.T) {
		host := newDirectTestHost(t, "infrahub")
		iops := newDirectModeOps(host)
		ops, err := iops.externalCaptureOps(serviceTaskManagerDB)
		if err != nil {
			t.Fatalf("externalCaptureOps() = %v", err)
		}

		target, release, err := ops.startCapture(serviceTaskManagerDB, 1<<20)
		if err != nil {
			t.Fatalf("startCapture() = %v", err)
		}

		if _, err := iops.execBoundedAgainst(externalDBControlBound(iops.config), "writing", "test", serviceTaskManagerDB,
			[]string{"sh", "-c", "printf dump-bytes > " + externalPostgresDumpFile}, &ExecOptions{Pod: target}); err != nil {
			t.Fatalf("writing into scratch = %v", err)
		}

		dest := filepath.Join(t.TempDir(), "backup", "prefect.dump")
		if err := iops.copyFromTransientWorkload(target, externalPostgresDumpFile, dest); err != nil {
			t.Fatalf("copyFromTransientWorkload() = %v", err)
		}
		if content, err := os.ReadFile(dest); err != nil || string(content) != "dump-bytes" {
			t.Errorf("copied artifact = %q, %v; want %q", content, err, "dump-bytes")
		}

		release()
		if left := host.scratchEntries(t); len(left) != 0 {
			t.Errorf("scratch left after release: %v", left)
		}
	})

	t.Run("failure: a failed command, then release, leaves nothing", func(t *testing.T) {
		host := newDirectTestHost(t, "infrahub")
		iops := newDirectModeOps(host)
		ops, _ := iops.externalCaptureOps(serviceTaskManagerDB)

		target, release, err := ops.startCapture(serviceTaskManagerDB, 1<<20)
		if err != nil {
			t.Fatalf("startCapture() = %v", err)
		}

		_, err = iops.execBoundedAgainst(externalDBControlBound(iops.config), "failing", "test", serviceTaskManagerDB,
			[]string{"sh", "-c", "echo partial > /scratch/partial; echo boom >&2; exit 3"}, &ExecOptions{Pod: target})
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err = %v, want the failure with its stderr", err)
		}

		release()
		if left := host.scratchEntries(t); len(left) != 0 {
			t.Errorf("scratch left after a failure: %v", left)
		}
	})

	t.Run("a capture that does not fit is refused and leaves nothing", func(t *testing.T) {
		host := newDirectTestHost(t, "infrahub")
		host.freeSpace = func(string) (uint64, error) { return 1 << 30, nil }
		iops := newDirectModeOps(host)
		ops, _ := iops.externalCaptureOps(serviceTaskManagerDB)

		_, _, err := ops.startCapture(serviceTaskManagerDB, 100<<30)
		if err == nil || !strings.Contains(err.Error(), "TMPDIR") || !strings.Contains(err.Error(), "--external-db-scratch-size") {
			t.Fatalf("err = %v, want a refusal naming TMPDIR and the override", err)
		}
		if left := host.scratchEntries(t); len(left) != 0 {
			t.Errorf("scratch left after the refusal: %v", left)
		}
	})

	t.Run("an unknown store size still names the override (FR-022)", func(t *testing.T) {
		host := newDirectTestHost(t, "infrahub")
		iops := newDirectModeOps(host)
		ops, _ := iops.externalCaptureOps(serviceTaskManagerDB)

		if _, _, err := ops.startCapture(serviceTaskManagerDB, 0); err == nil || !strings.Contains(err.Error(), "--external-db-scratch-size") {
			t.Fatalf("err = %v, want the FR-022 refusal", err)
		}
	})

	t.Run("the run's backstop removes a workload nobody released", func(t *testing.T) {
		host := newDirectTestHost(t, "infrahub")
		iops := newDirectModeOps(host)
		ops, _ := iops.externalCaptureOps(serviceTaskManagerDB)

		if _, _, err := ops.startRestore(serviceTaskManagerDB); err != nil {
			t.Fatalf("startRestore() = %v", err)
		}
		if len(host.scratchEntries(t)) != 1 {
			t.Fatalf("want one scratch directory while the restore holds it")
		}

		iops.releaseTransientWorkloads()
		if left := host.scratchEntries(t); len(left) != 0 {
			t.Errorf("scratch left after the backstop: %v", left)
		}
	})
}

// TestDirectModeRestoreDumpFitReadsLocalFreeSpace is the restore's fit check in
// direct mode: the free space of the scratch filesystem, not the Job's sized
// volume, and the copy into scratch when it fits.
func TestDirectModeRestoreDumpFitReadsLocalFreeSpace(t *testing.T) {
	writeFakeTools(t, map[string]string{})

	host := newDirectTestHost(t, "infrahub")
	host.freeSpace = func(string) (uint64, error) { return 1 << 20, nil }
	iops := newDirectModeOps(host)
	ops, _ := iops.externalCaptureOps(serviceTaskManagerDB)

	target, release, err := ops.startRestore(serviceTaskManagerDB)
	if err != nil {
		t.Fatalf("startRestore() = %v", err)
	}
	defer release()

	if err := iops.refuseUnfittableRestoreDump(target, 2<<20); err == nil || !strings.Contains(err.Error(), "TMPDIR") {
		t.Errorf("refuseUnfittableRestoreDump() = %v, want a refusal naming TMPDIR", err)
	}
	if err := iops.refuseUnfittableRestoreDump(target, 1<<10); err != nil {
		t.Errorf("refuseUnfittableRestoreDump() = %v, want nil for a dump that fits", err)
	}

	dump := filepath.Join(t.TempDir(), "prefect.dump")
	if err := os.WriteFile(dump, []byte("restore-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := iops.copyToTransientWorkload(target, dump, externalPostgresRestoreFile); err != nil {
		t.Fatalf("copyToTransientWorkload() = %v", err)
	}
	staged := filepath.Join(iops.localWorkloadFor(target).scratch, filepath.Base(externalPostgresRestoreFile))
	if content, err := os.ReadFile(staged); err != nil || string(content) != "restore-bytes" {
		t.Errorf("staged dump = %q, %v; want it in the run's scratch", content, err)
	}

	if err := iops.copyToTransientWorkload(target, dump, "/etc/passwd"); err == nil {
		t.Error("a copy outside the scratch space was accepted, want a refusal")
	}
}

// TestDirectModeCATrustLandsInScratch is the CA path in direct mode: the
// deployment's certificate is written into the run's scratch directory and
// imported from there by the local keytool, with the trust store path the JVM
// is pointed at translated the same way.
func TestDirectModeCATrustLandsInScratch(t *testing.T) {
	argvLog, _, _ := writeFakeTools(t, map[string]string{
		// keytool proves the PEM is where -file says, then creates the store.
		"keytool": `file=""; store=""; while [ $# -gt 0 ]; do case "$1" in -file) file="$2"; shift ;; -keystore) store="$2"; shift ;; esac; shift; done; grep -q "BEGIN CERTIFICATE" "$file" && : > "$store"`,
	})

	host := newDirectTestHost(t, "infrahub")
	iops := newDirectModeOps(host)
	iops.config.ExternalDB.Neo4jTLS.CAFile = "/etc/infrahub/ca.pem"
	iops.deploymentCAFiles = map[string]string{"infrahub-server:/etc/infrahub/ca.pem": pemCertificateMarker + "\nMIIB\n-----END CERTIFICATE-----\n"}

	ops, err := iops.externalCaptureOps(serviceNeo4j)
	if err != nil {
		t.Fatalf("externalCaptureOps() = %v", err)
	}
	target, release, err := ops.startRestore(serviceNeo4j)
	if err != nil {
		t.Fatalf("startRestore() = %v", err)
	}
	scratch := iops.localWorkloadFor(target).scratch

	env := iops.installExternalCATrust(target, externalTLSDecision{Service: serviceNeo4j, Encrypt: true})
	if env == nil {
		t.Fatalf("installExternalCATrust() = nil, want the JVM trust environment (keytool log:\n%s)", readLog(t, argvLog))
	}
	if _, err := os.Stat(filepath.Join(scratch, filepath.Base(externalCATrustStorePath))); err != nil {
		t.Errorf("trust store not in scratch: %v", err)
	}

	// The environment as the child receives it points at the scratch copy.
	childEnv := strings.Join(iops.localWorkloadFor(target).environment(env), "\n")
	if !strings.Contains(childEnv, "-Djavax.net.ssl.trustStore="+filepath.Join(scratch, filepath.Base(externalCATrustStorePath))) {
		t.Errorf("JAVA_OPTS reaching the child does not name the scratch trust store:\n%s", childEnv)
	}

	release()
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("scratch %s still exists after release (err %v)", scratch, err)
	}
}

// TestLocalFreeSpaceReadsTheHost is the real statfs read, on the host the test
// runs on: a temporary directory has some space free.
func TestLocalFreeSpaceReadsTheHost(t *testing.T) {
	free, err := localFreeSpace(t.TempDir())
	if err != nil {
		t.Fatalf("localFreeSpace() = %v", err)
	}
	if free == 0 {
		t.Error("localFreeSpace() = 0, want the free space of the test's temporary directory")
	}
}
