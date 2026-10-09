package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// Restoring into a database that lives outside the deployment inverts the data
// path a capture uses, and every difference on this page follows from that one
// fact.
//
// A capture pulls: the transient workload runs the vendor's backup client
// against the server and the bytes come back through the pod. A Neo4j restore
// pushes nothing. There is no remote equivalent of `neo4j-admin database
// restore` — that command needs the server's own store directory — so the only
// documented way to put a backup artifact into a server this tool cannot reach
// the filesystem of is to ask the *server* to fetch it:
//
//	CREATE OR REPLACE DATABASE <db> OPTIONS { existingData: 'use', seedURI: 's3://…' }
//
// Three consequences run through everything below (research R2):
//
//   - **The database host does the reading, so the artifact has to be somewhere
//     it can read from.** That is a customer-side prerequisite this tool cannot
//     arrange and must not assume: the run stages the artifact in the object
//     store the operator already configured, proves an independent reader can
//     address it, and names the remaining prerequisite plainly (FR-007,
//     FR-020). What it cannot do is verify the route from the database host,
//     which is why the confirmation below exists rather than being optional.
//   - **Accepting the seed request proves nothing.** The download and its
//     validation happen when the database starts, not when the statement
//     returns, and a seed that fails leaves the database offline with a
//     `statusMessage` rather than failing the statement. Reporting success on
//     the statement's return would be precisely the silent data loss
//     constitution Principle II exists to prevent, so the run polls until the
//     database is online and fails otherwise (FR-021).
//   - **Only the cloud providers are built in.** s3, gs and azb need no
//     server-side provider configuration; anything else requires
//     `dbms.databases.seed_from_uri_providers` on every server, which this tool
//     cannot see. So the staged URI is an object-store one, and a scheme that
//     is not is refused before the destructive step rather than after it.
//
// PostgreSQL keeps the shape it already had. `pg_restore` is a client, so the
// external path differs from the internal one in the three places the dump
// path differs: the host it connects to, where its credentials come from, and
// whether it verifies the server's certificate.
//
// The workload is created once, at the gate, and held for the whole run. That
// is not the capture's two-phase shape and it is deliberate: see
// workloadRoleRestore.

// externalRestore is what a run established about one database outside the
// deployment that it is about to write to, together with the workload the
// writing happens in.
//
// It embeds the same externalSource a capture records — the endpoint, this
// run's certificate decision, and what the probe read — because the questions
// that have to be settled before data moves are the same ones, and FR-006's
// version gate is answered from the same two fields on either path.
type externalRestore struct {
	*externalSource

	// Pod is the transient workload's pod, named on every exec and every copy
	// this restore makes. It is named rather than registered, for the reason
	// ExecOptions.Pod exists: a restore calls StartServices, which replaces the
	// resolver's cache wholesale.
	Pod string

	// Bound is the timeout on a single restore operation (FR-025). It is the
	// operator's --external-db-timeout, because seeding a large database is
	// legitimately slow — and because this is the path where a stall happens
	// with Infrahub scaled to zero.
	Bound time.Duration

	// ClientEnv is what the clients in this pod need beyond their credentials:
	// the trust anchor for a Bolt session, or the TLS mode and root
	// certificate for libpq. It is resolved once, when the workload is created,
	// because resolving it writes material into that pod — and because it is
	// read out of infrahub-server and task-manager, which this run scales to
	// zero before the destructive step.
	ClientEnv map[string]string

	// Seed is the artifact the database server will fetch, established by the
	// pre-flight before anything destructive happened (FR-007). Its zero value
	// means no pre-flight ran, which the seed refuses to proceed on rather than
	// treating as "no seed needed": the whole point of the pre-flight is that
	// the destructive step cannot be reached without it.
	Seed restoreSeed
}

// There is deliberately no Release here, and no release function carried on the
// value, which is the one place this type differs from externalCapture rather
// than mirroring it.
//
// A capture gives its workload back as soon as the capture is done, so it holds
// the means to. A restore's workload is needed until the run ends — the seed and
// then the wait for the database to come online are the last things a restore
// does — so the deferred releaseTransientWorkloads of whichever entry point was
// called is its only exit path (FR-011), and a second one carried here would be
// a field nothing reads. Preparation's own failure arms release the workload
// directly, before it has been recorded as the run's.
//
// "Whichever entry point" rather than "the run's single one": `restore --latest
// --s3` runs the gate before its download and then delegates to RestoreBackup,
// which runs it again, so two entry points on that path each defer one. Both
// have to, because the paths between them — a failed download, a second
// database that fails after the first was recorded — return without ever
// reaching the inner one.

// execOptions names this restore's workload as the target of one command, with
// the client environment the pod's tooling needs.
//
// The credentials are never here, for the reason externalCapture.execOptions
// gives: ExecOptions.Env becomes an `env KEY=VALUE` prefix on the exec command
// line, which is the exposure FR-014 forbids. The clients read their
// credentials from the secret the workload owns.
func (r *externalRestore) execOptions() *ExecOptions {
	return &ExecOptions{Pod: r.Pod, Env: r.ClientEnv}
}

// boltURI is the client URI the seed and the confirmation are sent to: the
// member that answered the probe, which is the one endpoint this run has
// evidence is reachable.
//
// One member rather than the whole list, unlike a capture's `--from`. The seed
// is a destructive statement against the system database, and walking a list
// with it would risk issuing it twice — once against a member whose answer was
// lost, once against the next.
func (r *externalRestore) boltURI() (string, error) {
	answered := r.Facts.Answered
	if answered.Host == "" {
		return "", fmt.Errorf("cannot address the external %s database: no member answered the probe, so there is no endpoint to send the restore to", r.Endpoint.Service)
	}

	return r.TLS.boltScheme(r.Endpoint.effectiveProtocol()) + "://" + answered.String(), nil
}

// ---------------------------------------------------------------------------
// Preparing the restore (the gate)
// ---------------------------------------------------------------------------

// prepareExternalRestores prepares every database this restore will write to
// that lives outside the deployment: it resolves the endpoint, decides the
// certificate question, creates the workload the restore will run in, probes
// the server through it, and applies the refusals that must land before
// anything is stopped.
//
// It returns before doing anything at all when no database is external, which
// is what keeps FR-015 structural rather than promised.
//
// It takes the targets the gate resolved rather than service names, for the
// reason prepareExternalSources does: where each database lives is exactly what
// a target already carries, and asking again would be a second answer to a
// question the gate has already answered — with only one of the two being the
// gate.
func (iops *InfrahubOps) prepareExternalRestores(targets databaseTargets) error {
	external := iops.unpreparedExternalRestores(targets)
	if len(external) == 0 {
		return nil
	}

	// The one line that records the destructive authorisation actually being
	// exercised, and it names the channel rather than only the fact.
	//
	// Which channel matters on its own (FR-010): the flag is an operator at a
	// terminal, and the configuration key is the only way an unattended
	// restore can carry it — nothing here ever prompts. An operator reading a
	// scheduled job's log needs to see which of the two authorised writing to
	// infrastructure this tool does not manage, because "somebody set an
	// environment variable on the job" and "somebody typed it just now" are
	// very different findings after an unexpected restore.
	logrus.Warnf("This restore writes to %d database(s) the deployment does not manage (%v), authorised by %s",
		len(external), external.services(), external[0].Restore.describe())

	ops, err := iops.externalCaptureOps()
	if err != nil {
		return err
	}

	for _, target := range external {
		if err := iops.prepareExternalRestoreWith(ops, target); err != nil {
			return err
		}
	}

	return nil
}

