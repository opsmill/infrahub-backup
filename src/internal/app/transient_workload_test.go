package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// documentedServiceNames are the deployment service names the constitution
// makes a public contract. The transient pod's name must contain none of them
// as a substring: the shared pod resolvers fall back to
// `strings.Contains(podName, service)`, so a name that happened to contain one
// would put the workload into replica enumeration regardless of its labels
// (FR-028).
var documentedServiceNames = []string{
	"database",
	"task-manager-db",
	"infrahub-server",
	"task-worker",
	"task-manager",
	"task-manager-background-svc",
	"cache",
	"message-queue",
}

// podSelectorLabelKeys are the label keys the shared resolvers select on. The
// transient workload must set none of them, or registering it for execution
// would also publish it as a replica of the service it stands in for (FR-028).
var podSelectorLabelKeys = []string{
	"app.kubernetes.io/component",
	"app.kubernetes.io/name",
	"app",
	"component",
	"infrahub/service",
}

func testCaptureSpec() transientWorkloadSpec {
	return transientWorkloadSpec{
		RunID:       "a1b2c3d4",
		Service:     serviceNeo4j,
		Role:        workloadRoleCapture,
		Image:       "neo4j:2025.10.1-enterprise",
		Namespace:   "infrahub",
		ScratchSize: "40Gi",
		Deadline:    2*time.Hour + 10*time.Minute,
		Credentials: map[string]string{"NEO4J_USERNAME": "neo4j", "NEO4J_PASSWORD": "s3cr3t-pw"},
	}
}

// decodePod unmarshals a built manifest into a generic map, so the assertions
// read the JSON that will actually reach the cluster rather than the Go value
// it was built from — the difference matters for the fields whose *presence* is
// the requirement.
func decodePod(t *testing.T, manifest []byte) map[string]any {
	t.Helper()

	var decoded map[string]any
	if err := json.Unmarshal(manifest, &decoded); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}

	return decoded
}

func decodeJobPodSpec(t *testing.T, manifest []byte) map[string]any {
	t.Helper()

	return nested(t, decodePod(t, manifest), "spec", "template", "spec").(map[string]any)
}

func nested(t *testing.T, root map[string]any, path ...string) any {
	t.Helper()

	var current any = root
	for _, key := range path {
		asMap, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("path %s: %q is not an object", strings.Join(path, "."), key)
		}
		current, ok = asMap[key]
		if !ok {
			t.Fatalf("path %s: %q is absent", strings.Join(path, "."), key)
		}
	}

	return current
}

// TestBuildTransientPodManifestShape pins every field of the pod that a
// requirement names: the run-ID label, the restart policy and deadline that
// let the cluster reclaim the pod without the tool (FR-011), the restricted
// security context (FR-027), and the explicit ephemeral-storage sizing
// (FR-022).
func TestBuildTransientPodManifestShape(t *testing.T) {
	spec := testCaptureSpec()

	manifest, err := buildTransientWorkloadManifest(spec)
	if err != nil {
		t.Fatalf("buildTransientWorkloadManifest() error = %v", err)
	}

	pod := decodePod(t, manifest)

	if got := nested(t, pod, "apiVersion"); got != "batch/v1" {
		t.Errorf("apiVersion = %v, want batch/v1", got)
	}
	if got := nested(t, pod, "kind"); got != "Job" {
		t.Errorf("kind = %v, want Job", got)
	}
	if got := nested(t, pod, "metadata", "name"); got != "infrahub-backup-xdb-neo4j-capture-a1b2c3d4" {
		t.Errorf("metadata.name = %v", got)
	}
	if got := nested(t, pod, "metadata", "namespace"); got != "infrahub" {
		t.Errorf("metadata.namespace = %v, want infrahub", got)
	}

	labels, ok := nested(t, pod, "metadata", "labels").(map[string]any)
	if !ok {
		t.Fatal("metadata.labels is not an object")
	}

	// FR-011: the run-ID label is how a later run identifies a stray.
	if labels[transientLabelRunID] != "a1b2c3d4" {
		t.Errorf("label %s = %v, want a1b2c3d4", transientLabelRunID, labels[transientLabelRunID])
	}
	if labels[transientLabelMarker] != "true" {
		t.Errorf("label %s = %v, want true", transientLabelMarker, labels[transientLabelMarker])
	}
	if labels[transientLabelService] != serviceNeo4j {
		t.Errorf("label %s = %v, want %s", transientLabelService, labels[transientLabelService], serviceNeo4j)
	}
	if labels[transientLabelRole] != string(workloadRoleCapture) {
		t.Errorf("label %s = %v, want capture", transientLabelRole, labels[transientLabelRole])
	}

	// FR-028: none of the keys the shared resolvers select on.
	for _, key := range podSelectorLabelKeys {
		if _, present := labels[key]; present {
			t.Errorf("label %q is set; it would make the transient pod discoverable as a replica (FR-028)", key)
		}
	}

	// FR-011: restartPolicy and activeDeadlineSeconds.
	if got := nested(t, pod, "spec", "template", "spec", "restartPolicy"); got != "Never" {
		t.Errorf("spec.restartPolicy = %v, want Never", got)
	}
	wantDeadline := float64((2*time.Hour + 10*time.Minute).Seconds())
	if got := nested(t, pod, "spec", "activeDeadlineSeconds"); got != wantDeadline {
		t.Errorf("spec.activeDeadlineSeconds = %v, want %v", got, wantDeadline)
	}
	// Restated on the pod, because the Job controller does not copy it down
	// and the stray reaper reads it from the pod listing (FR-011).
	if got := nested(t, pod, "spec", "template", "spec", "activeDeadlineSeconds"); got != wantDeadline {
		t.Errorf("spec.template.spec.activeDeadlineSeconds = %v, want %v", got, wantDeadline)
	}

	if got := nested(t, pod, "spec", "template", "spec", "automountServiceAccountToken"); got != false {
		t.Errorf("spec.automountServiceAccountToken = %v, want false", got)
	}
	if got := nested(t, pod, "spec", "template", "spec", "enableServiceLinks"); got != false {
		t.Errorf("spec.enableServiceLinks = %v, want false", got)
	}

	// FR-027: restricted pod-security fields, at the pod level.
	if got := nested(t, pod, "spec", "template", "spec", "securityContext", "runAsNonRoot"); got != true {
		t.Errorf("spec.securityContext.runAsNonRoot = %v, want true", got)
	}
	if got := nested(t, pod, "spec", "template", "spec", "securityContext", "fsGroup"); got != float64(7474) {
		t.Errorf("spec.securityContext.fsGroup = %v, want 7474 (the neo4j image's own user)", got)
	}
	if got := nested(t, pod, "spec", "template", "spec", "securityContext", "seccompProfile", "type"); got != "RuntimeDefault" {
		t.Errorf("spec.securityContext.seccompProfile.type = %v, want RuntimeDefault", got)
	}
	// runAsUser must be stated. Neither pinned image declares a user
	// (Config.User is empty in both, verified with docker image inspect), and
	// Command replaces the entrypoint that would have dropped privileges, so
	// runAsNonRoot with runAsUser absent makes the kubelet resolve the
	// effective user from the image, find root, and refuse the container with
	// CreateContainerConfigError. This assertion previously required the field
	// to be *absent*, which is why a pod that could never start had passing
	// tests: it asserted the manifest's shape, and no unit test connects that
	// shape to an admission decision.
	if got := nested(t, pod, "spec", "template", "spec", "securityContext", "runAsUser"); got != float64(7474) {
		t.Errorf("spec.securityContext.runAsUser = %v, want 7474 (the neo4j image's own uid); absent means the kubelet rejects the pod with CreateContainerConfigError", got)
	}
	if got := nested(t, pod, "spec", "template", "spec", "securityContext", "runAsGroup"); got != float64(7474) {
		t.Errorf("spec.securityContext.runAsGroup = %v, want 7474 (the neo4j image's own gid)", got)
	}

	// FR-027: restricted pod-security fields, at the container level. These
	// must be *explicitly* false, not absent — an absent
	// allowPrivilegeEscalation is a restricted-policy violation.
	container, ok := nested(t, pod, "spec", "template", "spec", "containers").([]any)
	if !ok || len(container) != 1 {
		t.Fatalf("spec.containers = %v, want exactly one", container)
	}
	first, ok := container[0].(map[string]any)
	if !ok {
		t.Fatal("spec.containers[0] is not an object")
	}
	containerSecurity, ok := nested(t, first, "securityContext").(map[string]any)
	if !ok {
		t.Fatal("spec.containers[0].securityContext is not an object")
	}
	for _, field := range []string{"allowPrivilegeEscalation", "privileged"} {
		value, present := containerSecurity[field]
		if !present {
			t.Errorf("spec.containers[0].securityContext.%s is absent; restricted pod security reads absent as a violation", field)

			continue
		}
		if value != false {
			t.Errorf("spec.containers[0].securityContext.%s = %v, want false", field, value)
		}
	}
	if got := containerSecurity["runAsNonRoot"]; got != true {
		t.Errorf("spec.containers[0].securityContext.runAsNonRoot = %v, want true", got)
	}
	drop, ok := nested(t, containerSecurity, "capabilities", "drop").([]any)
	if !ok || len(drop) != 1 || drop[0] != "ALL" {
		t.Errorf("spec.containers[0].securityContext.capabilities.drop = %v, want [ALL]", drop)
	}

	if got := nested(t, first, "image"); got != spec.Image {
		t.Errorf("container image = %v, want %v", got, spec.Image)
	}

	// FR-022: the scratch allocation is stated twice on purpose — as the
	// volume's own sizeLimit and as the container's ephemeral-storage
	// accounting, the second being what makes the kubelet evict this pod
	// rather than let it fill the node.
	if got := nested(t, first, "resources", "requests", "ephemeral-storage"); got != "40Gi" {
		t.Errorf("requests.ephemeral-storage = %v, want 40Gi", got)
	}
	if got := nested(t, first, "resources", "limits", "ephemeral-storage"); got != "41Gi" {
		t.Errorf("limits.ephemeral-storage = %v, want 41Gi (the scratch allocation plus the container's own overhead)", got)
	}
	volumes, ok := nested(t, pod, "spec", "template", "spec", "volumes").([]any)
	if !ok || len(volumes) != 1 {
		t.Fatalf("spec.volumes = %v, want exactly one", volumes)
	}
	volume, ok := volumes[0].(map[string]any)
	if !ok {
		t.Fatal("spec.volumes[0] is not an object")
	}
	if got := nested(t, volume, "emptyDir", "sizeLimit"); got != "40Gi" {
		t.Errorf("spec.volumes[0].emptyDir.sizeLimit = %v, want 40Gi", got)
	}

	// FR-014: the credentials reach the container only by secret reference.
	// Neither the value nor an inline env entry appears anywhere in the
	// manifest that will be visible in the pod spec.
	if strings.Contains(string(manifest), "s3cr3t-pw") {
		t.Error("the pod manifest carries the credential value; credentials must reach the container only through the owned secret (FR-014)")
	}
	if _, present := first["env"]; present {
		t.Error("spec.containers[0].env is set; credentials must arrive via envFrom on the owned secret (FR-014)")
	}
	envFrom, ok := nested(t, first, "envFrom").([]any)
	if !ok || len(envFrom) != 1 {
		t.Fatalf("spec.containers[0].envFrom = %v, want exactly one", envFrom)
	}
	source, ok := envFrom[0].(map[string]any)
	if !ok {
		t.Fatal("spec.containers[0].envFrom[0] is not an object")
	}
	if got := nested(t, source, "secretRef", "name"); got != spec.secretName() {
		t.Errorf("envFrom secretRef.name = %v, want %v", got, spec.secretName())
	}
	if got := nested(t, source, "secretRef", "optional"); got != false {
		t.Errorf("envFrom secretRef.optional = %v, want false: a missing credential secret must hold the container pending, not start it unauthenticated", got)
	}
}

// TestBuildTransientWorkloadManifestIsTTLManaged is FR-011's abnormal-exit
// guarantee: reaching the workload deadline must eventually remove the API
// object, not merely leave a failed pod for a later CLI run to reap.
func TestBuildTransientWorkloadManifestIsTTLManaged(t *testing.T) {
	manifest, err := buildTransientWorkloadManifest(testCaptureSpec())
	if err != nil {
		t.Fatalf("buildTransientWorkloadManifest() error = %v", err)
	}

	workload := decodePod(t, manifest)
	if got := nested(t, workload, "apiVersion"); got != "batch/v1" {
		t.Errorf("apiVersion = %v, want batch/v1", got)
	}
	if got := nested(t, workload, "kind"); got != "Job" {
		t.Errorf("kind = %v, want Job", got)
	}
	if got := nested(t, workload, "spec", "ttlSecondsAfterFinished"); got == nil {
		t.Error("spec.ttlSecondsAfterFinished is absent; a finished workload would remain indefinitely")
	}
}

