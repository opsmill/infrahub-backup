package app

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"
)

// The deployment records the certificate authority its own database connections
// trust in INFRAHUB_DB_TLS_CA_FILE, and discovery reads that setting out of the
// infrahub-server container. It was read and then used for one thing: appending
// `,ca=…` to the endpoint report. Nothing carried it to the client that opens
// the connection, so a deployment whose database certificate is issued by a
// private CA — the ordinary managed-database case — failed at the first probe on
// an unknown issuer, and the only way past it was --external-db-insecure-tls,
// which drops verification altogether rather than making it succeed.
//
// The file cannot be mounted. Its path names a location inside the
// *deployment's* own container: a pod's volumes address the cluster's storage
// rather than another pod's filesystem, and the operator's host does not have
// the file either. So the run carries the bytes across itself, in two steps:
//
//   - read the PEM out of the container whose environment named it, with the
//     same bounded exec discovery already uses to read that environment;
//   - install it where the client will look. The clients here are JVM programs —
//     `cypher-shell`, which every Bolt probe statement runs through — and a JVM
//     does not read a PEM. It reads a keystore, so the PEM is imported into one
//     inside the workload with the `keytool` that ships beside the JVM, and
//     JAVA_OPTS points the client at it.
//
// PostgreSQL is the same problem with a different client and a different
// setting, and it was the worse half: `PGSSLMODE=verify-full` was set with no
// `PGSSLROOTCERT` beside it, so libpq looked for `~/.postgresql/root.crt` in a
// transient workload that has no such file and refused the connection before
// reaching the server. The secure default was not merely unhelpful, it was
// unusable, and the only escape — --external-db-insecure-tls — gives up
// verification entirely. Two things fix it:
//
//   - the deployment's own anchor, where it names one. The task-manager
//     connection URL's `sslrootcert` is the PostgreSQL counterpart of
//     INFRAHUB_DB_TLS_CA_FILE, read out of the task-manager container the same
//     way and written into the workload as a PEM — which is what libpq reads,
//     so there is no keystore step on this arm.
//   - `sslrootcert=system` where it names none, which points libpq at the
//     image's own trust store. That is what makes verify-full a mode a publicly
//     issued certificate can actually satisfy, and it is the same place the
//     Neo4j arm leaves its client when there is nothing to install.
//
// What this covers is exactly what the settings describe. INFRAHUB_DB_TLS_* are
// the settings of Infrahub's own driver connection to Neo4j, so they are the
// trust anchor for the Bolt connections this run opens and for nothing else:
// Neo4j's backup protocol authenticates through the server's own backup SSL
// policy rather than through this file.
//
// Every step degrades rather than fails. A CA that cannot be read, written or
// imported leaves the client on the image's default trust store — which is
// where it is today — so the worst outcome of a workload without `keytool`, or
// a JVM launcher that ignores JAVA_OPTS, is the failure the operator already
// gets, never a run that worked yesterday and stops working.

const (
	// externalCAPEMPath and externalCATrustStorePath sit on the scratch volume
	// rather than in the image's own filesystem, which is the one directory the
	// transient workload is guaranteed to be able to write to.
	externalCAPEMPath        = transientScratchPath + "/infrahub-external-ca.pem"
	externalCATrustStorePath = transientScratchPath + "/infrahub-external-ca.p12"

	// externalPostgresCAPEMPath is the same file for the PostgreSQL arm, whose
	// client reads the PEM directly and needs no keystore. It is named
	// separately from externalCAPEMPath because the two hold different
	// deployments' anchors — INFRAHUB_DB_TLS_CA_FILE and the task-manager
	// connection's sslrootcert are not the same setting and need not be the
	// same certificate.
	externalPostgresCAPEMPath = transientScratchPath + "/infrahub-external-postgres-ca.pem"

	// externalCATrustStoreAlias names the single entry the store holds.
	externalCATrustStoreAlias = "infrahub-external-ca"

	// postgresSystemTrustStore is libpq's own name for "verify against the
	// image's trust store" (PostgreSQL 16 and later, which the pinned
	// defaultExternalDBImagePostgres is). Naming it explicitly is what makes
	// verify-full satisfiable: left unset, libpq looks for
	// ~/.postgresql/root.crt and fails on its absence rather than falling back
	// to anything.
	postgresSystemTrustStore = "system"

	// externalCATrustStorePassword protects a keystore whose entire contents is
	// one public certificate the deployment already publishes to every client
	// it has. It is not a credential, and FR-014 — which keeps passwords off
	// command lines — is about the credentials that authenticate to the
	// database. `keytool` requires a store password whether or not there is a
	// secret to protect, so this is a fixed, documented literal rather than
	// something a caller could mistake for one.
	externalCATrustStorePassword = "infrahub-ops"
)

// pemCertificateMarker is the one thing a file has to contain to be the CA the
// setting claims it is. Checking for it is what turns "the path was wrong, or
// the key was mounted instead" into a message naming the file, rather than an
// import failure from inside the workload.
const pemCertificateMarker = "-----BEGIN CERTIFICATE-----"

