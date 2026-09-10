package app

import (
	"cmp"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/sirupsen/logrus"
)

// A database that lives outside the deployment has no container to resolve, so
// everything the run needs to reach it has to come from somewhere else: the
// deployment's own components describe most of it, the operator overrides the
// rest, and one detail — the backup listener's port — is not recorded anywhere
// at all. This file is where those three sources become one endpoint, and where
// that endpoint is reported before anything is attempted against it.

// The documented deployment service names an endpoint may stand for. FR-017
// forbids introducing a new assumed service name, so an external database
// resolves under the same name its container would have had.
const (
	serviceNeo4j         = "database"
	serviceTaskManagerDB = "task-manager-db"
)

// errNotADatabaseService refuses a service this feature has no database
// meaning for, naming the two that it does. action completes "cannot …".
//
// It is one function because the vocabulary sentence was written out at every
// site that guards on the pair, so a third database meant finding all of them.
func errNotADatabaseService(action, service string) error {
	return fmt.Errorf("cannot %s %q: the databases are %s and %s", action, service, serviceNeo4j, serviceTaskManagerDB)
}

// Infrahub's own defaults for the database settings discovery reads. They are
// repeated here rather than assumed present in a component's environment,
// because a deployment that leaves a setting at its default does not export it
// at all — so "absent" and "default" are the same observation.
const (
	// defaultNeo4jClientPort is the default of Infrahub's INFRAHUB_DB_PORT: the
	// Bolt port the application talks to, used here for the version probe and
	// for restore. It is emphatically not the backup listener's port
	// (defaultNeo4jBackupPort) — different listener, different purpose.
	defaultNeo4jClientPort = 7687

	// defaultNeo4jProtocol is the default of Infrahub's INFRAHUB_DB_PROTOCOL.
	// The routed neo4j:// form is what a multi-member deployment needs; bolt://
	// addresses one server.
	defaultNeo4jProtocol = "bolt"

	// defaultPostgresPort is the port a PostgreSQL connection URL means when it
	// carries none of its own.
	defaultPostgresPort = 5432
)

// EndpointLocation says whether a logical database lives inside the deployment
// or outside it. Both values are stated explicitly and neither is the zero
// value, so a failure to query the deployment cannot pass for "external":
// FR-002 requires that failure to surface as an error rather than as evidence
// the database is elsewhere.
type EndpointLocation string

const (
	EndpointLocationInternal EndpointLocation = "internal"
	EndpointLocationExternal EndpointLocation = "external"
)

// HostPort is one member of a database's address list, with the port it will
// actually be reached on — either the one the member carried or the client port
// it inherited.
type HostPort struct {
	Host string
	Port int
}

// String renders the member the way a client expects it, bracketing an IPv6
// literal so that host and port stay tellable apart.
func (hp HostPort) String() string {
	return net.JoinHostPort(hp.Host, strconv.Itoa(hp.Port))
}

// DatabaseEndpoint is where one logical database actually lives: resolved once
// per run per database, and thereafter the single source of truth for both the
// backup and the restore path.
//
// It deliberately carries no credential material. Credentials keep the
// resolution they already have on Configuration and reach a transient workload
// through a secret it owns, so there is no path by which reporting an endpoint
// can print a password (FR-014).
type DatabaseEndpoint struct {
	// Service is the documented deployment service name this endpoint stands
	// for: serviceNeo4j or serviceTaskManagerDB, never a new name (FR-017).
	Service string

	// Location is internal or external, positively established (FR-002).
	Location EndpointLocation

	// Hosts is the address list in the order it will be tried — which is the
	// order the deployment or the operator wrote it in, because that order is
	// what the backup command consumes (FR-005).
	Hosts []HostPort

	// ClientPort is the port of the application protocol: Bolt for Neo4j, the
	// wire protocol for PostgreSQL. A member of Hosts written without a port of
	// its own inherits it. Zero means the deployment records none, in which case
	// Infrahub's own default applies.
	ClientPort int

	// BackupPort is the port the Neo4j backup service listens on. Unused for
	// PostgreSQL, whose dump travels over the client port.
	BackupPort int

	// Protocol is bolt or neo4j, from INFRAHUB_DB_PROTOCOL. Empty for
	// PostgreSQL, which has no such setting.
	Protocol string

	// TLS describes the client connection's TLS as the deployment's own
	// INFRAHUB_DB_TLS_* settings state it.
	TLS ExternalDBTLS

	// Database names the database inside the server. It is carried rather than
	// assumed: it names the entry in the artifact, and a cross-name restore is a
	// trap this codebase has already had to fix once.
	Database string

	// reported records that the endpoint has been logged, so that resolution
	// reporting it and a connection path re-asserting it do not put the same
	// line in front of the operator twice.
	reported bool
}

// resolveHosts fills the endpoint's Hosts from a comma-separated host[:port]
// list, applying the endpoint's client port to every member that carries no
// port of its own.
func (e *DatabaseEndpoint) resolveHosts(address string) error {
	hosts, err := parseHostPortList(address, e.effectiveClientPort())
	if err != nil {
		return fmt.Errorf("failed to resolve the %s endpoint: %w", e.Service, err)
	}

	e.Hosts = hosts

	return nil
}

// effectiveClientPort is the client port the run will actually use: the one the
// deployment records, or Infrahub's own default for the service when it records
// none.
func (e DatabaseEndpoint) effectiveClientPort() int {
	if e.ClientPort > 0 {
		return e.ClientPort
	}

	if e.Service == serviceTaskManagerDB {
		return defaultPostgresPort
	}

	return defaultNeo4jClientPort
}

// effectiveProtocol is the protocol the run will actually use. PostgreSQL has no
// equivalent setting, so it reports none rather than inventing one.
func (e DatabaseEndpoint) effectiveProtocol() string {
	if e.Protocol != "" {
		return e.Protocol
	}

	if e.Service == serviceNeo4j {
		return defaultNeo4jProtocol
	}

	return ""
}

// Report logs the endpoint the run resolved. Every path that opens a connection
// to an endpoint MUST call this before opening it, so that the address a run is
// about to talk to reaches the operator's terminal ahead of the attempt
// (FR-003): when a connection then hangs or is refused, the line naming the
// endpoint is already there instead of being lost with the failure. It is
// idempotent, which is what makes calling it defensively from a connection path
// free even when resolution has already reported.
func (e *DatabaseEndpoint) Report() {
	if e.reported {
		return
	}
	e.reported = true

	logrus.Infof("Resolved %s endpoint: %s", e.Service, e.Describe())
}

// Describe renders the endpoint in one line, for the report above and for a
// failure message that has to name the resource it failed on. It states the
// values the run will use rather than the ones it read, so a setting left at its
// default still appears; it names no credential — see the type's own note.
func (e DatabaseEndpoint) Describe() string {
	hosts := cmp.Or(renderHostPorts(e.Hosts), "none")

	fields := []string{
		fmt.Sprintf("location=%s", e.Location),
		fmt.Sprintf("hosts=%s", hosts),
	}
	if protocol := e.effectiveProtocol(); protocol != "" {
		fields = append(fields, "protocol="+protocol)
	}
	fields = append(fields, fmt.Sprintf("client-port=%d", e.effectiveClientPort()))
	if e.BackupPort > 0 {
		fields = append(fields, fmt.Sprintf("backup-port=%d", e.BackupPort))
	}
	if e.Database != "" {
		fields = append(fields, "database="+e.Database)
	}
	fields = append(fields, "tls="+describeTLS(e.TLS))

	return strings.Join(fields, " ")
}

// describeTLS renders the client connection's TLS settings for the endpoint
// report. It reports tls_insecure because that is what the deployment says, not
// because this tool takes it as licence to skip verification for the connections
// it opens (FR-029).
func describeTLS(tls ExternalDBTLS) string {
	if !tls.Enabled {
		return "disabled"
	}

	description := "enabled"
	if tls.CAFile != "" {
		description += ",ca=" + tls.CAFile
	}
	if tls.Insecure {
		description += ",deployment-insecure"
	}

	return description
}

// resolveNeo4jBackupPort is the port the backup service will be reached on: the
// operator's --neo4j-backup-port when they set one, and defaultNeo4jBackupPort
// otherwise.
//
// This is the one endpoint detail discovery cannot supply at all. Infrahub never
// connects to the backup listener, so no setting in the deployment records its
// port and there is nothing to read (research R4) — which also means the default
// here is the database vendor's rather than the deployment's, and a server whose
// backup listener was configured onto another port needs the flag.
func resolveNeo4jBackupPort(cfg ExternalDBConfig) int {
	if cfg.Neo4jBackupPort > 0 {
		return cfg.Neo4jBackupPort
	}

	return defaultNeo4jBackupPort
}

// discoveredEndpoint is what the deployment's own components say about where a
// database lives: INFRAHUB_DB_ADDRESS and INFRAHUB_DB_PORT for Neo4j, the host
// and port of the Prefect connection URL for PostgreSQL.
//
// It is kept apart from ExternalDBConfig on purpose. That struct carries the
// operator's overrides, and an override has to survive discovery rather than be
// overwritten by it, so the two are recorded separately and reconciled when the
// endpoint is resolved.
type discoveredEndpoint struct {
	// Address is a comma-separated host[:port] list for Neo4j, and a single host
	// for PostgreSQL. Empty when the deployment exports no address.
	Address string

	// ClientPort is the port members of Address inherit when they carry none.
	// Zero when the deployment exports no port.
	ClientPort int

	// read records that the deployment's own configuration for this database has
	// already been read, so the external path asks the deployment once rather
	// than once per consultation. It is distinct from "Address is non-empty":
	// a deployment that exports no address at all has still been asked, and
	// asking again would not change the answer.
	read bool
}

