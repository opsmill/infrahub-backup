package app

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/sirupsen/logrus"
)

// A transient workload has to authenticate to the database it stands in for,
// and FR-014 keeps the credentials off every command line and out of every
// inline value in a pod spec. Until this file, the only way to satisfy both was
// for this tool to write a Secret of its own — which made `create`, `list` and
// `delete` on secrets part of what a namespace had to grant before an external
// database could be backed up at all.
//
// Most deployments already hold those credentials in a Secret: the Infrahub
// server takes INFRAHUB_DB_USERNAME and INFRAHUB_DB_PASSWORD from one, and the
// prefect-server chart can take its database user and password from one
// (externalDatabase.usernameSecretKey / passwordSecretKey render
// PREFECT_SERVER_DATABASE_USER and PREFECT_SERVER_DATABASE_PASSWORD as
// secretKeyRefs). When the credentials this run is about to use are exactly
// the ones such a Secret holds, the Job can reference the same Secret key with
// `env[].valueFrom.secretKeyRef`. The kubelet resolves that reference, not this
// tool, so no Secret is created and no secrets verb is needed. A reference is
// not the value, so FR-014 holds unchanged.
//
// Reuse rests on evidence the run already has, never on a guess:
//
//   - the credential came from the deployment component's own environment, not
//     from the operator's environment and not from a default — an operator who
//     supplied credentials asked for those, and the Secret may hold others;
//   - the component's running pod declares the variable that carried it as a
//     Secret key (pods `get`, which the run already needs);
//   - for PostgreSQL, whose credentials arrive inside the Prefect connection
//     URL, a Secret-backed variable on the task-manager pod held exactly the
//     user or the password the URL carried when the environment was read.
//
// Anything short of that — an inline value, a ConfigMap, a variable reached
// only through envFrom (which names no keys), a pod that cannot be
// read — falls back to the owned Secret, which is the behaviour every run had
// before this file existed.

// deploymentCredentialSource records where in the deployment the credentials a
// run holds for one database were read from: the component whose environment
// supplied them, and the variables in that environment which held each value.
//
// It is recorded only when *both* the username and the password came from the
// deployment in the same read. One from the operator and one from the
// deployment is a pair the deployment never held together, so no Secret of its
// can stand in for it.
type deploymentCredentialSource struct {
	component string
	// username and password are variable names in the component's
	// environment, in preference order. Each held the value the run now uses
	// at the moment the environment was read.
	username []string
	password []string
}

// transientSecretKeyRef is one key of a Secret the deployment already has.
// Optional is never carried: the transient container always states it false,
// so a key that has gone missing since the deployment's pod started holds the
// container in a visible pending state rather than starting it unauthenticated.
type transientSecretKeyRef struct {
	Name string
	Key  string
}

// prefectCredentialEnvVars are the variables prefect-server renders the
// database user and password into when the chart takes them from a Secret.
// They are tried before any other variable that happens to hold the same
// value, because they are the ones that mean it.
var (
	prefectUserEnvVars     = []string{"PREFECT_SERVER_DATABASE_USER"}
	prefectPasswordEnvVars = []string{"PREFECT_SERVER_DATABASE_PASSWORD"}
)

// transientCredentialNames is the pair of variables the database's own client
// reads its username and password from inside the transient container.
func transientCredentialNames(service string) (username, password string, err error) {
	switch service {
	case serviceNeo4j:
		return "NEO4J_USERNAME", "NEO4J_PASSWORD", nil
	case serviceTaskManagerDB:
		return "PGUSER", "PGPASSWORD", nil
	default:
		return "", "", errNotADatabaseService("resolve credentials for", service)
	}
}

// recordDeploymentCredentials remembers that the credentials for service came
// from the deployment, and where. A source with no candidate variable for
// either value is not recorded: nothing could be mapped from it.
func (iops *InfrahubOps) recordDeploymentCredentials(service string, source deploymentCredentialSource) {
	if len(source.username) == 0 || len(source.password) == 0 {
		return
	}

	if iops.deploymentCredentials == nil {
		iops.deploymentCredentials = map[string]deploymentCredentialSource{}
	}
	iops.deploymentCredentials[service] = source
}

