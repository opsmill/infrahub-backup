package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// podEnvListing renders what podEnvDeclarationsJSONPath returns for a pod, so
// the tests read the same shape the cluster produces.
func podEnvListing(defaultContainer string, lines ...string) string {
	return "A\t" + defaultContainer + "\n" + strings.Join(lines, "\n")
}

func neo4jDeploymentSource() deploymentCredentialSource {
	return deploymentCredentialSource{
		component: componentInfrahubServer,
		username:  []string{"INFRAHUB_DB_USERNAME"},
		password:  []string{"INFRAHUB_DB_PASSWORD"},
	}
}

// TestPodEnvDeclarationsJSONPathReadsNoValues pins the property that makes the
// pod-spec read safe: it asks where each variable comes from and never what it
// holds, so an inline credential in the deployment's spec never reaches this
// process or its debug log (FR-014).
func TestPodEnvDeclarationsJSONPathReadsNoValues(t *testing.T) {
	for _, forbidden := range []string{"{.value}", ".env[*].value", "{.data", "stringData"} {
		if strings.Contains(podEnvDeclarationsJSONPath, forbidden) {
			t.Errorf("podEnvDeclarationsJSONPath contains %q: it must read references only, never a value", forbidden)
		}
	}
	for _, want := range []string{"secretKeyRef.name", "secretKeyRef.key", "envFrom", "secretRef.name", `kubectl\.kubernetes\.io/default-container`} {
		if !strings.Contains(podEnvDeclarationsJSONPath, want) {
			t.Errorf("podEnvDeclarationsJSONPath = %q, want it to read %q", podEnvDeclarationsJSONPath, want)
		}
	}
}

