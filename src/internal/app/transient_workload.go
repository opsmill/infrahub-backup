package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// A database that lives outside the deployment has no container to exec into,
// and every existing backup primitive is written in terms of one. This file
// supplies the missing container: a pod carrying the database's own tooling,
// registered under the canonical service name so that Exec, ExecStreamPipe and
// CopyFrom keep working unchanged, and bounded so the cluster reclaims it even
// if this process never returns.
//
// The Job is created by piping a manifest to `kubectl create` — no Kubernetes
// client library, per ADR-0001 and constitution Principle III. A manifest
// rather than `kubectl run` because the pod needs a run-ID label, a deadline,
// explicitly sized ephemeral storage, a restricted security context and
// credentials from a Secret, all of which a manifest states directly
// (research R5).

// Labels on the transient objects. They are deliberately outside the
// `app.kubernetes.io/*` and `infrahub/*` namespaces the deployment's own pods
// use, because two shared resolvers would otherwise find this pod:
// podSelectors matches on app.kubernetes.io/component, app, component and
// infrahub/service, and ListKubernetesNamespaces matches on
// app.kubernetes.io/name=infrahub. Setting none of those keys is what keeps the
// pod out of service enumeration and out of namespace detection (FR-028).
const (
	// transientLabelMarker marks every object this file creates. It is the
	// selector the replica-discovery exclusion and the stray reaper both use.
	transientLabelMarker = "infrahub-backup/transient"
	// transientLabelRunID carries the run that owns the object, so a later run
	// can tell a stray from a live peer's workload and reap only its own
	// (FR-011, and the data-model invariant that a workload is adopted by
	// exactly one run).
	transientLabelRunID = "infrahub-backup/run-id"
	// transientLabelService records the deployment service the workload stands
	// in for. It is not one of the keys podSelectors searches, so recording it
	// does not make the pod discoverable as a replica of that service.
	transientLabelService = "infrahub-backup/service"
	// transientLabelRole distinguishes the short probe workload from the
	// capture workload, so two pods in one run are tellable apart by an
	// operator reading `kubectl get pods` as well as by the reaper.
	transientLabelRole = "infrahub-backup/role"
)

// Naming. The prefix contains none of the documented deployment service names
// as a substring, which matters: getAllPodsWith and getPodForServiceWith fall
// back to `strings.Contains(podName, service)`, so a pod named after the
// service it stands in for would surface in replica enumeration however
// carefully its labels were chosen (FR-028).
const (
	transientObjectPrefix  = "infrahub-backup-xdb"
	transientContainerName = "tools"
	transientScratchPath   = "/scratch"
	transientScratchVol    = "scratch"
)

const (
	// externalDBScratchHeadroomFactor multiplies the queried store size. The
	// backup command stages the store and the transaction logs into a local
	// temporary directory and then produces the artifact from that staging
	// copy, and the vendor requires free space "at least equal to the size of
	// the database being backed up" for the staging alone (research R5).
	// Doubling covers staging plus the artifact produced beside it.
	externalDBScratchHeadroomFactor = 2.0

	// externalDBScratchFloorGiB is the smallest scratch allocation the sizing
	// will hand out. Transaction logs and archive metadata do not scale down
	// with a small store, so a faithfully-computed 1Gi for a near-empty
	// database would fail for reasons that have nothing to do with its size.
	externalDBScratchFloorGiB = 5

	// externalDBProbeScratch is the fixed, small allocation the probe workload
	// gets. The probe reads versions and the store size and writes nothing, but
	// it is still bounded so it cannot consume the node's disk.
	externalDBProbeScratch = "1Gi"

	// externalDBRestoreScratch is the fixed allocation a restore workload gets,
	// and --external-db-scratch-size overrides it.
	//
	// It is a default rather than a derived size because the two databases put
	// very different things through this pod, and neither is the store size
	// FR-022 sizes a capture from. Neo4j's data never enters it: the server
	// fetches its own seed from the object store, so all the pod holds is the
	// trust store its Bolt client reads. PostgreSQL's dump does travel through
	// it, and a task-manager dump is small next to a graph store — but it is
	// checked against this allocation before it is copied (see
	// refuseUnfittableDump), so an oversized dump names the override rather
	// than filling the volume mid-transfer.
	externalDBRestoreScratch = "10Gi"

	// externalDBWorkloadReadyTimeout bounds the wait for a workload to become
	// usable. It is generous because it covers pulling a database image onto a
	// node that has never run one.
	externalDBWorkloadReadyTimeout = 10 * time.Minute

	// externalDBWorkloadDeadlineMargin is added to the operation timeout to get
	// the Job's own deadline, so that the tool's timeout fires first and
	// reports which operation stalled, rather than the cluster removing the pod
	// underneath an operation still in flight.
	externalDBWorkloadDeadlineMargin = 10 * time.Minute

	// transientTerminationGrace keeps reaping quick: nothing in the pod holds
	// state worth flushing, because the artifact has already been copied out.
	transientTerminationGrace = 5

	// transientFinishedTTL removes a completed or failed Job shortly after its
	// deadline. The Job owns both its pod and the credential Secret, so this is
	// the cluster-side cleanup path when the CLI is killed (FR-011, FR-023).
	transientFinishedTTL = 60
)

// WorkloadState is the transient workload's lifecycle position. Only
// workloadStateReady may be used as an execution target: registering a pending
// pod would make every exec that followed fail with a resolution error that
// pointed at the wrong thing.
type WorkloadState string

const (
	workloadStatePending WorkloadState = "pending"
	workloadStateReady   WorkloadState = "ready"

	// workloadStateReleasing is "removal asked for, not confirmed". It is a
	// state of its own because the two things marking a workload released does
	// are needed at different moments: it must stop being an execution target
	// as soon as the removal is attempted, but it must not count as removed
	// until the deletion actually succeeded, or a delete that failed would be
	// skipped by every sweep that followed rather than retried by the next one.
	workloadStateReleasing WorkloadState = "releasing"

	workloadStateReleased WorkloadState = "released"
)

// workloadRole says what a workload is for.
//
// Two roles exist because of an ordering problem FR-022 creates and cannot
// solve on its own: the scratch space must be sized from the server's store
// size, queried before the capture begins, but a pod's storage sizing is fixed
// when the pod is created and the only client that can query the server lives
// in the pod. So a short probe workload — same pinned image, no scratch to
// size, nothing to stage — answers the three questions that must be answered
// before a capture pod can be built at all: the server's version (the FR-006
// gate), the utility's version as shipped in that same image, and the store
// size that sizes the capture pod.
//
// This is not the two-phase discovery research R3 rejected. That was two-phase
// *image selection* — spin a default image, probe it, respin a matched one — and
// it was rejected because it bought a second failure surface to derive
// something the operator already knows. The image here is selected once, from
// the pinned default or the operator's flag, and both roles run it. What the
// probe derives is what nobody knows in advance.
type workloadRole string

const (
	workloadRoleProbe   workloadRole = "probe"
	workloadRoleCapture workloadRole = "capture"

	// workloadRoleRestore is the one workload a restore into an external
	// database uses. It is one rather than two because the split above exists
	// only to size a capture pod from a store size that pod cannot itself read:
	// a restore's scratch requirement is known before it starts — the
	// PostgreSQL dump is on this host, and Neo4j's data never enters the pod at
	// all, since the server fetches its own seed — so there is nothing to
	// discover between the two phases.
	//
	// Holding one workload across the run is also what keeps the trust anchor
	// available. The deployment's own CA is read out of infrahub-server and
	// task-manager, which a restore scales to zero before the destructive step,
	// so a workload created after that point cannot verify a privately issued
	// certificate any more.
	workloadRoleRestore workloadRole = "restore"
)

// TransientWorkload is the execution context standing in for an absent
// database container: one per run per external database, plus the probe that
// preceded it.
//
// It carries what the workload's *lifetime* needs and nothing else. The role,
// the image, the deadline and the scratch size are all properties of the spec
// the pod was built from, and they were copied onto here as well — where
// nothing ever read them, while every reader went to the spec. Two records of
// one fact, the second one write-only: manifest construction, the log lines and
// the tests all read transientWorkloadSpec, which is the value the pod's shape
// is asserted against.
type TransientWorkload struct {
	// RunID is unique to the run that created the workload, and is labelled
	// onto every object so a later run can identify strays (FR-011).
	RunID string

	// Service is the canonical deployment service name the workload stands in
	// for, so execution resolves under the name it always had (FR-017).
	Service string

	// JobName identifies the controller whose TTL bounds every object in the
	// workload. PodName is the Job-created pod used as the execution target.
	// SecretName is the credential object owned by the Job.
	JobName    string
	PodName    string
	SecretName string
	Namespace  string

	// JobUID is the created Job's UID. It is what binds the credential
	// secret's lifetime to the Job's, and an owner reference carrying a wrong
	// or empty UID fails to bind without the cluster saying so — so it is
	// verified non-empty before the secret is built (FR-023).
	JobUID string

	// State is pending, ready or released.
	State WorkloadState

	// ops is how the workload reaches its cluster, and is the whole of what
	// releasing one needs. A *KubernetesBackend was kept beside it, documented
	// as being what makes that true; nothing read it, and Release, waitUntilReady
	// and readinessDiagnosis all go through ops — which is also what makes them
	// drivable in a test.
	ops transientClusterOps
}

// transientWorkloadSpec is everything manifest construction needs. Keeping it
// a plain value, and manifest construction a pure function over it, is what
// makes the shape of the pod assertable without a cluster.
type transientWorkloadSpec struct {
	RunID       string
	Service     string
	Role        workloadRole
	Image       string
	Namespace   string
	ScratchSize string
	Deadline    time.Duration
	// Credentials is the environment the tooling needs to authenticate, and it
	// reaches the container only through the owned secret: never a command-line
	// argument, and never an inline env value in the pod spec, either of which
	// a deployment observer can read (FR-014).
	Credentials map[string]string
	// Scheduling is what the operator's cluster requires of this pod: the
	// registry credentials for its image, the identity it runs under and where
	// it may be placed. Its zero value adds nothing to the manifest.
	Scheduling transientScheduling
}

// transientScheduling is ExternalDBScheduling after parsing: the manifest
// fields themselves, validated once, before any object is created.
type transientScheduling struct {
	ImagePullSecrets []transientLocalObjectRef
	ServiceAccount   string
	NodeSelector     map[string]string
	Tolerations      []transientToleration
	PriorityClass    string
}

// jobName is the transient Job's name: the prefix, which database it stands in
// for, its role, and the run that owns it. The Job-created pod appends its own
// controller suffix.
//
// All four segments are load-bearing, and the database one was missing. The run
// ID is minted once per run and not once per database — deliberately, because
// the reaper and the create-collision refusal both rest on a workload being
// adopted by exactly one run — so a deployment with *both* databases external
// produced two probe pods, then two capture pods, under one name each: an
// `AlreadyExists` that reads as a concurrent operator, or, where the first was
// already gone, a PostgreSQL pod created from a spec carrying Neo4j's
// credentials. The database segment is what makes one run's two workloads two
// objects.
//
// It is the database's own name (`neo4j`, `postgres`) and not the deployment
// service name the workload stands in for (`database`, `task-manager-db`),
// which is the point: see transientDatabaseToken.
func (s transientWorkloadSpec) jobName() string {
	return fmt.Sprintf("%s-%s-%s-%s", transientObjectPrefix, transientDatabaseToken(s.Service), s.Role, s.RunID)
}

// secretName is the credential secret's name, derived from the Job's so the two
// cannot be named by two rules. The secret is owned by that Job (FR-023), so a
// secret whose name did not track the Job's would be a secret bound to a Job
// this spec did not describe.
func (s transientWorkloadSpec) secretName() string {
	return s.jobName() + "-creds"
}