// TestBuildTransientPodManifestPostgresRunsAsItsOwnUser pins the other half of
// the FR-027 reconciliation: the user the pod runs as follows the image's own
// database user, which differs between the two images.
func TestBuildTransientPodManifestPostgresRunsAsItsOwnUser(t *testing.T) {
	spec := testCaptureSpec()
	spec.Service = serviceTaskManagerDB
	spec.Image = "postgres:18-alpine"
	spec.Credentials = map[string]string{"PGUSER": "postgres", "PGPASSWORD": "pw"}

	manifest, err := buildTransientWorkloadManifest(spec)
	if err != nil {
		t.Fatalf("buildTransientWorkloadManifest() error = %v", err)
	}

	pod := decodePod(t, manifest)
	for _, field := range []string{"runAsUser", "runAsGroup", "fsGroup"} {
		if got := nested(t, pod, "spec", "template", "spec", "securityContext", field); got != float64(70) {
			t.Errorf("spec.securityContext.%s = %v, want 70 (the alpine postgres image's own uid/gid)", field, got)
		}
	}
	if got := nested(t, pod, "spec", "template", "spec", "securityContext", "runAsNonRoot"); got != true {
		t.Errorf("spec.securityContext.runAsNonRoot = %v, want true", got)
	}
}

// TestTransientPodDeclaresANonRootUserForEveryService is the assertion that
// would have caught the defect this replaced. runAsNonRoot is a promise the
// kubelet checks against the image's declared user, and neither pinned image
// declares one — Config.User is empty for both, so the effective user resolves
// to root and the container is refused with CreateContainerConfigError before
// anything runs. Asserting the field is present and non-zero for every service
// is what keeps the promise checkable without a cluster; the value itself is
// measured from the image, not derived (see databaseImageUser).
func TestTransientPodDeclaresANonRootUserForEveryService(t *testing.T) {
	for _, service := range []string{serviceNeo4j, serviceTaskManagerDB} {
		for _, role := range []workloadRole{workloadRoleProbe, workloadRoleCapture} {
			spec := testCaptureSpec()
			spec.Service = service
			spec.Role = role
			spec.Credentials = map[string]string{"CRED": "value"}

			manifest, err := buildTransientWorkloadManifest(spec)
			if err != nil {
				t.Fatalf("buildTransientWorkloadManifest(%s/%s) error = %v", service, role, err)
			}

			pod := decodePod(t, manifest)
			security, ok := nested(t, pod, "spec", "template", "spec", "securityContext").(map[string]any)
			if !ok {
				t.Fatalf("%s/%s: spec.securityContext is not an object", service, role)
			}
			if security["runAsNonRoot"] != true {
				t.Errorf("%s/%s: runAsNonRoot = %v, want true", service, role, security["runAsNonRoot"])
			}
			user, present := security["runAsUser"]
			if !present {
				t.Errorf("%s/%s: runAsUser is absent, so the kubelet reads the effective user from an image that declares none and refuses the pod as root", service, role)

				continue
			}
			uid, ok := user.(float64)
			if !ok || uid <= 0 {
				t.Errorf("%s/%s: runAsUser = %v, want a positive uid: zero is root, which runAsNonRoot forbids", service, role, user)
			}
			if security["runAsGroup"] != user {
				t.Errorf("%s/%s: runAsGroup = %v, want it paired with runAsUser %v", service, role, security["runAsGroup"], user)
			}
			if security["fsGroup"] != user {
				t.Errorf("%s/%s: fsGroup = %v, want it paired with runAsUser %v so the scratch volume is the running user's", service, role, security["fsGroup"], user)
			}
		}
	}
}

// TestTransientObjectNamesAvoidServiceSubstrings makes the FR-028 naming
// constraint structural rather than accidental.
func TestTransientObjectNamesAvoidServiceSubstrings(t *testing.T) {
	for _, service := range []string{serviceNeo4j, serviceTaskManagerDB} {
		for _, role := range []workloadRole{workloadRoleProbe, workloadRoleCapture} {
			spec := transientWorkloadSpec{RunID: "a1b2c3d4", Service: service, Role: role}
			for _, name := range []string{spec.jobName(), spec.secretName()} {
				for _, documented := range documentedServiceNames {
					if strings.Contains(name, documented) {
						t.Errorf("object name %q contains the service name %q; the shared pod resolvers substring-match on names, so it would surface in replica enumeration (FR-028)", name, documented)
					}
				}
				if len(name) > 63 {
					t.Errorf("object name %q is %d characters; it must fit a DNS label", name, len(name))
				}
			}
		}
	}
}

// TestBuildTransientPodManifestRejects covers the specs that must not produce
// a manifest at all.
func TestBuildTransientPodManifestRejects(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*transientWorkloadSpec)
		wantMsg string
	}{
		{
			name:    "no run ID",
			mutate:  func(s *transientWorkloadSpec) { s.RunID = "" },
			wantMsg: "without a run ID",
		},
		{
			name:    "a service that is not a database",
			mutate:  func(s *transientWorkloadSpec) { s.Service = "cache" },
			wantMsg: "the databases are",
		},
		{
			name:    "no role",
			mutate:  func(s *transientWorkloadSpec) { s.Role = "" },
			wantMsg: "role",
		},
		{
			name:    "no image names the image flag",
			mutate:  func(s *transientWorkloadSpec) { s.Image = "" },
			wantMsg: "--external-db-image-neo4j",
		},
		{
			name:    "no namespace",
			mutate:  func(s *transientWorkloadSpec) { s.Namespace = "" },
			wantMsg: "without a namespace",
		},
		{
			name:    "no scratch size",
			mutate:  func(s *transientWorkloadSpec) { s.ScratchSize = "" },
			wantMsg: "without a scratch size",
		},
		{
			name:    "an unreadable scratch size",
			mutate:  func(s *transientWorkloadSpec) { s.ScratchSize = "quite big" },
			wantMsg: "is not a storage quantity",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := testCaptureSpec()
			tt.mutate(&spec)

			if _, err := buildTransientWorkloadManifest(spec); err == nil {
				t.Fatal("buildTransientWorkloadManifest() error = nil, want an error")
			} else if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("buildTransientWorkloadManifest() error = %q, want it to mention %q", err, tt.wantMsg)
			}
		})
	}
}

// TestTransientObjectNamesAreOnePerDatabase is T099. The run ID is minted once
// per run — deliberately, because the reaper and the create-collision refusal
// both rest on a workload being adopted by exactly one run — so a deployment
// with both databases external produced two probe pods and then two capture
// pods under one name each: an AlreadyExists that reads as a concurrent
// operator, or, where the first was already gone, a PostgreSQL pod created from
// a spec carrying Neo4j's credentials.
func TestTransientObjectNamesAreOnePerDatabase(t *testing.T) {
	names := map[string]bool{}
	for _, service := range []string{serviceNeo4j, serviceTaskManagerDB} {
		for _, role := range []workloadRole{workloadRoleProbe, workloadRoleCapture} {
			spec := testCaptureSpec()
			spec.Service = service
			spec.Role = role

			for _, name := range []string{spec.jobName(), spec.secretName()} {
				if names[name] {
					t.Errorf("one run produces the name %q twice", name)
				}
				names[name] = true

				// The prefix's own contract: no documented deployment service
				// name appears in a transient object's name, because two pod
				// resolvers still fall back to matching a name against a
				// service (FR-028). The database segment is `neo4j`/`postgres`
				// for exactly that reason.
				for _, documented := range []string{serviceNeo4j, serviceTaskManagerDB} {
					if strings.Contains(name, documented) {
						t.Errorf("transient object name %q carries the deployment service name %q, which a name-shaped match can find", name, documented)
					}
				}
				if len(name) > 63 {
					t.Errorf("transient object name %q is %d characters, over the 63-character limit for a DNS label", name, len(name))
				}
			}
		}
	}

	if len(names) != 8 {
		t.Errorf("got %d distinct names, want 8: a pod and a secret for each of two roles for each of two databases", len(names))
	}

	// The consequence the collision actually caused: the credentials in the
	// secret belong to the database whose pod owns it.
	postgres := testCaptureSpec()
	postgres.Service = serviceTaskManagerDB
	postgres.Credentials = map[string]string{"PGUSER": "prefect", "PGPASSWORD": "pg-pw"}

	manifest, err := buildTransientSecretManifest(postgres, "8f14e45f-ea3b-4b0e-9d3e-2a1c5f6b7d80")
	if err != nil {
		t.Fatalf("buildTransientSecretManifest() error = %v", err)
	}
	secret := decodePod(t, manifest)
	if got := nested(t, secret, "metadata", "name"); got != "infrahub-backup-xdb-postgres-capture-a1b2c3d4-creds" {
		t.Errorf("metadata.name = %v, want the PostgreSQL run's own secret name", got)
	}
	owner, ok := nested(t, secret, "metadata", "ownerReferences").([]any)
	if !ok || len(owner) != 1 {
		t.Fatalf("metadata.ownerReferences = %v, want exactly one", nested(t, secret, "metadata", "ownerReferences"))
	}
	if got := owner[0].(map[string]any)["name"]; got != "infrahub-backup-xdb-postgres-capture-a1b2c3d4" {
		t.Errorf("ownerReferences[0].name = %v, want the PostgreSQL pod rather than the Neo4j one", got)
	}
}

// TestBuildTransientSecretManifestOwnerReference is FR-023's structural half:
// the secret is created owned by the Job, so the cluster's garbage collector
// reclaims it when the TTL removes the Job rather than the tool remembering.
func TestBuildTransientSecretManifestOwnerReference(t *testing.T) {
	spec := testCaptureSpec()

	manifest, err := buildTransientSecretManifest(spec, "8f14e45f-ea3b-4b0e-9d3e-2a1c5f6b7d80")
	if err != nil {
		t.Fatalf("buildTransientSecretManifest() error = %v", err)
	}

	secret := decodePod(t, manifest)

	if got := nested(t, secret, "kind"); got != "Secret" {
		t.Errorf("kind = %v, want Secret", got)
	}
	if got := nested(t, secret, "metadata", "name"); got != spec.secretName() {
		t.Errorf("metadata.name = %v, want %v", got, spec.secretName())
	}
	if got := nested(t, secret, "metadata", "namespace"); got != spec.Namespace {
		t.Errorf("metadata.namespace = %v, want %v: an owner reference only binds within a namespace", got, spec.Namespace)
	}

	refs, ok := nested(t, secret, "metadata", "ownerReferences").([]any)
	if !ok || len(refs) != 1 {
		t.Fatalf("metadata.ownerReferences = %v, want exactly one", refs)
	}
	ref, ok := refs[0].(map[string]any)
	if !ok {
		t.Fatal("metadata.ownerReferences[0] is not an object")
	}

	want := map[string]any{
		"apiVersion":         "batch/v1",
		"kind":               "Job",
		"name":               spec.jobName(),
		"uid":                "8f14e45f-ea3b-4b0e-9d3e-2a1c5f6b7d80",
		"controller":         false,
		"blockOwnerDeletion": false,
	}
	if !reflect.DeepEqual(ref, want) {
		t.Errorf("ownerReference = %#v, want %#v", ref, want)
	}

	data, ok := nested(t, secret, "stringData").(map[string]any)
	if !ok {
		t.Fatal("stringData is not an object")
	}
	if data["NEO4J_PASSWORD"] != "s3cr3t-pw" || data["NEO4J_USERNAME"] != "neo4j" {
		t.Errorf("stringData = %v, want the resolved credentials", data)
	}

	labels, ok := nested(t, secret, "metadata", "labels").(map[string]any)
	if !ok {
		t.Fatal("metadata.labels is not an object")
	}
	if labels[transientLabelRunID] != spec.RunID {
		t.Errorf("secret label %s = %v, want %v: the secret carries the same run ID as the workload", transientLabelRunID, labels[transientLabelRunID], spec.RunID)
	}
}