// envNamesHolding lists the variables in an `env` dump whose value is exactly
// value: the preferred names first, in their order, then every other match in
// name order so the choice does not depend on the order the container printed
// its environment. The Prefect connection URLs are never candidates — they hold
// the whole URL, which is never equal to a user or a password, and naming them
// here would only invite reading one as if it were.
func envNamesHolding(env map[string]string, value string, preferred []string) []string {
	if value == "" {
		return nil
	}

	names := []string{}
	for _, name := range preferred {
		if env[name] == value {
			names = append(names, name)
		}
	}

	others := []string{}
	for name, held := range env {
		if held != value || slices.Contains(preferred, name) || slices.Contains(prefectConnectionEnvVars, name) {
			continue
		}
		others = append(others, name)
	}
	sort.Strings(others)

	return append(names, others...)
}

// ---------------------------------------------------------------------------
// Reading what a running pod declares about its environment
// ---------------------------------------------------------------------------

// podDefaultContainerAnnotation names the container `kubectl exec` enters when
// no -c is given. The environment the credentials were read from is that
// container's, because the read was an exec with no -c.
const podDefaultContainerAnnotation = "kubectl.kubernetes.io/default-container"

// podEnvDeclarationsJSONPath reads where each variable of each container comes
// from, and never what it holds.
//
// It is a jsonpath rather than `-o json` on purpose. A pod spec may carry a
// credential as an inline value — the case this file exists to fall back from
// — and `-o json` would hand that value to this process and to anything that
// logs a command's output. The jsonpath asks for names, Secret and ConfigMap
// references and field paths only, so an inline value never leaves the API
// server. Its presence is still detectable: an `E` line with no reference of
// any kind is a variable whose value the spec states inline.
//
// Every line carries a leading tag because the runner trims its output, and an
// empty annotation on an untagged first line would be trimmed away and shift
// every line after it.
var podEnvDeclarationsJSONPath = `jsonpath=` +
	`A{"\t"}{.metadata.annotations.` + escapeJSONPathKey(podDefaultContainerAnnotation) + `}{"\n"}` +
	`{range .spec.containers[*]}` +
	`C{"\t"}{.name}{"\n"}` +
	`{range .env[*]}E{"\t"}{.name}{"\t"}{.valueFrom.secretKeyRef.name}{"\t"}{.valueFrom.secretKeyRef.key}{"\t"}` +
	`{.valueFrom.configMapKeyRef.name}{.valueFrom.fieldRef.fieldPath}{.valueFrom.resourceFieldRef.resource}{"\n"}{end}` +
	`{range .envFrom[*]}F{"\t"}{.prefix}{"\t"}{.secretRef.name}{"\t"}{.configMapRef.name}{"\n"}{end}` +
	`{end}`

// podEnvVar is one `env` entry: the Secret key it comes from, or the fact that
// it comes from something else (a ConfigMap, a field of the pod) — or, with
// neither, an inline value.
type podEnvVar struct {
	name       string
	secretName string
	secretKey  string
	other      bool
}

// podEnvFrom is one `envFrom` entry.
type podEnvFrom struct {
	prefix        string
	secretName    string
	configMapName string
}

type podContainerEnv struct {
	name    string
	env     []podEnvVar
	envFrom []podEnvFrom
}

type podEnvDeclarations struct {
	defaultContainer string
	containers       []podContainerEnv
}

// parsePodEnvDeclarations reads podEnvDeclarationsJSONPath's output. A line it
// does not recognise is skipped rather than guessed at; the worst that can do
// is make a variable look undeclared, which falls back.
func parsePodEnvDeclarations(output string) podEnvDeclarations {
	declarations := podEnvDeclarations{}

	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		field := func(i int) string {
			if i < len(fields) {
				return strings.TrimSpace(fields[i])
			}

			return ""
		}

		switch field(0) {
		case "A":
			declarations.defaultContainer = field(1)
		case "C":
			declarations.containers = append(declarations.containers, podContainerEnv{name: field(1)})
		case "E":
			if len(declarations.containers) == 0 || field(1) == "" {
				continue
			}
			current := &declarations.containers[len(declarations.containers)-1]
			current.env = append(current.env, podEnvVar{
				name:       field(1),
				secretName: field(2),
				secretKey:  field(3),
				other:      field(4) != "",
			})
		case "F":
			if len(declarations.containers) == 0 {
				continue
			}
			current := &declarations.containers[len(declarations.containers)-1]
			current.envFrom = append(current.envFrom, podEnvFrom{
				prefix:        field(1),
				secretName:    field(2),
				configMapName: field(3),
			})
		}
	}

	return declarations
}