// unpreparedExternalRestores are the external databases this run has not
// already prepared a restore for.
//
// It exists because the gate is not called once per run. `restore --latest
// --s3` calls it before pulling a multi-gigabyte object out of a bucket that a
// refusal would make pointless, and then RestoreBackup calls it again — which
// was free while the gate only asked a question. It is not free now: a second
// pass would create a second workload for the same database under the same run
// identifier, which is the same pod name, so the create would collide and the
// run would fail reporting a concurrent operator. The second pass must
// therefore do nothing at all, not merely nothing destructive, which is why
// this is decided before a run identifier or a cluster call is spent.
func (iops *InfrahubOps) unpreparedExternalRestores(targets databaseTargets) databaseTargets {
	pending := databaseTargets{}
	for _, target := range targets.external() {
		if iops.externalRestores[target.Service] == nil {
			pending = append(pending, target)
		}
	}

	return pending
}

// prepareExternalRestoreWith prepares one external database's restore.
//
// The order is the requirement. The workload is created and probed before the
// version gate and the edition refusal, because the probe is the only thing
// that can answer them; and all of it happens before a single Infrahub service
// has been stopped, because FR-006 requires its answer before any data is
// written and a run that aborts here has changed nothing.
//
// It takes the target rather than a service name so the one thing it may only
// be called for — a database the gate established lives outside the deployment,
// with FR-009's authorisation already checked on that target — is carried by
// its argument rather than checked by its caller. That is only sound while a
// target cannot be built without passing the gate, which is what the first
// line below requires and what databaseTarget.gated makes checkable.
func (iops *InfrahubOps) prepareExternalRestoreWith(ops externalCaptureOps, target databaseTarget) error {
	service := target.Service

	endpoint, tls, err := iops.resolveExternalTarget(target, "restore into")
	if err != nil {
		return err
	}

	pod, release, err := ops.startRestore(service)
	if err != nil {
		return err
	}

	// Until the workload is recorded it belongs to this function, and every way
	// out of here that is not the record below gives it back. A deferred guard
	// rather than an unwind at each return, so a branch added later cannot leak
	// the pod (FR-011).
	//
	// Installed on the line after the workload exists, before anything is done
	// with it: a guard that only covers the statements written below it on the
	// day it was added is the same guard the unwinds were.
	recorded := false
	defer func() {
		if !recorded {
			release()
		}
	}()

	// Before the probe, because the probe's own Bolt statements need the trust
	// anchor this installs, and because it is read out of the deployment's
	// running components — which are up now and will not be when the restore
	// itself runs.
	clientEnv := iops.restoreClientEnv(service, pod, tls)

	facts, err := ops.probe(pod, endpoint, tls)
	if err != nil {
		return fmt.Errorf("failed to read what the external %s restore needs from %s: %w", service, endpoint.endpointTarget(), err)
	}

	if service == serviceNeo4j {
		if err := refuseExternalCommunityRestore(endpoint, facts.Edition); err != nil {
			return err
		}
	}

	// FR-006, at the only point where it can hold: the utility has been read
	// from the image that will drive the restore, the server has been read over
	// the wire, and no data has been written and no service stopped.
	if err := checkVersionCompatibility(service, facts.UtilityVersion, facts.ServerVersion); err != nil {
		return err
	}

	// Recorded, and from here on the workload is the run's: the deferred
	// releaseTransientWorkloads each restore entry point makes is what gives it
	// back, on the failing paths as well as the succeeding one (FR-011).
	iops.recordExternalRestore(service, &externalRestore{
		externalSource: &externalSource{Endpoint: endpoint, TLS: tls, Facts: facts},
		Pod:            pod,
		Bound:          externalDBBound(iops.config),
		ClientEnv:      clientEnv,
	})
	recorded = true

	return nil
}

// restoreClientEnv is the environment the clients in a freshly created restore
// workload need beyond their credentials.
//
// Both arms are populated here, unlike the capture's. A capture of Neo4j speaks
// the backup protocol, which authenticates through the server's own backup SSL
// policy and has nothing to do with INFRAHUB_DB_TLS_CA_FILE; a restore speaks
// Bolt, so the deployment's own trust anchor is exactly what its client needs
// (see external_ca.go).
func (iops *InfrahubOps) restoreClientEnv(service, pod string, tls externalTLSDecision) map[string]string {
	if service == serviceTaskManagerDB {
		return iops.postgresClientEnv(pod, tls)
	}

	return iops.installExternalCATrust(pod, tls)
}

// recordExternalRestore stores what this run prepared for one database.
func (iops *InfrahubOps) recordExternalRestore(service string, restore *externalRestore) {
	if iops.externalRestores == nil {
		iops.externalRestores = map[string]*externalRestore{}
	}

	iops.externalRestores[service] = restore
}

// externalRestoreFor is what the restore paths dispatch on: what this run
// prepared for a database that lives outside the deployment, and nil for one
// that lives inside it.
//
// A nil source against a database located *external* is neither branch, and it
// is an error naming what was not done. That asymmetry is the whole point, and
// it is the lesson externalDatabaseFor records: branching on "did
// preparation record something?" reads a nil as internal, and the run then
// execs the vendor utility inside a container the database does not live in —
// silently, because nothing further along asks the question again.
//
// It issues no query a deployment did not already make. The gate memoises every
// location it establishes, and on Docker Compose the answer is internal by
// construction, so a run that went through the gate reads a memo and an
// internal deployment's path is unchanged (FR-015).
func (iops *InfrahubOps) externalRestoreFor(service string) (*externalRestore, error) {
	if restore := iops.externalRestores[service]; restore != nil {
		return restore, nil
	}

	return nil, iops.refuseUnpreparedExternal(service,
		"nothing was prepared to restore into it, so there is no container to write it in; "+
			"this run reached a restore path without establishing what the restore needs")
}

// ---------------------------------------------------------------------------
// The edition refusal (FR-008's restore side)
// ---------------------------------------------------------------------------