// TestBuildTransientSecretManifestRefusesUnboundCredentials is FR-023's
// failure half. An owner reference with an empty UID is accepted by the API
// server and then never acted on, so the secret would outlive the run with
// nothing reporting that it had.
func TestBuildTransientSecretManifestRefusesUnboundCredentials(t *testing.T) {
	tests := []struct {
		name    string
		uid     string
		mutate  func(*transientWorkloadSpec)
		wantMsg string
	}{
		{
			name:    "an empty UID does not bind",
			uid:     "",
			wantMsg: "empty UID does not bind",
		},
		{
			name:    "a whitespace UID does not bind",
			uid:     "   ",
			wantMsg: "empty UID does not bind",
		},
		{
			name:    "no credentials to carry",
			uid:     "8f14e45f",
			mutate:  func(s *transientWorkloadSpec) { s.Credentials = nil },
			wantMsg: "empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := testCaptureSpec()
			if tt.mutate != nil {
				tt.mutate(&spec)
			}

			if _, err := buildTransientSecretManifest(spec, tt.uid); err == nil {
				t.Fatal("buildTransientSecretManifest() error = nil, want an error")
			} else if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantMsg)
			}
		})
	}
}

// TestResolveScratchSize covers FR-022's three outcomes: the operator's
// override, the size derived from a queried store, and the refusal that names
// the override when no size could be determined.
func TestResolveScratchSize(t *testing.T) {
	tests := []struct {
		name      string
		override  string
		storeSize int64
		want      string
		wantErr   string
	}{
		{
			name:      "the override wins over the queried size",
			override:  "200Gi",
			storeSize: 10 << 30,
			want:      "200Gi",
		},
		{
			name:     "the override is used when no size was queried",
			override: "  75Gi  ",
			want:     "75Gi",
		},
		{
			// This parser accepts internal whitespace and the API server does
			// not, so passing the operator's text through unchanged only moves
			// the refusal to manifest validation — where the message names
			// neither the flag nor the value.
			name:     "an override this parser accepts but Kubernetes would not is normalised",
			override: "50 Gi",
			want:     "50Gi",
		},
		{
			// Rounding up: normalising must never hand back less space than
			// the operator asked for.
			name:     "a fractional override rounds up to whole GiB",
			override: "1.5Gi",
			want:     "2Gi",
		},
		{
			name:     "an override below a GiB still gets a whole one",
			override: "500Mi",
			want:     "1Gi",
		},
		{
			name:     "an unreadable override is refused",
			override: "loads",
			wantErr:  "is not a storage quantity",
		},
		{
			name:     "a zero override is refused",
			override: "0Gi",
			wantErr:  "not a positive storage quantity",
		},
		{
			name:      "the queried size gets the headroom factor",
			storeSize: 40 << 30,
			want:      "80Gi",
		},
		{
			name:      "a fractional result rounds up to whole GiB",
			storeSize: 41*(1<<30) + 1,
			want:      "83Gi",
		},
		{
			name:      "a small store still gets the floor",
			storeSize: 512 << 20,
			want:      "5Gi",
		},
		{
			name:    "an undetermined size names the override rather than guessing",
			wantErr: "--external-db-scratch-size",
		},
		{
			name:      "a negative size is undetermined, not a size",
			storeSize: -1,
			wantErr:   "--external-db-scratch-size",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveScratchSize(serviceNeo4j, tt.override, tt.storeSize)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("resolveScratchSize() = %q, want an error mentioning %q", got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("resolveScratchSize() error = %q, want it to mention %q", err, tt.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("resolveScratchSize() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveScratchSize() = %q, want %q", got, tt.want)
			}
		})
	}
}

// gibOf reads back a quantity this package rendered with formatGiB, which
// always writes whole GiB. It exists so a comparison between two rendered
// quantities can be made at sizes where their byte counts no longer fit in an
// int64.
func gibOf(t *testing.T, quantity string) int64 {
	t.Helper()

	gibs, err := strconv.ParseInt(strings.TrimSuffix(quantity, "Gi"), 10, 64)
	if err != nil {
		t.Fatalf("quantity %q is not the whole-GiB form formatGiB renders: %v", quantity, err)
	}

	return gibs
}

// TestTransientNumericGuards covers the three places a number that came from
// outside this process is multiplied or added into a comparison. Each wraps at
// a boundary a real deployment can reach — through operator input, or through
// another tool's pod — and each wrap turns its guard into its opposite rather
// than merely computing a wrong number.
func TestTransientNumericGuards(t *testing.T) {
	t.Run("a pod with an absurd deadline is left alone rather than reaped", func(t *testing.T) {
		// Nanoseconds overflow an int64 above roughly 9.2e9 seconds, so
		// multiplying this deadline up wraps it negative — and against a
		// negative deadline every pod compares as expired. The pod deleted
		// would be a live peer's Running one.
		pod := peerCapturePod("bbbb2222")
		pod.DeadlineSeconds = math.MaxInt64

		if strays := selectTransientStrays([]transientCandidate{pod}, "aaaa1111", reapNow); len(strays) != 0 {
			t.Errorf("selectTransientStrays() reaped %v; a Running pod must never be reaped for a deadline that has not passed", strays)
		}

		// The saturation must not cost the ordinary case its answer: a pod
		// genuinely past a sane deadline is still reaped.
		pod.DeadlineSeconds = int64((2 * time.Hour).Seconds())
		pod.Created = reapNow.Add(-(3*time.Hour + transientStrayGrace))
		if strays := selectTransientStrays([]transientCandidate{pod}, "aaaa1111", reapNow); len(strays) != 1 {
			t.Errorf("selectTransientStrays() = %v, want the pod past its deadline to be reaped", strays)
		}
	})

	t.Run("the ephemeral-storage limit never falls below the scratch it covers", func(t *testing.T) {
		// One GiB below the point where adding the margin wraps. formatGiB
		// floors a negative at 1Gi, so the wrap does not merely misreport the
		// limit — it puts it below the volume it is supposed to sit above.
		spec := testCaptureSpec()
		spec.ScratchSize = "8589934591Gi"

		manifest, err := buildTransientWorkloadManifest(spec)
		if err != nil {
			t.Fatalf("buildTransientWorkloadManifest() error = %v", err)
		}

		pod := decodeJobPodSpec(t, manifest)
		limit := pod["containers"].([]any)[0].(map[string]any)["resources"].(map[string]any)["limits"].(map[string]any)["ephemeral-storage"].(string)
		sizeLimit := pod["volumes"].([]any)[0].(map[string]any)["emptyDir"].(map[string]any)["sizeLimit"].(string)

		// Compared in whole GiB, which is the unit both are rendered in. A
		// saturated limit is larger than any int64 count of bytes, so parsing
		// the two back into bytes is exactly what cannot be done here — and the
		// invariant was never about bytes, only about which of the two is
		// larger.
		if gibOf(t, limit) < gibOf(t, sizeLimit) {
			t.Errorf("ephemeral-storage limit = %s, below the %s scratch volume it must cover", limit, sizeLimit)
		}
	})

	t.Run("a store size whose headroom does not fit is refused, not floored", func(t *testing.T) {
		// T112. The product was converted to int64 with no range check, and
		// converting a float64 that does not fit is undefined in Go: the two
		// architectures `make build-all` ships disagree, arm64 saturating to
		// MaxInt64 and amd64 wrapping to MinInt64 — where the floor clamp
		// rewrites it to 5Gi and the capture runs with a scratch allocation
		// orders of magnitude too small, reported as a sized one.
		//
		// The assertion is the refusal, which is what makes it hold on both:
		// saturation answers 8589934592Gi and the wrap answers 5Gi, and
		// neither is an answer this input has.
		for _, storeSize := range []int64{math.MaxInt64, math.MaxInt64 / 2, int64(1) << 62} {
			got, err := resolveScratchSize(serviceNeo4j, "", storeSize)
			if err == nil {
				t.Errorf("resolveScratchSize(store=%d) = %q, want a refusal: %.1fx that store does not fit in a storage quantity", storeSize, got, externalDBScratchHeadroomFactor)

				continue
			}
			if !strings.Contains(err.Error(), "--external-db-scratch-size") {
				t.Errorf("resolveScratchSize(store=%d) error = %q, want it to name the override that unblocks the operator", storeSize, err)
			}
		}

		// The guard must not cost the largest size that does fit its answer.
		got, err := resolveScratchSize(serviceNeo4j, "", int64(1)<<61)
		if err != nil {
			t.Fatalf("resolveScratchSize(store=2^61) error = %v, want the doubled size", err)
		}
		if want := "4294967296Gi"; got != want {
			t.Errorf("resolveScratchSize(store=2^61) = %q, want %q", got, want)
		}
	})

	t.Run("saturatingAdd reaches the ceiling instead of wrapping past it", func(t *testing.T) {
		if got := saturatingAdd(math.MaxInt64-1, 5); got != math.MaxInt64 {
			t.Errorf("saturatingAdd(MaxInt64-1, 5) = %d, want %d", got, int64(math.MaxInt64))
		}
		if got := saturatingAdd(2, 3); got != 5 {
			t.Errorf("saturatingAdd(2, 3) = %d, want 5", got)
		}
	})
}

// TestTransientPodPlacement is T111. The manifest emitted no imagePullSecrets,
// serviceAccountName, nodeSelector, tolerations or priorityClassName, and
// nothing could supply them — so the mirrored-image path the image flags exist
// for sat in ImagePullBackOff until the readiness wait gave up, with nothing the
// operator could pass to fix it.
func TestTransientPodPlacement(t *testing.T) {
	t.Run("a cluster that requires none of it gets the pod it got before", func(t *testing.T) {
		manifest, err := buildTransientWorkloadManifest(testCaptureSpec())
		if err != nil {
			t.Fatalf("buildTransientWorkloadManifest() error = %v", err)
		}

		spec := decodeJobPodSpec(t, manifest)
		for _, field := range []string{"serviceAccountName", "imagePullSecrets", "nodeSelector", "tolerations", "priorityClassName"} {
			if _, present := spec[field]; present {
				t.Errorf("spec.%s is present with nothing configured, want it absent (FR-015)", field)
			}
		}
	})

	t.Run("what the operator supplied reaches the pod", func(t *testing.T) {
		spec := testCaptureSpec()
		scheduling, err := resolveTransientScheduling(schedulingConfig(ExternalDBScheduling{
			ImagePullSecrets: "mirror-creds, backup-mirror-creds",
			ServiceAccount:   "infrahub-backup",
			NodeSelector:     "node-role=backup,disk=ssd",
			Tolerations:      "dedicated=backup:NoSchedule,maintenance:NoExecute,spot",
			PriorityClass:    "backup-critical",
		}))
		if err != nil {
			t.Fatalf("resolveTransientScheduling() error = %v", err)
		}
		spec.Scheduling = scheduling

		manifest, err := buildTransientWorkloadManifest(spec)
		if err != nil {
			t.Fatalf("buildTransientWorkloadManifest() error = %v", err)
		}

		pod := decodeJobPodSpec(t, manifest)

		if got := pod["serviceAccountName"]; got != "infrahub-backup" {
			t.Errorf("serviceAccountName = %v, want infrahub-backup", got)
		}
		if got := pod["priorityClassName"]; got != "backup-critical" {
			t.Errorf("priorityClassName = %v, want backup-critical", got)
		}

		// Naming an account must not have given the pod API access: the token
		// stays unmounted whatever identity it runs under.
		if got := pod["automountServiceAccountToken"]; got != false {
			t.Errorf("automountServiceAccountToken = %v, want false even under a named service account", got)
		}

		secrets := pod["imagePullSecrets"].([]any)
		if len(secrets) != 2 {
			t.Fatalf("imagePullSecrets = %v, want both secrets", secrets)
		}
		if got := secrets[0].(map[string]any)["name"]; got != "mirror-creds" {
			t.Errorf("imagePullSecrets[0].name = %v, want mirror-creds", got)
		}
		if got := secrets[1].(map[string]any)["name"]; got != "backup-mirror-creds" {
			t.Errorf("imagePullSecrets[1].name = %v, want backup-mirror-creds", got)
		}

		selector := pod["nodeSelector"].(map[string]any)
		if selector["node-role"] != "backup" || selector["disk"] != "ssd" {
			t.Errorf("nodeSelector = %v, want both labels", selector)
		}

		tolerations := pod["tolerations"].([]any)
		if len(tolerations) != 3 {
			t.Fatalf("tolerations = %v, want three", tolerations)
		}
		equal := tolerations[0].(map[string]any)
		if equal["key"] != "dedicated" || equal["operator"] != "Equal" || equal["value"] != "backup" || equal["effect"] != "NoSchedule" {
			t.Errorf("tolerations[0] = %v, want the key=value:Effect form as Equal", equal)
		}
		exists := tolerations[1].(map[string]any)
		if exists["key"] != "maintenance" || exists["operator"] != "Exists" || exists["effect"] != "NoExecute" {
			t.Errorf("tolerations[1] = %v, want the key:Effect form as Exists", exists)
		}
		if _, hasValue := exists["value"]; hasValue {
			t.Errorf("tolerations[1] = %v, want no value: Exists must not carry one", exists)
		}
		bare := tolerations[2].(map[string]any)
		if bare["key"] != "spot" || bare["operator"] != "Exists" {
			t.Errorf("tolerations[2] = %v, want a bare key as Exists", bare)
		}
		if _, hasEffect := bare["effect"]; hasEffect {
			t.Errorf("tolerations[2] = %v, want no effect: a bare key tolerates every effect", bare)
		}
	})

	// Both roles run the same image, so a registry that needs credentials or a
	// node pool that needs a toleration blocks the probe first — and a probe
	// that cannot start is where the operator meets the problem.
	t.Run("both workload roles carry it", func(t *testing.T) {
		cfg := schedulingConfig(ExternalDBScheduling{ImagePullSecrets: "mirror-creds"})
		cfg.Neo4jUsername, cfg.Neo4jPassword = "neo4j", "s3cr3t-pw"

		probe, err := probeWorkloadSpec(cfg, "infrahub", "a1b2c3d4", serviceNeo4j, 1)
		if err != nil {
			t.Fatalf("probeWorkloadSpec() error = %v", err)
		}
		capture, err := captureWorkloadSpec(cfg, "infrahub", "a1b2c3d4", serviceNeo4j, 40<<30)
		if err != nil {
			t.Fatalf("captureWorkloadSpec() error = %v", err)
		}

		for role, spec := range map[workloadRole]transientWorkloadSpec{workloadRoleProbe: probe, workloadRoleCapture: capture} {
			if len(spec.Scheduling.ImagePullSecrets) != 1 || spec.Scheduling.ImagePullSecrets[0].Name != "mirror-creds" {
				t.Errorf("%s spec ImagePullSecrets = %v, want the operator's secret", role, spec.Scheduling.ImagePullSecrets)
			}
		}
	})
}