// transientDatabaseToken is the name segment that tells one database's
// transient objects from the other's.
//
// It is the database's own name rather than the deployment service name,
// because the prefix's contract is that no documented deployment service name
// appears in a transient object's name: two pod resolvers still fall back to
// matching a name against a service, so a pod named `…-database-…` could be
// found by a name match for `database` on any listing that reached one of them
// without the transient exclusion. `neo4j` and `postgres` carry neither
// `database` nor `task-manager-db` as a substring, so the objects are tellable
// apart without putting a service name where a name match could find it.
//
// The two names are the ones the artifact's own components already use
// (ComponentNeo4j, ComponentPostgres), so an operator reading
// `kubectl get pods` beside a snapshot listing sees one vocabulary.
func transientDatabaseToken(service string) string {
	if service == serviceTaskManagerDB {
		return ComponentPostgres
	}

	return ComponentNeo4j
}

func (s transientWorkloadSpec) labels() map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by": "infrahub-backup",
		transientLabelMarker:           "true",
		transientLabelRunID:            s.RunID,
		transientLabelService:          s.Service,
		transientLabelRole:             string(s.Role),
	}
}

// deadlineSeconds is the Job's activeDeadlineSeconds, floored at one second so
// a zero or negative configured timeout cannot produce a manifest the API
// server rejects.
func (s transientWorkloadSpec) deadlineSeconds() int64 {
	seconds := int64(s.Deadline.Seconds())
	if seconds < 1 {
		seconds = 1
	}

	return seconds
}

// ---------------------------------------------------------------------------
// Manifest types. A minimal, local subset of the Kubernetes API shapes, so the
// manifests are built by marshalling typed values rather than by interpolating
// strings into YAML — and without taking on a client library (ADR-0001).
//
// Every bool is a pointer because a restricted pod-security policy reads the
// difference between "false" and "absent": allowPrivilegeEscalation and
// runAsNonRoot must be *explicitly* set, and `omitempty` on a bool erases a
// false into an absence.
// ---------------------------------------------------------------------------

type transientObjectMeta struct {
	Name            string              `json:"name"`
	Namespace       string              `json:"namespace,omitempty"`
	Labels          map[string]string   `json:"labels,omitempty"`
	OwnerReferences []transientOwnerRef `json:"ownerReferences,omitempty"`
}

type transientOwnerRef struct {
	APIVersion         string `json:"apiVersion"`
	Kind               string `json:"kind"`
	Name               string `json:"name"`
	UID                string `json:"uid"`
	Controller         *bool  `json:"controller"`
	BlockOwnerDeletion *bool  `json:"blockOwnerDeletion"`
}

type transientSeccompProfile struct {
	Type string `json:"type"`
}

type transientPodSecurityContext struct {
	RunAsNonRoot        *bool                    `json:"runAsNonRoot"`
	RunAsUser           *int64                   `json:"runAsUser,omitempty"`
	RunAsGroup          *int64                   `json:"runAsGroup,omitempty"`
	FSGroup             *int64                   `json:"fsGroup,omitempty"`
	FSGroupChangePolicy string                   `json:"fsGroupChangePolicy,omitempty"`
	SeccompProfile      *transientSeccompProfile `json:"seccompProfile,omitempty"`
}

type transientCapabilities struct {
	Drop []string `json:"drop"`
}

type transientContainerSecurityContext struct {
	AllowPrivilegeEscalation *bool                  `json:"allowPrivilegeEscalation"`
	Privileged               *bool                  `json:"privileged"`
	RunAsNonRoot             *bool                  `json:"runAsNonRoot"`
	ReadOnlyRootFilesystem   *bool                  `json:"readOnlyRootFilesystem"`
	Capabilities             *transientCapabilities `json:"capabilities,omitempty"`
}

type transientSecretRef struct {
	Name     string `json:"name"`
	Optional *bool  `json:"optional"`
}

type transientEnvFrom struct {
	SecretRef transientSecretRef `json:"secretRef"`
}

type transientResources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

type transientVolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
}

type transientContainer struct {
	Name            string                             `json:"name"`
	Image           string                             `json:"image"`
	ImagePullPolicy string                             `json:"imagePullPolicy,omitempty"`
	Command         []string                           `json:"command,omitempty"`
	Args            []string                           `json:"args,omitempty"`
	EnvFrom         []transientEnvFrom                 `json:"envFrom,omitempty"`
	Resources       transientResources                 `json:"resources"`
	VolumeMounts    []transientVolumeMount             `json:"volumeMounts,omitempty"`
	SecurityContext *transientContainerSecurityContext `json:"securityContext"`
}

type transientEmptyDir struct {
	SizeLimit string `json:"sizeLimit,omitempty"`
}

type transientVolume struct {
	Name     string            `json:"name"`
	EmptyDir transientEmptyDir `json:"emptyDir"`
}

type transientLocalObjectRef struct {
	Name string `json:"name"`
}

type transientToleration struct {
	Key      string `json:"key,omitempty"`
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect,omitempty"`
}

type transientPodSpec struct {
	RestartPolicy string `json:"restartPolicy"`
	// ActiveDeadlineSeconds is the Job's own deadline, restated on the pod.
	//
	// The Job controller does not copy its activeDeadlineSeconds down onto the
	// pod it creates, and the pod is the only object the stray reaper lists
	// (see transientListJSONPath). Stating it here is what keeps the reaper's
	// deadline arm — "this pod has outlived a bound the cluster should already
	// have enforced" — reading a value that is actually there, instead of the
	// zero every Job-created pod would otherwise report (FR-011).
	//
	// It is also a second, independent bound on the pod: a Job controller that
	// is wedged terminates nothing, while the kubelet enforces this one.
	ActiveDeadlineSeconds         int64                        `json:"activeDeadlineSeconds"`
	TerminationGracePeriodSeconds int64                        `json:"terminationGracePeriodSeconds"`
	AutomountServiceAccountToken  *bool                        `json:"automountServiceAccountToken"`
	EnableServiceLinks            *bool                        `json:"enableServiceLinks"`
	SecurityContext               *transientPodSecurityContext `json:"securityContext"`
	Containers                    []transientContainer         `json:"containers"`
	Volumes                       []transientVolume            `json:"volumes,omitempty"`
	// The placement and image-access fields are every one of them omitempty:
	// an operator who needs none of them gets the manifest this file produced
	// before they existed, which is what keeps FR-015 true for a deployment
	// that never had to name any of this.
	ServiceAccountName string                    `json:"serviceAccountName,omitempty"`
	ImagePullSecrets   []transientLocalObjectRef `json:"imagePullSecrets,omitempty"`
	NodeSelector       map[string]string         `json:"nodeSelector,omitempty"`
	Tolerations        []transientToleration     `json:"tolerations,omitempty"`
	PriorityClassName  string                    `json:"priorityClassName,omitempty"`
}

type transientPodTemplate struct {
	Metadata transientObjectMeta `json:"metadata"`
	Spec     transientPodSpec    `json:"spec"`
}

type transientJobSpec struct {
	ActiveDeadlineSeconds   int64                `json:"activeDeadlineSeconds"`
	TTLSecondsAfterFinished int64                `json:"ttlSecondsAfterFinished"`
	BackoffLimit            int32                `json:"backoffLimit"`
	Template                transientPodTemplate `json:"template"`
}

type transientJobManifest struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Metadata   transientObjectMeta `json:"metadata"`
	Spec       transientJobSpec    `json:"spec"`
}

type transientSecretManifest struct {
	APIVersion string              `json:"apiVersion"`
	Kind       string              `json:"kind"`
	Type       string              `json:"type"`
	Metadata   transientObjectMeta `json:"metadata"`
	StringData map[string]string   `json:"stringData,omitempty"`
}

func boolPtr(v bool) *bool    { return &v }
func int64Ptr(v int64) *int64 { return &v }

// databaseImageUser is the uid the pinned image's own database user has, and
// the value the pod runs as. uid, gid and fsGroup are the same number in both
// images, so one function supplies all three.
//
// This is where FR-027's tension resolves, and the resolution turns on a fact
// about the images that is easy to assume and wrong. A restricted pod-security
// policy wants runAsNonRoot, dropped capabilities and no privilege escalation;
// the vendor documents the backup tooling as needing to run as the database's
// own user. It is tempting to conclude that stating runAsNonRoot and leaving
// runAsUser unset gets both — the container would then run as whatever
// non-root user its image declares, without this tool naming a uid.
//
// The images declare no such user:
//
//	$ docker image inspect neo4j:2025.10.1-enterprise --format '{{.Config.User}}'
//
//	$ docker image inspect postgres:18-alpine --format '{{.Config.User}}'
//
// Both are empty, which means uid 0. Both images run as root and drop
// privileges *inside their own entrypoint* — neo4j via
// /startup/docker-entrypoint.sh, postgres via gosu — and Command below replaces
// that entrypoint, so the drop never happens. runAsNonRoot with no runAsUser
// therefore makes the kubelet resolve the effective user from the image config,
// find root, and refuse the container with `CreateContainerConfigError:
// container has runAsNonRoot and image will run as root`. Not on an edge case:
// on every probe and capture pod, for both databases, on the shipped defaults.
// See dev/knowledge/container-image-user-semantics.md.
//
// The uids are real users inside the images, they are just not declared to
// Kubernetes, so the manifest has to name them:
//
//	uid=7474(neo4j) gid=7474(neo4j), owning /var/lib/neo4j
//	uid=70(postgres) gid=70(postgres), owning /var/lib/postgresql
//
// An operator-supplied image (--external-db-image-neo4j /
// --external-db-image-postgres) may run its tooling as a different user, and
// this tool cannot read an image's config from inside the cluster, so for that
// image the value below is an assumption rather than a measurement. It is still
// the right one to make, because of what a mismatch costs and does not cost:
//
//   - The scratch volume stays writable whatever the uid. The kubelet chowns
//     the emptyDir to fsGroup and adds fsGroup to the process's supplementary
//     groups, so "the pod starts and cannot write its own volume" — the outcome
//     that would make this worse than not starting — cannot happen.
//   - What a mismatch can cost is access to paths inside the image that its own
//     user owns, and that fails loudly at the first tooling call, naming the
//     path and the uid.
//
// Refusing every image but the pinned default was the alternative, and it takes
// away the flag FR-027 explicitly allows. An operator whose image needs a
// different uid must supply one whose database user matches the value here; if
// that ever proves too narrow, the uid belongs beside the image as a flag of
// its own rather than guessed differently.
func databaseImageUser(service string) int64 {
	if service == serviceTaskManagerDB {
		return 70
	}

	return 7474
}