// refuseExternalCommunityRestore refuses a restore into an external Community
// server, saying why the mechanism is unavailable.
//
// It is the mirror of refuseExternalCommunityCapture and it is separate because
// it refuses for a different reason and offers a different remedy. A Community
// *capture* is impossible because the dump reads the store files directly. A
// Community *restore* is impossible because the mechanism is a Cypher database
// creation seeded from a URI, and Community hosts exactly one database and
// cannot create another — so there is no statement to issue, not merely no file
// to read.
//
// An edition the server did not report is not read as Community, for the reason
// its sibling gives: guessing the destructive branch from a probe that did not
// answer is what Principle II forbids.
func refuseExternalCommunityRestore(endpoint *DatabaseEndpoint, edition string) error {
	if !isCommunityEdition(edition) {
		return nil
	}

	return fmt.Errorf(
		"cannot restore into %s: %w, and the remote alternative — directing the server to seed a database from the backup artifact — creates a database, which Community Edition cannot do. "+
			"Seeding from a URI is a Neo4j Enterprise Edition feature. "+
			"Nothing is missing from the deployment — the %s service has no container here because the database is managed outside it. "+
			"No Infrahub service was stopped and no data was changed",
		endpoint.endpointTarget(), errExternalCommunityRestore, serviceNeo4j,
	)
}

// errExternalCommunityRestore is the refusal above as a value callers and tests
// can identify rather than a message they have to match.
var errExternalCommunityRestore = errors.New("the Community Edition restore mechanism cannot create the database a seed would populate")

// refuseExternalCommunityDumpRestore is where a Plakar restore that would load
// a Neo4j dump into a database outside the deployment is refused: at the gate,
// before anything is stopped, wiped or restarted.
//
// The refusal used to be raised from inside restoreNeo4jCommunityStream, which
// is where the impossibility actually bites but which both Plakar paths reach
// only after stopAppContainers, confirmAppContainersQuiesced and
// restartDependencies. So the run took the deployment down to discover something
// it already knew — the artifact's edition is read from the snapshot's own tags
// long before — and then said "No Infrahub service was stopped", which by then
// was untrue. Every other refusal on this feature is raised before the first
// destructive step (see external_backup_gate.go), and this is now one of them.
//
// loadsDump is the caller's own answer to "will this restore load a Neo4j
// dump?", which the two entry points establish differently: a group has a Neo4j
// component and a metadata edition, a single snapshot has a component tag and an
// edition tag. Taking the answer rather than re-deriving it is what keeps the
// refusal from disagreeing with the branch it guards.
// It reads what the gate prepared rather than going through
// externalRestoreFor: both callers run after prepareDatabaseRestore, so a
// service absent from externalRestores is an in-deployment one — which is what
// every other path on this feature branches on. externalRestoreFor would ask
// the cluster where the database lives a second time, a listing an ordinary
// internal deployment would newly pay for on every Plakar restore (FR-015). Its
// own location query stays where it is needed: in the load, which can be
// reached without a gate.
func (iops *InfrahubOps) refuseExternalCommunityDumpRestore(loadsDump bool) error {
	if !loadsDump {
		return nil
	}

	restore := iops.externalRestores[serviceNeo4j]
	if restore == nil {
		return nil
	}

	return refuseExternalDumpRestore(restore.Endpoint)
}

// refuseExternalDumpRestore refuses to load a Community dump into a database
// outside the deployment.
//
// It exists because the Plakar paths stream a dump straight into the database's
// container without going through restoreNeo4j, so the dispatch that sends an
// external database to the seed path is not on that route. Loading a dump means
// running `neo4j-admin database load` against the server's own store
// directory — which is what makes it work on an in-deployment database and what
// makes it impossible here — so the refusal names the artifact rather than the
// server: an Enterprise online backup can seed an external server, and a dump
// cannot.
func refuseExternalDumpRestore(endpoint *DatabaseEndpoint) error {
	return fmt.Errorf(externalDumpRestoreRefusal+
		"No Infrahub service was stopped and no data was changed", endpoint.endpointTarget())
}

// externalDumpRestoreRefusal is the half of the refusal that states a fact
// about the artifact rather than about the run, so the two callers below cannot
// drift on it. What they must not share is the closing clause: whether anything
// has been stopped is a fact about where the refusal was reached, and asserting
// the wrong one is what went wrong when this refusal lived in only one place.
const externalDumpRestoreRefusal = "cannot restore this snapshot into %s: it carries a Neo4j dump, which is loaded by writing into the server's own store directory, and this run has no access to the storage of a server it does not host. " +
	"The artifact an external server can be restored from is an Enterprise online backup, which it fetches itself. "

// refuseExternalDumpRestoreAfterQuiescing is the same refusal reached from
// inside the stream load, which the gate is meant to make unreachable.
//
// It is kept rather than deleted because restoreNeo4jCommunityStream is where
// the container is actually needed, so it is the last place the guarantee can be
// held, and because a Plakar path added later could reach it without passing a
// gate. What it does not do is repeat the gate's "nothing was stopped" claim: by
// here something has been, and a refusal that asserts otherwise is exactly what
// went wrong when this one lived only here.
func refuseExternalDumpRestoreAfterQuiescing(endpoint *DatabaseEndpoint) error {
	return fmt.Errorf(externalDumpRestoreRefusal+
		"This run had already quiesced the deployment before reaching this point, so its services are returned to their prior scale; no database was written to",
		endpoint.endpointTarget())
}

// ---------------------------------------------------------------------------
// Staging the seed the server will fetch (FR-007, FR-020)
// ---------------------------------------------------------------------------

// seedKeySegment is the key segment the staged artifact is written under, so it
// sits beside the archives rather than among them.
//
// Retention only ever recognises an object whose key is exactly the one
// buildS3Key would produce for a backup's own base name, so an object one
// segment deeper is invisible to it. That is what keeps a staged seed out of a
// retention decision without retention having to learn about seeds.
const seedKeySegment = "seed"

// neo4jSeedArtifactSuffix is the extension the online backup gives its
// artifact. The archive carries exactly one of them for the database it was
// taken from.
const neo4jSeedArtifactSuffix = ".backup"

// restoreSeed is the artifact a database server will fetch, and the evidence
// this run has that it is there.
type restoreSeed struct {
	// URI is what the seed statement will name. It addresses an object store,
	// because those are the providers a server supports without configuration.
	URI string

	// Key is the object's key within the bucket, kept so the staged copy can be
	// removed once the restore has actually landed.
	Key string

	// Bucket is where it was staged, for the same reason.
	Bucket string

	// Bytes is the size an independent reader saw at that URI, which is the
	// pre-flight's actual finding rather than the size the upload reported
	// writing.
	Bytes int64
}