// schedulingConfig is a Configuration carrying nothing but the placement inputs
// under test.
func schedulingConfig(scheduling ExternalDBScheduling) *Configuration {
	cfg := NewInfrahubOps().Config()
	cfg.ExternalDB.Scheduling = scheduling

	return cfg
}

// TestResolveTransientSchedulingRefusals asserts a malformed placement input
// fails the run where the message can still name the operator's own flag,
// rather than as a pod the API server will not admit — the same reason
// parseStorageQuantity refuses a quantity here.
func TestResolveTransientSchedulingRefusals(t *testing.T) {
	tests := []struct {
		name       string
		scheduling ExternalDBScheduling
		wantFlag   string
	}{
		{
			name:       "a pull secret name with a space in it",
			scheduling: ExternalDBScheduling{ImagePullSecrets: "mirror creds"},
			wantFlag:   imagePullSecretsFlag,
		},
		{
			name:       "more than one service account",
			scheduling: ExternalDBScheduling{ServiceAccount: "one,two"},
			wantFlag:   serviceAccountFlag,
		},
		{
			name:       "more than one priority class",
			scheduling: ExternalDBScheduling{PriorityClass: "high,higher"},
			wantFlag:   priorityClassFlag,
		},
		{
			name:       "a node selector entry that is not a pair",
			scheduling: ExternalDBScheduling{NodeSelector: "node-role"},
			wantFlag:   nodeSelectorFlag,
		},
		{
			name:       "a node selector entry with no key",
			scheduling: ExternalDBScheduling{NodeSelector: "=backup"},
			wantFlag:   nodeSelectorFlag,
		},
		{
			name:       "a toleration naming an effect Kubernetes does not have",
			scheduling: ExternalDBScheduling{Tolerations: "dedicated=backup:NoScheduling"},
			wantFlag:   tolerationsFlag,
		},
		{
			name:       "a toleration with no key",
			scheduling: ExternalDBScheduling{Tolerations: ":NoSchedule"},
			wantFlag:   tolerationsFlag,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveTransientScheduling(schedulingConfig(tt.scheduling))
			if err == nil {
				t.Fatalf("resolveTransientScheduling() = %+v, want an error naming %s", got, tt.wantFlag)
			}
			if !strings.Contains(err.Error(), tt.wantFlag) {
				t.Errorf("resolveTransientScheduling() error = %q, want it to name %s", err, tt.wantFlag)
			}
		})
	}

	t.Run("empty entries and surrounding whitespace are not malformed", func(t *testing.T) {
		got, err := resolveTransientScheduling(schedulingConfig(ExternalDBScheduling{
			ImagePullSecrets: " mirror-creds , ",
			NodeSelector:     " node-role = backup ,",
			Tolerations:      " dedicated=backup:NoSchedule ,",
		}))
		if err != nil {
			t.Fatalf("resolveTransientScheduling() error = %v", err)
		}
		if len(got.ImagePullSecrets) != 1 || got.ImagePullSecrets[0].Name != "mirror-creds" {
			t.Errorf("ImagePullSecrets = %v, want the one secret", got.ImagePullSecrets)
		}
		if got.NodeSelector["node-role"] != "backup" {
			t.Errorf("NodeSelector = %v, want node-role=backup", got.NodeSelector)
		}
		if len(got.Tolerations) != 1 || got.Tolerations[0].Key != "dedicated" {
			t.Errorf("Tolerations = %v, want the one toleration", got.Tolerations)
		}
	})
}

func TestParseStorageQuantity(t *testing.T) {
	tests := []struct {
		text    string
		want    int64
		wantErr bool
	}{
		{text: "50Gi", want: 50 << 30},
		{text: "1Ki", want: 1024},
		{text: "2Mi", want: 2 << 20},
		{text: "1Ti", want: 1 << 40},
		{text: "1Pi", want: 1 << 50},
		{text: "1G", want: 1_000_000_000},
		{text: "1M", want: 1_000_000},
		{text: "1k", want: 1000},
		{text: "1K", want: 1000},
		{text: "1T", want: 1_000_000_000_000},
		{text: "1024", want: 1024},
		{text: " 8Gi ", want: 8 << 30},
		{text: "1.5Gi", want: 1610612736},
		{text: "", wantErr: true},
		{text: "Gi", wantErr: true},
		{text: "-4Gi", wantErr: true},
		{text: "0", wantErr: true},
		{text: "40 gigabytes", wantErr: true},
		{text: "1e400Gi", wantErr: true},
		// The int64 ceiling. math.MaxInt64 is not representable as a float64 —
		// it rounds up to 2^63 — so a comparison against it let exactly 2^63
		// through to a conversion Go leaves undefined. Both the boundary and
		// the value that rounds onto it are refused.
		{text: "9223372036854775808", wantErr: true},
		{text: "9223372036854775807", wantErr: true},
		// Just under it still converts, so the refusal is a ceiling rather
		// than a narrowing of what a quantity may be.
		{text: "9223372035781033984", want: 9223372035781033984},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.text), func(t *testing.T) {
			got, err := parseStorageQuantity(tt.text)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseStorageQuantity(%q) = %d, want an error", tt.text, got)
				}

				return
			}

			if err != nil {
				t.Fatalf("parseStorageQuantity(%q) error = %v", tt.text, err)
			}
			if got != tt.want {
				t.Errorf("parseStorageQuantity(%q) = %d, want %d", tt.text, got, tt.want)
			}
		})
	}
}

func TestFormatGiB(t *testing.T) {
	tests := []struct {
		bytes int64
		want  string
	}{
		{bytes: 0, want: "1Gi"},
		{bytes: 1, want: "1Gi"},
		{bytes: 1 << 30, want: "1Gi"},
		{bytes: (1 << 30) + 1, want: "2Gi"},
		{bytes: 50 << 30, want: "50Gi"},
	}

	for _, tt := range tests {
		if got := formatGiB(tt.bytes); got != tt.want {
			t.Errorf("formatGiB(%d) = %q, want %q", tt.bytes, got, tt.want)
		}
	}
}

// TestTransientCredentials pins the per-service environment the tooling reads,
// and the refusal when the run holds no credentials to give it.
func TestTransientCredentials(t *testing.T) {
	cfg := &Configuration{
		Neo4jUsername:    "neo4j",
		Neo4jPassword:    "np",
		PostgresUsername: "postgres",
		PostgresPassword: "pp",
	}

	neo4j, err := transientCredentials(cfg, serviceNeo4j)
	if err != nil {
		t.Fatalf("transientCredentials(neo4j) error = %v", err)
	}
	if !reflect.DeepEqual(neo4j, map[string]string{"NEO4J_USERNAME": "neo4j", "NEO4J_PASSWORD": "np"}) {
		t.Errorf("transientCredentials(neo4j) = %v", neo4j)
	}

	postgres, err := transientCredentials(cfg, serviceTaskManagerDB)
	if err != nil {
		t.Fatalf("transientCredentials(task-manager-db) error = %v", err)
	}
	if !reflect.DeepEqual(postgres, map[string]string{"PGUSER": "postgres", "PGPASSWORD": "pp"}) {
		t.Errorf("transientCredentials(task-manager-db) = %v", postgres)
	}

	if _, err := transientCredentials(&Configuration{}, serviceNeo4j); err == nil {
		t.Error("transientCredentials() with no credentials = nil error, want a refusal")
	}
	if _, err := transientCredentials(cfg, "cache"); err == nil {
		t.Error("transientCredentials(cache) = nil error, want a refusal")
	}
}

// TestProbeWorkloadSpecNeedsNoStoreSize is the chicken-and-egg resolution
// asserted: the probe workload is constructible before anything is known about
// the store, which is what makes it able to go and find out.
func TestProbeWorkloadSpecNeedsNoStoreSize(t *testing.T) {
	cfg := NewInfrahubOps().Config()
	cfg.Neo4jUsername = "neo4j"
	cfg.Neo4jPassword = "pw"

	spec, err := probeWorkloadSpec(cfg, "infrahub", "a1b2c3d4", serviceNeo4j, 3)
	if err != nil {
		t.Fatalf("probeWorkloadSpec() error = %v", err)
	}

	if spec.ScratchSize != externalDBProbeScratch {
		t.Errorf("probe ScratchSize = %q, want the fixed %q", spec.ScratchSize, externalDBProbeScratch)
	}
	if spec.Role != workloadRoleProbe {
		t.Errorf("probe Role = %q", spec.Role)
	}
	if spec.Image != defaultExternalDBImageNeo4j {
		t.Errorf("probe Image = %q, want the same pinned image the capture uses (%q)", spec.Image, defaultExternalDBImageNeo4j)
	}
	if want := externalDBProbeDeadline(cfg, 3); spec.Deadline != want {
		t.Errorf("probe Deadline = %v, want %v", spec.Deadline, want)
	}
	// The pod's deadline must outlive every bounded call the probe is
	// permitted to make, not just one of them, or the cluster removes the pod
	// while the tool still considers the probe to be working — which reports
	// the exec failing rather than which read stalled (FR-025).
	if work := time.Duration(externalDBProbeFixedCalls+3) * externalDBProbeBound(cfg); spec.Deadline <= work {
		t.Errorf("probe Deadline = %v, want more than the %v the probe's own bounds permit it to spend", spec.Deadline, work)
	}
	if _, err := buildTransientWorkloadManifest(spec); err != nil {
		t.Errorf("the probe spec must build a manifest without a store size: %v", err)
	}
}