// parseHostPortList parses a comma-separated host[:port] list the way Infrahub
// itself reads INFRAHUB_DB_ADDRESS: "Database host, or a comma-separated list of
// cluster members in host[:port] format. Members without an explicit port use
// the value of the port setting." defaultPort is that port setting, and it is
// what a member without a port of its own inherits.
//
// Order is preserved, because it is the order the backup command will try the
// members in (FR-005). An empty list is not an error — it is a deployment that
// configures no address, which the caller distinguishes from a broken one — but
// a member that cannot be read is, since dropping it silently would hand the
// caller a shorter list than the operator wrote and a capture from fewer
// endpoints than they asked for.
func parseHostPortList(address string, defaultPort int) ([]HostPort, error) {
	if strings.TrimSpace(address) == "" {
		return nil, nil
	}

	members := strings.Split(address, ",")
	hosts := make([]HostPort, 0, len(members))
	for _, member := range members {
		host, err := parseHostPort(strings.TrimSpace(member), defaultPort)
		if err != nil {
			return nil, fmt.Errorf("invalid address list %q: %w", address, err)
		}

		hosts = append(hosts, host)
	}

	return hosts, nil
}

// parseHostPort parses one member of an address list. The member may be a host
// name, an IPv4 address or an IPv6 literal, with or without a port; a bare IPv6
// literal has to be recognised by its colons, since it looks exactly like a
// host:port pair to a naive split.
func parseHostPort(member string, defaultPort int) (HostPort, error) {
	switch {
	case member == "":
		return HostPort{}, errors.New("it has an empty member")

	// A bracketed IPv6 literal carrying no port: [::1]. net.SplitHostPort
	// rejects it for having no port, and the brackets are notation rather than
	// part of the address.
	case strings.HasPrefix(member, "[") && strings.HasSuffix(member, "]"):
		host := strings.TrimSuffix(strings.TrimPrefix(member, "["), "]")
		switch {
		case host == "":
			return HostPort{}, fmt.Errorf("member %q has no host", member)
		case !isIPLiteral(host):
			return HostPort{}, fmt.Errorf("member %q is bracketed but %q is not an IPv6 address: brackets are the notation for an IPv6 literal, so a host name is written without them", member, host)
		}

		return HostPort{Host: host, Port: defaultPort}, nil

	// A bare IPv6 literal: ::1, fe80::1. More than one colon with no brackets
	// means every colon belongs to the address, so there is no port to split off
	// and the member inherits defaultPort.
	//
	// The literal is verified, not assumed from the colon count. Counting alone
	// reads anything with two colons as an address, so `db-a:7687;db-b:7687` —
	// a semicolon where the operator meant a comma — became a single host whose
	// name contains the whole typo, and that host reached the capture's --from
	// list and the artefact's own provenance metadata as a member this run
	// claims to have read. A name that cannot resolve is a failure either way;
	// what this stops is the failure arriving as a recorded endpoint rather
	// than as a refused address list.
	case !strings.HasPrefix(member, "[") && strings.Count(member, ":") > 1:
		if !isIPLiteral(member) {
			return HostPort{}, fmt.Errorf("member %q is not in host[:port] form: it holds more than one colon, which is only valid for an IPv6 literal, and it is not one — an IPv6 address with a port is written as [address]:port, and a list of members is separated by commas", member)
		}

		return HostPort{Host: member, Port: defaultPort}, nil

	// host:port, or [::1]:7687.
	case strings.Contains(member, ":"):
		host, portText, err := net.SplitHostPort(member)
		if err != nil {
			return HostPort{}, fmt.Errorf("member %q is not in host[:port] form: %w", member, err)
		}
		if host == "" {
			return HostPort{}, fmt.Errorf("member %q has no host", member)
		}
		if strings.HasPrefix(member, "[") && !isIPLiteral(host) {
			return HostPort{}, fmt.Errorf("member %q is bracketed but %q is not an IPv6 address: brackets are the notation for an IPv6 literal, so a host name is written without them", member, host)
		}

		port, err := parsePort(portText)
		if err != nil {
			return HostPort{}, fmt.Errorf("member %q: %w", member, err)
		}

		return HostPort{Host: host, Port: port}, nil

	// A host with no port of its own, which is the inheritance case.
	default:
		return HostPort{Host: member, Port: defaultPort}, nil
	}
}

// parsePort reads a port number, rejecting anything outside the range a listener
// can occupy. Zero is rejected along with the rest: a port the caller cannot
// connect to is not a usable default, and accepting it would push the failure
// down to the connection attempt where it reads as an unreachable server.
func parsePort(text string) (int, error) {
	port, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("port %q is not a number: %w", text, err)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("port %d is outside the range 1-65535", port)
	}

	return port, nil
}

// isIPLiteral reports whether text is an IP address rather than a host name.
//
// Its callers ask it only where the notation says the member must be a literal
// — inside brackets, or unbracketed with more than one colon — so in practice
// the question it answers is "is this really an IPv6 address". It is not
// narrowed to IPv6 on purpose: an IPv4-mapped literal such as `::ffff:10.0.0.1`
// carries the colons that make it look like host:port, and refusing it would
// refuse an address that resolves perfectly well.
//
// A zone identifier is accepted and kept: `fe80::1%eth0` is how a link-local
// address names the interface it is reachable on, and dropping it would leave
// an address that cannot be connected to — which is why this parses with
// netip, which takes the zone, rather than net.ParseIP, which does not.
func isIPLiteral(text string) bool {
	_, err := netip.ParseAddr(text)

	return err == nil
}

// serviceLocation answers the one question the internal-versus-external
// decision rests on: does this deployment service have a container of its own?
//
// It is a function value rather than a method so resolution can be driven in a
// test without a cluster, the way getAllPodsWith takes its runner.
type serviceLocation func(service string) (EndpointLocation, error)

// deploymentQueries are the two questions endpoint resolution puts to the
// deployment: where a service's container is, and — only once a database has
// been established as external — what the deployment's own components say about
// the database behind it.
type deploymentQueries struct {
	locate   serviceLocation
	discover func(service string) error
}

// ResolveEndpoint resolves where one logical database lives. It is called once
// per database per run, and each database is resolved independently of the
// other, so a deployment with one internal and one external database needs no
// additional operator input (FR-001).
func (iops *InfrahubOps) ResolveEndpoint(service string) (*DatabaseEndpoint, error) {
	queries, err := iops.deploymentQueries()
	if err != nil {
		return nil, err
	}

	return iops.resolveEndpointWith(queries, service)
}

// deploymentQueries binds the two queries to the detected environment. The
// location query is backend-specific; discovery is not, because it reads a
// component's environment through the shared Exec primitive.
func (iops *InfrahubOps) deploymentQueries() (deploymentQueries, error) {
	backend, err := iops.ensureBackend()
	if err != nil {
		return deploymentQueries{}, fmt.Errorf("cannot resolve database endpoints without a detected environment: %w", err)
	}

	return deploymentQueries{
		locate:   locateServiceIn(backend),
		discover: iops.discoverEndpoint,
	}, nil
}

// serviceLocator is a backend that can say whether a deployment service runs
// inside it. Taking the capability as an interface rather than as the concrete
// Kubernetes backend is what lets the paths that gate on the location be driven
// in a test without a cluster; in production only KubernetesBackend implements
// it.
type serviceLocator interface {
	locateService(service string) (EndpointLocation, error)
}

// locateServiceIn is the backend's answer to "does this service run here?".
//
// Only the Kubernetes backend can answer it meaningfully: external databases are
// Kubernetes-only for this feature, so for Docker Compose the question does not
// arise and the answer is internal by construction. Reporting internal there is
// not an assumption about the deployment — it is the scope, and it keeps a
// Compose deployment on exactly the path it is on today (FR-015).
func locateServiceIn(backend EnvironmentBackend) serviceLocation {
	if locator, ok := backend.(serviceLocator); ok {
		return locator.locateService
	}

	return func(string) (EndpointLocation, error) {
		return EndpointLocationInternal, nil
	}
}

// A path that operates on a database has to establish that it can reach it,
// and it gets exactly one chance to do so: before anything is stopped.
//
// That used to be a step — a gate each entry point remembered to call. Which is
// how both capture entry points had one while all three restore entry points
// shipped without one, and how the next entry point would have shipped without
// one too. A step can be forgotten; a property cannot. So the location and the
// refusal live together on the target the operation resolved, and a
// databaseTarget exists only where the location was established and this build
// can actually perform the operation against it. Holding one is the evidence,
// and there is one way to obtain one.

// databaseOperation is what a run intends to do to a database.
//
// The two are not interchangeable. Their refusals differ in the fact that
// matters most on each path — a restore's says no data was changed — and the
// two capabilities do not become available together, because restoring into a
// database the deployment does not manage additionally requires FR-009's
// authorisation.
type databaseOperation string

const (
	databaseCapture databaseOperation = "capture"
	databaseRestore databaseOperation = "restore"

	// databaseInspect reads where a database lives and says so, and is the only
	// operation that moves no data and creates nothing. It exists so that
	// `environment detect` resolves locations through resolveDatabaseTargets
	// like every other path rather than through a second resolver of its own —
	// and so that its failures name the report rather than a backup the
	// operator did not ask for. `infrahub-collect` runs it, so it must stay one
	// that creates no workload (ADR-0003).
	databaseInspect databaseOperation = "inspect"
)

// describe names the run in the failure a location that could not be
// established produces: "cannot start the backup", "cannot start the restore".
func (o databaseOperation) describe() string {
	switch o {
	case databaseRestore:
		return "restore"
	case databaseInspect:
		return "environment report"
	}

	return "backup"
}