// stageNeo4jSeed puts the restore artifact where the database server can fetch
// it and establishes that it is readable there, before anything destructive
// happens (FR-007).
//
// What it proves and what it cannot prove are worth stating exactly, because
// the difference is the residual risk this whole path carries. It proves that
// an artifact exists in the archive, that an object store is configured, that
// the object was written, and that a *reader* — a client built from the URI
// that will be published, not the one that wrote it — finds it at that URI. It
// cannot prove there is a route from the database host to the object store, or
// that the server has the provider tooling and credentials the fetch needs:
// neither is visible from here, and no documented interface answers either
// question. Both are the customer-side prerequisites FR-020 requires be named,
// and the confirmation in waitForNeo4jRestore is what turns a prerequisite that
// does not hold into a failed run rather than a database reported restored.
func (iops *InfrahubOps) stageNeo4jSeed(workDir string) (restoreSeed, error) {
	artifact, err := neo4jSeedArtifact(workDir)
	if err != nil {
		return restoreSeed{}, err
	}

	configured := iops.objectStore()
	if err := configured.ValidateConfig(); err != nil {
		return restoreSeed{}, fmt.Errorf(
			"cannot restore into an external Neo4j database: the server fetches the backup artifact itself, so the artifact has to be staged in an object store the database host can read, and none is configured (%w). "+
				"Configure the bucket this run may stage into with --s3-bucket (plus --s3-prefix, --s3-endpoint and --s3-region as your provider needs), and make sure the Neo4j servers can read from it. "+
				"No Infrahub service was stopped and no data was changed", err)
	}

	client, err := seedStoreClient(configured, configured.Bucket, seedPrefix(configured.Prefix))
	if err != nil {
		return restoreSeed{}, fmt.Errorf("failed to reach the object store the restore artifact must be staged in: %w", err)
	}

	logrus.Infof("Staging %s in %s so the Neo4j servers can fetch it themselves", filepath.Base(artifact), client.locationName())

	ctx, cancel := context.WithTimeout(context.Background(), externalDBBound(iops.config))
	defer cancel()

	uri, err := client.Upload(ctx, artifact)
	if err != nil {
		return restoreSeed{}, fmt.Errorf("failed to stage the restore artifact where the Neo4j servers can fetch it: %w", err)
	}

	seed, err := verifySeedReadable(ctx, configured, uri)
	if err != nil {
		return restoreSeed{}, err
	}

	logrus.Infof("The restore artifact is readable at %s (%s). The Neo4j servers fetch it themselves, so each of them needs a route to that object store and the provider credentials for it — a prerequisite this run cannot verify from here; a fetch that fails leaves the database offline, which the confirmation after the restore reports",
		seed.URI, formatBytes(seed.Bytes))

	return seed, nil
}

// objectStore is the operator's S3 configuration, tolerating a Configuration
// that was never given one. That shape is not theoretical — a run configured
// entirely by flags that name no bucket has it — and what such a run must read
// is the refusal in stageNeo4jSeed, not a panic on a nil dereference.
func (iops *InfrahubOps) objectStore() *S3Config {
	if iops.config == nil || iops.config.S3 == nil {
		return &S3Config{}
	}

	return iops.config.S3
}

// seedPrefix is the staged artifact's key prefix: one segment below wherever
// the archives are written, or that segment alone where no prefix is
// configured.
func seedPrefix(prefix string) string {
	trimmed := strings.Trim(strings.TrimSpace(prefix), "/")
	if trimmed == "" {
		return seedKeySegment
	}

	return trimmed + "/" + seedKeySegment
}

// verifySeedReadable reads the staged object back through a client built from
// the URI that will be published, which is the closest this host can get to the
// read the database server will perform.
//
// A fresh client rather than the uploader is the whole point of the check. The
// uploader reports what it believes it wrote; this addresses the object the way
// a reader addresses it, so a prefix that does not compose, a bucket the
// endpoint resolves differently, or an object that never landed is a failure
// here rather than a database left offline after the destructive step.
func verifySeedReadable(ctx context.Context, cfg *S3Config, uri string) (restoreSeed, error) {
	bucket, key, ok := ParseS3URI(uri)
	if !ok {
		return restoreSeed{}, fmt.Errorf("cannot verify the staged restore artifact: %q is not an object-store URI, and only the object-store providers are ones a Neo4j server can fetch from without server-side configuration", uri)
	}

	reader, err := seedStoreClient(cfg, bucket, "")
	if err != nil {
		return restoreSeed{}, fmt.Errorf("failed to build a reader for the staged restore artifact at %s: %w", uri, err)
	}

	size, err := reader.Stat(ctx, key)
	if err != nil {
		return restoreSeed{}, unreadableSeedArtifact(uri, err)
	}

	return restoreSeed{URI: uri, Key: key, Bucket: bucket, Bytes: size}, nil
}

// seedStoreClient is a client on the object store the seed is staged in: the
// configured endpoint and region, addressing one bucket under one prefix. The
// upload, the read-back and the removal each build their own, on purpose (see
// verifySeedReadable), so the recipe is stated once here.
func seedStoreClient(configured *S3Config, bucket, prefix string) (*S3Client, error) {
	return NewS3Client(&S3Config{Bucket: bucket, Prefix: prefix, Endpoint: configured.Endpoint, Region: configured.Region})
}

// errSeedArtifactUnreadable is FR-007's refusal, as a value callers and tests
// can identify rather than a message they have to match.
var errSeedArtifactUnreadable = errors.New("the restore artifact could not be read back from where it was staged")

// unreadableSeedArtifact is what an operator reads when the pre-flight staged
// the artifact and then could not read it back.
//
// The failure-message contract requires it to name the URI attempted and the
// server-side prerequisite, which are what separate this from an ordinary
// object-store error: the Neo4j server fetches this artifact itself, so the
// remedy is a route and a policy on the *database host* rather than anything on
// the host running this tool. It states that nothing was stopped and nothing
// changed, because on the restore path that is the fact an operator needs
// first.
//
// It is a named constructor rather than an inline message so that the three
// elements can be asserted where they are produced — the same reason
// externalCommunityCaptureRefusal and neo4jRestoreFailure are named.
func unreadableSeedArtifact(uri string, cause error) error {
	return fmt.Errorf(
		"%w: %s cannot be read back: %w. "+
			"The Neo4j servers fetch this artifact themselves, so a URI this run cannot read is one they certainly cannot — the bucket has to exist, hold the object, and be readable both from here and from the database host. "+
			"No Infrahub service was stopped and no data was changed",
		errSeedArtifactUnreadable, uri, cause)
}