// TestExternalDBProbeDeadlineBudgetsEveryCall pins the deadline to the work the
// probe is permitted to do rather than to one call of it. The probe walks the
// endpoints looking for one that answers, so a three-member cluster can spend
// three bounds before it reaches the reads the fixed budget covers.
func TestExternalDBProbeDeadlineBudgetsEveryCall(t *testing.T) {
	cfg := NewInfrahubOps().Config()
	bound := externalDBProbeBound(cfg)

	single := externalDBProbeDeadline(cfg, 1)
	cluster := externalDBProbeDeadline(cfg, 3)

	if cluster-single != 2*bound {
		t.Errorf("deadline grew by %v across two extra members, want %v", cluster-single, 2*bound)
	}

	// The fixed floor and the member walk are both in there: a probe that
	// spends its whole permitted budget must still finish inside the pod.
	if want := externalDBWorkloadReadyTimeout + time.Duration(externalDBProbeFixedCalls+1)*bound + externalDBWorkloadDeadlineMargin; single != want {
		t.Errorf("externalDBProbeDeadline(cfg, 1) = %v, want %v", single, want)
	}

	// A count nothing resolved is the single endpoint every probe has, not an
	// empty budget: flooring it at one is what keeps a caller that has not
	// resolved the members yet from building a pod that dies on its first read.
	for _, members := range []int{0, -1} {
		if got := externalDBProbeDeadline(cfg, members); got != single {
			t.Errorf("externalDBProbeDeadline(cfg, %d) = %v, want the single-endpoint %v", members, got, single)
		}
	}
}

// TestExternalDBCaptureDeadlineUsesTheResolvedBound is the sibling defect: the
// capture pod's deadline was read off the operator's raw setting, which is not
// the bound any operation runs under. Both directions of that mistake are
// asserted, because both produce a pod that dies under a working capture.
func TestExternalDBCaptureDeadlineUsesTheResolvedBound(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
	}{
		// Unset. The raw field is zero, so the old arithmetic gave the pod ten
		// minutes while the operation it hosts is allowed two hours.
		{name: "unset", timeout: 0},
		// Negative. The raw field floors deadlineSeconds at one second, so the
		// cluster removes the pod almost immediately — while externalDBBound
		// reads the same value as unset and lets the operation run for hours.
		{name: "negative", timeout: -time.Hour},
		// Below the floor. resolveExternalDBTimeout raises it, so the pod has
		// to be sized against the raised value rather than the written one.
		{name: "below the floor", timeout: time.Second},
		{name: "set", timeout: 45 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := NewInfrahubOps().Config()
			cfg.Neo4jUsername, cfg.Neo4jPassword = "neo4j", "pw"
			cfg.ExternalDB.Timeout = tt.timeout

			spec, err := captureWorkloadSpec(cfg, "infrahub", "a1b2c3d4", serviceNeo4j, 30<<30)
			if err != nil {
				t.Fatalf("captureWorkloadSpec() error = %v", err)
			}

			if want := externalDBCaptureDeadline(cfg); spec.Deadline != want {
				t.Errorf("capture Deadline = %v, want %v", spec.Deadline, want)
			}
			// The property that matters, stated directly: the pod outlives
			// *both* the full-bound operations it hosts, whatever the operator
			// wrote. The capture is one; copyFromTransientWorkload opens its
			// own externalDBBound to take the artifact back out, and budgeting
			// for one killed the pod mid-copy after a capture that had already
			// succeeded.
			if spec.Deadline <= externalDBCaptureFullBoundCalls*externalDBBound(cfg) {
				t.Errorf("capture Deadline = %v, want more than the %d operation bounds of %v the pod hosts",
					spec.Deadline, externalDBCaptureFullBoundCalls, externalDBBound(cfg))
			}
		})
	}
}

// TestExternalDBCaptureDeadlineBudgetsTheCopyToo is T108, which is
// TestExternalDBProbeDeadlineBudgetsEveryCall's finding at the sibling
// function: the capture pod hosts two operations that each get the whole bound,
// and the deadline budgeted one. The consequence is the worst shape this path
// has — the cluster deletes the pod during the copy, discarding a capture that
// cost a full read of a production database.
func TestExternalDBCaptureDeadlineBudgetsTheCopyToo(t *testing.T) {
	cfg := NewInfrahubOps().Config()
	bound := externalDBBound(cfg)

	if externalDBCaptureFullBoundCalls < 2 {
		t.Fatalf("externalDBCaptureFullBoundCalls = %d, want at least the capture and the copy out",
			externalDBCaptureFullBoundCalls)
	}

	want := externalDBWorkloadReadyTimeout + externalDBCaptureFullBoundCalls*bound + externalDBWorkloadDeadlineMargin
	if got := externalDBCaptureDeadline(cfg); got != want {
		t.Errorf("externalDBCaptureDeadline(cfg) = %v, want %v", got, want)
	}

	// Stated as the sequence rather than as the formula: a capture that used
	// its whole bound, and then a copy that uses its whole bound, both have to
	// fit inside the pod's life with the image pull ahead of them.
	if externalDBCaptureDeadline(cfg) < externalDBWorkloadReadyTimeout+2*bound {
		t.Errorf("externalDBCaptureDeadline(cfg) = %v, want room for the image pull, the capture and the copy",
			externalDBCaptureDeadline(cfg))
	}
}

// TestCaptureWorkloadSpec covers the capture spec's two sizing outcomes and
// the deadline that keeps the pod alive past the operation it hosts.
func TestCaptureWorkloadSpec(t *testing.T) {
	cfg := NewInfrahubOps().Config()
	cfg.Neo4jUsername = "neo4j"
	cfg.Neo4jPassword = "pw"

	spec, err := captureWorkloadSpec(cfg, "infrahub", "a1b2c3d4", serviceNeo4j, 30<<30)
	if err != nil {
		t.Fatalf("captureWorkloadSpec() error = %v", err)
	}
	if spec.ScratchSize != "60Gi" {
		t.Errorf("capture ScratchSize = %q, want 60Gi", spec.ScratchSize)
	}
	if want := externalDBCaptureDeadline(cfg); spec.Deadline != want {
		t.Errorf("capture Deadline = %v, want %v: the pod must outlive the operation it hosts", spec.Deadline, want)
	}

	// FR-022: no size and no override stops the run naming the override.
	if _, err := captureWorkloadSpec(cfg, "infrahub", "a1b2c3d4", serviceNeo4j, 0); err == nil {
		t.Error("captureWorkloadSpec() with no store size = nil error, want a refusal")
	} else if !strings.Contains(err.Error(), "--external-db-scratch-size") {
		t.Errorf("captureWorkloadSpec() error = %q, want it to name --external-db-scratch-size", err)
	}

	cfg.ExternalDB.ScratchSize = "500Gi"
	sized, err := captureWorkloadSpec(cfg, "infrahub", "a1b2c3d4", serviceNeo4j, 0)
	if err != nil {
		t.Fatalf("captureWorkloadSpec() with an override error = %v", err)
	}
	if sized.ScratchSize != "500Gi" {
		t.Errorf("capture ScratchSize = %q, want the override 500Gi", sized.ScratchSize)
	}
}

// fakeCluster records what the lifecycle asked the cluster to do and answers
// with what the test wants it to have found.
type fakeCluster struct {
	created    [][]byte
	deleted    [][]string
	uid        string
	uidErr     error
	podName    string
	podNameErr error
	podWaitErr error
	waitErr    error
	createErrs map[string]error
	// removeErrs are returned by successive remove calls, so a test can fail a
	// delete once and let the retry that follows succeed.
	removeErrs []error
	// waitBounds records the bound the readiness wait asked for, so a test can
	// assert the wait is not bounded at the control bound (FR-025).
	waitBounds []time.Duration
}

func (f *fakeCluster) ops() transientClusterOps {
	query := func(_ string, args ...string) (string, error) {
		joined := strings.Join(args, " ")
		switch {
		case args[0] == "wait" && strings.Contains(joined, "--for=create"):
			return "", f.podWaitErr
		case args[0] == "wait":
			return "", f.waitErr
		// The Job's pod listing: `get pods`, plural and label-selected. The
		// readiness diagnosis reads `get pod <name>`, singular, and falls
		// through to the default below.
		case args[0] == "get" && args[1] == "pods":
			if !strings.Contains(joined, livePodFieldSelector) {
				return "", fmt.Errorf("the Job's pod was resolved without the live-pod field selector: a terminated attempt carrying the same job-name label would be exec'd into (args: %s)", joined)
			}
			if f.podName == "" {
				return "infrahub-backup-xdb-neo4j-capture-a1b2c3d4-abcde", f.podNameErr
			}

			return f.podName, f.podNameErr
		case strings.Contains(joined, "metadata.uid"):
			return f.uid, f.uidErr
		default:
			return "Pending: ImagePullBackOff no such image", nil
		}
	}

	return transientClusterOps{
		create: func(manifest []byte) error {
			f.created = append(f.created, manifest)
			var probe struct {
				Kind string `json:"kind"`
			}
			_ = json.Unmarshal(manifest, &probe)
			if err, ok := f.createErrs[probe.Kind]; ok {
				return err
			}

			return nil
		},
		query: query,
		queryWithin: func(bound time.Duration) podRunner {
			f.waitBounds = append(f.waitBounds, bound)

			return query
		},
		remove: func(args ...string) error {
			f.deleted = append(f.deleted, args)
			if len(f.removeErrs) > 0 {
				err := f.removeErrs[0]
				f.removeErrs = f.removeErrs[1:]

				return err
			}

			return nil
		},
	}
}

func (f *fakeCluster) createdKinds() []string {
	kinds := []string{}
	for _, manifest := range f.created {
		var probe struct {
			Kind string `json:"kind"`
		}
		_ = json.Unmarshal(manifest, &probe)
		kinds = append(kinds, probe.Kind)
	}

	return kinds
}

func testBackend() *KubernetesBackend {
	backend := NewKubernetesBackend(&Configuration{}, NewCommandExecutor())
	backend.namespace = "infrahub"

	return backend
}

// TestCreateTransientWorkloadTracksBeforeTheCreate is FR-011's orphan case: a
// create that fails does not mean no pod was made. A bound expiring after the
// API server accepted the manifest fails the call with the pod standing, and
// the run has only one chance to record the name that could find it again.
func TestCreateTransientWorkloadTracksBeforeTheCreate(t *testing.T) {
	backend := testBackend()
	cluster := &fakeCluster{
		uid:        "u1",
		createErrs: map[string]error{"Job": errors.New("context deadline exceeded")},
	}

	spec := testCaptureSpec()
	if _, err := backend.createTransientWorkloadWith(cluster.ops(), spec); err == nil {
		t.Fatal("createTransientWorkloadWith() = nil error, want the create failure")
	}

	if len(backend.transientWorkloads) != 1 {
		t.Fatalf("tracked %d workloads after a failed create, want the Job that may exist to still be tracked", len(backend.transientWorkloads))
	}

	// The property that matters is not the tracking itself but what it buys:
	// the run's exit path removes the Job.
	if err := backend.ReleaseTransientWorkloads(); err != nil {
		t.Fatalf("ReleaseTransientWorkloads() error = %v", err)
	}
	if len(cluster.deleted) != 1 || !slices.Contains(cluster.deleted[0], spec.jobName()) {
		t.Errorf("deleted = %v, want one delete naming %s", cluster.deleted, spec.jobName())
	}
}

// TestCreateTransientWorkloadUntracksAnotherRunsPod is the opposite mistake,
// and the more damaging one. AlreadyExists is the single create failure that
// also says whose pod it is, so it is the single case where the run must give
// up the tracking it took out a moment earlier.
func TestCreateTransientWorkloadUntracksAnotherRunsPod(t *testing.T) {
	backend := testBackend()
	cluster := &fakeCluster{
		uid:        "u1",
		createErrs: map[string]error{"Job": errors.New(`jobs "x" already exists`)},
	}

	if _, err := backend.createTransientWorkloadWith(cluster.ops(), testCaptureSpec()); err == nil {
		t.Fatal("createTransientWorkloadWith() = nil error, want the collision to be refused")
	}

	if len(backend.transientWorkloads) != 0 {
		t.Fatalf("tracked %d workloads after a name collision, want none: the Job belongs to another run", len(backend.transientWorkloads))
	}

	if err := backend.ReleaseTransientWorkloads(); err != nil {
		t.Fatalf("ReleaseTransientWorkloads() error = %v", err)
	}
	if len(cluster.deleted) != 0 {
		t.Errorf("deleted = %v, want nothing: deleting here would remove a workload another run is still using", cluster.deleted)
	}
}