// databaseTarget is one database an operation may act on: which database, where
// the deployment says it lives, and what is about to be done to it.
//
// Holding one is meant to be the evidence that the location was established and
// that this run may perform the operation. That was true of every target
// production builds, and true only by convention: with four exported fields and
// nothing else, `databaseTarget{Service: serviceNeo4j, Location:
// EndpointLocationExternal, Operation: databaseRestore}` was a value any caller
// could write, and it would have carried a zero Restore — no authorisation —
// into prepareExternalRestoreWith, which takes a target by value and
// deliberately does not re-check it. An unauthorised external restore, refused
// nowhere.
//
// So the evidence is now a field only databaseTargetWith can set. See gated.
type databaseTarget struct {
	Service   string
	Location  EndpointLocation
	Operation databaseOperation

	// Restore is FR-009's authorisation, carried on the target because that is
	// the only thing every path that could write to a database has to hold.
	// It is consulted only for a restore into an external database; nothing
	// else in the tool reads it, and an internal restore is unaffected by it.
	Restore ExternalRestoreAuth

	// gated is set by databaseTargetWith, after reachable has answered, and by
	// nothing else — it is unexported, so no code outside this package can
	// produce a target that claims to have passed the gate, and inside the
	// package the compiler makes the one assignment easy to find.
	//
	// It is checked by the two preparers, which are what a target authorises.
	// This is the same shape the two defects on this branch had: an invariant
	// that held because every constructor happened to be correct, with the
	// reader trusting a value the type could not promise. Both times the fix
	// was to make the type carry the promise (see requireGated).
	gated bool
}

// requireGated refuses a target that did not come from databaseTargetWith.
//
// It is what the preparers hold instead of re-deriving the location and the
// authorisation for themselves. Re-deriving would be a second answer to a
// question the gate exists to settle once — and the reason the gate exists is
// that all three restore entry points shipped with no reachability check at all
// (T083). So the preparers check the evidence rather than the facts, and this
// is what makes that sound.
func (t databaseTarget) requireGated(action string) error {
	if t.gated {
		return nil
	}

	return fmt.Errorf(
		"cannot %s the external %s database: this target did not come from the gate, so neither its location nor FR-009's authorisation has been established. "+
			"No data was changed", action, t.Service)
}

// reachable reports whether this run may operate on this target, and it is the
// refusal itself rather than a check that produces one. An internal database is
// reachable by construction: the container is there, and every existing path
// reaches it through the deployment.
func (t databaseTarget) reachable() error {
	if t.Location != EndpointLocationExternal {
		return nil
	}

	switch t.Operation {
	case databaseCapture:
		// Reachable: the capture path exists, and what it needs is prepared by
		// prepareDatabaseCaptureWith immediately after this resolution. Holding
		// the target is still the evidence that the location was established
		// positively rather than inferred from a failed query (FR-002).
		return nil
	case databaseInspect:
		// Always reachable, and it is the one arm where that needs no
		// qualification: an inspection reads the location and reports it,
		// creates nothing, writes nothing, and so has no capability to
		// establish and nothing to authorise. FR-009's authorisation is
		// emphatically not consulted — requiring it would make `environment
		// detect` refuse to describe a deployment the operator is diagnosing
		// precisely because they cannot back it up.
		return nil
	case databaseRestore:
		// FR-009's authorisation, and this is the one place it is read. It is
		// required for every restore into a database the deployment does not
		// manage, and this constructor is the one thing every such path holds —
		// so the check cannot be omitted by a path that lands later, which is
		// exactly how the three restore entry points shipped without a
		// reachability check at all (see T083).
		//
		// Nothing else in the tool consults it, and in particular no restore
		// path asks again: holding the target *is* the answer. That is what
		// keeps --force and the scheduled-restore setting from conferring it by
		// accident — neither is on this line, and there is no second line.
		if !t.Restore.allowed() {
			return externalRestoreUnauthorised(t.Service)
		}

		// Authorised, and the restore path for an external database exists:
		// what it needs is prepared by prepareDatabaseRestoreWith immediately
		// after this resolution. Holding the target is still the evidence that
		// the location was established positively rather than inferred from a
		// failed query (FR-002).
		return nil
	}

	return fmt.Errorf("cannot operate on the %s database: %q is not an operation this build knows", t.Service, t.Operation)
}

// databaseTargets are the databases one operation will act on, in the order
// they were gated.
//
// It is a named type because the targets are the run's evidence and not a
// by-product of producing an error: every caller of resolveDatabaseTargets used
// to discard them — `if _, err := …` has a caller and no consumer — so the
// location each target carried was established, thrown away, and then asked for
// again by the preparation that followed. The methods below are what the
// preparation reads instead of re-querying the deployment.
type databaseTargets []databaseTarget

// external returns the targets whose database lives outside the deployment.
//
// This is the answer prepareExternalSources used to recompute with a second
// locate call per database, having been handed the service names rather than
// the targets. The locations are memoised (see locateService), so the duplicate
// was a memo read rather than a second API call — but two readings of one
// question is the shape this phase exists to remove, and only one of them was
// the gate.
func (t databaseTargets) external() databaseTargets {
	external := make(databaseTargets, 0, len(t))
	for _, target := range t {
		if target.Location == EndpointLocationExternal {
			external = append(external, target)
		}
	}

	return external
}

// services names the databases these targets cover, for the one line an
// operator reads about what the gate established.
func (t databaseTargets) services() []string {
	services := make([]string, 0, len(t))
	for _, target := range t {
		services = append(services, target.Service)
	}

	return services
}

// databaseTargetWith resolves one database an operation is about to act on,
// against the supplied queries, following resolveEndpointWith's injection idiom
// so the decision is testable without a cluster.
//
// A location that could not be established is an error, not a location: FR-002
// forbids reading a failure to query the deployment as evidence that the
// database is elsewhere, and here that would be the difference between stopping
// and scaling Infrahub down for nothing.
func databaseTargetWith(queries deploymentQueries, operation databaseOperation, service string, auth ExternalRestoreAuth) (databaseTarget, error) {
	location, err := queries.locate(service)
	if err != nil {
		return databaseTarget{}, fmt.Errorf("cannot start the %s: %w", operation.describe(), err)
	}

	target := databaseTarget{Service: service, Location: location, Operation: operation, Restore: auth}
	if err := target.reachable(); err != nil {
		return databaseTarget{}, err
	}

	// The one assignment of the witness, and it is here rather than in the
	// literal above so that it cannot be set by a target reachable refused.
	target.gated = true

	return target, nil
}

// resolveEndpointWith is the resolution itself, against the supplied queries.
//
// The order is deliberate: the location is established first, and everything
// external mode needs is read only afterwards. That is what makes FR-015
// structural rather than a promise — a deployment whose databases are all
// internal returns from here before any new query is issued or any new setting
// is consulted.
func (iops *InfrahubOps) resolveEndpointWith(queries deploymentQueries, service string) (*DatabaseEndpoint, error) {
	if service != serviceNeo4j && service != serviceTaskManagerDB {
		return nil, errNotADatabaseService("resolve a database endpoint for", service)
	}

	location, err := queries.locate(service)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve the %s endpoint: %w", service, err)
	}

	endpoint := &DatabaseEndpoint{Service: service, Location: location}

	if location != EndpointLocationExternal {
		// The container is there, so there is nothing to resolve: every existing
		// path reaches it through the deployment. Recorded at debug level so an
		// all-internal deployment's output is the output it has always had.
		logrus.Debugf("Resolved %s endpoint: %s", service, endpoint.Describe())

		return endpoint, nil
	}

	// Discovery can fail without the run failing: an operator who supplied the
	// address explicitly has given the one thing that cannot be defaulted, and
	// refusing work they fully specified would be worse than proceeding on the
	// documented defaults. The warning names the reason, and the endpoint report
	// below states the values the run will actually use.
	//
	// The failure is carried forward rather than only logged, because it decides
	// which refusal an operator who supplied *nothing* gets. "The settings were
	// read and none of them records an address" (FR-004) and "the settings could
	// not be read" (FR-019) are two conditions with different remedies, and a
	// warning in a terminal is not where a refused run says which it met.
	discoveryErr := queries.discover(service)
	if discoveryErr != nil {
		logrus.Warnf("Could not read the %s settings from the deployment, falling back to the supplied configuration: %v", service, discoveryErr)
	}

	if err := iops.applyDiscoveredFacts(endpoint, discoveryErr); err != nil {
		return nil, err
	}

	// FR-003: the address reaches the operator's terminal before anything is
	// attempted against it, so a connection that then hangs or is refused has
	// already been preceded by the endpoint it was made to.
	endpoint.Report()

	return endpoint, nil
}

// ReportDatabaseLocations states, per database, whether it runs in this
// deployment and — where it does not — the endpoint it was resolved to
// (US2 scenario 5).
//
// `environment detect` is the first command an operator reaches for when a
// backup fails, and until now it named the deployment and its credentials and
// said nothing at all about where the databases were. An operator whose backup
// had just failed on an external database therefore learnt nothing from the one
// command written to tell them what the tool sees.
//
// It is shared by all three binaries through AttachEnvironmentCommands, so it
// must be correct for the read-only one: it resolves the location, which is a
// pod listing, and then reads the deployment's own configuration through the
// existing exec — no workload is created, and databaseInspect is what makes
// that structural rather than a property of this function (ADR-0003).
func (iops *InfrahubOps) ReportDatabaseLocations() error {
	queries, err := iops.deploymentQueries()
	if err != nil {
		return err
	}

	return iops.reportDatabaseLocationsWith(queries)
}