// TestDeploymentCredentialRefs covers each way a deployment can declare the
// variables the credentials were read from, and which of them can be
// referenced from the transient container without this tool reading a Secret.
func TestDeploymentCredentialRefs(t *testing.T) {
	tests := []struct {
		name       string
		listing    string
		want       map[string]transientSecretKeyRef
		wantReason string
	}{
		{
			name: "env secretKeyRef maps onto the client's own variable names",
			listing: podEnvListing("",
				"C\tinfrahub-server",
				"E\tINFRAHUB_DB_ADDRESS\t\t\t",
				"E\tINFRAHUB_DB_USERNAME\tdb-auth\tusername\t",
				"E\tINFRAHUB_DB_PASSWORD\tdb-auth\tpassword\t"),
			want: map[string]transientSecretKeyRef{
				"NEO4J_USERNAME": {Name: "db-auth", Key: "username"},
				"NEO4J_PASSWORD": {Name: "db-auth", Key: "password"},
			},
		},
		{
			// envFrom names no keys and the kubelet skips a key the Secret
			// lacks, so a matching prefix does not show the Secret holds the
			// variable; a mandatory secretKeyRef to a missing key would block
			// the run, so the owned Secret is used instead.
			name: "a sole envFrom secretRef with no prefix falls back",
			listing: podEnvListing("",
				"C\tinfrahub-server",
				"F\t\tinfrahub-db\t",
				"F\tAPP_\tunrelated\t"),
			wantReason: "envFrom source",
		},
		{
			name: "a sole envFrom secretRef whose prefix the variable carries falls back",
			listing: podEnvListing("",
				"C\tinfrahub-server",
				"F\tINFRAHUB_DB_\tneo4j-auth\t"),
			wantReason: "envFrom source",
		},
		{
			name: "an inline value falls back",
			listing: podEnvListing("",
				"C\tinfrahub-server",
				"E\tINFRAHUB_DB_USERNAME\tdb-auth\tusername\t",
				"E\tINFRAHUB_DB_PASSWORD\t\t\t"),
			wantReason: "inline",
		},
		{
			name: "an env entry overrides envFrom, so an inline override falls back",
			listing: podEnvListing("",
				"C\tinfrahub-server",
				"E\tINFRAHUB_DB_USERNAME\t\t\t",
				"E\tINFRAHUB_DB_PASSWORD\t\t\t",
				"F\t\tinfrahub-db\t"),
			wantReason: "inline",
		},
		{
			name: "of two env entries with one name the later wins",
			listing: podEnvListing("",
				"C\tinfrahub-server",
				"E\tINFRAHUB_DB_USERNAME\tdb-auth\tusername\t",
				"E\tINFRAHUB_DB_PASSWORD\tdb-auth\tpassword\t",
				"E\tINFRAHUB_DB_PASSWORD\t\t\t"),
			wantReason: "inline",
		},
		{
			name: "a ConfigMap reference falls back",
			listing: podEnvListing("",
				"C\tinfrahub-server",
				"E\tINFRAHUB_DB_USERNAME\t\t\tsettings",
				"E\tINFRAHUB_DB_PASSWORD\tdb-auth\tpassword\t"),
			wantReason: "ConfigMap",
		},
		{
			name: "two envFrom sources that could supply the variable fall back",
			listing: podEnvListing("",
				"C\tinfrahub-server",
				"F\t\tfirst\t",
				"F\t\tsecond\t"),
			wantReason: "envFrom source",
		},
		{
			name: "an envFrom Secret beside an envFrom ConfigMap falls back",
			listing: podEnvListing("",
				"C\tinfrahub-server",
				"F\t\tinfrahub-db\t",
				"F\t\t\tsettings"),
			wantReason: "envFrom source",
		},
		{
			name: "a variable the spec does not declare falls back",
			listing: podEnvListing("",
				"C\tinfrahub-server",
				"E\tINFRAHUB_DB_ADDRESS\t\t\t"),
			wantReason: "does not declare",
		},
		{
			name: "the default-container annotation names the container exec reads",
			listing: podEnvListing("server",
				"C\tsidecar",
				"E\tINFRAHUB_DB_USERNAME\t\t\t",
				"E\tINFRAHUB_DB_PASSWORD\t\t\t",
				"C\tserver",
				"E\tINFRAHUB_DB_USERNAME\tdb-auth\tusername\t",
				"E\tINFRAHUB_DB_PASSWORD\tdb-auth\tpassword\t"),
			want: map[string]transientSecretKeyRef{
				"NEO4J_USERNAME": {Name: "db-auth", Key: "username"},
				"NEO4J_PASSWORD": {Name: "db-auth", Key: "password"},
			},
		},
		{
			name: "without the annotation the first container is the one exec reads",
			listing: podEnvListing("",
				"C\tserver",
				"E\tINFRAHUB_DB_USERNAME\t\t\t",
				"E\tINFRAHUB_DB_PASSWORD\t\t\t",
				"C\tsidecar",
				"E\tINFRAHUB_DB_USERNAME\tdb-auth\tusername\t",
				"E\tINFRAHUB_DB_PASSWORD\tdb-auth\tpassword\t"),
			wantReason: "inline",
		},
		{
			name:       "a pod with no containers falls back",
			listing:    podEnvListing(""),
			wantReason: "no containers",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs, reason := deploymentCredentialRefs(serviceNeo4j, neo4jDeploymentSource(), parsePodEnvDeclarations(tt.listing))
			if !reflect.DeepEqual(refs, tt.want) {
				t.Errorf("deploymentCredentialRefs() = %v (%s), want %v", refs, reason, tt.want)
			}
			if tt.wantReason != "" && !strings.Contains(reason, tt.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", reason, tt.wantReason)
			}
		})
	}
}