// execContainer is the container an exec with no -c enters, which is the one
// whose environment the credentials were read from: the one the default-container
// annotation names, or the first.
func (d podEnvDeclarations) execContainer() (podContainerEnv, bool) {
	if d.defaultContainer != "" {
		for _, container := range d.containers {
			if container.name == d.defaultContainer {
				return container, true
			}
		}
	}

	if len(d.containers) == 0 {
		return podContainerEnv{}, false
	}

	return d.containers[0], true
}

// secretKeyFor reports which Secret key a variable in this container comes
// from, or why it cannot be shown to come from one.
//
// It follows the precedence the kubelet applies: an `env` entry overrides
// anything from `envFrom`, and of two `env` entries with one name the later
// wins. A variable reached only through `envFrom` is never attributed to a
// Secret. An `envFrom` source names no keys and the kubelet silently skips a
// key the Secret lacks, so a name that matches a source's prefix (an empty
// prefix matches every name) does not show that the Secret holds the key: the
// value may come from the image instead. A `secretKeyRef` to a missing key
// would hold the transient container pending and block the run, and checking
// that the key exists would mean reading the Secret, which is the permission
// this file exists to avoid needing. Such a variable falls back.
func (c podContainerEnv) secretKeyFor(variable string) (transientSecretKeyRef, string, bool) {
	for i := len(c.env) - 1; i >= 0; i-- {
		entry := c.env[i]
		if entry.name != variable {
			continue
		}

		switch {
		case entry.secretName != "" && entry.secretKey != "":
			return transientSecretKeyRef{Name: entry.secretName, Key: entry.secretKey}, "", true
		case entry.other:
			return transientSecretKeyRef{}, fmt.Sprintf("%s comes from a ConfigMap or a field of the pod, not a Secret", variable), false
		default:
			return transientSecretKeyRef{}, fmt.Sprintf("%s is stated inline in the pod spec", variable), false
		}
	}

	sources := 0
	for _, source := range c.envFrom {
		if !strings.HasPrefix(variable, source.prefix) || len(variable) == len(source.prefix) {
			continue
		}
		if source.secretName != "" || source.configMapName != "" {
			sources++
		}
	}

	if sources == 0 {
		return transientSecretKeyRef{}, fmt.Sprintf("the pod spec does not declare %s, so the image or its entrypoint sets it", variable), false
	}

	return transientSecretKeyRef{}, fmt.Sprintf("%s could come from %d envFrom source(s), which name no keys, so whether a Secret holds it would mean reading the Secret", variable, sources), false
}

// firstSecretKey is secretKeyFor over the candidate variables in order, the
// first that maps winning. The reason returned when none does is the first
// candidate's: it is the variable that means the credential, and the others
// merely held the same value.
func (c podContainerEnv) firstSecretKey(candidates []string) (transientSecretKeyRef, string, bool) {
	firstReason := ""
	for _, variable := range candidates {
		ref, reason, ok := c.secretKeyFor(variable)
		if ok {
			return ref, "", true
		}
		if firstReason == "" {
			firstReason = reason
		}
	}

	return transientSecretKeyRef{}, firstReason, false
}

// deploymentCredentialRefs maps the transient container's credential variables
// onto the Secret keys the deployment's pod declares, or reports why it cannot.
// It is pure over what was read, which is what lets every arm of the decision
// be exercised without a cluster.
func deploymentCredentialRefs(service string, source deploymentCredentialSource, declarations podEnvDeclarations) (map[string]transientSecretKeyRef, string) {
	userVar, passwordVar, err := transientCredentialNames(service)
	if err != nil {
		return nil, err.Error()
	}

	container, ok := declarations.execContainer()
	if !ok {
		return nil, fmt.Sprintf("the %s pod spec lists no containers", source.component)
	}

	user, reason, ok := container.firstSecretKey(source.username)
	if !ok {
		return nil, "the username: " + reason
	}

	password, reason, ok := container.firstSecretKey(source.password)
	if !ok {
		return nil, "the password: " + reason
	}

	return map[string]transientSecretKeyRef{userVar: user, passwordVar: password}, ""
}