// reportDatabaseLocationsWith is the report against the supplied queries,
// following resolveEndpointWith's injection idiom so it is testable without a
// cluster.
//
// Neither half of it fails the command. A location that could not be
// established is reported as exactly that — never as a database outside the
// deployment (FR-002) — because this is the diagnostic an operator runs when
// something else has already failed, and a detect that exits on the first
// unanswered query tells them less than the deployment and credentials it would
// otherwise have printed. An endpoint that could not be resolved is reported
// with the reason, which for an external database is FR-004's refusal naming
// where discovery looked and the override that supplies it.
func (iops *InfrahubOps) reportDatabaseLocationsWith(queries deploymentQueries) error {
	// Both databases, not the run's selection: nothing here reads or writes
	// either one, and an operator diagnosing a deployment wants the whole
	// picture rather than the part some later --exclude-task-manager would have
	// covered.
	targets, err := resolveDatabaseTargets(queries, databaseInspect, true, ExternalRestoreAuth{})
	if err != nil {
		logrus.Warnf("Could not establish where the databases live: %v", err)

		return nil
	}

	for _, target := range targets {
		if target.Location != EndpointLocationExternal {
			logrus.Infof("Database %s: runs in this deployment", target.Service)

			continue
		}

		endpoint, err := iops.resolveEndpointWith(queries, target.Service)
		if err != nil {
			logrus.Warnf("Database %s: does not run in this deployment, and its endpoint could not be resolved: %v", target.Service, err)

			continue
		}

		// The values repeat the line resolveEndpointWith has just logged for
		// FR-003, and that is deliberate here: US2 scenario 5 asks for a
		// statement per database that carries both the location and where it
		// resolved to, and an operator reading a failing deployment should not
		// have to join two lines to get one answer. The duplication is one line
		// per external database, on the one command whose whole purpose is to
		// describe what the tool sees.
		logrus.Infof("Database %s: does not run in this deployment; resolved to %s", target.Service, endpoint.Describe())
	}

	return nil
}

// discoverEndpoint reads the deployment's own configuration for one database.
// It runs only on the external path, and only once per database per run.
//
// Its final arm is unreachable through resolveEndpointWith, which is the only
// caller and which refuses anything that is not one of the two databases before
// issuing a query. It reports that rather than returning nil all the same: a
// nil here says "the deployment's own settings were read", and the one thing
// that could ever reach this arm is a third database arriving, whose settings
// would then be silently unread while the endpoint claimed to describe it. Its
// sibling applyDiscoveredFacts already draws the line here.
func (iops *InfrahubOps) discoverEndpoint(service string) error {
	switch service {
	case serviceNeo4j:
		return iops.ensureNeo4jDiscovery()
	case serviceTaskManagerDB:
		return iops.ensurePostgresDiscovery()
	default:
		return errNotADatabaseService("read the settings from the deployment for", service)
	}
}

// applyDiscoveredFacts fills an external endpoint from the two sources that
// describe it: the operator's overrides on Configuration, and what the
// deployment's own components reported. The override wins wherever both speak,
// which is the reason the two are recorded separately in the first place.
//
// discoveryErr is why the second source is empty, when it is. It is carried in
// rather than looked up because only these functions know whether it mattered:
// an override supplies the address and the failure is a warning, while nothing
// at all makes it the whole reason the run cannot proceed (see
// undiscoverableEndpoint and unreadableDeploymentSettings).
func (iops *InfrahubOps) applyDiscoveredFacts(endpoint *DatabaseEndpoint, discoveryErr error) error {
	switch endpoint.Service {
	case serviceNeo4j:
		return iops.applyNeo4jFacts(endpoint, discoveryErr)
	case serviceTaskManagerDB:
		return iops.applyPostgresFacts(endpoint, discoveryErr)
	default:
		return errNotADatabaseService("resolve a database endpoint for", endpoint.Service)
	}
}

// applyNeo4jFacts resolves the external Neo4j endpoint. The client port,
// protocol and TLS settings are the deployment's own, reconciled with the
// operator's overrides when chunk-2a's discovery read them; the backup port is
// the one detail no deployment records (research R4).
func (iops *InfrahubOps) applyNeo4jFacts(endpoint *DatabaseEndpoint, discoveryErr error) error {
	endpoint.ClientPort = iops.discoveredNeo4j.ClientPort
	endpoint.BackupPort = resolveNeo4jBackupPort(iops.config.ExternalDB)
	endpoint.Protocol = iops.config.ExternalDB.Neo4jProtocol
	endpoint.TLS = iops.config.ExternalDB.Neo4jTLS
	endpoint.Database = iops.config.Neo4jDatabase

	address := cmp.Or(iops.config.ExternalDB.Neo4jAddress, iops.discoveredNeo4j.Address)
	if address == "" {
		return noAddressFor(serviceNeo4j, componentInfrahubServer,
			[]string{neo4jAddressEnvVar, neo4jPortEnvVar}, neo4jAddressFlag, discoveryErr)
	}

	return endpoint.resolveHosts(address)
}

// applyPostgresFacts resolves the external task-manager endpoint. It carries no
// protocol or TLS settings because the deployment records none for PostgreSQL —
// its host, port and database name all come from the Prefect connection URL.
func (iops *InfrahubOps) applyPostgresFacts(endpoint *DatabaseEndpoint, discoveryErr error) error {
	endpoint.ClientPort = iops.discoveredPostgres.ClientPort
	endpoint.Database = iops.config.PostgresDatabase

	address := cmp.Or(iops.config.ExternalDB.PostgresAddress, iops.discoveredPostgres.Address)
	if address == "" {
		return noAddressFor(serviceTaskManagerDB, componentTaskManager,
			prefectConnectionEnvVars, postgresAddressFlag, discoveryErr)
	}

	return endpoint.resolveHosts(address)
}

// noAddressFor is the one place the two reasons a run has no address for an
// external database are told apart.
//
// Both arms are reached with the location positively established — the database
// demonstrably does not run in this deployment — and with nothing to address it
// by. What differs is whether the deployment was asked and answered:
//
//   - it answered, and none of its settings records an address: FR-004, and
//     undiscoverableEndpoint says so.
//   - it could not be asked at all — `pods/exec` denied, the component's own
//     pod gone, the control bound expired: FR-019, and asserting the settings
//     "were read for it" would be a claim about the customer's configuration
//     made on evidence this run never obtained. That conflation is what caused
//     this feature's original reset, and it is the same one
//     KubernetesBackend.undeterminedLocation exists to prevent one question
//     earlier.
//
// The two have different remedies — set the address, versus restore the
// permission — so which one an operator reads decides where they spend the
// outage.
func noAddressFor(service, component string, settings []string, flag string, discoveryErr error) error {
	if discoveryErr != nil {
		return unreadableDeploymentSettings(service, component, settings, flag, discoveryErr)
	}

	return undiscoverableEndpoint(service, component, settings, flag)
}

// errDeploymentSettingsUnreadable is FR-019's refusal at this point, as a value
// callers and tests can identify rather than a message they have to match. It is
// deliberately a different value from errEndpointUndiscoverable: a caller that
// treats "not configured" and "could not be read" alike is the bug this
// separation exists for.
var errDeploymentSettingsUnreadable = errors.New("the deployment's own settings for it could not be read, so whether one records an address is unknown")

// unreadableDeploymentSettings is what an operator reads when a database is
// demonstrably not in the deployment, nothing was supplied for it, and the
// deployment could not be asked what it uses.
//
// It states the three elements the failure-message contract requires against
// that condition: what happened (the component could not be read, with the
// cluster's own reason attached), the resource (the settings that would have
// answered, and where they live) and the action — which is two, in the order an
// operator can act on them: supply the address, which unblocks the run now, or
// restore the ability to read the component, which is what the tool was relying
// on.
func unreadableDeploymentSettings(service, component string, settings []string, flag string, err error) error {
	return fmt.Errorf(
		"the %s database does not run in this deployment and %w: %s would have been read from the %s component, and it could not be read (%v). "+
			"Supply the endpoint with --%s, or set %s for an unattended run, to proceed without reading it; "+
			"otherwise restore this run's access to that component — this is not a report that the setting is unset. %s",
		service, errDeploymentSettingsUnreadable,
		strings.Join(settings, ", "), component, err,
		flag, flagEnvVar(flag),
		externalMisdiagnosisHint(service))
}

// errEndpointUndiscoverable is FR-004's refusal, as a value callers and tests
// can identify rather than a message they have to match.
var errEndpointUndiscoverable = errors.New("no address for it could be discovered from the deployment's own configuration")

// undiscoverableEndpoint is what an operator reads when a database is
// demonstrably not in the deployment and nothing records where it is.
//
// The three elements the failure-message contract requires are the condition
// (discovery ran and found nothing), the resource (the settings that were read,
// named, in the component they were read out of) and the action (the override
// flag, plus its configuration key, which is the only channel an unattended run
// has). Stating where discovery looked is the part that was missing: "supply
// --neo4j-address" on its own leaves an operator whose deployment *does* export
// the address unable to tell a setting they never set from a component this run
// could not read, and those two have different remedies.
//
// It is deliberately distinct from the refusal a denied deployment query
// produces (see KubernetesBackend.undeterminedLocation). This one is reached
// only after the location was positively established, so it says the deployment
// was asked and answered; conflating the two is what once reported an RBAC
// failure as a database nobody could find.
//
// "Asked and answered" is now a property of the call rather than of the comment:
// a run whose discovery *failed* reaches unreadableDeploymentSettings instead,
// because this message's central claim — that the settings were read — was
// false for it, and noAddressFor is what routes between them.
func undiscoverableEndpoint(service, component string, settings []string, flag string) error {
	return fmt.Errorf(
		"the %s database does not run in this deployment and %w: %s in the %s component were read for it, and none of them records an address. "+
			"Supply the endpoint with --%s, or set %s for an unattended run. %s",
		service, errEndpointUndiscoverable,
		strings.Join(settings, ", "), component,
		flag, flagEnvVar(flag),
		externalMisdiagnosisHint(service))
}

// flagEnvVar is the configuration key a flag also reads from, under the binding
// convention ConfigureRootCommand sets up: the INFRAHUB_ prefix, with dashes
// replaced by underscores. Derived rather than written out beside each flag, so
// a message naming both cannot state a key viper does not bind.
func flagEnvVar(flag string) string {
	return "INFRAHUB_" + strings.ToUpper(strings.ReplaceAll(flag, "-", "_"))
}