// TestApplyNeo4jEnvironmentRecordsProvenance asserts the deployment is
// recorded as the credentials' source only when this read supplied both.
func TestApplyNeo4jEnvironmentRecordsProvenance(t *testing.T) {
	dump := "INFRAHUB_DB_USERNAME=neo4j\nINFRAHUB_DB_PASSWORD=from-deployment\n"

	t.Run("both read from the deployment", func(t *testing.T) {
		iops := NewInfrahubOps()
		iops.applyNeo4jEnvironment(dump)

		if got, ok := iops.deploymentCredentials[serviceNeo4j]; !ok || !reflect.DeepEqual(got, neo4jDeploymentSource()) {
			t.Errorf("deploymentCredentials[neo4j] = %#v, %v, want the infrahub-server source", got, ok)
		}
	})

	t.Run("the operator's environment wins and nothing is recorded", func(t *testing.T) {
		t.Setenv("INFRAHUB_DB_USERNAME", "operator")
		t.Setenv("INFRAHUB_DB_PASSWORD", "operator-pw")

		iops := NewInfrahubOps()
		iops.loadCredentialsFromEnvironment()
		iops.applyNeo4jEnvironment(dump)

		if _, ok := iops.deploymentCredentials[serviceNeo4j]; ok {
			t.Error("deploymentCredentials[neo4j] recorded for credentials the operator supplied")
		}
		if iops.config.Neo4jPassword != "operator-pw" {
			t.Errorf("Neo4jPassword = %q, want the operator's", iops.config.Neo4jPassword)
		}
	})

	t.Run("one from the operator and one from the deployment is not a pair the deployment holds", func(t *testing.T) {
		t.Setenv("INFRAHUB_DB_USERNAME", "")
		t.Setenv("INFRAHUB_DB_PASSWORD", "operator-pw")

		iops := NewInfrahubOps()
		iops.loadCredentialsFromEnvironment()
		iops.applyNeo4jEnvironment(dump)

		if _, ok := iops.deploymentCredentials[serviceNeo4j]; ok {
			t.Error("deploymentCredentials[neo4j] recorded for a mixed pair")
		}
	})
}