// buildTransientWorkloadManifest renders the Job which owns the execution pod. It
// is pure over its spec: no
// cluster is consulted and nothing is read from the environment, which is what
// makes the label, deadline, security-context and sizing requirements
// assertable in a unit test.
func buildTransientWorkloadManifest(spec transientWorkloadSpec) ([]byte, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}

	scratch, err := parseStorageQuantity(spec.ScratchSize)
	if err != nil {
		return nil, fmt.Errorf("cannot build the transient %s workload: scratch size %q: %w", spec.Service, spec.ScratchSize, err)
	}

	// The container's ephemeral-storage limit sits above the scratch volume's
	// own sizeLimit, because the volume's usage counts towards that limit and
	// the container's logs and writable layer also do. Without the margin a
	// capture that exactly filled its scratch would be evicted for the
	// container's own log output.
	//
	// Both are stated on purpose: the sizeLimit stops the staging directory
	// growing past its allocation, and the resource limit is what makes the
	// kubelet evict this pod rather than let it fill the node and take
	// neighbouring workloads with it — a backup damaging the running instance,
	// which constitution Principle II forbids.
	//
	// The addition saturates because scratch comes from operator input: a
	// quantity close enough to the int64 ceiling wraps it negative, and
	// formatGiB floors a negative at 1Gi — an ephemeral-storage limit *below*
	// the scratch volume it is supposed to sit above, which is the exact
	// inversion the margin exists to prevent.
	limit := formatGiB(saturatingAdd(scratch, transientScratchLimitMargin))
	imageUser := databaseImageUser(spec.Service)

	manifest := transientJobManifest{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata: transientObjectMeta{
			Name:      spec.jobName(),
			Namespace: spec.Namespace,
			Labels:    spec.labels(),
		},
		Spec: transientJobSpec{
			ActiveDeadlineSeconds:   spec.deadlineSeconds(),
			TTLSecondsAfterFinished: transientFinishedTTL,
			BackoffLimit:            0,
			Template: transientPodTemplate{
				Metadata: transientObjectMeta{Labels: spec.labels()},
				Spec: transientPodSpec{
					// Never, so a failed container is not restarted behind the tool's
					// back and so the Job deadline terminates the pod rather than
					// beginning a restart loop (FR-011).
					RestartPolicy: "Never",
					// The same deadline the Job carries, restated so the pod
					// carries its own — see transientPodSpec.
					ActiveDeadlineSeconds: spec.deadlineSeconds(),
					// The pod holds no state worth flushing: the artifact has already
					// been copied out by the time it is reaped.
					TerminationGracePeriodSeconds: transientTerminationGrace,
					// The workload talks to a database, never to the API server.
					AutomountServiceAccountToken: boolPtr(false),
					// Service links inject an env var per service in the namespace,
					// which is both noise and a collision risk against the credential
					// variables the tooling reads.
					EnableServiceLinks: boolPtr(false),
					// Supplied by the operator, and absent unless they supplied it.
					// The pod still mounts no service-account token: naming an account
					// here is how a cluster attaches its registry credentials and
					// satisfies its own admission, not a way for this pod to reach the
					// API server.
					ServiceAccountName: spec.Scheduling.ServiceAccount,
					ImagePullSecrets:   spec.Scheduling.ImagePullSecrets,
					NodeSelector:       spec.Scheduling.NodeSelector,
					Tolerations:        spec.Scheduling.Tolerations,
					PriorityClassName:  spec.Scheduling.PriorityClass,
					// runAsUser is stated, not inferred: see databaseImageUser for
					// why leaving it to the image makes the kubelet refuse every one
					// of these pods. runAsGroup pairs with it so the staged artefact
					// is owned by the database's own user and group, and fsGroup is
					// what makes the scratch volume writable even where the uid turns
					// out not to match the image's.
					SecurityContext: &transientPodSecurityContext{
						RunAsNonRoot:        boolPtr(true),
						RunAsUser:           int64Ptr(imageUser),
						RunAsGroup:          int64Ptr(imageUser),
						FSGroup:             int64Ptr(imageUser),
						FSGroupChangePolicy: "OnRootMismatch",
						SeccompProfile:      &transientSeccompProfile{Type: "RuntimeDefault"},
					},
					Containers: []transientContainer{{
						Name:            transientContainerName,
						Image:           spec.Image,
						ImagePullPolicy: "IfNotPresent",
						Command:         []string{"sh"},
						// The container does nothing on its own; it exists to be
						// exec'd into. Sleeping exactly the pod's deadline means the
						// container ends when the deadline does, so the pod's own
						// bound and its process agree.
						Args: []string{"-c", fmt.Sprintf("exec sleep %d", spec.deadlineSeconds())},
						EnvFrom: []transientEnvFrom{{
							SecretRef: transientSecretRef{
								Name: spec.secretName(),
								// Not optional: a missing credential secret must hold
								// the container in a visible pending state, never let
								// it start unauthenticated.
								Optional: boolPtr(false),
							},
						}},
						Resources: transientResources{
							Requests: map[string]string{
								"cpu":               "100m",
								"memory":            "256Mi",
								"ephemeral-storage": spec.ScratchSize,
							},
							Limits: map[string]string{
								"memory":            "2Gi",
								"ephemeral-storage": limit,
							},
						},
						VolumeMounts: []transientVolumeMount{{
							Name:      transientScratchVol,
							MountPath: transientScratchPath,
						}},
						SecurityContext: &transientContainerSecurityContext{
							AllowPrivilegeEscalation: boolPtr(false),
							Privileged:               boolPtr(false),
							RunAsNonRoot:             boolPtr(true),
							// Restricted pod security does not require a read-only
							// root filesystem, and the database tooling writes config
							// and logs outside the scratch mount, so requiring it here
							// would break the tooling for no policy gain.
							ReadOnlyRootFilesystem: boolPtr(false),
							Capabilities:           &transientCapabilities{Drop: []string{"ALL"}},
						},
					}},
					Volumes: []transientVolume{{
						Name:     transientScratchVol,
						EmptyDir: transientEmptyDir{SizeLimit: spec.ScratchSize},
					}},
				},
			},
		},
	}

	return json.Marshal(manifest)
}

// buildTransientSecretManifest renders the credential secret owned by the Job.
//
// The owner reference is the whole of FR-023. The Job's deadline and TTL bound
// the controller, but nothing bounds a secret; binding the secret's lifetime
// to the Job is what makes "credentials cannot outlive the run" hold when this
// process is killed before it can delete anything.
//
// An owner reference with an empty or wrong UID is silently ignored — the
// object is created, the reference is stored, and nothing ever collects it — so
// an empty UID is refused here rather than written.
func buildTransientSecretManifest(spec transientWorkloadSpec, jobUID string) ([]byte, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}

	if strings.TrimSpace(jobUID) == "" {
		return nil, fmt.Errorf("refusing to create the %s credential secret without the transient Job's UID: an owner reference with an empty UID does not bind, and the credentials would outlive the run (FR-023)", spec.Service)
	}

	if len(spec.Credentials) == 0 {
		return nil, fmt.Errorf("refusing to create an empty %s credential secret: no credentials were resolved for the external database", spec.Service)
	}

	manifest := transientSecretManifest{
		APIVersion: "v1",
		Kind:       "Secret",
		Type:       "Opaque",
		Metadata: transientObjectMeta{
			Name:      spec.secretName(),
			Namespace: spec.Namespace,
			Labels:    spec.labels(),
			OwnerReferences: []transientOwnerRef{{
				APIVersion: "batch/v1",
				Kind:       "Job",
				Name:       spec.jobName(),
				UID:        strings.TrimSpace(jobUID),
				// The Job is the secret's owner for garbage-collection
				// purposes, not its controller, and it must not block its own
				// deletion — the TTL is the mechanism, so it has to
				// stay unobstructed.
				Controller:         boolPtr(false),
				BlockOwnerDeletion: boolPtr(false),
			}},
		},
		StringData: spec.Credentials,
	}

	return json.Marshal(manifest)
}