// TestReleaseRetriesAFailedDelete pins the ordering inside Release. Recording
// the removal before attempting it makes the attempt's outcome irrelevant, so
// the one case the retry exists for — an API server having a bad minute — is
// the one case that never gets retried.
func TestReleaseRetriesAFailedDelete(t *testing.T) {
	backend := testBackend()
	cluster := &fakeCluster{uid: "u1", removeErrs: []error{errors.New("the server is currently unable to handle the request")}}

	workload, err := backend.createTransientWorkloadWith(cluster.ops(), testCaptureSpec())
	if err != nil {
		t.Fatalf("createTransientWorkloadWith() error = %v", err)
	}

	if err := backend.ReleaseTransientWorkloads(); err == nil {
		t.Fatal("ReleaseTransientWorkloads() = nil error, want the delete failure reported")
	}

	// Not released, because it was not removed — and still tracked, because
	// something has to be left holding the name for the retry to use.
	if workload.State == workloadStateReleased {
		t.Error("workload State = released after a delete that failed")
	}
	if len(backend.transientWorkloads) != 1 {
		t.Fatalf("tracked %d workloads after a failed delete, want the Job that is still standing to be kept", len(backend.transientWorkloads))
	}
	// It must also stop being usable: the run asked for it to go.
	if _, err := workload.ExecutionTarget(); err == nil {
		t.Error("ExecutionTarget() = nil error after a release, want a refusal")
	}

	if err := backend.ReleaseTransientWorkloads(); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if len(cluster.deleted) != 2 {
		t.Errorf("deleted %d times, want the failed delete to have been retried", len(cluster.deleted))
	}
	if workload.State != workloadStateReleased {
		t.Errorf("workload State = %q after a delete that succeeded, want %q", workload.State, workloadStateReleased)
	}
	if len(backend.transientWorkloads) != 0 {
		t.Errorf("tracked %d workloads after a delete that succeeded, want none", len(backend.transientWorkloads))
	}
}

// TestCreateTransientWorkloadWith pins the creation order FR-023 forces — pod
// first, then the secret owned by it — and the execution target FR-017 asks
// for, which is the workload's own answer rather than an entry in the
// resolver's cache.
func TestCreateTransientWorkloadWith(t *testing.T) {
	backend := testBackend()
	cluster := &fakeCluster{uid: "8f14e45f-ea3b-4b0e-9d3e-2a1c5f6b7d80"}

	workload, err := backend.createTransientWorkloadWith(cluster.ops(), testCaptureSpec())
	if err != nil {
		t.Fatalf("createTransientWorkloadWith() error = %v", err)
	}

	if got := cluster.createdKinds(); !reflect.DeepEqual(got, []string{"Job", "Secret"}) {
		t.Errorf("created %v, want the Job before the secret so the credentials are never unowned (FR-023)", got)
	}

	var secret transientSecretManifest
	if err := json.Unmarshal(cluster.created[1], &secret); err != nil {
		t.Fatalf("secret manifest is not valid JSON: %v", err)
	}
	if len(secret.Metadata.OwnerReferences) != 1 || secret.Metadata.OwnerReferences[0].UID != cluster.uid {
		t.Errorf("secret owner reference = %#v, want the created Job's UID %q", secret.Metadata.OwnerReferences, cluster.uid)
	}

	if workload.State != workloadStateReady {
		t.Errorf("State = %q, want %q", workload.State, workloadStateReady)
	}
	if _, registered := backend.podCache[serviceNeo4j]; registered {
		t.Errorf("the workload registered itself in podCache; Start and scaleServices replace that map wholesale, and a restore calls StartServices by design, so a registration is gone by the copy that needs it")
	}
	target, err := workload.ExecutionTarget()
	if err != nil || target != workload.PodName {
		t.Errorf("ExecutionTarget() = %q, %v, want %q, nil", target, err, workload.PodName)
	}
	if len(cluster.deleted) != 0 {
		t.Errorf("deleted %v on the success path, want nothing", cluster.deleted)
	}
}

// TestCreateTransientWorkloadWithFailures asserts every path that fails after
// the pod exists removes it, and that none of them offers a pod that cannot be
// exec'd into as an execution target.
func TestCreateTransientWorkloadWithFailures(t *testing.T) {
	tests := []struct {
		name       string
		cluster    *fakeCluster
		wantMsg    string
		wantDelete bool
	}{
		{
			name:    "the pod cannot be created",
			cluster: &fakeCluster{uid: "u1", createErrs: map[string]error{"Job": errors.New("forbidden")}},
			wantMsg: "failed to create the transient database workload",
		},
		{
			name:       "the Job's UID cannot be read",
			cluster:    &fakeCluster{uidErr: errors.New("connection refused")},
			wantMsg:    "failed to read the UID",
			wantDelete: true,
		},
		{
			name:       "the pod reports no UID",
			cluster:    &fakeCluster{uid: "  "},
			wantMsg:    "empty UID does not bind",
			wantDelete: true,
		},
		{
			name:       "the secret cannot be created",
			cluster:    &fakeCluster{uid: "u1", createErrs: map[string]error{"Secret": errors.New("forbidden")}},
			wantMsg:    "failed to create the credential secret",
			wantDelete: true,
		},
		{
			name:       "the Job does not create a pod",
			cluster:    &fakeCluster{uid: "u1", podWaitErr: errors.New("timed out waiting for the condition")},
			wantMsg:    "did not create a pod",
			wantDelete: true,
		},
		{
			name:       "the Job pod cannot be found",
			cluster:    &fakeCluster{uid: "u1", podNameErr: errors.New("connection refused")},
			wantMsg:    "failed to find the pod",
			wantDelete: true,
		},
		{
			name:       "the pod never becomes ready",
			cluster:    &fakeCluster{uid: "u1", waitErr: errors.New("timed out waiting for the condition")},
			wantMsg:    "ImagePullBackOff",
			wantDelete: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := testBackend()

			workload, err := backend.createTransientWorkloadWith(tt.cluster.ops(), testCaptureSpec())
			if err == nil {
				t.Fatalf("createTransientWorkloadWith() = %#v, want an error", workload)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantMsg)
			}
			if _, registered := backend.podCache[serviceNeo4j]; registered {
				t.Error("a failure path wrote to podCache; the transient workload registers nowhere")
			}
			if workload != nil {
				if _, err := workload.ExecutionTarget(); err == nil {
					t.Error("ExecutionTarget() answered on a failure path; a pod that is not ready is not an execution target")
				}
			}

			if tt.wantDelete {
				if len(tt.cluster.deleted) != 1 {
					t.Fatalf("deleted %v, want the Job removed once (FR-011)", tt.cluster.deleted)
				}
				if got := strings.Join(tt.cluster.deleted[0], " "); !strings.HasPrefix(got, "job infrahub-backup-xdb-neo4j-capture-") {
					t.Errorf("delete args = %q, want the transient Job", got)
				}
			} else if len(tt.cluster.deleted) != 0 {
				t.Errorf("deleted %v, want nothing: the pod was never created", tt.cluster.deleted)
			}
		})
	}
}

// TestTransientWorkloadRelease asserts releasing removes the Job, stops it
// being an execution target, and is safe to repeat — the last of which matters
// because the next chunk wires Release into every exit path, several of which
// can overlap.
func TestTransientWorkloadRelease(t *testing.T) {
	backend := testBackend()
	cluster := &fakeCluster{uid: "u1"}

	workload, err := backend.createTransientWorkloadWith(cluster.ops(), testCaptureSpec())
	if err != nil {
		t.Fatalf("createTransientWorkloadWith() error = %v", err)
	}

	if err := workload.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if workload.State != workloadStateReleased {
		t.Errorf("State = %q, want %q", workload.State, workloadStateReleased)
	}
	if _, err := workload.ExecutionTarget(); err == nil {
		t.Error("ExecutionTarget() on a released workload = nil error, want a refusal")
	}

	if err := workload.Release(); err != nil {
		t.Fatalf("Release() a second time error = %v", err)
	}
	if len(cluster.deleted) != 1 {
		t.Errorf("deleted %v, want exactly one delete across two Release calls", cluster.deleted)
	}
}

// TestTransientWorkloadReleaseLeavesAnotherServicesCacheAlone pins that the
// workload's lifecycle touches the resolver's cache at neither end: a run with
// one external and one internal database has the internal one cached, and
// creating and releasing the external one must leave it exactly as it was.
func TestTransientWorkloadReleaseLeavesAnotherServicesCacheAlone(t *testing.T) {
	backend := testBackend()
	backend.podCache[serviceTaskManagerDB] = "task-manager-db-0"
	cluster := &fakeCluster{uid: "u1"}

	workload, err := backend.createTransientWorkloadWith(cluster.ops(), testCaptureSpec())
	if err != nil {
		t.Fatalf("createTransientWorkloadWith() error = %v", err)
	}
	if err := workload.Release(); err != nil {
		t.Fatalf("Release() error = %v", err)
	}

	if got := backend.podCache[serviceTaskManagerDB]; got != "task-manager-db-0" {
		t.Errorf("podCache[%s] = %q, want it untouched", serviceTaskManagerDB, got)
	}
}

// TestExecutionTargetRefusesPendingWorkload is the data-model invariant that
// only a ready workload is an execution target.
func TestExecutionTargetRefusesPendingWorkload(t *testing.T) {
	workload := &TransientWorkload{Service: serviceNeo4j, PodName: "p", State: workloadStatePending}

	if _, err := workload.ExecutionTarget(); err == nil {
		t.Fatal("ExecutionTarget() on a pending workload = nil error, want a refusal")
	}
}

// TestNewRunIDIsUnique guards the label that reaping and the one-run-one-
// workload invariant both rest on.
func TestNewRunIDIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id, err := newRunID()
		if err != nil {
			t.Fatalf("newRunID() error = %v", err)
		}
		if id == "" {
			t.Fatal("newRunID() = empty")
		}
		if seen[id] {
			t.Fatalf("newRunID() repeated %q", id)
		}
		seen[id] = true
	}
}

// ---------------------------------------------------------------------------
// Reaping (FR-011) and the one-run-one-workload invariant
// ---------------------------------------------------------------------------

// reapNow is the fixed clock the reaping tests judge ages against, so every
// case states an age rather than depending on when the suite runs.
var reapNow = time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)

// peerCapturePod is another run's capture pod, Running and well inside its own
// deadline. It is the object the reaper must never touch: a stray from a dead
// run and a workload hosting a backup happening right now carry the same marker
// label and both carry a run ID that is not ours, so reaping on that alone
// would have two concurrent runs destroy each other's captures.
func peerCapturePod(runID string) transientCandidate {
	return transientCandidate{
		Kind:            transientKindPod,
		Name:            transientObjectPrefix + "-capture-" + runID,
		RunID:           runID,
		Phase:           "Running",
		Created:         reapNow.Add(-20 * time.Minute),
		DeadlineSeconds: int64((2 * time.Hour).Seconds()),
	}
}

func peerCredentialSecret(runID string, age time.Duration) transientCandidate {
	return transientCandidate{
		Kind:    transientKindSecret,
		Name:    transientObjectPrefix + "-capture-" + runID + "-creds",
		RunID:   runID,
		Created: reapNow.Add(-age),
	}
}

func strayNames(strays []transientStray) []string {
	names := []string{}
	for _, stray := range strays {
		names = append(names, stray.Name)
	}

	return names
}