// TestApplyPostgresEnvironmentRecordsProvenance covers the PostgreSQL finding:
// the credentials arrive inside the connection URL, which no client can be
// handed a username and a password from, so reuse needs variables beside the
// URL that held the same values — what prefect-server renders when its chart
// takes the user and password from Secret keys.
func TestApplyPostgresEnvironmentRecordsProvenance(t *testing.T) {
	t.Run("component variables beside the URL are recorded, the chart's own names first", func(t *testing.T) {
		iops := NewInfrahubOps()
		iops.applyPostgresEnvironment(strings.Join([]string{
			"PREFECT_SERVER_DATABASE_CONNECTION_URL=postgresql+asyncpg://prefect:pg-secret@db.example:5432/prefect",
			"PREFECT_SERVER_DATABASE_USER=prefect",
			"PREFECT_SERVER_DATABASE_PASSWORD=pg-secret",
			"A_COPY_OF_THE_PASSWORD=pg-secret",
		}, "\n"))

		got, ok := iops.deploymentCredentials[serviceTaskManagerDB]
		if !ok {
			t.Fatal("deploymentCredentials[task-manager-db] not recorded")
		}
		want := deploymentCredentialSource{
			component: componentTaskManager,
			username:  []string{"PREFECT_SERVER_DATABASE_USER"},
			password:  []string{"PREFECT_SERVER_DATABASE_PASSWORD", "A_COPY_OF_THE_PASSWORD"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("deploymentCredentials[task-manager-db] = %#v, want %#v", got, want)
		}

		listing := podEnvListing("",
			"C\tprefect-server",
			"E\tPREFECT_SERVER_DATABASE_USER\tpg-app\tusername\t",
			"E\tPREFECT_SERVER_DATABASE_PASSWORD\tpg-app\tpassword\t",
			"E\tPREFECT_SERVER_DATABASE_CONNECTION_URL\t\t\t")
		refs, reason := deploymentCredentialRefs(serviceTaskManagerDB, got, parsePodEnvDeclarations(listing))
		wantRefs := map[string]transientSecretKeyRef{
			"PGUSER":     {Name: "pg-app", Key: "username"},
			"PGPASSWORD": {Name: "pg-app", Key: "password"},
		}
		if !reflect.DeepEqual(refs, wantRefs) {
			t.Errorf("deploymentCredentialRefs() = %v (%s), want %v", refs, reason, wantRefs)
		}
	})

	t.Run("a URL alone, as the default chart renders it, records nothing", func(t *testing.T) {
		iops := NewInfrahubOps()
		iops.applyPostgresEnvironment("PREFECT_API_DATABASE_CONNECTION_URL=postgresql+asyncpg://prefect:pg-secret@db.example:5432/prefect\n")

		if _, ok := iops.deploymentCredentials[serviceTaskManagerDB]; ok {
			t.Error("deploymentCredentials[task-manager-db] recorded with nothing beside the URL to reference")
		}
		if iops.config.PostgresPassword != "pg-secret" {
			t.Errorf("PostgresPassword = %q, want the URL's: the credentials themselves still resolve", iops.config.PostgresPassword)
		}
	})

	t.Run("a partial operator URL is replaced by the deployment's pair, which is recorded", func(t *testing.T) {
		iops := NewInfrahubOps()
		iops.applyPrefectConnection("postgresql://operator@db.example:5432/prefect")
		iops.applyPostgresEnvironment("PREFECT_SERVER_DATABASE_CONNECTION_URL=postgresql://prefect:pg-secret@db.example/prefect\nPREFECT_SERVER_DATABASE_USER=prefect\nPREFECT_SERVER_DATABASE_PASSWORD=pg-secret\n")

		if iops.config.PostgresUsername != "prefect" || iops.config.PostgresPassword != "pg-secret" {
			t.Fatalf("credentials = %q/%q, want the deployment's prefect/pg-secret", iops.config.PostgresUsername, iops.config.PostgresPassword)
		}
		got, ok := iops.deploymentCredentials[serviceTaskManagerDB]
		if !ok {
			t.Fatal("deploymentCredentials[task-manager-db] not recorded although the deployment supplied both values in use")
		}
		want := deploymentCredentialSource{
			component: componentTaskManager,
			username:  []string{"PREFECT_SERVER_DATABASE_USER"},
			password:  []string{"PREFECT_SERVER_DATABASE_PASSWORD"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("deploymentCredentials[task-manager-db] = %#v, want %#v", got, want)
		}
	})

	t.Run("a deployment URL without a password keeps the operator's and records nothing", func(t *testing.T) {
		t.Setenv("PGPASSWORD", "")
		t.Setenv("PGPASSFILE", filepath.Join(t.TempDir(), "absent"))
		iops := NewInfrahubOps()
		iops.applyPrefectConnection("postgresql://operator:operator-pw@db.example:5432/prefect")
		iops.applyPostgresEnvironment("PREFECT_SERVER_DATABASE_CONNECTION_URL=postgresql://prefect@db.example/prefect\nPREFECT_SERVER_DATABASE_USER=prefect\nPREFECT_SERVER_DATABASE_PASSWORD=operator-pw\n")

		if iops.config.PostgresPassword != "operator-pw" {
			t.Fatalf("PostgresPassword = %q, want the operator's", iops.config.PostgresPassword)
		}
		if _, ok := iops.deploymentCredentials[serviceTaskManagerDB]; ok {
			t.Error("deploymentCredentials[task-manager-db] recorded for a pair mixing operator and deployment values")
		}
	})
}

// TestResolveDeploymentCredentialRefs covers the decision and its memory:
// operator credentials never read the pod, a read pod is decided once, and a
// failed read is not mistaken for a decision.
func TestResolveDeploymentCredentialRefs(t *testing.T) {
	secretBacked := podEnvListing("",
		"C\tinfrahub-server",
		"E\tINFRAHUB_DB_USERNAME\tdb-auth\tusername\t",
		"E\tINFRAHUB_DB_PASSWORD\tdb-auth\tpassword\t")

	t.Run("operator credentials fall back without reading the pod", func(t *testing.T) {
		iops := NewInfrahubOps()
		refs := iops.resolveDeploymentCredentialRefs(serviceNeo4j, func(string) (string, error) {
			t.Fatal("the pod spec was read for credentials the deployment did not supply")

			return "", nil
		})
		if refs != nil {
			t.Errorf("refs = %v, want nil: the operator's credentials are carried in an owned Secret", refs)
		}
	})

	t.Run("a decision is remembered", func(t *testing.T) {
		iops := NewInfrahubOps()
		iops.recordDeploymentCredentials(serviceNeo4j, neo4jDeploymentSource())

		reads := 0
		read := func(component string) (string, error) {
			reads++
			if component != componentInfrahubServer {
				t.Errorf("read the %s pod, want %s", component, componentInfrahubServer)
			}

			return secretBacked, nil
		}

		first := iops.resolveDeploymentCredentialRefs(serviceNeo4j, read)
		second := iops.resolveDeploymentCredentialRefs(serviceNeo4j, read)
		if len(first) != 2 || !reflect.DeepEqual(first, second) {
			t.Errorf("refs = %v then %v, want the same two references both times", first, second)
		}
		if reads != 1 {
			t.Errorf("read the pod %d times, want once: the component may be scaled to zero by the second workload", reads)
		}
	})

	t.Run("a failed read is asked again", func(t *testing.T) {
		iops := NewInfrahubOps()
		iops.recordDeploymentCredentials(serviceNeo4j, neo4jDeploymentSource())

		failing := func(string) (string, error) { return "", errors.New("connection refused") }
		if refs := iops.resolveDeploymentCredentialRefs(serviceNeo4j, failing); refs != nil {
			t.Errorf("refs = %v after a failed read, want nil", refs)
		}

		refs := iops.resolveDeploymentCredentialRefs(serviceNeo4j, func(string) (string, error) { return secretBacked, nil })
		if len(refs) != 2 {
			t.Errorf("refs = %v, want the references once the pod answered", refs)
		}
	})
}

func testReusingSpec() transientWorkloadSpec {
	spec := testCaptureSpec()
	spec.Credentials = nil
	spec.CredentialRefs = map[string]transientSecretKeyRef{
		"NEO4J_USERNAME": {Name: "db-auth", Key: "username"},
		"NEO4J_PASSWORD": {Name: "db-auth", Key: "password"},
	}

	return spec
}

// TestReusingWorkloadCreatesNoSecret is the reviewer's request asserted: a
// workload whose credentials the deployment already keeps in a Secret
// references that Secret, so the run creates a Job and nothing else, and no
// credential value appears in what it sends to the cluster.
func TestReusingWorkloadCreatesNoSecret(t *testing.T) {
	spec := testReusingSpec()

	manifest, err := buildTransientWorkloadManifest(spec)
	if err != nil {
		t.Fatalf("buildTransientWorkloadManifest() error = %v", err)
	}

	var job transientJobManifest
	if err := json.Unmarshal(manifest, &job); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	container := job.Spec.Template.Spec.Containers[0]
	if len(container.EnvFrom) != 0 {
		t.Errorf("envFrom = %#v, want none: there is no owned Secret to read", container.EnvFrom)
	}
	want := []transientEnvVar{
		{Name: "NEO4J_PASSWORD", ValueFrom: &transientEnvVarSource{SecretKeyRef: &transientSecretKeySelector{Name: "db-auth", Key: "password", Optional: boolPtr(false)}}},
		{Name: "NEO4J_USERNAME", ValueFrom: &transientEnvVarSource{SecretKeyRef: &transientSecretKeySelector{Name: "db-auth", Key: "username", Optional: boolPtr(false)}}},
	}
	if !reflect.DeepEqual(container.Env, want) {
		t.Errorf("env = %s, want secretKeyRefs onto the deployment's Secret, not optional", mustJSON(t, container.Env))
	}

	backend := testBackend()
	cluster := &fakeCluster{uid: "8f14e45f-ea3b-4b0e-9d3e-2a1c5f6b7d80"}
	workload, err := backend.createTransientWorkloadWith(cluster.ops(), spec)
	if err != nil {
		t.Fatalf("createTransientWorkloadWith() error = %v", err)
	}
	if got := cluster.createdKinds(); !reflect.DeepEqual(got, []string{"Job"}) {
		t.Errorf("created %v, want the Job alone: a reusing workload needs no secrets verb", got)
	}
	if workload.SecretName != "" {
		t.Errorf("SecretName = %q, want empty: the workload owns no Secret", workload.SecretName)
	}
	for _, created := range cluster.created {
		for _, value := range []string{"s3cr3t-pw", "stringData"} {
			if strings.Contains(string(created), value) {
				t.Errorf("a created manifest carries %q: a reusing workload sends no credential value", value)
			}
		}
	}
}

// TestOwnedSecretWorkloadIsUnchanged pins the fallback: a spec carrying values
// still gets its envFrom of the owned Secret and no secretKeyRef.
func TestOwnedSecretWorkloadIsUnchanged(t *testing.T) {
	manifest, err := buildTransientWorkloadManifest(testCaptureSpec())
	if err != nil {
		t.Fatalf("buildTransientWorkloadManifest() error = %v", err)
	}

	var job transientJobManifest
	if err := json.Unmarshal(manifest, &job); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	container := job.Spec.Template.Spec.Containers[0]
	if len(container.Env) != 0 {
		t.Errorf("env = %#v, want none on the owned-Secret path", container.Env)
	}
	if len(container.EnvFrom) != 1 || container.EnvFrom[0].SecretRef.Name != testCaptureSpec().secretName() {
		t.Errorf("envFrom = %#v, want the owned Secret", container.EnvFrom)
	}
}

// TestReusingSpecRefusals covers the guards that keep a reusing spec from
// shipping a value or creating the Secret it exists to avoid.
func TestReusingSpecRefusals(t *testing.T) {
	both := testReusingSpec()
	both.Credentials = map[string]string{"NEO4J_USERNAME": "neo4j", "NEO4J_PASSWORD": "pw"}
	if _, err := buildTransientWorkloadManifest(both); err == nil {
		t.Error("buildTransientWorkloadManifest() with values and references = nil error, want a refusal")
	}

	missing := testReusingSpec()
	delete(missing.CredentialRefs, "NEO4J_PASSWORD")
	if _, err := buildTransientWorkloadManifest(missing); err == nil {
		t.Error("buildTransientWorkloadManifest() with no password reference = nil error, want a refusal")
	}

	if _, err := buildTransientSecretManifest(testReusingSpec(), "uid"); err == nil {
		t.Error("buildTransientSecretManifest() for a reusing spec = nil error, want a refusal")
	}
}

// TestReapToleratesForbiddenSecretListing is the other half of dropping the
// secrets verbs: a namespace that refuses the secrets listing still has its
// abandoned pods reaped, and the run is not failed over it.
func TestReapToleratesForbiddenSecretListing(t *testing.T) {
	old := reapNow.Add(-4 * time.Hour).UTC().Format(time.RFC3339)
	pods := fmt.Sprintf("%s-probe-cccc3333\tcccc3333\tFailed\t%s\t1200\n", transientObjectPrefix, old)

	t.Run("forbidden is a warning", func(t *testing.T) {
		backend := testBackend()
		cluster := &fakeReapCluster{
			pods:       pods,
			secretsErr: errors.New(`Error from server (Forbidden): secrets is forbidden: User "backup" cannot list resource "secrets" in API group "" in the namespace "infrahub"`),
		}

		if err := backend.reapTransientStraysWith(cluster.ops(), "aaaa1111", reapNow); err != nil {
			t.Fatalf("reapTransientStraysWith() error = %v, want nil when only the secrets listing is forbidden", err)
		}
		if got := cluster.deletedNames(); !reflect.DeepEqual(got, []string{"pod " + transientObjectPrefix + "-probe-cccc3333"}) {
			t.Errorf("deleted %v, want the abandoned pod still reaped", got)
		}
	})

	t.Run("any other secrets listing failure still stops the sweep", func(t *testing.T) {
		backend := testBackend()
		cluster := &fakeReapCluster{pods: pods, secretsErr: errors.New("connection refused")}

		if err := backend.reapTransientStraysWith(cluster.ops(), "aaaa1111", reapNow); err == nil {
			t.Fatal("reapTransientStraysWith() = nil, want an error for a listing that failed rather than was refused")
		}
		if len(cluster.deleted) != 0 {
			t.Errorf("deleted %v, want nothing: the listing was incomplete", cluster.deletedNames())
		}
	})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()

	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	return string(out)
}