func (s transientWorkloadSpec) validate() error {
	switch {
	case strings.TrimSpace(s.RunID) == "":
		return fmt.Errorf("cannot build a transient workload without a run ID")
	case s.Service != serviceNeo4j && s.Service != serviceTaskManagerDB:
		return errNotADatabaseService("build a transient workload for", s.Service)
	case s.Role != workloadRoleProbe && s.Role != workloadRoleCapture && s.Role != workloadRoleRestore:
		return fmt.Errorf("cannot build a transient workload with role %q: the roles are %s, %s and %s", s.Role, workloadRoleProbe, workloadRoleCapture, workloadRoleRestore)
	case strings.TrimSpace(s.Image) == "":
		return fmt.Errorf("cannot build the transient %s workload without an image: supply %s", s.Service, versionImageFlag(s.Service))
	case strings.TrimSpace(s.Namespace) == "":
		return fmt.Errorf("cannot build the transient %s workload without a namespace", s.Service)
	case strings.TrimSpace(s.ScratchSize) == "":
		return fmt.Errorf("cannot build the transient %s workload without a scratch size", s.Service)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Placement and image access (FR-027)
// ---------------------------------------------------------------------------

// The flags the operator supplies these through. Named here so the parsers can
// put the flag into their own failure messages, which is what the CLI-surface
// contract asks of every one of them: the condition, the resource, the action.
const (
	imagePullSecretsFlag = "--external-db-image-pull-secrets"
	serviceAccountFlag   = "--external-db-service-account"
	nodeSelectorFlag     = "--external-db-node-selector"
	tolerationsFlag      = "--external-db-tolerations"
	priorityClassFlag    = "--external-db-priority-class"
)

// tolerationEffects are the effects a toleration may name. The API server
// rejects anything else, so refusing it here is the difference between a
// message naming the flag and a manifest validation error the operator has to
// work backwards from — the same reason parseStorageQuantity exists.
var tolerationEffects = []string{"NoSchedule", "PreferNoSchedule", "NoExecute"}

// resolveTransientScheduling parses the operator's placement inputs once per
// workload, before anything is created.
//
// It is called from both spec builders rather than from manifest construction
// so that a malformed input fails the run at the point the operator can still
// read it as being about their own flag, rather than as a pod that would not
// admit.
func resolveTransientScheduling(cfg *Configuration) (transientScheduling, error) {
	scheduling := transientScheduling{}
	if cfg == nil {
		return scheduling, nil
	}

	supplied := cfg.ExternalDB.Scheduling

	secrets, err := parseObjectNames(supplied.ImagePullSecrets, imagePullSecretsFlag)
	if err != nil {
		return transientScheduling{}, err
	}
	for _, name := range secrets {
		scheduling.ImagePullSecrets = append(scheduling.ImagePullSecrets, transientLocalObjectRef{Name: name})
	}

	if scheduling.ServiceAccount, err = parseSingleObjectName(supplied.ServiceAccount, serviceAccountFlag, "run", "service accounts"); err != nil {
		return transientScheduling{}, err
	}

	if scheduling.PriorityClass, err = parseSingleObjectName(supplied.PriorityClass, priorityClassFlag, "admit", "priority classes"); err != nil {
		return transientScheduling{}, err
	}

	if scheduling.NodeSelector, err = parseNodeSelector(supplied.NodeSelector); err != nil {
		return transientScheduling{}, err
	}

	if scheduling.Tolerations, err = parseTolerations(supplied.Tolerations); err != nil {
		return transientScheduling{}, err
	}

	return scheduling, nil
}

// parseSingleObjectName is parseObjectNames for a flag that takes one name: the
// name, "" when none was supplied, and a refusal naming the flag when several
// were. verb and plural word that refusal for the flag's own kind of object.
func parseSingleObjectName(text, flag, verb, plural string) (string, error) {
	names, err := parseObjectNames(text, flag)
	if err != nil {
		return "", err
	}
	if len(names) > 1 {
		return "", fmt.Errorf("cannot %s the transient workload under %d %s: %s takes one name", verb, len(names), plural, flag)
	}
	if len(names) == 0 {
		return "", nil
	}

	return names[0], nil
}

// parseObjectNames reads a comma-separated list of Kubernetes object names.
//
// Whitespace inside a name is refused rather than trimmed away in the middle:
// a name with a space in it is a typo in the operator's list, and silently
// repairing it would create a reference to an object that is not the one they
// meant to name.
func parseObjectNames(text, flag string) ([]string, error) {
	names := []string{}
	for _, name := range commaFields(text) {
		if strings.ContainsAny(name, " \t") {
			return nil, fmt.Errorf("%q is not a Kubernetes object name: %s takes a comma-separated list of names, and this one contains a space", name, flag)
		}

		names = append(names, name)
	}

	return names, nil
}

// parseNodeSelector reads a comma-separated list of key=value node labels.
func parseNodeSelector(text string) (map[string]string, error) {
	selector := map[string]string{}
	for _, pair := range commaFields(text) {
		key, value, found := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !found || key == "" {
			return nil, fmt.Errorf("%q is not a node label: %s takes a comma-separated list of key=value pairs, such as node-role=backup", pair, nodeSelectorFlag)
		}

		selector[key] = strings.TrimSpace(value)
	}

	if len(selector) == 0 {
		return nil, nil
	}

	return selector, nil
}

// parseTolerations reads a comma-separated list of taints the workload
// tolerates, in the notation kubectl itself uses for a taint: key=value:Effect,
// key:Effect, or a bare key for every effect that key carries.
func parseTolerations(text string) ([]transientToleration, error) {
	tolerations := []transientToleration{}
	for _, entry := range commaFields(text) {
		toleration, err := parseToleration(entry)
		if err != nil {
			return nil, err
		}

		tolerations = append(tolerations, toleration)
	}

	if len(tolerations) == 0 {
		return nil, nil
	}

	return tolerations, nil
}

func parseToleration(entry string) (transientToleration, error) {
	malformed := func() (transientToleration, error) {
		return transientToleration{}, fmt.Errorf("%q is not a toleration: %s takes a comma-separated list written the way a taint is — key=value:Effect, key:Effect, or a bare key — with the effect one of %s", entry, tolerationsFlag, strings.Join(tolerationEffects, ", "))
	}

	keyed, effect, hasEffect := strings.Cut(entry, ":")
	effect = strings.TrimSpace(effect)
	if hasEffect && !slices.Contains(tolerationEffects, effect) {
		return malformed()
	}

	key, value, hasValue := strings.Cut(keyed, "=")
	key = strings.TrimSpace(key)
	if key == "" {
		return malformed()
	}

	// Equal needs a value to compare against and Exists must not carry one, so
	// the operator's notation picks the operator rather than this defaulting to
	// one of them: `key=value:Effect` is Equal, and `key:Effect` — a taint with
	// no value — is Exists.
	toleration := transientToleration{Key: key, Operator: "Exists", Effect: effect}
	if hasValue {
		toleration.Operator = "Equal"
		toleration.Value = strings.TrimSpace(value)
	}

	return toleration, nil
}

// ---------------------------------------------------------------------------
// Scratch sizing (FR-022)
// ---------------------------------------------------------------------------

// resolveScratchSize decides the scratch allocation for a capture.
//
// The operator's override wins outright, because it is the only input that can
// account for a database about to grow or a cluster with a tighter disk budget
// than the arithmetic assumes. Otherwise the size is the queried store size
// times the headroom factor, floored, and rounded up to whole GiB.
//
// storeSizeBytes of zero or less means the query did not produce an answer.
// That is not a licence to guess: FR-022's closing clause makes the failure
// name the override, so the operator is told the one thing that unblocks them
// rather than being handed a size that may silently be too small.
// normaliseScratchOverride reads an operator-supplied scratch quantity, and
// reports whether one was supplied at all so that each arm can apply its own
// answer to "and if not?".
//
// The normalisation is why both arms come through here rather than each doing
// it. It is not cosmetic: parseStorageQuantity is deliberately more tolerant
// than the API server's quantity parser — it accepts "50 Gi", which Kubernetes
// rejects — so returning the operator's text unchanged only moves the refusal
// to manifest validation, where the message names neither the flag nor the
// value and the operator has to work backwards from it. Rounding up to whole
// GiB can only give more space than was asked for, never less.
//
// That reason used to be written on the capture arm alone, so the restore arm
// depended on an unstated rule: a reader had no way to tell that its formatGiB
// was load-bearing, and removing it would have looked like a simplification.
//
// subject names whose scratch space this is and appears in the refusal, which
// is the only thing the two arms differ on.
func normaliseScratchOverride(override, subject string) (size string, supplied bool, err error) {
	trimmed := strings.TrimSpace(override)
	if trimmed == "" {
		return "", false, nil
	}

	bytes, err := parseStorageQuantity(trimmed)
	if err != nil {
		return "", true, fmt.Errorf("cannot use the scratch size %q supplied for %s: %w", trimmed, subject, err)
	}

	size = formatGiB(bytes)
	if size != trimmed {
		logrus.Debugf("Normalised the scratch size %q supplied for %s to %s", trimmed, subject, size)
	}

	return size, true, nil
}

func resolveScratchSize(service, override string, storeSizeBytes int64) (string, error) {
	if size, supplied, err := normaliseScratchOverride(override, service); supplied {
		return size, err
	}

	if storeSizeBytes <= 0 {
		return "", fmt.Errorf("cannot size the scratch space for the external %s capture: the store size could not be determined, so supply --external-db-scratch-size (a storage quantity such as 50Gi) rather than have the tool guess one", service)
	}

	// The product is range-checked before it is converted, because converting a
	// float64 that does not fit in an int64 is undefined in Go and the two
	// architectures `make build-all` ships disagree about what it does: arm64
	// saturates to MaxInt64, amd64 wraps to MinInt64. The wrap is the dangerous
	// one — a negative size is below the floor, so the clamp below silently
	// rewrites an unrepresentable store into 5Gi and the capture runs with a
	// scratch allocation several orders of magnitude too small, on the
	// architecture the tool is most often built for.
	//
	// The bound is the same maxStorageQuantityBytes parseStorageQuantity uses,
	// and for the same reason: it is the first power of two above the int64
	// range, and unlike math.MaxInt64 it is exactly representable as a float64.
	// Beyond it there is no size to hand out — the API server would refuse the
	// quantity even if the arithmetic held — so this takes FR-022's closing
	// clause and names the override, which is the one thing that unblocks the
	// operator.
	product := math.Ceil(float64(storeSizeBytes) * externalDBScratchHeadroomFactor)
	if product >= maxStorageQuantityBytes {
		return "", fmt.Errorf("cannot size the scratch space for the external %s capture: the server reported a store of %s, and %.1fx that is too large to allocate, so supply --external-db-scratch-size (a storage quantity such as 50Gi) if that store size is real", service, formatBytes(storeSizeBytes), externalDBScratchHeadroomFactor)
	}

	sized := int64(product)
	if floor := int64(externalDBScratchFloorGiB) << 30; sized < floor {
		sized = floor
	}

	size := formatGiB(sized)
	logrus.Infof("Sized the %s capture scratch space at %s, from a store of %s with a %.1fx headroom factor", service, size, formatBytes(storeSizeBytes), externalDBScratchHeadroomFactor)

	return size, nil
}

// storageSuffixes are the quantity suffixes Kubernetes accepts for storage,
// binary forms first so that "Ki" is not read as "K" followed by junk.
var storageSuffixes = []struct {
	suffix     string
	multiplier float64
}{
	{"Ki", 1 << 10},
	{"Mi", 1 << 20},
	{"Gi", 1 << 30},
	{"Ti", 1 << 40},
	{"Pi", 1 << 50},
	{"k", 1e3},
	{"K", 1e3},
	{"M", 1e6},
	{"G", 1e9},
	{"T", 1e12},
	{"P", 1e15},
}

// parseStorageQuantity reads a Kubernetes storage quantity into bytes.
//
// It exists so that an override the cluster would reject is refused here, where
// the message can name the flag, rather than by the API server as a manifest
// validation error the operator has to work backwards from. It is not a full
// quantity parser — storage never needs the milli or exponent forms — but it
// accepts every shape a storage value is written in, fractions included.
func parseStorageQuantity(text string) (int64, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, fmt.Errorf("storage quantity is empty")
	}

	multiplier := 1.0
	mantissa := trimmed

	for _, candidate := range storageSuffixes {
		if after, ok := strings.CutSuffix(trimmed, candidate.suffix); ok {
			multiplier = candidate.multiplier
			mantissa = after

			break
		}
	}

	value, err := strconv.ParseFloat(strings.TrimSpace(mantissa), 64)
	if err != nil {
		return 0, fmt.Errorf("%q is not a storage quantity (expected a number with an optional Ki/Mi/Gi/Ti suffix, such as 50Gi)", trimmed)
	}

	// Rounded before the range check, so the bound is tested against the value
	// that will actually be converted rather than the one before it.
	//
	// The bound is 2^63 and not math.MaxInt64 because math.MaxInt64 is not
	// representable as a float64: converting it rounds *up* to 2^63, so
	// `bytes > math.MaxInt64` compares against a number one larger than the
	// largest int64 and lets exactly 2^63 through — and converting a float64
	// that does not fit in an int64 is undefined in Go, so what came out the
	// other side was whatever the hardware did. 2^63 is representable exactly,
	// and the comparison is >= so the value that rounds onto it is refused
	// rather than converted.
	bytes := math.Ceil(value * multiplier)
	switch {
	case bytes <= 0:
		return 0, fmt.Errorf("%q is not a positive storage quantity", trimmed)
	case bytes >= maxStorageQuantityBytes:
		return 0, fmt.Errorf("%q is too large to be a storage quantity", trimmed)
	}

	return int64(bytes), nil
}

// maxStorageQuantityBytes is the exclusive ceiling on a storage quantity: the
// first power of two above the int64 range, which unlike math.MaxInt64 is
// exactly representable as a float64.
const maxStorageQuantityBytes = float64(1 << 63)

// transientScratchLimitMargin is how far the container's ephemeral-storage
// limit sits above the scratch volume's own sizeLimit.
const transientScratchLimitMargin = int64(1) << 30

// saturatingAdd adds two non-negative values, returning math.MaxInt64 where the
// sum would not fit rather than wrapping into a negative one. Every caller here
// is guarding a size or a deadline read from outside this process, where a
// wrapped value does not merely compute wrongly — it inverts the comparison it
// feeds and turns a guard into its opposite.
func saturatingAdd(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}

	return a + b
}

// formatGiB rounds up to whole GiB, which is the granularity a scratch
// allocation is worth expressing in and the one an operator reading the pod
// spec can check against their node's disk at a glance.
func formatGiB(bytes int64) string {
	const giB = int64(1) << 30

	gibs := bytes / giB
	if bytes%giB != 0 {
		gibs++
	}
	if gibs < 1 {
		gibs = 1
	}

	return fmt.Sprintf("%dGi", gibs)
}

// ---------------------------------------------------------------------------
// Exclusion from replica discovery (FR-028)
// ---------------------------------------------------------------------------

// transientWorkloadExclusion is the label-selector clause that keeps the
// transient workload out of a pod listing. `!key` is a does-not-exist
// requirement, so it drops every object this file creates — whatever it is
// named, whatever else it is labelled with — and nothing else in the namespace.
const transientWorkloadExclusion = "!" + transientLabelMarker

// excludeTransientWorkloads adds the exclusion to a service selector, so the
// cluster never returns the transient pod in the first place.
func excludeTransientWorkloads(selector string) string {
	if strings.TrimSpace(selector) == "" {
		return transientWorkloadExclusion
	}

	return selector + "," + transientWorkloadExclusion
}

// isTransientObjectName reports whether a name belongs to an object this file
// creates.
//
// It is the *last* half of the FR-028 exclusion, and it exists for the case
// neither the selector nor the pod's own label can cover: a listing issued
// without going through podListingArgsFor, of an output shape that carries no
// label fields — podNamePhaseJSONPath is exactly that shape. Matching on the
// prefix is sound for that job because the prefix is this package's own
// constant, not a guess about what the cluster contains.
//
// It is deliberately not the only client-side half. A name test makes the
// exclusion rest on transientObjectPrefix's spelling, and the prefix is a
// constant a later change may reasonably alter — to one containing a service
// name, at which point a pod that escaped the selector would be *claimed* by
// the name fallback rather than merely missed by the filter. Where the listing
// carries labels, labelledPod.Transient is what withoutTransientPods reads
// first.
func isTransientObjectName(name string) bool {
	return strings.HasPrefix(strings.TrimSpace(name), transientObjectPrefix+"-")
}