// discardStagedSeed removes the staged copy of the artifact.
//
// Only ever after the restore has been confirmed online. A seed that is still
// being fetched — which is every moment between the statement returning and the
// database coming up — must not have its source deleted underneath it, and a
// restore that failed is one an operator is about to look into, so the object
// stays and its URI is in the log.
func (iops *InfrahubOps) discardStagedSeed(seed restoreSeed) {
	client, err := seedStoreClient(iops.objectStore(), seed.Bucket, "")
	if err != nil {
		logrus.Warnf("Could not remove the staged restore artifact at %s: %v", seed.URI, err)

		return
	}

	if err := client.Delete(context.Background(), seed.Key); err != nil {
		logrus.Warnf("Could not remove the staged restore artifact at %s; it is a copy of the archive's own contents and is safe to delete by hand: %v", seed.URI, err)

		return
	}

	logrus.Debugf("Removed the staged restore artifact at %s", seed.URI)
}

// neo4jSeedArtifact is the one artifact in the extracted archive a server can
// be seeded from.
//
// Exactly one, and named rather than assumed. The online backup produces a
// single `<database>-<timestamp>.backup` file and the archive carries it raw
// (see contracts/artifact-and-metadata.md), so zero of them means this archive
// is not an Enterprise one and several means the archive holds captures of two
// databases — and picking one of those by sort order would seed a database from
// another database's data.
func neo4jSeedArtifact(workDir string) (string, error) {
	databaseDir := filepath.Join(workDir, "backup", "database")

	matches, err := filepath.Glob(filepath.Join(databaseDir, "*"+neo4jSeedArtifactSuffix))
	if err != nil {
		return "", fmt.Errorf("failed to look for the Neo4j backup artifact in %s: %w", databaseDir, err)
	}

	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf(
			"cannot restore into an external Neo4j database: the archive holds no %s artifact in %s, which is what an Enterprise online backup produces and what a server can be seeded from. "+
				"An archive taken from a Community deployment carries a dump instead, and a dump can only be loaded by a server whose store files this run can reach. "+
				"No Infrahub service was stopped and no data was changed", neo4jSeedArtifactSuffix, databaseDir)
	default:
		return "", fmt.Errorf(
			"cannot restore into an external Neo4j database: %s holds %d %s artifacts, so which database this run would seed from is ambiguous. "+
				"No Infrahub service was stopped and no data was changed", databaseDir, len(matches), neo4jSeedArtifactSuffix)
	}
}

// ---------------------------------------------------------------------------
// The seed statement (research R2)
// ---------------------------------------------------------------------------

// neo4jSeedReplaceFloor is the first server version whose CREATE OR REPLACE
// DATABASE accepts a seed URI. At or above it the destructive step is one
// atomic replacement; below it the database has to be dropped first, which
// opens a window in which it does not exist at all.
const neo4jSeedReplaceFloor = "2025.01"

// neo4jSeedStatements is the Cypher that seeds a database from a URI, in the
// form the server version supports.
//
// Two forms rather than one, and the newer one is not merely tidier. `CREATE OR
// REPLACE DATABASE … OPTIONS { seedURI: … }` is a single replacement, so a
// failure to fetch the seed leaves a database that is offline with a reason
// attached. The older servers need `DROP DATABASE … IF EXISTS` first, and
// between that statement and the create the database does not exist — so a run
// interrupted in between leaves the deployment with no database at all, which
// is worth an operator knowing before they start.
//
// A server version that cannot be ordered takes the older form. It works on
// every version, where the newer one does not, so an unreadable version costs
// the atomicity rather than the restore. In practice it does not arise:
// checkVersionCompatibility has already aborted on a version neither side could
// read.
func neo4jSeedStatements(database, seedURI, serverVersion string) ([]string, error) {
	name, err := safeDatabaseIdentifier(database)
	if err != nil {
		return nil, fmt.Errorf("cannot build the statement that seeds the external %s database: %w", serviceNeo4j, err)
	}

	uri, err := safeSeedURI(seedURI)
	if err != nil {
		return nil, err
	}

	options := fmt.Sprintf("OPTIONS {existingData: 'use', seedURI: '%s'}", uri)

	if atomicSeedReplaceSupported(serverVersion) {
		return []string{fmt.Sprintf("CREATE OR REPLACE DATABASE %s %s", name, options)}, nil
	}

	logrus.Warnf("This server (%s) is older than %s, so the database is dropped before it is created from the seed; between those two statements %s does not exist",
		serverVersion, neo4jSeedReplaceFloor, name)

	return []string{
		fmt.Sprintf("DROP DATABASE %s IF EXISTS", name),
		fmt.Sprintf("CREATE DATABASE %s %s", name, options),
	}, nil
}

// atomicSeedReplaceSupported reports whether this server's CREATE OR REPLACE
// DATABASE accepts a seed URI.
func atomicSeedReplaceSupported(serverVersion string) bool {
	server, err := parseDatabaseVersion(serverVersion)
	if err != nil {
		logrus.Warnf("Could not read the server version %q, so the restore takes the drop-then-create form that every version accepts: %v", serverVersion, err)

		return false
	}

	floor, err := parseDatabaseVersion(neo4jSeedReplaceFloor)
	if err != nil {
		return false
	}

	return server.compare(floor) >= 0
}

// safeSeedURI refuses a URI that carries anything but a URI before it is
// interpolated into a Cypher string literal.
//
// The value is this run's own construction rather than an operator's text, so
// this is a belt rather than braces — but it is the one value on this path that
// reaches a statement inside quotes, and a quote or a newline in it would close
// the literal and continue the statement. It also refuses a scheme that is not
// an object store's, which is a correctness check rather than a safety one: the
// other providers need `dbms.databases.seed_from_uri_providers` configured on
// every server, and a run that cannot see that configuration must not depend
// on it.
func safeSeedURI(uri string) (string, error) {
	trimmed := strings.TrimSpace(uri)
	if trimmed == "" {
		return "", fmt.Errorf("cannot seed the external %s database: no artifact URI was established", serviceNeo4j)
	}

	if strings.ContainsAny(trimmed, "'\"`\\\n\r\t ") {
		return "", fmt.Errorf("cannot seed the external %s database from %q: the URI carries a character a URI may not", serviceNeo4j, trimmed)
	}

	scheme, _, found := strings.Cut(trimmed, "://")
	if !found {
		return "", fmt.Errorf(
			"cannot seed the external %s database from %q: it names no scheme, and a server can only fetch an artifact it has a provider for. The schemes every server supports as shipped are %s",
			serviceNeo4j, trimmed, strings.Join(cloudSeedSchemes, ", "))
	}

	if !isCloudSeedScheme(scheme) {
		return "", fmt.Errorf(
			"cannot seed the external %s database from %q: a server fetches %s URIs only where dbms.databases.seed_from_uri_providers has been configured for that scheme, which this run cannot see. "+
				"The schemes every server supports as shipped are %s",
			serviceNeo4j, trimmed, scheme, strings.Join(cloudSeedSchemes, ", "))
	}

	return trimmed, nil
}

// cloudSeedSchemes are the seed providers a Neo4j server carries as shipped:
// Amazon S3, Google Cloud Storage and Azure Cloud Storage. Every other scheme
// — http, https, ftp, file — requires per-server configuration (research R2).
var cloudSeedSchemes = []string{"s3", "gs", "azb"}