// The tooling that will read a database and the server it will read are two
// versions the run has to reconcile before it moves any data (FR-006). That
// reconciliation cannot be one numeric comparison, because Neo4j numbers its
// releases two different ways — 5.x.y up to the 5.26 LTS, and YYYY.MM.p since —
// and which of the two a version belongs to is a fact about the vendor's release
// timeline rather than something the digits state. So the scheme is classified
// first and the comparison happens within it.
//
// The repository's other version comparison, in internal/updater, is strict
// semver over this tool's own release tags. It is deliberately not reused here:
// it requires a leading "v", has no notion of calendar versioning, and would
// read the "-enterprise" of the pinned image tag as a pre-release — see
// prereleaseMarkers for why that particular reading would abort working runs.

// versionScheme is the numbering scheme a database version belongs to.
type versionScheme int

const (
	// versionSchemeSequential is the traditional major[.minor[.patch]] form:
	// Neo4j 5.26.1, PostgreSQL 18.1, or a bare major series.
	versionSchemeSequential versionScheme = iota

	// versionSchemeCalendar is the YYYY.MM[.p] form Neo4j has published since
	// 2025.
	versionSchemeCalendar
)

// calendarYearFloor is the first-field value at or above which a version reads
// as a calendar year rather than a major series. Nothing in a version string
// declares its scheme, so the classification rests on the one property the two
// schemes cannot share: no database this tool supports numbers a major series in
// the thousands, and every calendar release the vendor has published carries a
// four-digit year.
const calendarYearFloor = 2000

// prereleaseMarkers are the qualifiers that mean "these numbers, before their
// finished release".
//
// They are matched by name rather than by "the version carries a qualifier at
// all", because the qualifiers this project meets most are -enterprise,
// -community and -alpine, which say which build of a release an image carries
// rather than that it precedes one. Ranking those below the bare numbers, the
// way semver ranks any pre-release, would make the pinned
// neo4j:2025.10.1-enterprise tooling older than a 2025.10.1 server and abort a
// run that would have worked.
var prereleaseMarkers = []string{"alpha", "beta", "rc", "preview", "snapshot"}

// databaseVersion is a database version parsed far enough to be ordered.
type databaseVersion struct {
	// Original is the version as it was read, so that a message about it quotes
	// what the server or the operator actually said rather than a normalised
	// rendering of it.
	Original string

	// scheme is which numbering scheme the version belongs to, which is what
	// orders two versions drawn from different ones.
	scheme versionScheme

	// fields are the numeric components in the order they were written, only as
	// many as the version states. They are deliberately not padded to three: a
	// version that stops at its major states nothing about a minor, and reading
	// the absent field as zero is what would make the pinned postgres:18-alpine
	// tooling look older than the 18.1 server it reads perfectly well.
	fields []int

	// prerelease records that the version named itself one of
	// prereleaseMarkers.
	prerelease bool
}

// String renders the version as the string it was read from, so it can be
// dropped into a message with %s.
func (v databaseVersion) String() string {
	return v.Original
}

// parseDatabaseVersion reads a version string into an orderable value.
//
// It is tolerant of the shapes these servers and their images actually produce —
// a build qualifier (5.26.1-enterprise, 18-alpine) and the packaging detail
// PostgreSQL appends to its own server_version ("18.1 (Debian
// 18.1-1.pgdg120+1)") — and refuses everything else rather than guessing at it.
// The gate below turns that refusal into an abort: a version that cannot be
// ordered is not evidence of compatibility.
func parseDatabaseVersion(text string) (databaseVersion, error) {
	original := strings.TrimSpace(text)
	if original == "" {
		return databaseVersion{}, errors.New("version was not reported")
	}

	core, qualifier := splitVersionQualifier(original)

	fields, err := parseVersionFields(core)
	if err != nil {
		return databaseVersion{}, fmt.Errorf("version %q cannot be read: %w", original, err)
	}

	return databaseVersion{
		Original:   original,
		scheme:     classifyVersionScheme(fields),
		fields:     fields,
		prerelease: isPrereleaseQualifier(qualifier),
	}, nil
}

// splitVersionQualifier separates the numeric core of a version from whatever
// follows it. The core ends at the first hyphen, plus or whitespace, whichever
// comes first: those are the separators that start a build qualifier, a build
// metadata suffix, and the packaging detail a server appends to its version.
func splitVersionQualifier(version string) (core, qualifier string) {
	if index := strings.IndexAny(version, "-+ \t"); index >= 0 {
		return version[:index], version[index+1:]
	}

	return version, ""
}

// parseVersionFields reads the dot-separated numeric components of a version
// core, keeping exactly as many as were written. An empty or non-numeric
// component is an error rather than a zero, because a version this tool cannot
// read is one it must not pretend to have compared.
func parseVersionFields(core string) ([]int, error) {
	if core == "" {
		return nil, errors.New("it states no version number")
	}

	parts := strings.Split(core, ".")
	fields := make([]int, 0, len(parts))
	for _, part := range parts {
		value, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("component %q is not a number", part)
		}

		fields = append(fields, value)
	}

	return fields, nil
}

// classifyVersionScheme decides which scheme a parsed version belongs to. It is
// a step of its own because that answer, rather than the digits, is what orders
// two versions from different schemes.
func classifyVersionScheme(fields []int) versionScheme {
	if len(fields) > 0 && fields[0] >= calendarYearFloor {
		return versionSchemeCalendar
	}

	return versionSchemeSequential
}

// isPrereleaseQualifier reports whether a version's qualifier names it a
// pre-release. The match is on the qualifier's opening text so that alpha09, rc1
// and beta.2 are recognised alongside the bare markers.
func isPrereleaseQualifier(qualifier string) bool {
	qualifier = strings.ToLower(strings.TrimSpace(qualifier))
	for _, marker := range prereleaseMarkers {
		if strings.HasPrefix(qualifier, marker) {
			return true
		}
	}

	return false
}

// majorOnly returns v reduced to its leading numeric component, so that two
// versions compare on their major alone. The Original is kept, because it is
// what a message about the version has to quote; the scheme is unaffected,
// since it is decided by that same leading field.
func (v databaseVersion) majorOnly() databaseVersion {
	if len(v.fields) <= 1 {
		return v
	}

	reduced := v
	reduced.fields = v.fields[:1]

	return reduced
}

// compare orders v against other: negative when v is the older of the two, zero
// when neither is older, positive when v is the newer.
func (v databaseVersion) compare(other databaseVersion) int {
	// The scheme decides first. A calendar release is later than any 5.x one
	// because the vendor moved from the one to the other; that 2025 also happens
	// to exceed 5 is a coincidence of the digits, and relying on it would leave
	// the ordering resting on an accident — while comparing the same two as text
	// orders them backwards outright.
	if v.scheme != other.scheme {
		if v.scheme == versionSchemeCalendar {
			return 1
		}

		return -1
	}

	// Within a scheme, only the fields both versions state are compared. An
	// absent field is unstated rather than zero — see databaseVersion.fields.
	for i := 0; i < len(v.fields) && i < len(other.fields); i++ {
		switch {
		case v.fields[i] < other.fields[i]:
			return -1
		case v.fields[i] > other.fields[i]:
			return 1
		}
	}

	// The same numbers to the precision both state, so the only thing left that
	// can separate them is one being a pre-release of those numbers. Two
	// pre-releases of the same numbers are not ordered against each other: the
	// markers carry no documented sequence, and inventing one would put a guess
	// inside an abort condition.
	switch {
	case v.prerelease && !other.prerelease:
		return -1
	case other.prerelease && !v.prerelease:
		return 1
	default:
		return 0
	}
}

// checkVersionCompatibility is the FR-006 gate, and it runs before any data is
// read or written: the utility that will handle a database must not be older
// than the server it handles.
//
// The rule is asymmetric on purpose. An older utility against a newer server is
// the direction with a known failure mode — pg_dump refuses it outright, and
// nothing documents Neo4j's backup client tolerating it — so that direction
// aborts, naming both versions so the operator can see which end to move. The
// other direction is documented as tolerated for restore and merely
// undocumented for backup, so it warns and continues (research R3): enforcing
// the stricter symmetric rule would encode a guess in an abort condition and
// decline work this tool can do.
//
// The precision of the comparison differs by service, because the two servers
// state different rules. PostgreSQL's is major-only and written down: pg_dump
// sets maxRemoteVersion to (PG_VERSION_NUM / 100) * 100 + 99, so an 18.1 client
// reads every 18.x server, and comparing the patch would abort on a
// provider-patched 18.2 while naming an image flag that cannot fix it — the
// official images publish only the current patch and the bare major, so no
// 18.2-alpine exists to move to. Neo4j publishes no equivalent rule, so its arm
// compares every field both versions state; loosening it would be a guess, and
// the tolerated direction only warns anyway.
//
// The decision is pure, over the two version strings. Reading the server's
// version needs a client that exists only once the transient workload is
// running, so the caller supplies both — the server version from the probe it
// runs there, and the utility version from the tooling in that same image.
func checkVersionCompatibility(service, utilityVersion, serverVersion string) error {
	// A version neither side can order is not a compatible one. FR-006 requires
	// the comparison to have happened before data moves, and a comparison that
	// could not be made has not happened, so an unreadable version on either
	// side aborts rather than passing the gate.
	utility, err := parseDatabaseVersion(utilityVersion)
	if err != nil {
		return fmt.Errorf("cannot compare the %s utility and server versions before moving data (the server reported %q): the utility's %w", service, serverVersion, err)
	}

	server, err := parseDatabaseVersion(serverVersion)
	if err != nil {
		return fmt.Errorf("cannot compare the %s utility and server versions before moving data (the utility reported %q): the server's %w", service, utilityVersion, err)
	}

	// The versions in the messages stay as they were reported: the operator has
	// to see the versions their deployment states, not the truncation this
	// comparison happens to make of them.
	switch ordering := comparedAt(service, utility).compare(comparedAt(service, server)); {
	case ordering < 0:
		return fmt.Errorf("the %s utility (%s) is older than the server (%s): supply %s with an image whose tooling is at least as new as the server", service, utility, server, versionImageFlag(service))
	case ordering > 0:
		logrus.Warnf("The %s utility (%s) is newer than the server (%s); continuing, since no documented rule forbids that direction", service, utility, server)
	default:
		logrus.Infof("The %s utility (%s) is compatible with the server (%s)", service, utility, server)
	}

	return nil
}

