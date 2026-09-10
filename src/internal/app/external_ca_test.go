package app

import (
	"strings"
	"testing"
)

func newCAOps(backend EnvironmentBackend, caFile string) *InfrahubOps {
	cfg := &Configuration{}
	cfg.ExternalDB.Neo4jTLS.CAFile = caFile

	return &InfrahubOps{config: cfg, backend: backend, executor: NewCommandExecutor()}
}

const testCAPEM = "-----BEGIN CERTIFICATE-----\nMIIBtest\n-----END CERTIFICATE-----\n"

// TestInstallExternalCATrustCarriesTheDeploymentsCA is F5. The CA the
// deployment records in INFRAHUB_DB_TLS_CA_FILE was read into configuration and
// used only to append ",ca=…" to a log line, so a private-CA deployment failed
// at the first probe on an unknown issuer and --external-db-insecure-tls was
// the only way past. The file lives inside the deployment's own container and
// cannot be mounted, so the run carries the bytes across itself.
func TestInstallExternalCATrustCarriesTheDeploymentsCA(t *testing.T) {
	verifying := externalTLSDecision{Service: serviceNeo4j, Encrypt: true}

	t.Run("the certificate is read, written and imported", func(t *testing.T) {
		backend := &scriptedProbeBackend{answers: map[string][]string{"cat": {testCAPEM}}}
		env := newCAOps(backend, "/etc/ssl/infrahub/ca.pem").installExternalCATrust("probe-pod", verifying)

		if env == nil {
			t.Fatal("installExternalCATrust() = nil, want the JVM trust-store environment")
		}
		opts := env["JAVA_OPTS"]
		for _, want := range []string{
			"-Djavax.net.ssl.trustStore=" + externalCATrustStorePath,
			"-Djavax.net.ssl.trustStoreType=PKCS12",
		} {
			if !strings.Contains(opts, want) {
				t.Errorf("JAVA_OPTS = %q, want it to carry %q", opts, want)
			}
		}

		if got := backend.argvFor("cat"); len(got) != 1 || got[0][1] != "/etc/ssl/infrahub/ca.pem" {
			t.Errorf("read commands = %v, want the CA read from the path the deployment named", got)
		}
		if got := backend.argvFor("keytool"); len(got) != 1 {
			t.Fatalf("keytool commands = %v, want exactly one import", got)
		}
		imported := strings.Join(backend.argvFor("keytool")[0], " ")
		if !strings.Contains(imported, externalCAPEMPath) || !strings.Contains(imported, externalCATrustStorePath) {
			t.Errorf("keytool argv = %q, want the PEM imported into the store JAVA_OPTS names", imported)
		}
		if got := backend.argvFor("sh"); len(got) != 1 || !strings.Contains(strings.Join(got[0], " "), externalCAPEMPath) {
			t.Errorf("write commands = %v, want the PEM materialised in the workload", got)
		}
	})

	t.Run("a deployment that records no CA installs nothing", func(t *testing.T) {
		backend := &scriptedProbeBackend{}
		if env := newCAOps(backend, "").installExternalCATrust("probe-pod", verifying); env != nil {
			t.Errorf("installExternalCATrust() = %v, want nil: there is nothing to trust differently", env)
		}
		if len(backend.calls) != 0 {
			t.Errorf("calls = %v, want none: a deployment with no CA must issue no command for one (FR-015)", backend.calls)
		}
	})

	t.Run("an opted-out run installs nothing", func(t *testing.T) {
		backend := &scriptedProbeBackend{answers: map[string][]string{"cat": {testCAPEM}}}
		optedOut := externalTLSDecision{Service: serviceNeo4j, Encrypt: true, OptedOut: true}
		if env := newCAOps(backend, "/etc/ssl/infrahub/ca.pem").installExternalCATrust("probe-pod", optedOut); env != nil {
			t.Errorf("installExternalCATrust() = %v, want nil: nothing is verified, so nothing needs an anchor", env)
		}
	})

	t.Run("a file that is not a certificate degrades rather than failing the run", func(t *testing.T) {
		backend := &scriptedProbeBackend{answers: map[string][]string{"cat": {"not a certificate"}}}
		if env := newCAOps(backend, "/etc/ssl/infrahub/ca.pem").installExternalCATrust("probe-pod", verifying); env != nil {
			t.Errorf("installExternalCATrust() = %v, want nil so the client falls back to the image's trust store", env)
		}
		if got := backend.argvFor("keytool"); len(got) != 0 {
			t.Errorf("keytool ran on a file that holds no certificate: %v", got)
		}
	})

	t.Run("a workload without keytool degrades rather than failing the run", func(t *testing.T) {
		backend := &scriptedProbeBackend{
			answers: map[string][]string{"cat": {testCAPEM}},
			fails:   map[string]error{"keytool": errTestCommandMissing},
		}
		if env := newCAOps(backend, "/etc/ssl/infrahub/ca.pem").installExternalCATrust("probe-pod", verifying); env != nil {
			t.Errorf("installExternalCATrust() = %v, want nil: a missing tool must not fail a run that works today", env)
		}
	})
}