// isCloudSeedScheme reports whether a scheme is one of those.
func isCloudSeedScheme(scheme string) bool {
	return slices.ContainsFunc(cloudSeedSchemes, func(supported string) bool {
		return strings.EqualFold(strings.TrimSpace(scheme), supported)
	})
}

// ---------------------------------------------------------------------------
// Confirming the restore actually landed (FR-021)
// ---------------------------------------------------------------------------

const (
	// neo4jStatusColumn and neo4jStatusMessageColumn are what SHOW DATABASE
	// reports about a database's own state, and the two fields the seed's real
	// outcome appears in.
	neo4jStatusColumn        = "currentStatus"
	neo4jStatusMessageColumn = "statusMessage"

	// neo4jOnlineStatus is the only status that means the seed was fetched,
	// validated and loaded.
	neo4jOnlineStatus = "online"

	// neo4jSeedPollInterval is how often the database's status is read while it
	// starts. It is a wait between reads rather than a budget: the window is
	// the operation bound, and this only decides how many statements are spent
	// inside it.
	neo4jSeedPollInterval = 5 * time.Second

	// neo4jStatusUnreadableGrace is how long the wait tolerates being unable to
	// ask the database anything at all before it gives up on the question
	// rather than on the database.
	//
	// It is short on purpose, and it is not a second copy of the operator's
	// bound. The condition it covers is a server refusing a session for a
	// moment after replacing a database — seconds — whereas the bound covers a
	// store that legitimately takes hours to load. Anything past this is the
	// run's own path to the database failing, which no amount of further
	// waiting improves, and which must not be reported as the database's state
	// (see neo4jRestoreUnreadable).
	neo4jStatusUnreadableGrace = time.Minute
)

// neo4jSeedFailureMarker is the statusMessage a server reports for a seed it
// could not download or validate. It is matched so the run can fail as soon as
// the server says so, rather than polling out a window whose answer is already
// known.
const neo4jSeedFailureMarker = "unable to start database"

// neo4jDatabaseStatus is one member's report of the database's state.
type neo4jDatabaseStatus struct {
	Status  string
	Message string
}

// online reports whether this member has the database up.
func (s neo4jDatabaseStatus) online() bool {
	return strings.EqualFold(strings.TrimSpace(s.Status), neo4jOnlineStatus)
}

// failed reports whether this member has given up on the seed. A server that
// could not fetch or validate the artifact says so in the status message, and
// waiting out the window after that only delays a failure the server has
// already reported.
func (s neo4jDatabaseStatus) failed() bool {
	return strings.Contains(strings.ToLower(s.Message), neo4jSeedFailureMarker)
}

// describe renders one member's report for the failure an operator reads.
func (s neo4jDatabaseStatus) describe() string {
	status := strings.TrimSpace(s.Status)
	if status == "" {
		status = "an unreported status"
	}
	if message := strings.TrimSpace(s.Message); message != "" {
		return fmt.Sprintf("%s (%s)", status, message)
	}

	return status
}

// neo4jDatabaseStatusCommand asks the server for the state of the database that
// was just seeded.
//
// It runs against the system database because the database being asked about
// may not be up — which is the entire point of asking.
func neo4jDatabaseStatusCommand(uri, database string) ([]string, error) {
	name, err := safeDatabaseIdentifier(database)
	if err != nil {
		return nil, fmt.Errorf("cannot query the state of the external %s database: %w", serviceNeo4j, err)
	}

	statement := fmt.Sprintf("SHOW DATABASE %s YIELD %s, %s", name, neo4jStatusColumn, neo4jStatusMessageColumn)

	return cypherShellCommand(uri, neo4jSystemDatabase, statement), nil
}

// parseNeo4jDatabaseStatuses reads every member's report out of one
// SHOW DATABASE result.
//
// Every member, not the first row. A clustered database is reported once per
// member hosting it, and a seed that loaded on one server and failed on another
// is a half-restored database — which reading row zero would report as a
// success or a failure depending on which member the listing happened to put
// first.
//
// It reads its columns by name and skips whatever the server printed above the
// header, which is the rule every parser on this path follows: a notice line
// read as data parses far enough to be acted on rather than far enough to fail.
func parseNeo4jDatabaseStatuses(output string) []neo4jDatabaseStatus {
	columns, rows := cypherResult(output, neo4jStatusColumn)
	if len(rows) == 0 {
		return nil
	}

	// The header was located by the status column, so it is present.
	statusAt, _ := columns.at(neo4jStatusColumn)
	messageAt, hasMessage := columns.at(neo4jStatusMessageColumn)

	statuses := make([]neo4jDatabaseStatus, 0, len(rows))
	for _, line := range rows {
		fields := splitPlainRow(line)
		if statusAt >= len(fields) {
			continue
		}

		status := neo4jDatabaseStatus{Status: fields[statusAt]}
		if hasMessage && messageAt < len(fields) {
			status.Message = fields[messageAt]
		}

		statuses = append(statuses, status)
	}

	return statuses
}

// seedVerdict is what one reading of SHOW DATABASE settles.
type seedVerdict struct {
	// Online is true only when every member hosting the database reports it up.
	Online bool

	// Failed is true when a member has reported it cannot start the database,
	// which is how a seed that could not be fetched or validated surfaces.
	Failed bool

	// Detail is what the members reported, for the operator-facing message.
	Detail string
}

// classifySeedStatuses turns one SHOW DATABASE result into a verdict.
//
// A result with no rows is neither online nor failed: a database being replaced
// is briefly absent from the listing, and reading that as a failure would fail
// a restore that is working.
func classifySeedStatuses(statuses []neo4jDatabaseStatus) seedVerdict {
	if len(statuses) == 0 {
		return seedVerdict{Detail: "the server reported no state for it yet"}
	}

	verdict := seedVerdict{Online: true}
	described := make([]string, 0, len(statuses))
	for _, status := range statuses {
		if !status.online() {
			verdict.Online = false
		}
		if status.failed() {
			verdict.Failed = true
		}
		described = append(described, status.describe())
	}

	verdict.Detail = strings.Join(described, "; ")

	return verdict
}

// waitForNeo4jRestore polls the server until the restored database is online,
// and fails otherwise (FR-021).
//
// This is not a nicety on top of the seed. Download and validation of a seed
// happen only as the database starts, so the statement that requested it
// returns success whatever the artifact turns out to be: a truncated upload, an
// artifact the server has no route to, a bucket it has no credentials for, and
// a perfectly good backup are indistinguishable at that moment. Reporting
// success there would be a restore that silently loaded nothing, which is the
// failure constitution Principle II exists to rule out.
//
// The window is the operation bound, and each statement inside it is bounded on
// its own, so a server that stops answering is reported rather than waited out
// (FR-025).
func (iops *InfrahubOps) waitForNeo4jRestore(restore *externalRestore, uri string) error {
	return iops.waitForNeo4jRestoreWithin(restore, uri, neo4jStatusUnreadableGrace)
}