// withoutTransientPods removes this run's own workloads from a pod listing.
//
// It is applied to the parsed listing rather than to a list of names, and that
// is what makes it one function. There were three — one for names, one for a
// name-and-phase listing and one for an ownership listing — differing only in
// which field each read the name out of, because the two name-taking callers
// filtered *after* projecting the listing down to names. Filtering first is the
// same set (the projection does not invent or rename a pod) and leaves one
// filter for one listing type.
//
// The transient pod declares its service under a key of this tool's own
// (transientLabelService), not one of serviceLabelKeys, so it contributes no
// ownership evidence either way — but a run's own workload must not be the
// reason the deployment is judged to claim a service it does not.
//
// A pod is dropped on what it *declares* first and on what it is called second.
// That order is the point: the name test rests on transientObjectPrefix, and the
// paths that reach this filter after a selector matched nothing — the running
// check and the singular resolver — go on to match a name against a service, so
// a prefix that ever came to contain a service name would turn a pod this filter
// missed into a pod the fallback claims. The marker cannot spell itself into
// that hazard, and it is on every object this file creates.
func withoutTransientPods(pods []labelledPod) []labelledPod {
	kept := make([]labelledPod, 0, len(pods))
	for _, pod := range pods {
		if pod.Transient || isTransientObjectName(pod.Name) {
			logrus.Debugf("Excluding the transient workload %s from pod discovery (FR-028)", pod.Name)

			continue
		}
		kept = append(kept, pod)
	}

	return kept
}

// ---------------------------------------------------------------------------
// Reaping (FR-011)
// ---------------------------------------------------------------------------

const (
	transientKindPod    = "pod"
	transientKindSecret = "secret"

	// transientStrayGrace is how long the reaper waits past the evidence of
	// abandonment before removing another run's object.
	//
	// It exists because the mechanism that reclaims a workload is the Job's
	// deadline and TTL, not this reaper; the reaper is the tidy-up for what the
	// cluster's own reclamation left or could not reach. Waiting past the
	// deadline before acting means a pod the cluster is in the middle of
	// reclaiming is not raced, and a peer run whose clock differs slightly from
	// ours is not misjudged.
	transientStrayGrace = 15 * time.Minute
)

// transientCandidate is one labelled object as the cluster reports it: the
// facts abandonment can be established from, and nothing else.
type transientCandidate struct {
	Kind  string
	Name  string
	RunID string
	// Phase is the pod's status phase; empty for a secret.
	Phase string
	// Created is the object's creation timestamp, or the zero time when it
	// could not be read — which is treated as "unknown", never as "old".
	Created time.Time
	// DeadlineSeconds is the pod's own activeDeadlineSeconds, or zero when it
	// has none.
	//
	// A pod this tool creates carries one because transientPodSpec restates
	// the Job's deadline on the pod template: the Job controller does not copy
	// it down, and this field is read from a pod listing, so without that
	// restatement the deadline arm of selectTransientStrays would read zero for
	// every candidate and never fire.
	//
	// A pod left behind by a version that did not restate it still reports
	// zero, which selection treats as insufficient evidence — the safe reading,
	// and one the terminal-phase arm covers anyway.
	DeadlineSeconds int64
}

// transientStray is a candidate the reaper has established is abandoned, with
// the evidence that established it — logged, so an operator who finds an object
// missing can see why it was judged abandoned.
type transientStray struct {
	Kind   string
	Name   string
	RunID  string
	Reason string
}

// transientListJSONPath is the jsonpath the reaper lists candidates with. The
// run-ID label is read through escapeJSONPathKey, the same way every other
// label key in this package is: a "/" needs no escaping and a "." does, and one
// convention is what keeps a key that later grows a dot from silently reading
// as a path.
func transientListJSONPath(kind string) string {
	runID := escapeJSONPathKey(transientLabelRunID)

	if kind == transientKindPod {
		return fmt.Sprintf(`jsonpath={range .items[*]}{.metadata.name}{"\t"}{.metadata.labels.%s}{"\t"}{.status.phase}{"\t"}{.metadata.creationTimestamp}{"\t"}{.spec.activeDeadlineSeconds}{"\n"}{end}`, runID)
	}

	return fmt.Sprintf(`jsonpath={range .items[*]}{.metadata.name}{"\t"}{.metadata.labels.%s}{"\t"}{"\t"}{.metadata.creationTimestamp}{"\n"}{end}`, runID)
}

// parseTransientCandidates reads the tab-separated listing into candidates.
//
// A field it cannot read is left at its zero value rather than guessed, and
// selection treats every zero value as insufficient evidence to reap. That is
// the safe direction: an unreadable timestamp leaves an object behind for the
// cluster's deadline to reclaim, where a guessed one could delete a live peer's
// workload.
func parseTransientCandidates(kind, output string) []transientCandidate {
	candidates := []transientCandidate{}

	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 2 || strings.TrimSpace(fields[0]) == "" {
			continue
		}

		candidate := transientCandidate{
			Kind:  kind,
			Name:  strings.TrimSpace(fields[0]),
			RunID: strings.TrimSpace(fields[1]),
		}

		if len(fields) > 2 {
			candidate.Phase = strings.TrimSpace(fields[2])
		}
		if len(fields) > 3 {
			if created, err := time.Parse(time.RFC3339, strings.TrimSpace(fields[3])); err == nil {
				candidate.Created = created
			}
		}
		if len(fields) > 4 {
			if seconds, err := strconv.ParseInt(strings.TrimSpace(fields[4]), 10, 64); err == nil {
				candidate.DeadlineSeconds = seconds
			}
		}

		candidates = append(candidates, candidate)
	}

	return candidates
}

// outlivedItsDeadline reports whether a pod is far enough past its own
// activeDeadlineSeconds that the cluster should already have reclaimed it.
//
// The comparison is in seconds rather than between durations, and that is the
// whole point of the function. activeDeadlineSeconds is an int64 read off a pod
// this run did not create, and time.Duration counts nanoseconds: multiplying up
// any value above about 9.2e9 seconds wraps the product negative, and a
// negative deadline makes *every* pod compare as expired. The pod the reaper
// then deletes is a live peer's Running one — the precise damage the selection
// rules this feeds exist to prevent, and what constitution Principle II
// forbids. The age is a duration this process computed, so converting it down
// carries no such risk, and the grace period is added with saturation so the
// same wrap cannot reappear one term to the right.
func outlivedItsDeadline(deadlineSeconds int64, age time.Duration) bool {
	return int64(age.Seconds()) > saturatingAdd(deadlineSeconds, int64(transientStrayGrace.Seconds()))
}

// selectTransientStrays decides which labelled objects are abandoned.
//
// This is the safety-critical judgement of the whole file, and it is pure over
// its inputs so it can be exercised exhaustively without a cluster.
//
// The trap it exists to avoid: "a stray from a dead run" and "a workload
// belonging to a backup running right now" carry exactly the same marker label
// and both carry a run ID that is not ours. Reaping on the label and a
// different run ID alone would make two operators backing up the same namespace
// destroy each other's captures, and would make a scheduled backup overlapping
// a manual one do the same — a safety mechanism causing precisely the damage to
// a running instance that constitution Principle II forbids.
//
// So a different run ID is never sufficient. Something that actually implies
// abandonment is required, and only two things do:
//
//   - the pod is terminal, so no capture can be running in it — including the
//     pod the cluster terminated at its own deadline, which lands in Failed. It
//     is the package's single terminal-phase predicate that says so
//     (podPhaseIsTerminal); or
//   - the pod has outlived its own activeDeadlineSeconds by the grace period,
//     which means the cluster should already have reclaimed it and no operation
//     inside it can still be legitimately running, because its own bound has
//     passed.
//
// A Running or Pending pod inside its deadline is left strictly alone, however
// old the run ID looks, because it is indistinguishable from a live peer.
//
// Secrets are the objects the cluster's own reclamation cannot always reach: a
// secret is collected when its owner pod goes, so one whose run has no pod at
// all is either mid-creation (seconds old) or an object the garbage collector
// never took. Only the second, established by the grace period, is reaped.
func selectTransientStrays(candidates []transientCandidate, ownRunID string, now time.Time) []transientStray {
	runsWithAPod := map[string]bool{}
	for _, candidate := range candidates {
		if candidate.Kind == transientKindPod && candidate.RunID != "" {
			runsWithAPod[candidate.RunID] = true
		}
	}

	strays := []transientStray{}

	for _, candidate := range candidates {
		switch candidate.RunID {
		case "":
			// Marked but unattributable. It cannot be shown to be abandoned, and
			// the tool did not create it in any form it recognises, so it is
			// left for its owner or its deadline.
			logrus.Debugf("Leaving the transient %s %s alone: it carries no %s label, so it cannot be shown to be abandoned", candidate.Kind, candidate.Name, transientLabelRunID)

			continue
		case ownRunID:
			// This run's own object. Release owns its removal; reaping it here
			// would delete the workload out from under the capture using it.
			continue
		}

		switch candidate.Kind {
		case transientKindPod:
			switch {
			case podPhaseIsTerminal(candidate.Phase):
				strays = append(strays, transientStray{
					Kind: candidate.Kind, Name: candidate.Name, RunID: candidate.RunID,
					Reason: fmt.Sprintf("the pod is %s, so nothing is running in it", candidate.Phase),
				})
			case candidate.DeadlineSeconds > 0 && !candidate.Created.IsZero() &&
				outlivedItsDeadline(candidate.DeadlineSeconds, now.Sub(candidate.Created)):
				strays = append(strays, transientStray{
					Kind: candidate.Kind, Name: candidate.Name, RunID: candidate.RunID,
					Reason: fmt.Sprintf("the pod has outlived its own %ds deadline by more than %s, so the cluster should already have reclaimed it", candidate.DeadlineSeconds, transientStrayGrace),
				})
			default:
				logrus.Debugf("Leaving the transient pod %s from run %s alone: it is %s and inside its own deadline, so it may belong to a backup running now", candidate.Name, candidate.RunID, candidate.Phase)
			}
		case transientKindSecret:
			switch {
			case runsWithAPod[candidate.RunID]:
				// Its owner is present, whatever phase it is in. The garbage
				// collector removes the secret when the pod goes, so there is
				// nothing to do and possibly a live run to damage.
				continue
			case candidate.Created.IsZero() || now.Sub(candidate.Created) <= transientStrayGrace:
				// Recent enough that the run may be between creating its pod and
				// creating this secret, or between the pod's deletion and the
				// collector following the owner reference.
				continue
			default:
				strays = append(strays, transientStray{
					Kind: candidate.Kind, Name: candidate.Name, RunID: candidate.RunID,
					Reason: fmt.Sprintf("the owning pod of run %s is gone and the garbage collector has not reclaimed the secret", candidate.RunID),
				})
			}
		}
	}

	return strays
}

// ReapTransientStrays removes transient objects earlier runs left behind, so
// an operator never has to clean up by hand (FR-011).
//
// It is called once per run, by reapTransientStrays, at the moment the run
// first establishes that it will create a transient workload of its own — which
// is the start of the external path rather than the start of the process. Not
// earlier, because this lists pods *and* secrets, and putting two API calls and
// two RBAC verbs on every Kubernetes run would make FR-015's "all-internal
// deployments unchanged" false and would fail runs on clusters where this tool
// cannot list secrets.
//
// It is a tidy-up, not the reclamation mechanism: the Job's deadline and TTL
// bound a workload when the tool never returns, and its owner references remove
// the pod and credential Secret with it. What is left for this to find is a
// terminal pod whose Job has not been collected yet, or an orphaned Secret.
func (k *KubernetesBackend) ReapTransientStrays(ownRunID string) error {
	return k.reapTransientStraysWith(k.transientOps(), ownRunID, time.Now())
}