// externalCATrustEnv is the environment that points a JVM client at the store
// installed below. JAVA_OPTS is the documented way to pass JVM flags to the
// Neo4j command-line tooling, and reaches the process as one `env VAR=value`
// argument (see prepareCommand) rather than through a shell, so the spaces in
// it need no quoting.
//
// The store replaces the JVM's default trust anchors rather than adding to
// them, and that is the correct reading of the setting: INFRAHUB_DB_TLS_CA_FILE
// is the anchor Infrahub's own driver trusts for this database, so a store
// holding exactly it gives this run the same trust the deployment has.
func externalCATrustEnv() map[string]string {
	return map[string]string{
		"JAVA_OPTS": strings.Join([]string{
			"-Djavax.net.ssl.trustStore=" + externalCATrustStorePath,
			"-Djavax.net.ssl.trustStoreType=PKCS12",
			"-Djavax.net.ssl.trustStorePassword=" + externalCATrustStorePassword,
		}, " "),
	}
}

// writeFileCommand materialises content at path inside a workload without a
// stdin pipe: the bytes travel base64-encoded in the argument vector, which is
// what lets the write go through the same bounded exec every other call on this
// path uses (FR-025) instead of an unbounded write pipe.
func writeFileCommand(path, content string) []string {
	encoded := base64.StdEncoding.EncodeToString([]byte(content))

	return []string{"sh", "-c", fmt.Sprintf("printf %%s %s | base64 -d > %s", shellQuote(encoded), shellQuote(path))}
}

// importCACommand imports the PEM written above into the keystore the JVM will
// read. -noprompt is required because the exec has no terminal to answer the
// "trust this certificate?" question on.
func importCACommand() []string {
	return []string{
		"keytool", "-importcert", "-noprompt",
		"-alias", externalCATrustStoreAlias,
		"-file", externalCAPEMPath,
		"-keystore", externalCATrustStorePath,
		"-storetype", "PKCS12",
		"-storepass", externalCATrustStorePassword,
	}
}

// readDeploymentCAFile reads the CA certificate the deployment configured out of
// the container whose environment named it — infrahub-server for Neo4j, the
// same container ensureNeo4jDiscovery reads the address settings from, and
// task-manager for PostgreSQL, whose connection URL is where its
// `sslrootcert` came from. The path means nothing outside the container that
// stated it, so the container is part of the read rather than a default.
//
// It is bounded on the control bound rather than the probe bound: it is a call
// to the deployment, not to the database, which is the same distinction
// ensureNeo4jDiscovery draws for the exec that produced the path.
// The certificate is remembered against the container and path it came from,
// because the component it is read out of does not stay up for the whole run.
// A capture workload for PostgreSQL is created after an offline Neo4j capture
// has already scaled task-manager to zero, and this read is part of building
// that workload's client environment — so re-reading it there fails, the
// install falls back to the system trust store, and pg_dump fails verify-full
// against a privately signed server. The gate probes every external database
// while the deployment is still up, which is what puts the entry here in time.
func (iops *InfrahubOps) readDeploymentCAFile(container, path string) (string, error) {
	key := container + ":" + path
	if certificate, read := iops.deploymentCAFiles[key]; read {
		return certificate, nil
	}

	output, err := iops.execBoundedAgainst(
		externalDBControlBound(iops.config), "reading the database CA certificate",
		"the "+container+" container", container,
		[]string{"cat", path}, nil,
	)
	if err != nil {
		return "", err
	}

	if !strings.Contains(output, pemCertificateMarker) {
		return "", fmt.Errorf("%s holds no PEM certificate", path)
	}

	if iops.deploymentCAFiles == nil {
		iops.deploymentCAFiles = map[string]string{}
	}
	iops.deploymentCAFiles[key] = output

	return output, nil
}

// installExternalCATrust puts the deployment's own CA in front of the probe's
// JVM clients and returns the environment that points them at it.
//
// A nil map is the answer wherever there is nothing to install, and it is not a
// failure: a deployment that configured no CA, or a run that was told not to
// verify at all, has nothing to trust differently. It is also the answer when a
// step fails, for the reason at the top of this file — the client then verifies
// against the image's default anchors, which is what it does today.
// It is also installed once per workload rather than once per caller. The
// restore path asks for the same pod's trust twice — the probe that reads the
// server's version runs Bolt statements, and so does the seed that follows it —
// and `keytool -importcert` refuses an alias the store already holds. Repeating
// the install would therefore fail on the second call and return nil, dropping
// a client that had a perfectly good trust store back onto the image's default
// anchors. So the answer is remembered against the pod that carries it.
func (iops *InfrahubOps) installExternalCATrust(pod string, tls externalTLSDecision) map[string]string {
	caFile := iops.config.ExternalDB.Neo4jTLS.CAFile
	if caFile == "" || !tls.verify() {
		return nil
	}

	if env, installed := iops.externalCATrust[pod]; installed {
		return env
	}

	env := iops.installExternalCATrustOnce(pod, caFile, tls)

	if iops.externalCATrust == nil {
		iops.externalCATrust = map[string]map[string]string{}
	}
	iops.externalCATrust[pod] = env

	return env
}