// TestSelectTransientStrays is the safety test of this chunk. Every case that
// expects nothing reaped is a case where deleting would damage either a
// concurrent backup or this run's own capture.
func TestSelectTransientStrays(t *testing.T) {
	const ownRun = "aaaa1111"
	const peerRun = "bbbb2222"

	withPhase := func(candidate transientCandidate, phase string) transientCandidate {
		candidate.Phase = phase

		return candidate
	}
	withAge := func(candidate transientCandidate, age time.Duration) transientCandidate {
		candidate.Created = reapNow.Add(-age)

		return candidate
	}

	tests := []struct {
		name       string
		candidates []transientCandidate
		want       []string
	}{
		{
			name:       "a peer run's running pod inside its deadline is left strictly alone",
			candidates: []transientCandidate{peerCapturePod(peerRun)},
			want:       []string{},
		},
		{
			name:       "a peer run's pending pod is left alone: it may still be pulling its image",
			candidates: []transientCandidate{withPhase(peerCapturePod(peerRun), "Pending")},
			want:       []string{},
		},
		{
			name:       "a failed pod is reaped: nothing runs in a terminal pod",
			candidates: []transientCandidate{withPhase(peerCapturePod(peerRun), "Failed")},
			want:       []string{transientObjectPrefix + "-capture-" + peerRun},
		},
		{
			name:       "a succeeded pod is reaped",
			candidates: []transientCandidate{withPhase(peerCapturePod(peerRun), "Succeeded")},
			want:       []string{transientObjectPrefix + "-capture-" + peerRun},
		},
		{
			name:       "a pod past its own deadline and the grace period is reaped",
			candidates: []transientCandidate{withAge(peerCapturePod(peerRun), 2*time.Hour+transientStrayGrace+time.Minute)},
			want:       []string{transientObjectPrefix + "-capture-" + peerRun},
		},
		{
			name:       "a pod past its deadline but inside the grace period is left alone",
			candidates: []transientCandidate{withAge(peerCapturePod(peerRun), 2*time.Hour+time.Minute)},
			want:       []string{},
		},
		{
			name:       "this run's own running pod is left to Release",
			candidates: []transientCandidate{peerCapturePod(ownRun)},
			want:       []string{},
		},
		{
			name:       "this run's own failed pod is left to Release",
			candidates: []transientCandidate{withPhase(peerCapturePod(ownRun), "Failed")},
			want:       []string{},
		},
		{
			name: "a marked pod carrying no run ID is left alone: abandonment cannot be shown",
			candidates: []transientCandidate{func() transientCandidate {
				candidate := withPhase(peerCapturePod(peerRun), "Failed")
				candidate.RunID = ""

				return candidate
			}()},
			want: []string{},
		},
		{
			name: "a running pod with no readable creation timestamp is left alone",
			candidates: []transientCandidate{func() transientCandidate {
				candidate := peerCapturePod(peerRun)
				candidate.Created = time.Time{}

				return candidate
			}()},
			want: []string{},
		},
		{
			name: "a running pod with no deadline is left alone however old it looks",
			candidates: []transientCandidate{func() transientCandidate {
				candidate := withAge(peerCapturePod(peerRun), 30*24*time.Hour)
				candidate.DeadlineSeconds = 0

				return candidate
			}()},
			want: []string{},
		},
		{
			name:       "a secret whose run still has a pod is left to the garbage collector",
			candidates: []transientCandidate{peerCapturePod(peerRun), peerCredentialSecret(peerRun, 3*time.Hour)},
			want:       []string{},
		},
		{
			name:       "a terminal pod is reaped and its secret is left to the owner reference",
			candidates: []transientCandidate{withPhase(peerCapturePod(peerRun), "Failed"), peerCredentialSecret(peerRun, 3*time.Hour)},
			want:       []string{transientObjectPrefix + "-capture-" + peerRun},
		},
		{
			name:       "a secret whose owner is gone is reaped once the grace period has passed",
			candidates: []transientCandidate{peerCredentialSecret(peerRun, time.Hour)},
			want:       []string{transientObjectPrefix + "-capture-" + peerRun + "-creds"},
		},
		{
			name:       "a fresh ownerless secret is left alone: its run may be mid-creation",
			candidates: []transientCandidate{peerCredentialSecret(peerRun, 5*time.Minute)},
			want:       []string{},
		},
		{
			name:       "this run's own secret is never reaped",
			candidates: []transientCandidate{peerCredentialSecret(ownRun, 3*time.Hour)},
			want:       []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strayNames(selectTransientStrays(tt.candidates, ownRun, reapNow))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("selectTransientStrays() reaped %v, want %v", got, tt.want)
			}
		})
	}
}

// TestSelectTransientStraysExplainsItself pins that every reaping decision
// carries the evidence it rested on, because an operator who finds an object
// gone needs to see why it was judged abandoned.
func TestSelectTransientStraysExplainsItself(t *testing.T) {
	strays := selectTransientStrays([]transientCandidate{
		{Kind: transientKindPod, Name: "p", RunID: "peer", Phase: "Failed"},
		{Kind: transientKindSecret, Name: "s", RunID: "other", Created: reapNow.Add(-time.Hour)},
	}, "own", reapNow)

	if len(strays) != 2 {
		t.Fatalf("selectTransientStrays() = %#v, want two strays", strays)
	}
	if !strings.Contains(strays[0].Reason, "Failed") {
		t.Errorf("pod reason = %q, want it to name the terminal phase", strays[0].Reason)
	}
	if !strings.Contains(strays[1].Reason, "garbage collector") {
		t.Errorf("secret reason = %q, want it to say the owner is gone", strays[1].Reason)
	}
}

// TestParseTransientCandidates pins the tab-separated listing the reaper reads,
// and that an unreadable field is left unknown rather than guessed — selection
// treats unknown as insufficient evidence to reap.
func TestParseTransientCandidates(t *testing.T) {
	created := reapNow.Add(-time.Hour).UTC().Format(time.RFC3339)
	output := fmt.Sprintf("%s-capture-abc\tabc\tRunning\t%s\t7800\n"+
		"%s-probe-def\tdef\tFailed\t%s\t\n"+
		"%s-probe-ghi\tghi\tRunning\tnot-a-timestamp\t1200\n"+
		"\n"+
		"orphan-line-with-one-field\n",
		transientObjectPrefix, created, transientObjectPrefix, created, transientObjectPrefix)

	candidates := parseTransientCandidates(transientKindPod, output)
	if len(candidates) != 3 {
		t.Fatalf("parseTransientCandidates() = %#v, want the three readable pods", candidates)
	}

	if candidates[0].RunID != "abc" || candidates[0].Phase != "Running" || candidates[0].DeadlineSeconds != 7800 {
		t.Errorf("first candidate = %#v, want run abc, Running, a 7800s deadline", candidates[0])
	}
	if !candidates[0].Created.Equal(reapNow.Add(-time.Hour)) {
		t.Errorf("first candidate created = %s, want %s", candidates[0].Created, reapNow.Add(-time.Hour))
	}
	if candidates[1].DeadlineSeconds != 0 {
		t.Errorf("second candidate deadline = %d, want 0 for a pod that reported none", candidates[1].DeadlineSeconds)
	}
	if !candidates[2].Created.IsZero() {
		t.Errorf("third candidate created = %s, want the zero time for an unreadable timestamp", candidates[2].Created)
	}

	secrets := parseTransientCandidates(transientKindSecret, fmt.Sprintf("%s-capture-abc-creds\tabc\t\t%s\n", transientObjectPrefix, created))
	if len(secrets) != 1 || secrets[0].Phase != "" || !secrets[0].Created.Equal(reapNow.Add(-time.Hour)) {
		t.Fatalf("parseTransientCandidates(secret) = %#v, want one secret with no phase and a read creation time", secrets)
	}
}

// fakeReapCluster answers the reaper's two listings and records what it deleted.
type fakeReapCluster struct {
	pods       string
	secrets    string
	podsErr    error
	secretsErr error
	deleteErr  error
	queried    [][]string
	deleted    [][]string
}

func (f *fakeReapCluster) ops() transientClusterOps {
	query := func(_ string, args ...string) (string, error) {
		f.queried = append(f.queried, args)
		switch args[1] {
		case "pods":
			return f.pods, f.podsErr
		case "secrets":
			return f.secrets, f.secretsErr
		default:
			return "", nil
		}
	}

	return transientClusterOps{
		create: func([]byte) error {
			return errors.New("the reaper must not create anything")
		},
		query: query,
		queryWithin: func(time.Duration) podRunner {
			return query
		},
		remove: func(args ...string) error {
			f.deleted = append(f.deleted, args)

			return f.deleteErr
		},
	}
}

func (f *fakeReapCluster) deletedNames() []string {
	names := []string{}
	for _, args := range f.deleted {
		names = append(names, strings.Join(args[:2], " "))
	}

	return names
}

// TestReapTransientStraysWith drives the reaper against a namespace holding one
// live peer workload, one abandoned pod and one orphaned secret.
func TestReapTransientStraysWith(t *testing.T) {
	live := reapNow.Add(-20 * time.Minute).UTC().Format(time.RFC3339)
	old := reapNow.Add(-4 * time.Hour).UTC().Format(time.RFC3339)

	pods := fmt.Sprintf("%s-capture-bbbb2222\tbbbb2222\tRunning\t%s\t7800\n%s-probe-cccc3333\tcccc3333\tFailed\t%s\t1200\n",
		transientObjectPrefix, live, transientObjectPrefix, old)
	secrets := fmt.Sprintf("%s-capture-bbbb2222-creds\tbbbb2222\t\t%s\n%s-capture-dddd4444-creds\tdddd4444\t\t%s\n",
		transientObjectPrefix, live, transientObjectPrefix, old)

	t.Run("removes what it can show is abandoned and nothing else", func(t *testing.T) {
		backend := testBackend()
		cluster := &fakeReapCluster{pods: pods, secrets: secrets}

		if err := backend.reapTransientStraysWith(cluster.ops(), "aaaa1111", reapNow); err != nil {
			t.Fatalf("reapTransientStraysWith() error = %v", err)
		}

		want := []string{
			"pod " + transientObjectPrefix + "-probe-cccc3333",
			"secret " + transientObjectPrefix + "-capture-dddd4444-creds",
		}
		if got := cluster.deletedNames(); !reflect.DeepEqual(got, want) {
			t.Errorf("deleted %v, want %v — a live peer run's pod and its secret must survive (Principle II)", got, want)
		}
		for _, args := range cluster.deleted {
			joined := strings.Join(args, " ")
			if !strings.Contains(joined, "--ignore-not-found") {
				t.Errorf("delete args = %q, want --ignore-not-found: the object may have gone since the listing", joined)
			}
		}

		if len(cluster.queried) != 2 {
			t.Fatalf("queried %v, want one listing per kind", cluster.queried)
		}
		for _, args := range cluster.queried {
			if !strings.Contains(strings.Join(args, " "), transientLabelMarker+"=true") {
				t.Errorf("listing args = %v, want them selected on %s", args, transientLabelMarker)
			}
		}
	})

	t.Run("a live peer's workload survives even when this run has none of its own", func(t *testing.T) {
		backend := testBackend()
		cluster := &fakeReapCluster{
			pods:    fmt.Sprintf("%s-capture-bbbb2222\tbbbb2222\tRunning\t%s\t7800\n", transientObjectPrefix, live),
			secrets: fmt.Sprintf("%s-capture-bbbb2222-creds\tbbbb2222\t\t%s\n", transientObjectPrefix, live),
		}

		if err := backend.reapTransientStraysWith(cluster.ops(), "aaaa1111", reapNow); err != nil {
			t.Fatalf("reapTransientStraysWith() error = %v", err)
		}
		if len(cluster.deleted) != 0 {
			t.Errorf("deleted %v, want nothing: a concurrent backup's capture must not be destroyed", cluster.deletedNames())
		}
	})

	t.Run("a failed listing stops the reaper without deleting anything", func(t *testing.T) {
		backend := testBackend()
		cluster := &fakeReapCluster{pods: pods, podsErr: errors.New("connection refused")}

		err := backend.reapTransientStraysWith(cluster.ops(), "aaaa1111", reapNow)
		if err == nil {
			t.Fatal("reapTransientStraysWith() = nil, want an error when the namespace cannot be listed")
		}
		if !strings.Contains(err.Error(), "infrahub") {
			t.Errorf("error = %q, want it to name the namespace", err)
		}
		if len(cluster.deleted) != 0 {
			t.Errorf("deleted %v, want nothing: the listing was incomplete", cluster.deletedNames())
		}
	})

	t.Run("a delete failure is reported without abandoning the rest", func(t *testing.T) {
		backend := testBackend()
		cluster := &fakeReapCluster{pods: pods, secrets: secrets, deleteErr: errors.New("forbidden")}

		err := backend.reapTransientStraysWith(cluster.ops(), "aaaa1111", reapNow)
		if err == nil {
			t.Fatal("reapTransientStraysWith() = nil, want the delete failures reported")
		}
		if len(cluster.deleted) != 2 {
			t.Errorf("attempted %v, want both removals attempted", cluster.deletedNames())
		}
		for _, want := range []string{"probe-cccc3333", "dddd4444-creds"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error = %q, want it to mention %q", err, want)
			}
		}
	})
}

func testProbeSpec() transientWorkloadSpec {
	spec := testCaptureSpec()
	spec.Role = workloadRoleProbe
	spec.ScratchSize = externalDBProbeScratch
	spec.Deadline = externalDBProbeDeadline(NewInfrahubOps().Config(), 1)

	return spec
}