// reapTransientStraysWith is the reaper against supplied cluster operations and
// a supplied clock, so both the listing and the age arithmetic are drivable in
// a test.
func (k *KubernetesBackend) reapTransientStraysWith(ops transientClusterOps, ownRunID string, now time.Time) error {
	candidates := []transientCandidate{}

	for _, kind := range []string{transientKindPod, transientKindSecret} {
		output, err := ops.query("kubectl", "get", kind+"s", "-n", k.namespace,
			"-l", transientLabelMarker+"=true", "-o", transientListJSONPath(kind))
		if err != nil {
			return fmt.Errorf("failed to list transient %ss left behind in namespace %s: %w", kind, k.namespace,
				workloadPermissionError(err, "list", kind+"s", k.namespace))
		}

		candidates = append(candidates, parseTransientCandidates(kind, output)...)
	}

	strays := selectTransientStrays(candidates, ownRunID, now)
	if len(strays) == 0 {
		logrus.Debugf("No transient external-database objects from earlier runs need removing in namespace %s", k.namespace)

		return nil
	}

	errs := []error{}
	for _, stray := range strays {
		logrus.Warnf("Removing the transient %s %s left behind by run %s: %s", stray.Kind, stray.Name, stray.RunID, stray.Reason)

		// --ignore-not-found because the object's own run, or the cluster's
		// garbage collector, may have removed it between the listing and now;
		// that is the expected outcome, not a failure.
		if err := ops.remove(stray.Kind, stray.Name, "--ignore-not-found", "--wait=false"); err != nil {
			errs = append(errs, fmt.Errorf("failed to remove the transient %s %s left behind by run %s: %w", stray.Kind, stray.Name, stray.RunID,
				workloadPermissionError(err, "delete", stray.Kind+"s", k.namespace)))
		}
	}

	return errors.Join(errs...)
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// transientClusterOps is the narrow set of cluster operations the workload
// lifecycle needs, injected so the lifecycle is drivable in a test without a
// cluster — the same seam getAllPodsWith and resolveEndpointWith already use.
type transientClusterOps struct {
	// create pipes a manifest to the cluster.
	create func(manifest []byte) error
	// query runs a read-only kubectl command and returns its trimmed output.
	query podRunner
	// queryWithin is query under a caller-supplied bound.
	//
	// It exists for the readiness wait, the one cluster call whose duration its
	// own argument sets: bounding `kubectl wait --timeout=600s` at the control
	// bound would kill it at two minutes, long before the image pull the
	// ten-minute readiness timeout was chosen to cover.
	queryWithin func(bound time.Duration) podRunner
	// remove deletes objects; args are appended to `kubectl delete -n <ns>`.
	remove func(args ...string) error
}

// transientOps binds the lifecycle's cluster operations to kubectl, every one of
// them time-bounded (FR-025).
//
// The workload exists only to reach a database outside the deployment, so a
// kubectl call that hangs here hangs the run just as surely as a stalled capture
// would — and on the restore path it does so with Infrahub scaled down. None of
// these calls touches the database, which is why they run under the short
// control bound rather than the operation bound.
func (k *KubernetesBackend) transientOps() transientClusterOps {
	bound := externalDBControlBound(k.config)
	target := externalDBTarget("the transient external-database workload", k.namespace)

	return transientClusterOps{
		create: func(manifest []byte) error {
			// `create`, not `apply`: a name collision means the object belongs
			// to something else, and adopting it would break the invariant
			// that a workload is owned by exactly one run.
			//
			// The manifest travels on stdin, which is also what keeps
			// credentials off a command line — the executor's debug log records
			// the arguments, and the arguments are `create -n <ns> -f -`
			// (FR-014).
			wait, err := k.executor.runCommandWritePipeContext(context.Background(), bound, bytes.NewReader(manifest), "kubectl", "create", "-n", k.namespace, "-f", "-")
			if err != nil {
				return err
			}

			return wrapExternalTimeout(wait(), bound, "kubectl create", target)
		},
		query: k.externalDBRunner(bound, target),
		queryWithin: func(waitBound time.Duration) podRunner {
			return k.externalDBRunner(waitBound, target)
		},
		remove: func(args ...string) error {
			full := append([]string{"delete", "-n", k.namespace}, args...)
			_, err := k.externalDBRunner(bound, target)("kubectl", full...)

			return err
		},
	}
}

// newRunID returns an identifier unique to this run, short enough to sit
// inside a label value and a pod name and safe in both.
func newRunID() (string, error) {
	raw := make([]byte, 4)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("failed to generate a run identifier for the transient workload: %w", err)
	}

	return hex.EncodeToString(raw), nil
}

// transientCredentials is the environment the database's own tooling reads to
// authenticate, resolved from the credentials the run already holds.
//
// It is returned as secret contents, never as arguments: FR-014 forbids the
// credentials appearing on a process command line or in workload configuration
// a deployment observer can read.
func transientCredentials(cfg *Configuration, service string) (map[string]string, error) {
	switch service {
	case serviceNeo4j:
		if cfg.Neo4jUsername == "" || cfg.Neo4jPassword == "" {
			return nil, fmt.Errorf("no Neo4j credentials are available for the external database: set INFRAHUB_DB_USERNAME and INFRAHUB_DB_PASSWORD, or make them discoverable from the deployment")
		}

		return map[string]string{
			"NEO4J_USERNAME": cfg.Neo4jUsername,
			"NEO4J_PASSWORD": cfg.Neo4jPassword,
		}, nil
	case serviceTaskManagerDB:
		if cfg.PostgresUsername == "" || cfg.PostgresPassword == "" {
			return nil, fmt.Errorf("no PostgreSQL credentials are available for the external task-manager database: make them discoverable from the deployment or supply them explicitly")
		}

		return map[string]string{
			"PGUSER":     cfg.PostgresUsername,
			"PGPASSWORD": cfg.PostgresPassword,
		}, nil
	default:
		return nil, errNotADatabaseService("resolve credentials for", service)
	}
}

// probeWorkloadSpec is the spec for the short workload that answers what has
// to be known before a capture pod can be built: the server's version, the
// utility's version in the same image, and the store size that sizes the
// capture pod's scratch space.
//
// Its scratch allocation is a small fixed value rather than a derived one,
// which is precisely why it can exist before the store size is known.
// members is how many endpoints the probe may have to walk before one answers;
// it sizes the deadline, and a count below one is read as the single endpoint
// every probe has.
func probeWorkloadSpec(cfg *Configuration, namespace, runID, service string, members int) (transientWorkloadSpec, error) {
	// The probe carries the same placement as the capture, deliberately: it
	// runs the same image, so a registry that needs credentials or a node pool
	// that needs a toleration blocks it first — and a probe that cannot start
	// is where the operator would meet the problem anyway.
	return newTransientWorkloadSpec(cfg, namespace, runID, service, workloadRoleProbe, externalDBProbeScratch, externalDBProbeDeadline(cfg, members))
}

// newTransientWorkloadSpec is the spec every role is built from: the
// credentials and placement the deployment supplies, the image the service
// needs, and the role's own scratch size and deadline. The three role builders
// differ only in how they arrive at those last two values.
func newTransientWorkloadSpec(cfg *Configuration, namespace, runID, service string, role workloadRole, scratch string, deadline time.Duration) (transientWorkloadSpec, error) {
	credentials, err := transientCredentials(cfg, service)
	if err != nil {
		return transientWorkloadSpec{}, err
	}

	scheduling, err := resolveTransientScheduling(cfg)
	if err != nil {
		return transientWorkloadSpec{}, err
	}

	return transientWorkloadSpec{
		RunID:       runID,
		Service:     service,
		Role:        role,
		Image:       transientImage(cfg, service),
		Namespace:   namespace,
		ScratchSize: scratch,
		Deadline:    deadline,
		Credentials: credentials,
		Scheduling:  scheduling,
	}, nil
}

// externalDBProbeFixedCalls is how many bounded calls the probe issues that do
// not depend on the endpoint count: the utility's version, the store size, and
// — on Neo4j — the member roles. The walk over the endpoints looking for one
// that answers the server version is the count on top of these.
const externalDBProbeFixedCalls = 3

// externalDBWorkloadDeadline is the activeDeadlineSeconds of a transient
// workload, from the number of bounded calls the pod hosts and the bound each
// of them runs under. It is the ordering FR-025 depends on, stated once: the
// pod outlives the image pull, then the calls it hosts, then a margin, which is
// what makes the tool's own timeout the thing that reports a stall rather than
// the cluster removing the pod underneath an operation still in flight.
//
// The bound is multiplied by the call count because each call gets the whole
// bound to itself. Budgeting for one where the pod hosts several puts the pod's
// deadline inside the window an operation is still permitted to be working in
// as soon as the second call is slow, which inverts the ordering above: the
// cluster removes the pod and the operator is told the exec failed rather than
// which call stalled.
//
// That is the reason this is a function rather than three spellings of the same
// arithmetic. The mistake was made once in the probe's deadline, fixed there,
// and left standing in the capture's — where it cost a completed capture of a
// production database, deleted part-way through the copy back out. The three
// roles differ only in how many calls they host and which bound applies, so
// those stay on each role's own constant; the arithmetic does not vary and no
// longer can.
func externalDBWorkloadDeadline(calls int, bound time.Duration) time.Duration {
	return externalDBWorkloadReadyTimeout + time.Duration(calls)*bound + externalDBWorkloadDeadlineMargin
}

// externalDBProbeDeadline is the probe Job's activeDeadlineSeconds. The probe
// issues its fixed calls plus one per endpoint it may have to walk before one
// answers, each under the probe's own — shorter — bound.
func externalDBProbeDeadline(cfg *Configuration, members int) time.Duration {
	if members < 1 {
		members = 1
	}

	return externalDBWorkloadDeadline(externalDBProbeFixedCalls+members, externalDBProbeBound(cfg))
}

// externalDBCaptureFullBoundCalls is how many operations the capture pod hosts
// that each get the whole operation bound to themselves: the capture, and the
// copy that takes the artifact back out of the pod
// (copyFromTransientWorkload opens its own externalDBBound). The pod has to
// outlive both of them, not one.
//
// The several short calls around them — preparing the scratch directory,
// listing what the capture produced, clearing it afterwards — are capped at
// externalDBControlTimeout and are covered by the margin.
const externalDBCaptureFullBoundCalls = 2

// externalDBCaptureDeadline is the capture Job's activeDeadlineSeconds: the
// operations it hosts each get the whole operation bound (see
// externalDBWorkloadDeadline for what the arithmetic guarantees, and for what
// this role's own history of getting it wrong cost).
//
// The bound comes from externalDBBound rather than from cfg.ExternalDB.Timeout
// directly, because that field is the operator's raw setting and not the bound
// anything actually runs under. externalDBBound is what applies the "zero means
// unset" reading and the floor, so the raw field is two hours short of the truth
// on a deployment that never set the flag, and negative — flooring the pod at
// one second — on one that set it wrongly.
//
// One caller is outside what this can promise: the streaming capture pipes the
// artifact out through ExecStreamPipe, which the tool bounds by an idle timeout
// rather than by a duration, so a slow-but-progressing transfer can outlive any
// deadline computed here. The deadline is a floor on that path, not a
// guarantee.
func externalDBCaptureDeadline(cfg *Configuration) time.Duration {
	return externalDBWorkloadDeadline(externalDBCaptureFullBoundCalls, externalDBBound(cfg))
}