// resolveDeploymentCredentialRefs decides, once per database per run, whether
// its transient workloads reference the deployment's own Secret or carry an
// owned one, and logs which — naming Secrets and keys, never a value.
//
// readPodEnv reads podEnvDeclarationsJSONPath off the component's running pod.
// A failed read is not remembered: it says the pod could not answer, not that
// the credentials are not Secret-backed, so a later workload asks again. A
// decision is remembered either way, which is also what keeps the answer
// available after a restore or an offline capture has scaled the component to
// zero — the gate creates the first workload while the deployment is up.
func (iops *InfrahubOps) resolveDeploymentCredentialRefs(service string, readPodEnv func(component string) (string, error)) map[string]transientSecretKeyRef {
	if refs, decided := iops.deploymentCredentialRefs[service]; decided {
		return refs
	}

	decide := func(refs map[string]transientSecretKeyRef) map[string]transientSecretKeyRef {
		if iops.deploymentCredentialRefs == nil {
			iops.deploymentCredentialRefs = map[string]map[string]transientSecretKeyRef{}
		}
		iops.deploymentCredentialRefs[service] = refs

		return refs
	}

	source, fromDeployment := iops.deploymentCredentials[service]
	if !fromDeployment {
		logrus.Infof("The %s credentials did not both come from the deployment (they were supplied by the operator or are defaults), so the transient workload carries them in a Secret of its own", service)

		return decide(nil)
	}

	output, err := readPodEnv(source.component)
	if err != nil {
		logrus.Infof("Could not read how the %s pod declares the %s credentials, so the transient workload carries them in a Secret of its own: %v", source.component, service, err)

		return nil
	}

	refs, reason := deploymentCredentialRefs(service, source, parsePodEnvDeclarations(output))
	if refs == nil {
		logrus.Infof("The %s credentials cannot be referenced from a Secret the deployment already has (%s), so the transient workload carries them in a Secret of its own", service, reason)

		return decide(nil)
	}

	described := make([]string, 0, len(refs))
	for variable, ref := range refs {
		described = append(described, fmt.Sprintf("%s from %s/%s", variable, ref.Name, ref.Key))
	}
	sort.Strings(described)
	logrus.Infof("The transient %s workloads reference the deployment's own Secret for their credentials (%s), so this run creates no Secret for them", service, strings.Join(described, ", "))

	return decide(refs)
}

// withDeploymentCredentialRefs points a spec at the deployment's own Secret
// when the run can show that Secret holds the credentials it would otherwise
// copy, and leaves the spec as it was otherwise. The credential values are
// dropped from a spec that references the Secret, so they cannot reach the
// manifest by any path.
func (iops *InfrahubOps) withDeploymentCredentialRefs(k *KubernetesBackend, spec transientWorkloadSpec) transientWorkloadSpec {
	refs := iops.resolveDeploymentCredentialRefs(spec.Service, k.readPodEnvDeclarations)
	if len(refs) == 0 {
		return spec
	}

	spec.CredentialRefs = refs
	spec.Credentials = nil

	return spec
}

// readPodEnvDeclarations reads where the variables of a component's running
// pod come from. It is a `get` on a pod — a verb every run on this path already
// holds — and it is bounded on the control bound, being a call to the API
// server rather than to the database (FR-025).
func (k *KubernetesBackend) readPodEnvDeclarations(component string) (string, error) {
	bound := externalDBControlBound(k.config)
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()

	pod, err := k.getPodForServiceContext(ctx, bound, component)
	if err != nil {
		return "", err
	}

	run := k.externalDBRunner(bound, externalDBTarget("the "+component+" pod", k.namespace))

	return run("kubectl", "get", "pod", pod, "-n", k.namespace, "-o", podEnvDeclarationsJSONPath)
}