// waitForNeo4jRestoreWithin is the wait under an explicit unreadable grace,
// following confirmAppContainersQuiescedWithin's idiom: it makes a run that can
// no longer ask the database anything assertable without waiting out the real
// grace. A grace of zero means the first unanswered read is the verdict.
func (iops *InfrahubOps) waitForNeo4jRestoreWithin(restore *externalRestore, uri string, grace time.Duration) error {
	command, err := neo4jDatabaseStatusCommand(uri, restore.Endpoint.Database)
	if err != nil {
		return err
	}

	logrus.Infof("Waiting for %s to come online; the seed is downloaded and validated as the database starts, so this is where a seed the server could not read is reported",
		restore.Endpoint.Database)

	deadline := time.Now().Add(restore.Bound)
	verdict := seedVerdict{Detail: "the server was never asked"}

	// Whether the run can still ask the question, and since when it could not.
	//
	// The operator's bound belongs to the database: a large store legitimately
	// takes hours to load, which is why it defaults to two of them. It does not
	// belong to the *question*. A read that never reached the server says
	// nothing about the database, and spending the whole bound on it with
	// Infrahub scaled to zero — then reporting "the database is not usable; its
	// reported state is the server could not be asked" — attributes a cluster
	// failure to the customer's database and sends the operator to the wrong
	// logs. So the two conditions are separated here, and each is bounded by
	// what it actually is.
	var (
		unreadableSince time.Time
		unreadableErr   error
	)

	for attempt := 1; ; attempt++ {
		output, err := iops.execBoundedAgainst(
			externalDBProbeBound(iops.config), "reading the restored database's state",
			restore.Endpoint.endpointTarget(), serviceNeo4j, command, restore.execOptions(),
		)
		if err != nil {
			// A server that has just replaced a database can refuse a session
			// for a moment, so a failed read is a retry rather than a verdict —
			// for as long as that moment lasts. Past the grace, the cause is not
			// a database settling: the transient workload was evicted,
			// `pods/exec` was revoked, the API server is gone.
			verdict = seedVerdict{Detail: fmt.Sprintf("the server could not be asked for its state: %v", err)}
			if unreadableSince.IsZero() {
				unreadableSince = time.Now()
			}
			unreadableErr = err
			if unreadable := time.Since(unreadableSince); unreadable >= grace {
				return neo4jRestoreUnreadable(restore, err, unreadable)
			}
			logrus.Debugf("Could not read the state of %s (attempt %d), retrying: %v", restore.Endpoint.Database, attempt, err)
		} else {
			// The server answered, so whatever failed before it was the moment
			// the grace exists for.
			unreadableSince = time.Time{}
			unreadableErr = nil

			verdict = classifySeedStatuses(parseNeo4jDatabaseStatuses(output))
			if verdict.Online {
				logrus.Infof("%s is online after %d status read(s): the seed was fetched, validated and loaded", restore.Endpoint.Database, attempt)

				return nil
			}
			if verdict.Failed {
				return neo4jRestoreFailure(restore, verdict.Detail, false)
			}

			logrus.Infof("%s is not online yet (%s); still waiting", restore.Endpoint.Database, verdict.Detail)
		}

		if sleepUntilNextPoll(deadline, neo4jSeedPollInterval) {
			// A bound shorter than the grace expires before it, and the same
			// distinction has to hold there: the last thing that happened was
			// still a question that went unanswered, not a database that failed
			// to come up.
			if unreadableErr != nil {
				return neo4jRestoreUnreadable(restore, unreadableErr, time.Since(unreadableSince))
			}

			return neo4jRestoreFailure(restore, verdict.Detail, true)
		}
	}
}

// neo4jRestoreUnreadable is what an operator reads when the run can no longer
// ask the database how the restore went.
//
// It is deliberately not neo4jRestoreFailure with a different detail, and the
// difference is attribution. That message says the database is not usable and
// points at the server's own logs, which is right when the server answered and
// reported a database it cannot start. Here the server said nothing: the
// command that asks runs in a transient pod, through this cluster's API, under
// a permission to exec — and any of those failing produces exactly this,
// while the database may well be loading the seed successfully.
//
// So it names the three elements the failure-message contract requires against
// *that* condition: what happened (the state could not be read, for how long),
// the resource (the endpoint, and the three things between this run and it),
// and the action (look at the pod and the permission, then read the database's
// own state directly — it may already be online).
func neo4jRestoreUnreadable(restore *externalRestore, err error, unreadable time.Duration) error {
	return fmt.Errorf(
		"the restore of %s into %s was accepted, and this run has been unable to read the database's state for %v: %w. "+
			"That is a failure of the path to the database, not a report about it — the read runs in this run's transient pod, through this cluster's API, under permission to exec into that pod — "+
			"so the database may still be loading the seed. Check that pod and that permission, then read the database's own state directly before restoring again. "+
			"Infrahub is returned to its prior scale, and the staged artifact is left in place so the restore can be repeated without re-staging it",
		restore.Endpoint.Database, restore.Endpoint.endpointTarget(), unreadable.Truncate(time.Second), err)
}

// neo4jRestoreFailure is what an operator reads when a seed was accepted and
// the database did not come up. It names the database's reported status and
// where the server records the cause, which is the failure-message contract's
// requirement for this condition.
func neo4jRestoreFailure(restore *externalRestore, detail string, timedOut bool) error {
	reason := "the server reported that it cannot start it"
	if timedOut {
		reason = fmt.Sprintf("it did not come online within %s, which --external-db-timeout sets", restore.Bound)
	}

	return fmt.Errorf(
		"the restore of %s into %s was accepted but the database is not usable: %s. Its reported state is %s. "+
			"A seed is downloaded and validated only as the database starts, so this is where an artifact the server could not fetch or could not read surfaces; "+
			"the cause is in that server's own logs (neo4j.log and debug.log on the database host). "+
			"Infrahub is returned to its prior scale, and the staged artifact is left in place so it can be examined",
		restore.Endpoint.Database, restore.Endpoint.endpointTarget(), reason, detail)
}

// ---------------------------------------------------------------------------
// The Neo4j restore itself
// ---------------------------------------------------------------------------