// TestReleaseTransientWorkloads asserts one call on a run's exit path removes
// every workload the run created (FR-011). The probe is the one a single
// per-creation defer leaks: a capture that fails to start returns before
// anything releases the probe that sized it.
func TestReleaseTransientWorkloads(t *testing.T) {
	backend := testBackend()
	cluster := &fakeCluster{uid: "u1"}

	probe, err := backend.createTransientWorkloadWith(cluster.ops(), testProbeSpec())
	if err != nil {
		t.Fatalf("createTransientWorkloadWith(probe) error = %v", err)
	}
	capture, err := backend.createTransientWorkloadWith(cluster.ops(), testCaptureSpec())
	if err != nil {
		t.Fatalf("createTransientWorkloadWith(capture) error = %v", err)
	}
	if err := backend.ReleaseTransientWorkloads(); err != nil {
		t.Fatalf("ReleaseTransientWorkloads() error = %v", err)
	}

	want := [][]string{
		{"job", capture.JobName, "--ignore-not-found", "--wait=false"},
		{"job", probe.JobName, "--ignore-not-found", "--wait=false"},
	}
	if !reflect.DeepEqual(cluster.deleted, want) {
		t.Errorf("deleted %v, want both workloads removed, most recent first", cluster.deleted)
	}
	if probe.State != workloadStateReleased || capture.State != workloadStateReleased {
		t.Errorf("states = %q/%q, want both %q", probe.State, capture.State, workloadStateReleased)
	}

	if err := backend.ReleaseTransientWorkloads(); err != nil {
		t.Fatalf("ReleaseTransientWorkloads() a second time error = %v", err)
	}
	if len(cluster.deleted) != 2 {
		t.Errorf("deleted %v, want exactly two deletes across two calls", cluster.deleted)
	}
}

// TestReleaseTransientWorkloadsAfterAFailedStart asserts the tracker does not
// re-delete a workload the creation path already released, and still reports
// nothing left to do.
func TestReleaseTransientWorkloadsAfterAFailedStart(t *testing.T) {
	backend := testBackend()
	cluster := &fakeCluster{uid: "u1", waitErr: errors.New("timed out waiting for the condition")}

	if _, err := backend.createTransientWorkloadWith(cluster.ops(), testCaptureSpec()); err == nil {
		t.Fatal("createTransientWorkloadWith() = nil error, want the readiness failure")
	}
	if len(cluster.deleted) != 1 {
		t.Fatalf("deleted %v, want the pod removed by the failure path", cluster.deleted)
	}

	if err := backend.ReleaseTransientWorkloads(); err != nil {
		t.Fatalf("ReleaseTransientWorkloads() error = %v", err)
	}
	if len(cluster.deleted) != 1 {
		t.Errorf("deleted %v, want no second delete of an already-released workload", cluster.deleted)
	}
}

// TestCreateTransientWorkloadNeverAdoptsAnotherRunsWorkload is the data-model
// invariant that a workload is adopted by exactly one run, at both points where
// adoption could happen: a service that already resolves to a pod in the
// deployment, and a name that is already taken.
func TestCreateTransientWorkloadNeverAdoptsAnotherRunsWorkload(t *testing.T) {
	t.Run("a service that already resolves to a pod this run did not create", func(t *testing.T) {
		for _, cached := range []string{"database-0", transientObjectPrefix + "-capture-99999999"} {
			backend := testBackend()
			backend.podCache[serviceNeo4j] = cached
			cluster := &fakeCluster{uid: "u1"}

			_, err := backend.createTransientWorkloadWith(cluster.ops(), testCaptureSpec())
			if err == nil {
				t.Fatalf("createTransientWorkloadWith() with %q cached = nil error, want a refusal", cached)
			}
			if !strings.Contains(err.Error(), cached) {
				t.Errorf("error = %q, want it to name the pod %q it refused to displace", err, cached)
			}
			if len(cluster.created) != 0 {
				t.Errorf("created %d objects, want none: the refusal comes before anything is built", len(cluster.created))
			}
			if got := backend.podCache[serviceNeo4j]; got != cached {
				t.Errorf("podCache[%s] = %q, want the existing entry left alone", serviceNeo4j, got)
			}
		}
	})

	t.Run("this run's own probe pod does not block its capture", func(t *testing.T) {
		// A run creates a probe and then a capture for the same service. The
		// probe used to be registered under that service's name, and the
		// refusal above had to carve out an exception for it; now nothing is
		// registered, so the two are independent and each answers for its own
		// pod.
		backend := testBackend()
		cluster := &fakeCluster{uid: "u1"}

		probe, err := backend.createTransientWorkloadWith(cluster.ops(), testProbeSpec())
		if err != nil {
			t.Fatalf("createTransientWorkloadWith(probe) error = %v", err)
		}

		capture, err := backend.createTransientWorkloadWith(cluster.ops(), testCaptureSpec())
		if err != nil {
			t.Fatalf("createTransientWorkloadWith(capture) error = %v, want the run's own probe not to block it", err)
		}

		probeTarget, err := probe.ExecutionTarget()
		if err != nil || probeTarget != probe.PodName {
			t.Errorf("probe.ExecutionTarget() = %q, %v, want %q, nil", probeTarget, err, probe.PodName)
		}
		captureTarget, err := capture.ExecutionTarget()
		if err != nil || captureTarget != capture.PodName {
			t.Errorf("capture.ExecutionTarget() = %q, %v, want %q, nil", captureTarget, err, capture.PodName)
		}
	})

	t.Run("a name already taken is refused rather than reused", func(t *testing.T) {
		backend := testBackend()
		cluster := &fakeCluster{
			uid:        "u1",
			createErrs: map[string]error{"Job": errors.New(`jobs "infrahub-backup-xdb-capture-a1b2c3d4" already exists`)},
		}

		_, err := backend.createTransientWorkloadWith(cluster.ops(), testCaptureSpec())
		if err == nil {
			t.Fatal("createTransientWorkloadWith() = nil error, want a refusal to reuse the existing pod")
		}
		if !strings.Contains(err.Error(), "belongs to another run") {
			t.Errorf("error = %q, want it to say the pod belongs to another run", err)
		}
		if len(cluster.deleted) != 0 {
			t.Errorf("deleted %v, want nothing: the pod is another run's to remove", cluster.deleted)
		}
		if _, registered := backend.podCache[serviceNeo4j]; registered {
			t.Error("another run's pod was written into the resolver's cache")
		}
	})
}

// TestExecTargetSurvivesTheCacheBeingCleared is the defect T084 removes, in the
// sequence that produced it: a restore calls StartServices by design, Start
// replaces the resolver's cache wholesale, and the transient workload's
// registration went with it — so the next copy resolved to nothing against a
// pod that was still ready and still running the capture.
//
// Naming the pod is not a second registration in a different map. It is the
// resolution the run already performed, carried by the operation instead of
// looked up again.
func TestExecTargetSurvivesTheCacheBeingCleared(t *testing.T) {
	backend := testBackend()
	cluster := &fakeCluster{uid: "u1"}

	workload, err := backend.createTransientWorkloadWith(cluster.ops(), testCaptureSpec())
	if err != nil {
		t.Fatalf("createTransientWorkloadWith() error = %v", err)
	}
	target, err := workload.ExecutionTarget()
	if err != nil {
		t.Fatalf("ExecutionTarget() error = %v", err)
	}

	// Exactly what Start and scaleServices do to the cache.
	backend.podCache = map[string]string{}

	args, err := backend.buildExecArgs(serviceNeo4j, []string{"neo4j-admin", "database", "dump"}, &ExecOptions{Pod: target})
	if err != nil {
		t.Fatalf("buildExecArgs() error = %v, want the named pod to need no resolution", err)
	}
	if !contains(args, target) {
		t.Errorf("exec args = %v, want them to name the workload's pod %q", args, target)
	}
}

// TestExecOptionsPodIsAdditive pins that naming a pod changes nothing for the
// callers that do not: every path that exists today resolves from the service,
// and the field is the zero value on all of them.
func TestExecOptionsPodIsAdditive(t *testing.T) {
	t.Run("the Kubernetes backend resolves from the service when no pod is named", func(t *testing.T) {
		backend := testBackend()
		backend.podCache[serviceNeo4j] = "infrahub-database-0"

		for _, opts := range []*ExecOptions{nil, {}, {User: "neo4j"}} {
			args, err := backend.buildExecArgs(serviceNeo4j, []string{"env"}, opts)
			if err != nil {
				t.Fatalf("buildExecArgs(%+v) error = %v", opts, err)
			}
			if !contains(args, "infrahub-database-0") {
				t.Errorf("exec args = %v for %+v, want the pod the service resolves to", args, opts)
			}
		}
	})

	t.Run("the Docker backend ignores a pod it has no way to address", func(t *testing.T) {
		// infrahub-collect and the backup tool share this backend, and external
		// databases are Kubernetes-only — so nothing sets the field here. This
		// pins that if something ever did, a Compose deployment would keep
		// addressing its service rather than growing a stray argument.
		docker := &DockerBackend{config: &Configuration{}, executor: NewCommandExecutor()}

		plain := docker.buildExecArgs("database", []string{"env"}, &ExecOptions{User: "neo4j"})
		withPod := docker.buildExecArgs("database", []string{"env"}, &ExecOptions{User: "neo4j", Pod: "infrahub-backup-xdb-capture-a1b2c3d4"})

		if !reflect.DeepEqual(plain, withPod) {
			t.Errorf("args with a pod named = %v, want them identical to %v", withPod, plain)
		}
	})
}

// TestTransientExclusionSurvivesARenamedPrefix is T074: FR-028's exclusion in
// the two name fallbacks must not rest on what the transient pod is called.
//
// TestTransientObjectNamesAvoidServiceSubstrings pins the naming convention,
// and while it holds the hazard below cannot fire — the first subtest proves
// that rather than assuming it. But that convention is a constant a later
// change may reasonably alter, and the paths reaching the client-side filter
// after a selector matched nothing go on to match a *name* against a service
// under the permissive app-service policy (unanchoredNamePolicyFor). So a
// prefix that ever came to contain an app service's name would turn a pod the
// name filter merely missed into a pod the fallback actively claims:
// stopAppContainers would read this run's own workload as `cache`'s status, and
// getPodForServiceWith would offer it as somewhere to exec. What holds then is
// the pod's own marker label, which cannot be spelled into the hazard.
func TestTransientExclusionSurvivesARenamedPrefix(t *testing.T) {
	t.Run("today's prefix names no stopped service, so the hazard is latent", func(t *testing.T) {
		name := transientObjectPrefix + "-neo4j-capture-a1b2c3d4"
		for _, service := range appServicesStoppedForBackup {
			if nameMatchesService(name, service) {
				t.Errorf("transientObjectPrefix %q yields the pod name %q, which the fallback claims as %q: the hazard T074 guards is live, not latent",
					transientObjectPrefix, name, service)
			}
		}
	})

	// The pod name a prefix containing an app service's name would produce. It
	// carries the marker every object transientPodSpec creates carries, and it
	// does not carry today's prefix — which is also what a stray left behind by
	// the version before such a rename looks like to the run that reaps it.
	renamed := labelledPod{Name: "infrahub-cache-xdb-neo4j-capture-a1b2c3d4", Phase: "Running", Transient: true}

	// The premise, asserted rather than assumed: without the marker this pod
	// reaches the fallback and is claimed there.
	if isTransientObjectName(renamed.Name) {
		t.Fatalf("pod %q carries today's prefix, so the subtests below would pass on the name filter alone", renamed.Name)
	}
	if !nameMatchesService(renamed.Name, "cache") {
		t.Fatalf("pod %q is not claimed as cache by name, so it does not reproduce T074's hazard", renamed.Name)
	}

	t.Run("the running check does not read its phase", func(t *testing.T) {
		run, _ := labelBlindCluster(renamed)
		statuses, err := newTestKubernetesBackend().getPodStatusesWith(run, "cache")
		if err != nil {
			t.Fatalf("getPodStatusesWith failed: %v", err)
		}
		if len(statuses) != 0 {
			t.Errorf("statuses = %v, want none: this run's own workload is not cache's status, and IsRunning answering true is what has stopAppContainers scale a service on the strength of it (FR-028)", statuses)
		}
	})

	t.Run("the singular resolver does not resolve to it", func(t *testing.T) {
		run, _ := labelBlindCluster(renamed)
		pod, err := newTestKubernetesBackend().getPodForServiceWith(run, "cache")
		if !errors.Is(err, errNoPodsMatched) {
			t.Fatalf("pod = %q, err = %v; want errNoPodsMatched: this run's own workload is not a replica of cache (FR-028)", pod, err)
		}
	})
}