// comparedAt reduces a version to the precision its service's own compatibility
// rule is written at. PostgreSQL's is major-only; every other service is
// compared at the full precision both versions state.
func comparedAt(service string, version databaseVersion) databaseVersion {
	if service == serviceTaskManagerDB {
		return version.majorOnly()
	}

	return version
}

// versionImageFlag names the flag that selects the tooling image for a service,
// so an abort tells the operator which end to move rather than only that the two
// versions disagree.
func versionImageFlag(service string) string {
	if service == serviceTaskManagerDB {
		return "--external-db-image-postgres"
	}

	return "--external-db-image-neo4j"
}

// ---------------------------------------------------------------------------
// Certificate verification (FR-029)
// ---------------------------------------------------------------------------

// The deployment records whether Infrahub verifies its own database server's
// certificate. That is a statement about Infrahub, not a licence for this tool,
// and the difference is the whole of FR-029: this tool sends database
// credentials of its own, so the decision to stop checking who it is sending
// them to has to be made about this tool, explicitly, by the operator.
//
// So a discovered tls_insecure never disables verification here. It is reported
// — silence would be worse, since an operator who set it probably expects it to
// apply — together with the flag that does disable verification, so the remedy
// is in the same line as the observation (SC-008).

// insecureTLSFlagName is the only opt-out from protecting a connection to an
// external database — from verifying the certificate, and from encrypting the
// channel at all — as cobra registers it. insecureTLSFlag is the same flag as a
// message names it, derived rather than restated so the flag an operator is
// told to supply and the flag that exists cannot drift apart.
const (
	insecureTLSFlagName = "external-db-insecure-tls"
	insecureTLSFlag     = "--" + insecureTLSFlagName
)

// neo4jSystemDatabase is the database every probe statement runs against. It is
// the one database a server always has, so a probe does not depend on the
// database being captured being online at the moment it runs.
const neo4jSystemDatabase = "system"

// roleUnknown is a real observed role rather than the absence of one: a member
// the server did not report a role for is distinguishable from a capture that
// never observed roles at all (contracts/artifact-and-metadata.md).
const roleUnknown = "unknown"

// externalTLSDecision is what this tool will do about TLS on the connections it
// opens to one external database. It is resolved once per database per run and
// passed to every command builder, so no builder decides it for itself.
type externalTLSDecision struct {
	// Service is the database the decision applies to.
	Service string

	// Encrypt says the channel will be encrypted at all. It is true by default
	// for both services: for PostgreSQL because libpq negotiates TLS and the
	// mode below is what decides the outcome, and for Neo4j because the
	// connection carries this tool's own database password out of the cluster
	// and an unencrypted channel is not something a deployment's setting gets
	// to choose on the operator's behalf.
	//
	// It used to follow the deployment's own tls_enabled for Neo4j, on the
	// reasoning that a server not speaking TLS on its client port cannot be
	// made to by this tool asking. That is true and it is not the question: the
	// setting is usually absent rather than false — a deployment that leaves it
	// at Infrahub's default exports nothing — so the common case sent the
	// password in clear with no flag able to prevent it, in a file that argues
	// at length that a discovered tls_insecure must be read and never obeyed.
	// The same reasoning applies a step further along: a channel that is not
	// encrypted verifies nothing, so FR-029's "unless the operator explicitly
	// opts out" has to cover it too. It now does, through OptedOut below, which
	// is the one input that can turn encryption off.
	Encrypt bool

	// OptedOut records that the operator supplied insecureTLSFlag, which is
	// distinct from verify() being false for want of an encrypted channel.
	OptedOut bool

	// deploymentInsecure records that the deployment configures insecure TLS
	// for Infrahub's own connections. It exists to be reported, never to
	// decide.
	deploymentInsecure bool
}

// resolveExternalTLS decides TLS for one external database.
//
// The default is a verified, encrypted channel, and the operator's own opt-out
// is the only thing that changes it. What the opt-out then gets is the weakest
// channel the deployment's own settings describe: where the deployment says it
// speaks TLS, an encrypted channel with the certificate unverified; where it
// says it does not — or says nothing, which is the same observation — an
// unencrypted one. So a deployment whose external Neo4j has no TLS at all is
// still reachable, but reaching it takes the operator saying so, rather than an
// absent environment variable deciding that this tool's password may travel in
// clear.
func resolveExternalTLS(service string, cfg ExternalDBConfig, tls ExternalDBTLS) externalTLSDecision {
	return externalTLSDecision{
		Service:            service,
		Encrypt:            service == serviceTaskManagerDB || !cfg.InsecureTLS || tls.Enabled,
		OptedOut:           cfg.InsecureTLS,
		deploymentInsecure: tls.Insecure,
	}
}

// verify says whether the server's certificate will be verified. It is the
// default, and only the operator's own opt-out clears it — the same opt-out
// being the only thing that can clear Encrypt, so an unencrypted channel
// verifies nothing without a second field having to record it.
func (d externalTLSDecision) verify() bool {
	return d.Encrypt && !d.OptedOut
}

// Report states the decision before any connection is opened, for the same
// reason DatabaseEndpoint.Report exists: a handshake that then fails should be
// preceded by what this run was going to do about certificates.
func (d externalTLSDecision) Report() {
	switch {
	case !d.Encrypt:
		logrus.Warnf("Opening unencrypted connections to the external %s database: %s was supplied and the deployment records TLS as disabled for it, so the credentials this run sends travel in clear and there is no certificate to verify", d.Service, insecureTLSFlag)
	case d.OptedOut:
		logrus.Warnf("Not verifying the external %s server's certificate because %s was supplied", d.Service, insecureTLSFlag)
	default:
		logrus.Infof("Verifying the external %s server's certificate", d.Service)
	}

	// Reported whenever the deployment said insecure and this tool did not
	// follow it, which is the case FR-029 exists for.
	if d.deploymentInsecure && !d.OptedOut {
		logrus.Warnf("The deployment configures insecure TLS for Infrahub's own %s connections; this run still verifies the server's certificate, because that setting was not made about the credentials this tool sends. Supply %s to opt out for this tool as well", d.Service, insecureTLSFlag)
	}
}

// boltScheme is the URI scheme the Bolt client is pointed at, which is where the
// decision above becomes an actual verification behaviour: +s verifies the
// certificate, +ssc accepts a self-signed one, and the bare scheme is
// unencrypted.
func (d externalTLSDecision) boltScheme(protocol string) string {
	base := protocol
	if base == "" {
		base = defaultNeo4jProtocol
	}

	switch {
	case !d.Encrypt:
		return base
	case d.OptedOut:
		return base + "+ssc"
	default:
		return base + "+s"
	}
}

// postgresSSLMode is the same decision for libpq. verify-full is the mode that
// checks both the chain and the host name; require encrypts and checks neither,
// which is what the opt-out asks for.
func (d externalTLSDecision) postgresSSLMode() string {
	if d.OptedOut {
		return "require"
	}

	return "verify-full"
}

// ---------------------------------------------------------------------------
// Addressing an external database (FR-005)
// ---------------------------------------------------------------------------

// captureTargets is the endpoint's members addressed on the port the capture
// will actually travel over, in the order they will be tried (FR-005).
//
// The port depends on the service, and stamping one service's port onto the
// other is not a cosmetic error. Neo4j's online backup speaks to a listener of
// its own, whose port no deployment records (research R4) and which
// --neo4j-backup-port is the only source of; a member's own port, read from
// Infrahub's address setting, is a *client* port and is therefore never the
// right answer for that list. PostgreSQL has no second listener at all — the
// dump travels over the client port each member already carries — so giving it
// the Neo4j backup port would record :6362 in provenance metadata for a dump
// that went over :5432, which is a false statement about where the data came
// from rather than a wrong flag value.
func (e DatabaseEndpoint) captureTargets() []HostPort {
	if e.Service != serviceNeo4j {
		return append([]HostPort(nil), e.Hosts...)
	}

	port := e.BackupPort
	if port <= 0 {
		port = defaultNeo4jBackupPort
	}

	targets := make([]HostPort, 0, len(e.Hosts))
	for _, host := range e.Hosts {
		targets = append(targets, HostPort{Host: host.Host, Port: port})
	}

	return targets
}

// renderHostPortList is members in try order, ports explicit, one string each.
// It is the form provenance metadata records, where the order is the fact being
// recorded and joining it away would lose it.
func renderHostPortList(hosts []HostPort) []string {
	if len(hosts) == 0 {
		return nil
	}

	rendered := make([]string, 0, len(hosts))
	for _, host := range hosts {
		rendered = append(rendered, host.String())
	}

	return rendered
}

// renderHostPorts joins members the way both --from and an operator-facing
// report want them: in try order, comma-separated, ports explicit.
func renderHostPorts(hosts []HostPort) string {
	return strings.Join(renderHostPortList(hosts), ",")
}

// boltURIs is one client URI per member, in try order. There is one per member
// rather than one for the endpoint because a probe against an unresponsive
// member should fall through to the next exactly as the capture's own --from
// list does, and a single URI would make the first member's health decide the
// run.
func (e DatabaseEndpoint) boltURIs(d externalTLSDecision) []string {
	scheme := d.boltScheme(e.effectiveProtocol())

	uris := make([]string, 0, len(e.Hosts))
	for _, host := range e.Hosts {
		uris = append(uris, scheme+"://"+host.String())
	}

	return uris
}