// installExternalCATrustOnce is the install itself, performed exactly once per
// workload by the memo above. Every failure arm returns nil, which the memo
// records: a CA that could not be placed in this pod will not be placeable by a
// later caller either, and retrying it would repeat the warning without
// changing the outcome.
func (iops *InfrahubOps) installExternalCATrustOnce(pod, caFile string, tls externalTLSDecision) map[string]string {
	if !iops.placeDeploymentCA(pod, "infrahub-server", caFile, externalCAPEMPath, tls) {
		return nil
	}

	if _, err := iops.execBoundedAgainst(
		externalDBControlBound(iops.config), "installing the database CA certificate",
		transientWorkloadTarget(tls.Service), tls.Service, importCACommand(), &ExecOptions{Pod: pod},
	); err != nil {
		logrus.Warnf("Could not import the deployment's certificate authority into a trust store the transient %s workload's clients read, so the server's certificate is verified against the workload's default trust store instead: %v", tls.Service, err)

		return nil
	}

	logrus.Infof("Verifying the external %s server's certificate against the authority the deployment records at %s", tls.Service, caFile)

	return externalCATrustEnv()
}

// placeDeploymentCA reads the certificate authority the deployment records at
// caFile out of one of its own containers and writes it into the transient
// workload at destPath, reporting whether it is there. Each failure is a
// warning naming what the client verifies against instead — the workload's
// default trust store — because, for the reason at the top of this file, the
// answer to an unplaceable CA is the weakest verifying configuration and never
// a non-verifying one. The callers add what their client then needs: a keytool
// import for the JVM, the path for libpq.
func (iops *InfrahubOps) placeDeploymentCA(pod, container, caFile, destPath string, tls externalTLSDecision) bool {
	certificate, err := iops.readDeploymentCAFile(container, caFile)
	if err != nil {
		logrus.Warnf("Could not read the certificate authority the deployment records at %s from the %s container, so the external %s server's certificate is verified against the transient workload's default trust store instead: %v", caFile, container, tls.Service, err)

		return false
	}

	if _, err := iops.execBoundedAgainst(
		externalDBControlBound(iops.config), "installing the database CA certificate",
		transientWorkloadTarget(tls.Service), tls.Service,
		writeFileCommand(destPath, certificate), &ExecOptions{Pod: pod},
	); err != nil {
		logrus.Warnf("Could not place the deployment's certificate authority in the transient %s workload, so the server's certificate is verified against the workload's default trust store instead: %v", tls.Service, err)

		return false
	}

	return true
}

// postgresClientEnv is the environment a PostgreSQL client in a transient
// workload needs beyond its credentials: the TLS mode, and the trust material
// that mode needs to be satisfiable.
//
// The credentials are deliberately not here. PGUSER and PGPASSWORD reach the
// container from the secret the transient workload owns; ExecOptions.Env
// becomes an `env KEY=VALUE` prefix on the exec command line (see
// prepareCommand), which is exactly the exposure FR-014 forbids. What is left
// is TLS configuration, which is a setting rather than a secret — and
// PGSSLROOTCERT names a path or the literal `system`, never a certificate.
func (iops *InfrahubOps) postgresClientEnv(pod string, tls externalTLSDecision) map[string]string {
	env := map[string]string{"PGSSLMODE": tls.postgresSSLMode()}
	if !tls.verify() {
		// require: encrypted, nothing checked. There is nothing for a trust
		// anchor to be used for, so naming one would only mislead a log reader.
		return env
	}

	env["PGSSLROOTCERT"] = iops.installExternalPostgresTrust(pod, tls)

	return env
}

// installExternalPostgresTrust puts the deployment's own certificate authority
// in front of the workload's libpq clients and returns what PGSSLROOTCERT
// should be: the path it wrote, or postgresSystemTrustStore.
//
// The system store is the answer wherever there is nothing to install and
// wherever a step fails, for the reason at the top of this file: it is the
// weakest verifying configuration rather than a non-verifying one, so the worst
// outcome of an unreadable CA is the "unknown issuer" the operator can act on,
// never a silently unverified connection.
func (iops *InfrahubOps) installExternalPostgresTrust(pod string, tls externalTLSDecision) string {
	caFile := iops.config.ExternalDB.PostgresTLS.CAFile
	if caFile == "" {
		return postgresSystemTrustStore
	}

	if !iops.placeDeploymentCA(pod, "task-manager", caFile, externalPostgresCAPEMPath, tls) {
		return postgresSystemTrustStore
	}

	logrus.Infof("Verifying the external %s server's certificate against the authority the deployment records at %s", tls.Service, caFile)

	return externalPostgresCAPEMPath
}