// TestDeploymentCAReadSurvivesTheScaleDown pins the ordering the capture path
// got wrong. A transient workload is created per role, and the PostgreSQL
// capture workload is created *after* an offline Neo4j capture has scaled
// task-manager to zero — so the CA read that builds that workload's client
// environment ran against a component that was no longer there. The install
// then degraded to the system trust store and pg_dump failed verify-full
// against a privately signed server, blaming the certificate rather than the
// container.
//
// The gate probes every external database while the deployment is still up, so
// the fix is that the read is remembered: a later workload uses what the gate
// read rather than asking a component that has since been stopped.
func TestDeploymentCAReadSurvivesTheScaleDown(t *testing.T) {
	backend := &scriptedProbeBackend{answers: map[string][]string{"cat": {testCAPEM}}}
	iops := newCAOps(backend, "")
	iops.config.ExternalDB.PostgresTLS.CAFile = "/etc/ssl/prefect/ca.pem"

	verifying := externalTLSDecision{Service: serviceTaskManagerDB, Encrypt: true}

	// The gate's probe, with task-manager still up.
	if got := iops.installExternalPostgresTrust("probe-pod", verifying); got != externalPostgresCAPEMPath {
		t.Fatalf("gate install = %q, want %q: the probe reads the CA while the deployment is up", got, externalPostgresCAPEMPath)
	}

	// stopAppContainers has since scaled task-manager to zero, so an exec into
	// it fails. The capture workload is created now and needs the same anchor.
	backend.fails = map[string]error{"cat": errTestCommandMissing}

	if got := iops.installExternalPostgresTrust("capture-pod", verifying); got != externalPostgresCAPEMPath {
		t.Errorf("capture install = %q, want %q: falling back to %q makes pg_dump fail verify-full against a privately signed server",
			got, externalPostgresCAPEMPath, postgresSystemTrustStore)
	}

	if got := backend.argvFor("cat"); len(got) != 1 {
		t.Errorf("CA reads = %d, want exactly one: the read is remembered rather than reissued per workload", len(got))
	}
	if got := backend.argvFor("sh"); len(got) != 2 {
		t.Errorf("PEM writes = %d, want one per workload: each pod needs the bytes in its own scratch volume", len(got))
	}
}

// TestWriteFileCommandRoundTrips pins the encoding the PEM travels in. The
// bytes go through the argument vector so the write uses the same bounded exec
// as everything else on this path (FR-025) rather than an unbounded write pipe.
func TestWriteFileCommandRoundTrips(t *testing.T) {
	command := writeFileCommand("/scratch/ca.pem", testCAPEM)
	if len(command) != 3 || command[0] != "sh" || command[1] != "-c" {
		t.Fatalf("writeFileCommand = %v, want an `sh -c` invocation", command)
	}
	if strings.Contains(command[2], "BEGIN CERTIFICATE") {
		t.Errorf("script = %q, want the payload encoded rather than pasted into the shell text", command[2])
	}
	if !strings.Contains(command[2], "base64 -d > '/scratch/ca.pem'") && !strings.Contains(command[2], "base64 -d > /scratch/ca.pem") {
		t.Errorf("script = %q, want the decoded bytes redirected to the named path", command[2])
	}
}