// safeDatabaseIdentifier refuses a database name that carries anything but a
// name before it is interpolated into a statement.
//
// Infrahub pattern-validates the name it sets, so this is a belt rather than a
// braces: the value reaches here from a component's environment, and a name
// that could close a quote and continue the statement must not be the thing
// standing between a discovered value and a Cypher session.
func safeDatabaseIdentifier(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", errors.New("no database name was resolved")
	}

	for _, r := range trimmed {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_':
		default:
			return "", fmt.Errorf("database name %q contains %q, which a database name may not carry", trimmed, string(r))
		}
	}

	return trimmed, nil
}

// ---------------------------------------------------------------------------
// Probe queries (FR-006, FR-008, FR-022, FR-005)
// ---------------------------------------------------------------------------

// Four facts have to be read from an external server before its capture can be
// built, and none of them can be read from outside the deployment: the server's
// version and the utility's, which are the FR-006 gate; the edition, which
// decides whether a remote capture is possible at all (FR-008); and the store
// size, which sizes the capture's scratch space (FR-022). The commands are
// built here as pure values so that what is sent to a customer's database is
// assertable without one, and so that no credential can drift onto one of them
// (FR-014).
//
// The credentials are conspicuously absent from every command below. The
// transient workload mounts them as environment from a secret it owns, under the
// names both clients read of their own accord — NEO4J_USERNAME/NEO4J_PASSWORD
// and PGUSER/PGPASSWORD — so authentication needs no argument and no ExecOptions
// environment, which is the exec path that would put them on a command line.

// cypherShellCommand builds the argv for one statement against an external Neo4j
// server. --format plain keeps the output to a header and its rows, which is
// what the parsers below read.
func cypherShellCommand(uri, database, statement string) []string {
	return []string{
		"cypher-shell",
		"--non-interactive",
		"--format", "plain",
		"-a", uri,
		"-d", database,
		statement,
	}
}

// neo4jComponentsStatement reads the server's version and edition in one
// round trip.
//
// They are asked for together rather than separately because they are read from
// the same call, they are needed at the same moment, and the probe pod's
// deadline is budgeted from the number of calls the probe issues
// (externalDBProbeDeadline). Splitting them would spend a whole extra bound to
// learn a second field of a result already in hand.
const neo4jComponentsStatement = "CALL dbms.components() YIELD versions, edition RETURN versions[0] AS " +
	neo4jVersionColumn + ", edition AS " + neo4jEditionColumn

// The column names the statements above and below ask for, which are also the
// names cypher-shell prints as the result's header and therefore what the
// parsers locate that header by.
//
// They are constants shared by the statement and its parser because that is
// the only thing that keeps the two in step: a statement whose column is
// renamed and a parser still looking for the old name do not fail, they find no
// header, and "the server answered nothing" is a wrong answer that reads like a
// server problem.
const (
	neo4jVersionColumn = "version"
	neo4jEditionColumn = "edition"

	// The store size is returned as the expression itself rather than under an
	// alias, so the header carries the expression text.
	neo4jStoreSizeColumn = "attributes.Value.value"

	neo4jMemberAddressColumn = "address"
	neo4jMemberRoleColumn    = "role"
	neo4jMemberWriterColumn  = "writer"
)

// neo4jComponentsCommand runs the statement above. It runs against the system
// database because that is the one database every server has, and the target
// database may be offline at the moment the version is read.
func neo4jComponentsCommand(uri string) []string {
	return cypherShellCommand(uri, neo4jSystemDatabase, neo4jComponentsStatement)
}

// neo4jUtilityVersionCommand reads the version of the tooling that will perform
// the capture, which is the other half of the FR-006 comparison. It is read from
// the image rather than assumed from the image's tag, because a tag is a label
// an operator chose and the version is a fact about what is installed.
func neo4jUtilityVersionCommand() []string {
	return []string{"neo4j-admin", "--version"}
}

// neo4jStoreSizeStatement asks for the store size through the server's own
// metrics, which is the only interface that reports it to a client with no
// access to the store's filesystem.
//
// The store-size metric is not part of SHOW DATABASES — that clause reports the
// store *format*, not its size — so this reads the metric bean instead. Where
// the metric is not exposed, the query answers nothing and sizing falls to
// FR-022's closing clause: the run fails naming --external-db-scratch-size
// rather than guessing a size. That fallback is the reason this query is allowed
// to be the one part of the probe whose availability depends on the server's
// configuration.
func neo4jStoreSizeStatement(database string) string {
	return fmt.Sprintf(`CALL dbms.queryJmx("neo4j.metrics:name=neo4j.database.%s.store.size.total") YIELD attributes RETURN %s`, database, neo4jStoreSizeColumn)
}

// neo4jStoreSizeCommand runs the statement above against the system database.
func neo4jStoreSizeCommand(uri, database string) ([]string, error) {
	name, err := safeDatabaseIdentifier(database)
	if err != nil {
		return nil, fmt.Errorf("cannot query the store size of the external %s database: %w", serviceNeo4j, err)
	}

	return cypherShellCommand(uri, neo4jSystemDatabase, neo4jStoreSizeStatement(name)), nil
}

// neo4jMemberRolesCommand asks the server which of its members hosts the
// database being captured and in what role (FR-005).
//
// It yields the address, the role and the writer flag and nothing else. It
// deliberately does not ask, and there is nothing to ask, which member will
// serve the capture: --from tries its list in order and reports no such thing,
// so a field asserting it would be a guess presented as provenance.
func neo4jMemberRolesCommand(uri, database string) ([]string, error) {
	name, err := safeDatabaseIdentifier(database)
	if err != nil {
		return nil, fmt.Errorf("cannot query the cluster members of the external %s database: %w", serviceNeo4j, err)
	}

	statement := fmt.Sprintf("SHOW DATABASE %s YIELD %s, %s, %s", name,
		neo4jMemberAddressColumn, neo4jMemberRoleColumn, neo4jMemberWriterColumn)

	return cypherShellCommand(uri, neo4jSystemDatabase, statement), nil
}

// psqlCommand builds the argv for one statement against an external PostgreSQL
// server. -t -A -q keeps the output to the value alone, with no header, no
// alignment and no status line.
func psqlCommand(host HostPort, database, statement string) []string {
	return []string{
		"psql",
		"-tAq",
		"-h", host.Host,
		"-p", strconv.Itoa(host.Port),
		"-d", database,
		"-c", statement,
	}
}

// postgresServerVersionCommand and postgresStoreSizeCommand are the PostgreSQL
// halves of the same two questions. pg_database_size is the server's own
// accounting of the database's size, in bytes.
func postgresServerVersionCommand(host HostPort, database string) []string {
	return psqlCommand(host, database, "SHOW server_version")
}

func postgresStoreSizeCommand(host HostPort, database string) []string {
	return psqlCommand(host, database, "SELECT pg_database_size(current_database())")
}

// postgresUtilityVersionCommand reads the version of the client that will take
// the dump. pg_dump is the utility whose version FR-006 compares, not psql.
func postgresUtilityVersionCommand() []string {
	return []string{"pg_dump", "--version"}
}

// ---------------------------------------------------------------------------
// Probe parsing
// ---------------------------------------------------------------------------

// extractVersionToken pulls the version out of what a utility or a server says
// when asked for it. The shapes differ — "pg_dump (PostgreSQL) 18.1",
// "neo4j-admin 2025.10.1", a bare "5.26.1", and PostgreSQL's own
// "18.1 (Debian 18.1-1.pgdg120+1)" — and what they share is that the first
// token beginning with a digit is the version.
//
// Taking the *first* such token rather than the last is what keeps PostgreSQL's
// parenthesised packaging detail from being read as the version, which is the
// one shape where the two rules disagree.
func extractVersionToken(output string) (string, error) {
	for _, field := range strings.Fields(output) {
		if field == "" {
			continue
		}
		if r := field[0]; r >= '0' && r <= '9' {
			return field, nil
		}
	}

	return "", fmt.Errorf("no version number appears in %q", strings.TrimSpace(output))
}

// cypherResult locates a statement's result inside everything cypher-shell
// wrote, and returns the header's fields and the data lines that follow it.
//
// The header is found by the column names the statement asked for, not by
// position — not even by position among the non-empty lines. Skipping one
// non-empty line, which is what this did, handles the blank line the format
// sometimes emits and gets the *notice* case exactly wrong: cypher-shell writes
// deprecation and routing notices above the result, and one of them makes the
// notice the line that is skipped and the header the line returned as data. So
// a version reads as "version", an edition as "edition" and a store size as
// "attributes.Value.value" — each of which parses far enough to be acted on
// (the FR-006 comparison against a version token that is not one) rather than
// far enough to fail. Locating by name cannot shift: however many notices
// precede the result, the line naming the columns is the header.
//
// An empty result is not an error here — a metric that is not exposed returns no
// rows, and the caller distinguishes "the server answered nothing" from "the
// query failed" — so it reports no rows rather than refusing. A result whose
// header never appears is reported the same way, which is the safe direction:
// every caller turns no answer into a failure naming what it asked for, and
// none of them can be handed a value the run made up.
//
// The header comes back already indexed, so a caller reads its columns from
// the same index that recognised the line — and the columns it asked for are
// present by construction, which is what let the readers stop re-checking.
func cypherResult(output string, columns ...string) (cypherColumns, []string) {
	if len(columns) == 0 {
		return nil, nil
	}

	lines := nonEmptyLines(output)
	for index, line := range lines {
		if present := indexCypherColumns(splitPlainRow(line)); present.has(columns...) {
			return present, lines[index+1:]
		}
	}

	logrus.Debugf("No result header naming %s appears in the %d line(s) the server returned, so the statement is treated as having answered nothing",
		strings.Join(columns, ", "), len(lines))

	return nil, nil
}