// preflightExternalNeo4jRestore is FR-007's pre-flight, and every restore entry
// point that will write to Neo4j performs it after the archive has been
// extracted and before the first destructive step.
//
// It is where the artifact becomes something the database server can read, and
// where a run that cannot make it so stops — with Infrahub still up, nothing
// wiped and nothing scaled to zero. A restore that got as far as the seed
// without it is refused there rather than proceeding, which is what keeps the
// pre-flight from being a step a later entry point can forget (see
// restoreNeo4jExternal).
//
// It returns without doing anything for an in-deployment database, which is
// every deployment that exists today (FR-015).
func (iops *InfrahubOps) preflightExternalNeo4jRestore(workDir string) error {
	restore, err := iops.externalRestoreFor(serviceNeo4j)
	if err != nil {
		return err
	}
	if restore == nil {
		return nil
	}

	seed, err := iops.stageNeo4jSeed(workDir)
	if err != nil {
		return err
	}

	restore.Seed = seed

	return nil
}

// restoreNeo4jExternal directs the server to seed the database from the staged
// artifact and then confirms it came up.
//
// The seed URI is established by the pre-flight, before anything was stopped,
// which is what makes FR-007's check a pre-flight: by the time this runs, the
// only remaining unknown is whether the server itself can reach what this run
// has already read.
func (iops *InfrahubOps) restoreNeo4jExternal(restore *externalRestore) error {
	seed := restore.Seed
	if seed.URI == "" {
		return fmt.Errorf(
			"cannot restore into the external %s database at %s: no artifact was staged for the server to fetch, so this run reached the destructive step without the pre-flight that establishes the server can read it",
			serviceNeo4j, restore.Endpoint.endpointTarget())
	}

	uri, err := restore.boltURI()
	if err != nil {
		return err
	}

	statements, err := neo4jSeedStatements(restore.Endpoint.Database, seed.URI, restore.Facts.ServerVersion)
	if err != nil {
		return err
	}

	logrus.Infof("Restoring the external Neo4j database %s at %s by seeding it from %s",
		restore.Endpoint.Database, restore.Endpoint.endpointTarget(), seed.URI)

	for _, statement := range statements {
		if _, err := iops.execBoundedAgainst(
			restore.Bound, "the seed request", restore.Endpoint.endpointTarget(), serviceNeo4j,
			cypherShellCommand(uri, neo4jSystemDatabase, statement), restore.execOptions(),
		); err != nil {
			return fmt.Errorf("failed to seed the external Neo4j database %s at %s: %w",
				restore.Endpoint.Database, restore.Endpoint.endpointTarget(), err)
		}
	}

	if err := iops.waitForNeo4jRestore(restore, uri); err != nil {
		return err
	}

	// Only now, and only because the database is up: until it is, the server may
	// still be fetching the object this would delete.
	iops.discardStagedSeed(seed)

	logrus.Info("Neo4j restore completed")

	return nil
}

// ---------------------------------------------------------------------------
// The PostgreSQL restore
// ---------------------------------------------------------------------------

// externalPostgresRestoreFile is where the dump is placed inside the restore
// workload: the scratch volume, which is the one directory the pod is
// guaranteed to be able to write to.
const externalPostgresRestoreFile = transientScratchPath + "/infrahubops_restore.dump"

// restorePostgreSQLExternal reads the dump back into a server outside the
// deployment.
//
// It is the internal path's own TCP branch with three substitutions — the host,
// the credentials' source, and certificate verification — because pg_restore is
// a client either way and the dump is the same file. What it does not do is
// start `task-manager-db`: there is no such workload here, which is exactly
// what the internal path's Kubernetes arm was already guessing at when it
// skipped the failure with "may be externally managed".
func (iops *InfrahubOps) restorePostgreSQLExternal(workDir string, restore *externalRestore) error {
	dump := filepath.Join(workDir, "backup", prefectDumpFilename)

	logrus.Infof("Restoring the external PostgreSQL database at %s (of the endpoints supplied, %s answered the probe)...",
		restore.Endpoint.endpointTarget(), restore.Facts.Answered)

	if err := iops.copyDumpIntoRestoreWorkload(restore, dump); err != nil {
		return err
	}
	defer iops.removeTransientScratch(serviceTaskManagerDB, "the staged dump", &ExecOptions{Pod: restore.Pod}, externalPostgresRestoreFile)

	host := restore.Facts.Answered
	command := postgresRestoreCommand(postgresRestoreRequest{
		Host: &host,
		File: externalPostgresRestoreFile,
	})

	if output, err := iops.execBoundedAgainst(
		restore.Bound, "the database restore", restore.Endpoint.endpointTarget(), serviceTaskManagerDB,
		command, restore.execOptions(),
	); err != nil {
		return fmt.Errorf("failed to restore the external PostgreSQL database at %s: %w\nOutput: %v", restore.Endpoint.endpointTarget(), err, output)
	}

	logrus.Info("PostgreSQL restore completed")

	return nil
}

// copyDumpIntoRestoreWorkload places the dump in the workload's scratch volume,
// having first established that it fits.
func (iops *InfrahubOps) copyDumpIntoRestoreWorkload(restore *externalRestore, dump string) error {
	info, err := os.Stat(dump)
	if err != nil {
		return fmt.Errorf("cannot read the task-manager dump to restore: %w", err)
	}

	if err := refuseUnfittableDump(iops.config.ExternalDB.ScratchSize, info.Size()); err != nil {
		return err
	}

	if err := iops.copyToTransientWorkload(restore.Pod, dump, externalPostgresRestoreFile); err != nil {
		return fmt.Errorf("failed to copy the task-manager dump into the transient workload: %w", err)
	}

	return nil
}

// refuseUnfittableDump refuses a dump larger than the workload's scratch
// volume, before the copy rather than part-way through it.
//
// The volume is sized by a default the operator can override, so a dump that
// does not fit has a remedy — and hitting it as a full volume mid-transfer
// names neither the size nor the flag, leaving an operator to work backwards
// from a write error inside a pod that no longer exists.
func refuseUnfittableDump(override string, dumpBytes int64) error {
	size, err := resolveRestoreScratchSize(serviceTaskManagerDB, override)
	if err != nil {
		return err
	}

	scratch, err := parseStorageQuantity(size)
	if err != nil {
		return fmt.Errorf("cannot check whether the task-manager dump fits the transient workload's scratch space of %s: %w", size, err)
	}

	if dumpBytes > scratch {
		return fmt.Errorf(
			"the task-manager dump is %s, which does not fit the transient workload's scratch space of %s: supply --external-db-scratch-size with a quantity larger than the dump. "+
				"No data was changed", formatBytes(dumpBytes), size)
	}

	return nil
}

// copyToTransientWorkload copies a file into a named transient pod, bounded
// like every other call this path makes (FR-025).
//
// It names the pod rather than resolving the service, for the reason
// copyFromTransientWorkload does: the workload stands in for a database with no
// container, so resolving `task-manager-db` here would find nothing.
func (iops *InfrahubOps) copyToTransientWorkload(pod, src, dest string) error {
	return iops.copyAcrossTransientWorkload(copyIntoTransientWorkload, pod, src, dest)
}