// captureWorkloadSpec is the spec for the workload the capture runs in. It
// takes the store size the probe reported, and fails naming the override when
// that size is not available (FR-022).
func captureWorkloadSpec(cfg *Configuration, namespace, runID, service string, storeSizeBytes int64) (transientWorkloadSpec, error) {
	scratch, err := resolveScratchSize(service, cfg.ExternalDB.ScratchSize, storeSizeBytes)
	if err != nil {
		return transientWorkloadSpec{}, err
	}

	return newTransientWorkloadSpec(cfg, namespace, runID, service, workloadRoleCapture, scratch, externalDBCaptureDeadline(cfg))
}

// externalDBRestoreFullBoundCalls is how many operations the restore workload
// hosts that each get the whole operation bound to themselves: the copy of the
// dump into the pod, the restore or seed request itself, and the wait for the
// restored database to become usable (FR-021), whose polling window is the same
// bound.
//
// The pod has to outlive all three, and it is created at the gate rather than
// beside the operation — so its deadline also has to cover the extraction,
// checksum validation and quiescing that happen in between. That is what the
// margin is for on this arm, and it is the reason this role's deadline is the
// widest of the three.
const externalDBRestoreFullBoundCalls = 3

// externalDBRestoreDeadline is the restore workload's activeDeadlineSeconds:
// the three operations it hosts each get the whole operation bound, on the same
// construction as its siblings (see externalDBWorkloadDeadline).
func externalDBRestoreDeadline(cfg *Configuration) time.Duration {
	return externalDBWorkloadDeadline(externalDBRestoreFullBoundCalls, externalDBBound(cfg))
}

// restoreWorkloadSpec is the spec for the workload a restore into an external
// database runs in: the Bolt client that issues the seed and then watches for
// the database to come online, or the PostgreSQL client that reads the dump
// back in.
//
// Its scratch size is a default the operator can override rather than a value
// derived from the server, because nothing here is sized by the store: see
// externalDBRestoreScratch.
func restoreWorkloadSpec(cfg *Configuration, namespace, runID, service string) (transientWorkloadSpec, error) {
	scratch, err := resolveRestoreScratchSize(service, cfg.ExternalDB.ScratchSize)
	if err != nil {
		return transientWorkloadSpec{}, err
	}

	return newTransientWorkloadSpec(cfg, namespace, runID, service, workloadRoleRestore, scratch, externalDBRestoreDeadline(cfg))
}

// resolveRestoreScratchSize is the restore arm of scratch sizing: the
// operator's override, normalised the same way a capture's is (see
// normaliseScratchOverride), or the fixed default.
//
// Unlike resolveScratchSize it cannot fail for want of a store size, because it
// never consults one. FR-022 is written about the backup operation, whose
// scratch has to hold a copy of the database being read; a restore's holds a
// dump this host already has, or nothing at all.
func resolveRestoreScratchSize(service, override string) (string, error) {
	if size, supplied, err := normaliseScratchOverride(override, "the "+service+" restore"); supplied {
		return size, err
	}

	return externalDBRestoreScratch, nil
}

// ---------------------------------------------------------------------------
// Copying across a transient workload's boundary (FR-025)
// ---------------------------------------------------------------------------

// transientCopy is one direction of a copy across a transient workload's
// boundary. The direction is `perform`; the two strings are that direction's
// wording, and are the only thing the two arms of this operation differ on.
type transientCopy struct {
	perform func(k *KubernetesBackend, ctx context.Context, bound time.Duration, pod, src, dest string) error

	// preposition completes "cannot copy %s a transient workload in a %s
	// deployment".
	preposition string

	// operation is what a timeout names, so the operator is told which
	// transfer ran out of time rather than that an exec failed.
	operation string
}

var (
	// copyOutOfTransientWorkload takes a capture's artifact out of the pod.
	copyOutOfTransientWorkload = transientCopy{
		perform:     (*KubernetesBackend).CopyFromPodContext,
		preposition: "from",
		operation:   "copying the artifact out of the transient workload",
	}

	// copyIntoTransientWorkload puts a restore's dump into it.
	copyIntoTransientWorkload = transientCopy{
		perform:     (*KubernetesBackend).CopyToPodContext,
		preposition: "into",
		operation:   "copying the dump into the transient workload",
	}
)

// copyAcrossTransientWorkload copies a file between this host and a named
// transient pod, bounded like every other call these paths make (FR-025).
//
// It names the pod rather than resolving the service, for the reason
// ExecOptions.Pod exists: the workload stands in for a database with no
// container, so resolving `database` or `task-manager-db` here would find
// nothing — and registering the pod under that name, which is what the
// reverted version of this path did, is undone by any StartServices the run
// makes.
//
// The two directions were written out twice and differed in the kubectl method
// and a preposition. They are one function because everything that has to be
// right about them is the same on both and was already stated twice: the
// Kubernetes-only refusal, the bound, the context that carries it, and the
// wrapping that turns a deadline into a message naming the pod.
func (iops *InfrahubOps) copyAcrossTransientWorkload(direction transientCopy, pod, src, dest string) error {
	backend, err := iops.ensureBackend()
	if err != nil {
		return err
	}

	k, ok := backend.(*KubernetesBackend)
	if !ok {
		return fmt.Errorf("cannot copy %s a transient workload in a %s deployment", direction.preposition, backend.Name())
	}

	bound := externalDBBound(iops.config)
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()

	err = direction.perform(k, ctx, bound, pod, src, dest)

	return wrapExternalTimeout(err, bound, direction.operation, "pod "+pod)
}

// transientImage is the tooling image for a service: the pinned default or the
// operator's explicit flag, and nothing else. FR-027 makes this deliberate —
// the tool holds workload-creation rights, so an image taken from ambient
// environment would turn those rights into a way of running arbitrary images.
func transientImage(cfg *Configuration, service string) string {
	if service == serviceTaskManagerDB {
		return cfg.ExternalDB.ImagePostgres
	}

	return cfg.ExternalDB.ImageNeo4j
}

// CreateTransientWorkload creates a workload, waits for it to be usable, and
// registers it as the execution target for its service.
func (k *KubernetesBackend) CreateTransientWorkload(spec transientWorkloadSpec) (*TransientWorkload, error) {
	return k.createTransientWorkloadWith(k.transientOps(), spec)
}

// createTransientWorkloadWith is the lifecycle against the supplied cluster
// operations.
//
// The order is forced by FR-023 and is the only order in which the credentials
// are never unowned: the pod first, then its UID, then the secret carrying an
// owner reference to it. Creating the secret first and patching the reference
// on afterwards would leave exactly the window FR-023 exists to close — a run
// killed between the two leaves a credential object nothing will collect.
//
// The cost of this order is a brief interval in which the pod's container
// cannot start because its secret does not exist yet. That is a visible pending
// state the kubelet retries out of, and it resolves long before the image has
// finished pulling.
func (k *KubernetesBackend) createTransientWorkloadWith(ops transientClusterOps, spec transientWorkloadSpec) (*TransientWorkload, error) {
	if spec.Namespace == "" {
		spec.Namespace = k.namespace
	}

	jobManifest, err := buildTransientWorkloadManifest(spec)
	if err != nil {
		return nil, err
	}

	// The contradiction this refuses: a transient workload stands in for a
	// database the deployment does not have, and a resolved pod for that
	// service says it does. The resolver excludes this tool's own objects
	// (FR-028), so the name can only be a real deployment pod — which is either
	// a location decision that was wrong or a database that arrived since, and
	// neither is a reason to build a second one beside it.
	if cached, ok := k.podCache[spec.Service]; ok && cached != "" {
		return nil, fmt.Errorf("refusing to create a transient %s workload: the service already resolves to pod %s in namespace %s, which this run did not create — a transient workload stands in for a database the deployment does not have", spec.Service, cached, spec.Namespace)
	}

	workload := &TransientWorkload{
		RunID:      spec.RunID,
		Service:    spec.Service,
		JobName:    spec.jobName(),
		SecretName: spec.secretName(),
		Namespace:  spec.Namespace,
		State:      workloadStatePending,
		ops:        ops,
	}

	logrus.Infof("Creating a transient %s workload for the external %s database (Job %s, image %s, scratch %s)", spec.Role, spec.Service, workload.JobName, spec.Image, spec.ScratchSize)

	// Tracked before the create, not after it. `create` returning an error does
	// not mean no Job was created: a bound expiring, or the connection dropping
	// after the API server accepted the manifest, fails the call while the Job
	// stands. Tracking afterwards leaves that Job on no exit path at all
	// (FR-011), and the run has already lost the one thing that could find it —
	// the name it was about to be tracked under. A tracked entry for a Job that
	// was never created costs nothing: the delete passes --ignore-not-found.
	k.transientWorkloads = append(k.transientWorkloads, workload)

	if err := ops.create(jobManifest); err != nil {
		// A name collision is the other place adoption could happen. `create`
		// refuses it rather than adopting the existing object, which is the
		// behaviour the invariant needs; what is added here is saying so, since
		// a bare AlreadyExists reads like a bug in this tool rather than as
		// another run's workload standing where this one wanted to build.
		if isAlreadyExistsError(err) {
			// The one create failure that also says the Job is not this run's.
			// Untracking is what stops the release path deleting a workload
			// another run is still using — the opposite mistake to the one
			// above, and the more damaging of the two.
			k.untrackTransientWorkload(workload)

			return nil, fmt.Errorf("refusing to use the existing Job %s as the transient %s workload: it belongs to another run and a transient workload is adopted by exactly one run, so this run will not reuse it: %w", workload.JobName, spec.Service, err)
		}

		return nil, fmt.Errorf("failed to create the transient %s workload %s in namespace %s: %w", spec.Service, workload.JobName, spec.Namespace,
			workloadPermissionError(err, "create", "jobs", spec.Namespace))
	}

	// From here every failure path releases the workload. It is created, so
	// leaving it behind on an error would leave a stray in the operator's
	// namespace even though this process is still alive to remove it
	// (Principle II, FR-011).
	uid, err := ops.query("kubectl", "get", "job", workload.JobName, "-n", spec.Namespace, "-o", "jsonpath={.metadata.uid}")
	if err != nil {
		workload.releaseAfterFailure()

		return nil, fmt.Errorf("failed to read the UID of the transient %s workload %s: %w", spec.Service, workload.JobName,
			workloadPermissionError(err, "get", "jobs", spec.Namespace))
	}

	workload.JobUID = strings.TrimSpace(uid)

	secretManifest, err := buildTransientSecretManifest(spec, workload.JobUID)
	if err != nil {
		workload.releaseAfterFailure()

		return nil, err
	}

	if err := ops.create(secretManifest); err != nil {
		workload.releaseAfterFailure()

		return nil, fmt.Errorf("failed to create the credential secret %s owned by the transient %s workload: %w", workload.SecretName, spec.Service,
			workloadPermissionError(err, "create", transientKindSecret+"s", spec.Namespace))
	}

	if err := workload.resolveJobPod(externalDBWorkloadReadyTimeout); err != nil {
		workload.releaseAfterFailure()

		return nil, err
	}

	if err := workload.waitUntilReady(externalDBWorkloadReadyTimeout); err != nil {
		workload.releaseAfterFailure()

		return nil, err
	}

	// Only now, and not before: the workload can be used as an execution
	// target, which is what ExecutionTarget answers and what a caller passes as
	// ExecOptions.Pod. It is not registered anywhere — the pod's name is handed
	// to each operation rather than published under the service's name, because
	// the resolver's cache is not a place to keep it: Start and scaleServices
	// replace that map wholesale, and a restore calls StartServices by design,
	// so a registration made here was gone by the copy that needed it.
	workload.State = workloadStateReady

	logrus.Infof("Transient %s workload %s is ready to run %s operations", spec.Role, workload.PodName, spec.Service)

	return workload, nil
}