// cypherColumns is a result header indexed so that a column can be found by
// the name the statement asked for it under.
//
// It exists because the same question had three answers. Two callers indexed
// the header the same way — lowercased and trimmed — and then read it
// differently: parseNeo4jDatabaseStatuses lowercased its column constant at the
// lookup, parseNeo4jMemberRoles used its constants raw. Raw works only for as
// long as every constant is itself already lowercase, which today's `address`,
// `role` and `writer` are. It is the *next* column that breaks it, and this is
// not a hypothetical spelling: the constant read from the same kind of result
// on the sibling path is `currentStatus`. A member column named that way would
// simply not be found, parseNeo4jMemberRoles would return nil, every member's
// role would vanish from SourceRoles, and FR-005's provenance would be silently
// empty with no error anywhere — the opposite of what recording an observed
// role is for.
//
// cypherResult and parseCypherRow answered the same question a third way, with
// strings.EqualFold at the comparison. There is one answer now, and both the
// indexing and the lookup go through cypherColumnKey, so neither side can
// normalise differently from the other again.
type cypherColumns map[string]int

// cypherColumnKey is that one normalisation. Case-insensitive and
// space-trimming, because the name a server prints under --format plain is the
// expression the statement gave it, padded to the column width.
func cypherColumnKey(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// indexCypherColumns indexes a header by position.
func indexCypherColumns(header []string) cypherColumns {
	columns := make(cypherColumns, len(header))
	for index, name := range header {
		columns[cypherColumnKey(name)] = index
	}

	return columns
}

// at is the position of a column, under whatever spelling the caller asks with.
func (columns cypherColumns) at(name string) (int, bool) {
	index, ok := columns[cypherColumnKey(name)]

	return index, ok
}

// has reports whether the header carries every column asked for — which is
// what makes one line the header of a result rather than a notice above it.
func (columns cypherColumns) has(names ...string) bool {
	for _, name := range names {
		if _, ok := columns.at(name); !ok {
			return false
		}
	}

	return true
}

// parseCypherRow reads the first data row of a statement's result under
// --format plain — which prints the column names and then the rows — and
// returns it keyed by the column names the caller asked for. Naming them is
// what tells the header from anything printed above it.
//
// Keyed rather than positional for the reason parseNeo4jMemberRoles already
// reads its columns by name: a two-column result read by position records the
// edition as the version if a server ever returns them the other way round,
// and that pair feeds the FR-006 version comparison and the FR-008 edition
// gate. The keys are the caller's own spellings, so a column constant is both
// what the statement asks for and what the answer is read back under.
func parseCypherRow(output string, columns ...string) map[string]string {
	present, rows := cypherResult(output, columns...)
	if len(rows) == 0 {
		return nil
	}

	fields := splitPlainRow(rows[0])
	row := make(map[string]string, len(columns))
	for _, column := range columns {
		if index, ok := present.at(column); ok && index < len(fields) {
			row[column] = fields[index]
		}
	}

	return row
}

// parseCypherScalar reads the single value a one-column statement returned.
func parseCypherScalar(output, column string) string {
	return parseCypherRow(output, column)[column]
}

// parseStoreSizeValue reads a byte count a server reported. A missing or
// non-numeric value is "the size could not be determined", which FR-022 turns
// into a failure naming the override rather than into a guessed size.
func parseStoreSizeValue(output string) (int64, error) {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" || strings.EqualFold(trimmed, "null") {
		return 0, errors.New("the server reported no store size")
	}

	size, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("the server reported the store size as %q, which is not a byte count", trimmed)
	}
	if size <= 0 {
		return 0, fmt.Errorf("the server reported the store size as %d bytes", size)
	}

	return size, nil
}

// observedMember is one member's role for the database being captured, as the
// server reported it at the moment of capture (FR-005).
type observedMember struct {
	// Address is the member's client address, which is what SHOW DATABASE
	// reports. It is not the backup listener's address, so matching it against
	// a supplied endpoint is done on the host.
	Address string

	// Role is primary, secondary, or one of the other values the server may
	// report — carried through as the server worded it rather than mapped onto
	// a vocabulary of this tool's own.
	Role string

	// Writer records the copy that accepts writes, which is the member a reader
	// of the metadata will care about most.
	Writer bool
}

// parseNeo4jMemberRoles reads the rows of `SHOW DATABASE … YIELD address, role,
// writer` under --format plain.
//
// It locates its columns from the header rather than by position. The statement
// names them itself, so position would work today; reading the header is what
// keeps a server that returns them in another order from silently recording
// every member's role as its address.
//
// The header itself is located by cypherResult, for the reason recorded there:
// taking the first non-empty line as the header put a notice in that position
// and then found neither column in it, so a cluster whose server had something
// to say reported no members at all.
func parseNeo4jMemberRoles(output string) []observedMember {
	columns, rows := cypherResult(output, neo4jMemberAddressColumn, neo4jMemberRoleColumn)
	if len(rows) == 0 {
		return nil
	}

	// The header was located by these two names, so both are present.
	addressAt, _ := columns.at(neo4jMemberAddressColumn)
	roleAt, _ := columns.at(neo4jMemberRoleColumn)
	writerAt, hasWriter := columns.at(neo4jMemberWriterColumn)

	members := make([]observedMember, 0, len(rows))
	for _, line := range rows {
		fields := splitPlainRow(line)
		if addressAt >= len(fields) || roleAt >= len(fields) {
			continue
		}

		member := observedMember{
			Address: fields[addressAt],
			Role:    fields[roleAt],
		}
		if member.Role == "" || strings.EqualFold(member.Role, "null") {
			member.Role = roleUnknown
		}
		if hasWriter && writerAt < len(fields) {
			member.Writer = strings.EqualFold(fields[writerAt], "true")
		}

		members = append(members, member)
	}

	return members
}

// splitPlainRow splits one --format plain row into its values, honouring the
// quotes the format puts around strings so that an address's own colon-and-port
// or a name containing a comma does not split a field in two.
func splitPlainRow(line string) []string {
	fields := []string{}
	current := strings.Builder{}
	quoted := false

	for _, r := range strings.TrimSpace(line) {
		switch {
		case r == '"':
			quoted = !quoted
		case r == ',' && !quoted:
			fields = append(fields, strings.TrimSpace(current.String()))
			current.Reset()
		default:
			current.WriteRune(r)
		}
	}

	return append(fields, strings.TrimSpace(current.String()))
}

// captureRoles maps every endpoint the run supplied to the role observed for it
// (FR-005, and `source_roles` in contracts/artifact-and-metadata.md).
//
// The keys are the endpoints as they were supplied, in try order, because those
// are what the run is accountable for having named.
//
// Matching takes the address whole first and falls back to the host. Whole is
// the stronger evidence and it is sometimes available — a PostgreSQL endpoint
// travels over the same port the server reports, and an operator may have
// supplied Neo4j client addresses — while the host-only fallback is what makes
// the ordinary Neo4j case work at all: a supplied endpoint there carries the
// backup listener's port and the server reports its client address, so
// comparing the two whole is comparing two different ports on one machine.
//
// The fallback is only sound while a host holds one instance, and this keys by
// host to a *set* for that reason. Two instances on one host — a colocated
// pair, or the same server reported twice — collapsed into whichever the
// listing mentioned last, so a supplied endpoint could be recorded as
// `secondary` when the run had read a primary at that host. That is the field
// FR-005 says bears on the recovery point, wrong in the artefact's own
// provenance, with nothing in the run to contradict it. Where the instances at
// a host disagree the answer is roleUnknown and the ambiguity is logged, since
// "the run could not tell which of these it named" is a fact and a coin toss
// between primary and secondary is not. Where they agree the role is recorded,
// because then it does not matter which of them the endpoint was.
//
// An endpoint no member matched gets roleUnknown, which is a value rather than
// an absence. Nothing here says which endpoint served the capture, because
// nothing observed it.
func captureRoles(targets []HostPort, members []observedMember) map[string]string {
	if len(targets) == 0 {
		return nil
	}

	byAddress := map[string]observedMember{}
	byHost := map[string][]observedMember{}
	for _, member := range members {
		address := strings.TrimSpace(member.Address)
		byAddress[strings.ToLower(address)] = member

		host := address
		if parsed, _, err := net.SplitHostPort(address); err == nil {
			host = parsed
		}
		key := strings.ToLower(host)
		byHost[key] = append(byHost[key], member)
	}

	roles := make(map[string]string, len(targets))
	for _, target := range targets {
		roles[target.String()] = observedRoleFor(target, byAddress, byHost)
	}

	return roles
}

// observedRoleFor is the per-endpoint half of captureRoles: the role the run
// may record for one supplied endpoint, or roleUnknown where the evidence does
// not single one out.
func observedRoleFor(target HostPort, byAddress map[string]observedMember, byHost map[string][]observedMember) string {
	if member, ok := byAddress[strings.ToLower(target.String())]; ok && member.Role != "" {
		return member.Role
	}

	// The empty string is the "nothing chosen yet" state rather than
	// roleUnknown, because roleUnknown is also a role a member can be reported
	// in: parseNeo4jMemberRoles maps a null role onto it. Starting from it made
	// the answer depend on the order the server happened to list the instances
	// in — a null before a primary recorded `primary`, the same pair the other
	// way round recorded `unknown`.
	chosen := ""
	for _, member := range byHost[strings.ToLower(target.Host)] {
		switch {
		case member.Role == "":
			continue
		case chosen == "":
			chosen = member.Role
		case !strings.EqualFold(chosen, member.Role):
			logrus.Warnf("The server reports more than one instance at %s in different roles, so no role is recorded for the endpoint %s: a role attributed to the wrong instance would misstate the recovery point", target.Host, target)

			return roleUnknown
		}
	}

	if chosen == "" {
		return roleUnknown
	}

	return chosen
}

// describeCaptureRoles renders the observed roles in try order, for the line the
// operator reads before a capture starts. It is deliberately ordered by the
// endpoint list rather than by map iteration, so two runs against the same
// cluster print the same line.
func describeCaptureRoles(targets []HostPort, roles map[string]string) string {
	rendered := make([]string, 0, len(targets))
	for _, target := range targets {
		role := roles[target.String()]
		if role == "" {
			role = roleUnknown
		}
		rendered = append(rendered, target.String()+"="+role)
	}

	return strings.Join(rendered, ",")
}