// resolveJobPod waits for the Job controller to create its pod, then records
// that generated name as the execution target. Waiting for creation avoids the
// race between the Job create returning and its controller reconciling it.
func (w *TransientWorkload) resolveJobPod(timeout time.Duration) error {
	selector := "job-name=" + w.JobName
	err := w.waitFor(timeout, "--for=create", "pod", "-l", selector)
	if err != nil {
		return fmt.Errorf("the transient %s Job %s did not create a pod within %s: %w", w.Service, w.JobName, timeout, err)
	}

	// livePodFieldSelector, and every name rather than .items[0]: the label
	// selects the Job's pods, not its *current* pod. An attempt the node
	// evicted or deleted leaves a terminal pod carrying the same label, and
	// `.items[0]` is whichever the API server returns first — by name, which
	// has nothing to do with which one is alive. Taking a terminated pod here
	// is not a failed lookup: every exec, copy and readiness wait that follows
	// targets a pod nothing can run in.
	listed, err := w.ops.query("kubectl", "get", "pods", "-l", selector, "-n", w.Namespace,
		"--field-selector", livePodFieldSelector, "-o", "jsonpath={range .items[*]}{.metadata.name}{\"\\n\"}{end}")
	if err != nil {
		return fmt.Errorf("failed to find the pod created by transient %s Job %s: %w", w.Service, w.JobName, err)
	}
	live := nonEmptyLines(listed)
	if len(live) == 0 {
		return fmt.Errorf("the transient %s Job %s created a pod, but none of its pods is in a phase a command can run in", w.Service, w.JobName)
	}
	w.PodName = live[0]

	return nil
}

// waitUntilReady blocks until the pod can be exec'd into, and reports why it
// could not when it cannot. `kubectl wait` alone says only that the condition
// was not met, which for an image that will not pull or a secret that never
// arrived is the least useful half of the answer.
//
// The wait runs under its own bound rather than the control bound, and that
// bound is still finite: `kubectl wait` is a watch, and a watch against an API
// server that stops answering is not returned by kubectl's own --timeout
// (FR-025). The margin is what makes the outer bound fire second, so this
// reports a readiness failure rather than a timeout wherever there is one.
func (w *TransientWorkload) waitUntilReady(timeout time.Duration) error {
	if err := w.waitFor(timeout, "--for=condition=Ready", "pod/"+w.PodName); err != nil {
		return fmt.Errorf("the transient %s workload %s did not become ready within %s (%s): %w", w.Service, w.PodName, timeout, w.readinessDiagnosis(), err)
	}

	return nil
}

// waitFor is the one `kubectl wait` this file issues, so its two callers cannot
// disagree about how the outer bound relates to the timeout handed to kubectl.
// The bound is the timeout plus a margin, which is what makes kubectl's own
// --timeout expire first: the caller then reports what it was waiting for
// rather than a watch that was cut off (FR-025).
func (w *TransientWorkload) waitFor(timeout time.Duration, condition string, target ...string) error {
	args := append([]string{"wait", condition}, target...)
	args = append(args, "-n", w.Namespace, fmt.Sprintf("--timeout=%ds", int64(timeout.Seconds())))

	_, err := w.ops.queryWithin(timeout+externalDBReadyBoundMargin)("kubectl", args...)

	return err
}

// readinessDiagnosis reports the container's own account of why it has not
// started. It is best-effort: a diagnosis that cannot be read must not replace
// the readiness failure it was meant to explain.
func (w *TransientWorkload) readinessDiagnosis() string {
	output, err := w.ops.query("kubectl", "get", "pod", w.PodName, "-n", w.Namespace,
		"-o", "jsonpath={.status.phase}{\": \"}{.status.containerStatuses[0].state.waiting.reason}{\" \"}{.status.containerStatuses[0].state.waiting.message}")
	if err != nil || strings.TrimSpace(output) == "" {
		return "no pod status available"
	}

	return strings.TrimSpace(output)
}

// ExecutionTarget is the pod name execution should resolve to, and it refuses
// to answer for a workload that is not ready. The data model allows only a
// ready workload as an execution target; this is where that is enforced rather
// than assumed.
//
// Its counterpart is the optional Pod field on ExecOptions, which an operation
// is given instead of resolving the service. externalCaptureOps asks every
// workload it creates for its name through here rather than reading PodName off
// the struct, which is what makes the precondition a check rather than an
// assumption the caller decided had been met.
func (w *TransientWorkload) ExecutionTarget() (string, error) {
	if w.State != workloadStateReady {
		return "", fmt.Errorf("the transient %s workload %s is %s, not %s: it cannot be used as an execution target", w.Service, w.PodName, w.State, workloadStateReady)
	}

	return w.PodName, nil
}

// Release removes the workload. Deleting the Job cascades to its pod and the
// credential Secret because both are owned by it.
func (w *TransientWorkload) Release() error {
	if w == nil || w.State == workloadStateReleased {
		return nil
	}

	// Nothing to de-register: the workload was never published under its
	// service's name (see createTransientWorkloadWith). Moving out of
	// workloadStateReady is what stops it being handed out as an execution
	// target, and ExecutionTarget is where that is enforced.
	//
	// The removal is recorded only once it has happened. Marking it released
	// first — as this did — makes the delete's outcome irrelevant: the early
	// return above then skips the workload on every later sweep, so the one
	// case the retry exists for, a delete that failed against an API server
	// having a bad minute, is exactly the case that never gets retried.
	w.State = workloadStateReleasing

	if err := w.ops.remove("job", w.JobName, "--ignore-not-found", "--wait=false"); err != nil {
		return fmt.Errorf("failed to remove the transient %s workload %s (its pod %s and credential secret %s are owned by it and go with it): %w", w.Service, w.JobName, w.PodName, w.SecretName,
			workloadPermissionError(err, "delete", "jobs", w.Namespace))
	}

	w.State = workloadStateReleased

	logrus.Debugf("Released the transient %s workload %s", w.Service, w.JobName)

	return nil
}

// untrackTransientWorkload drops a workload from the backend's tracking. It is
// for the one case where the run learns it does not own a pod it had already
// started tracking, so that nothing on the release path deletes it.
func (k *KubernetesBackend) untrackTransientWorkload(workload *TransientWorkload) {
	for i, tracked := range k.transientWorkloads {
		if tracked == workload {
			k.transientWorkloads = append(k.transientWorkloads[:i], k.transientWorkloads[i+1:]...)

			return
		}
	}
}

// isAlreadyExistsError reports whether a create failed because an object of
// that name is already there. The executor surfaces kubectl's message as text,
// so the message is what there is to read; the check is deliberately loose
// because misreading it only costs a less specific error message, never a
// wrong action.
func isAlreadyExistsError(err error) bool {
	return errorMentions(err, "already exists")
}

// errorMentions reports whether a kubectl failure's text carries a phrase. The
// executor surfaces kubectl's message as text, so the message is what there is
// to read, and both readers of it normalise case the same way rather than each
// spelling the match out.
//
// Both sides are lowered, not just the message: an API server that starts
// spelling a condition `Forbidden` is the case this exists to absorb, and a
// caller writing the phrase that way would otherwise get a match that silently
// never fires.
func errorMentions(err error, phrase string) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), strings.ToLower(phrase))
}

// errTransientWorkloadForbidden is FR-019's refusal, as a value callers and
// tests can identify rather than a message they have to match.
var errTransientWorkloadForbidden = errors.New("the deployment refused this tool the transient workload an external database is reached through")

// transientWorkloadRBAC is the whole of what a namespace must grant for an
// external database to be reachable at all, in the verb-on-resource form an
// operator writes a Role in.
//
// It names the full set rather than only the verb that was refused, because a
// namespace that denies one of these almost always denies the rest: an operator
// granted `create` on pods alone gets one step further and reads a second
// refusal. The pods/exec entry is easy to leave out of a Role and is what every
// operation the workload performs travels through.
const transientWorkloadRBAC = "create, get and delete on jobs; get, list, watch and delete on pods; create on pods/exec; and create, list and delete on secrets"

// isForbiddenError reports whether a kubectl failure was the API server
// refusing the request rather than failing to serve it.
//
// It matches on "forbidden" alone, which is what an RBAC denial always carries
// — `Error from server (Forbidden)`, and within it `pods is forbidden: User
// "…" cannot create resource "pods" …`. It deliberately does not match
// "unauthorized": that is authentication rather than authorisation, and its
// remedy is a credential rather than a Role, so naming FR-019's verbs for it
// would send the operator to grant a permission they already have.
func isForbiddenError(err error) bool {
	return errorMentions(err, "forbidden")
}

// workloadPermissionError is the cause a transient-object failure reports: the
// error kubectl gave, and — where the API server refused the request rather
// than failing it — FR-019's missing permission named alongside it.
//
// FR-019 makes the permission a documented prerequisite the tool must not work
// around, so the message's action is to grant it or to take the backup from a
// host that reaches the database directly (User Story 3), never a fallback this
// run could take by itself.
//
// It is deliberately a wrapper on the cause rather than a replacement for the
// message around it: every call site already names which object it was creating
// and in which namespace, which is the resource half of the contract's three
// elements, and rewriting those would have made the refusal say less.
func workloadPermissionError(cause error, verb, resource, namespace string) error {
	if !isForbiddenError(cause) {
		return cause
	}

	return fmt.Errorf(
		"%w — %w: it needs %s on %s in namespace %s, and the whole of what reaching an external database requires there is %s, bound to the identity this tool runs as. "+
			"FR-019 makes that a documented prerequisite rather than something this tool works around: grant it, or take the backup from a host that can reach the database directly",
		cause, errTransientWorkloadForbidden, verb, resource, namespace, transientWorkloadRBAC)
}

// ReleaseTransientWorkloads removes every transient workload this backend
// created, most recent first, and reports everything it could not remove
// rather than stopping at the first failure.
//
// It exists so that a run's exit path can be one deferred call regardless of
// how many workloads the run created (FR-011). A run creates a probe and then a
// capture workload per external database, and the probe is what a failure
// between the two would otherwise leave behind — the case a single
// `defer workload.Release()` at each creation site is easiest to get wrong.
//
// CreateBackup and CreatePlakarBackup each defer it once, ahead of the gate that
// creates the run's first workload — so a probe that answered and then hit a
// version refusal is given back on the way out, and so is a capture workload
// whose own release never ran. It is deliberately not the only teardown: the
// capture paths release what they finish with, and this removes what is left.
func (k *KubernetesBackend) ReleaseTransientWorkloads() error {
	errs := []error{}
	unreleased := make([]*TransientWorkload, 0, len(k.transientWorkloads))

	for i := len(k.transientWorkloads) - 1; i >= 0; i-- {
		if err := k.transientWorkloads[i].Release(); err != nil {
			errs = append(errs, err)
			unreleased = append(unreleased, k.transientWorkloads[i])
		}
	}

	// What was removed is dropped; what was not stays tracked, so a later call
	// retries it rather than the tracking forgetting a pod that is still
	// standing. Clearing unconditionally would make this the last chance to
	// remove a workload whose delete failed, and the Job's own deadline and TTL
	// only thing left that could — which is the fallback, not the plan.
	//
	// The kept entries go back into creation order, because the sweep above
	// reads that order to remove the capture before the probe that sized it.
	slices.Reverse(unreleased)
	k.transientWorkloads = unreleased

	return errors.Join(errs...)
}

// releaseAfterFailure removes a workload on a path that is already failing.
// The release error is logged rather than returned, because replacing the
// reason the run failed with the reason the cleanup failed loses the more
// useful of the two.
func (w *TransientWorkload) releaseAfterFailure() {
	if err := w.Release(); err != nil {
		logrus.Warnf("Could not remove the transient %s workload %s after a failed start; it carries %s=%s and the cluster will reclaim it at its deadline: %v", w.Service, w.PodName, transientLabelRunID, w.RunID, err)
	}
}
