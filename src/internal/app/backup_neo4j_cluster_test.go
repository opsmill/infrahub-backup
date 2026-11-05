package app

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	clusterStatusHeader = "currentStatus, address, statusMessage\n"
	testOnlineTimeout   = 20 * time.Millisecond
	testPollInterval    = time.Millisecond

	restoredServerID = "restored-server-id"
	leaderServerID   = "leader-server-id"

	cypher25RefusedLegacyOption = "22N04: data exception - invalid input value. Invalid input 'existingdataseedinstance' for 'OPTIONS'. " +
		"Expected 'existingData', 'existingDataSeedServer', 'replicaConfig', 'seedConfig', 'seedRestoreUntil', 'seedSourceDatabase', 'seedURI', 'storeFormat' or 'txLogEnrichment'."
)

// neo4jClusterBackend answers the queries of the Neo4j cluster restore the way a three-server
// cluster does when the restore runs on a server that does not lead the system database.
// statusOutputs are the successive answers to the database status query; the last one repeats.
type neo4jClusterBackend struct {
	bareBackend

	statusOutputs []string
	statusErr     error
	statusPolls   int

	// cypher25 refuses the legacy seed option, as Neo4j does when Cypher 25 is the default.
	cypher25 bool
	// createErr fails every CREATE DATABASE with createOutput.
	createErr    error
	createOutput string
	creates      []string
}

func (b *neo4jClusterBackend) Exec(service string, command []string, opts *ExecOptions) (string, error) {
	joined := strings.Join(command, " ")

	switch {
	case strings.Contains(joined, "dbms.cluster.statusCheck"):
		requester := leaderServerID
		if strings.Contains(joined, "-a "+neo4jLocalBoltAddress) {
			requester = restoredServerID
		}
		return "requester, serverId\nTRUE, \"" + requester + "\"\nFALSE, \"third-server-id\"\n", nil
	case strings.Contains(joined, "CREATE DATABASE"):
		b.creates = append(b.creates, joined)
		if b.createErr != nil {
			return b.createOutput, b.createErr
		}
		if b.cypher25 && strings.Contains(joined, neo4jLegacySeedServerOption) {
			return cypher25RefusedLegacyOption, errors.New("exit status 1")
		}
		return "", nil
	case strings.Contains(joined, "SHOW DATABASE"):
		b.statusPolls++
		if b.statusErr != nil {
			return "Unable to get a routing table for database 'neo4j' because this database is unavailable", b.statusErr
		}
		return b.statusOutputs[min(b.statusPolls, len(b.statusOutputs))-1], nil
	}

	return "", nil
}

func newNeo4jClusterTestOps(backend *neo4jClusterBackend) *InfrahubOps {
	iops := newNeo4jTestOps(backend)
	iops.config.Neo4jClusterOnlineTimeout = testOnlineTimeout

	return iops
}

func allOnlineStatus() string {
	return clusterStatusHeader +
		`"online", "neo4j-0:7687", ""` + "\n" +
		`"online", "neo4j-1:7687", ""` + "\n" +
		`"online", "neo4j-2:7687", ""` + "\n"
}

func TestWaitForNeo4jClusterDatabaseOnlineWaitsForEveryServer(t *testing.T) {
	backend := &neo4jClusterBackend{statusOutputs: []string{
		clusterStatusHeader +
			`"online", "neo4j-0:7687", ""` + "\n" +
			`"store copying", "neo4j-1:7687", ""` + "\n" +
			`"starting", "neo4j-2:7687", ""` + "\n",
		allOnlineStatus(),
	}}
	iops := newNeo4jClusterTestOps(backend)

	if err := iops.waitForNeo4jClusterDatabaseOnline(time.Second, testPollInterval); err != nil {
		t.Fatalf("waitForNeo4jClusterDatabaseOnline() = %v, want nil", err)
	}
	if backend.statusPolls != 2 {
		t.Errorf("status polled %d times, want 2: one server online is not the database online", backend.statusPolls)
	}
}

func TestWaitForNeo4jClusterDatabaseOnlineFailsAfterTheTimeout(t *testing.T) {
	backend := &neo4jClusterBackend{statusOutputs: []string{
		clusterStatusHeader +
			`"online", "neo4j-0:7687", ""` + "\n" +
			`"offline", "neo4j-1:7687", "Unable to start database, store copy \"failed\""` + "\n",
	}}
	iops := newNeo4jClusterTestOps(backend)

	err := iops.waitForNeo4jClusterDatabaseOnline(testOnlineTimeout, testPollInterval)

	if err == nil {
		t.Fatalf("waitForNeo4jClusterDatabaseOnline() = nil, want an error")
	}
	for _, want := range []string{
		"neo4j-0:7687: online",
		`neo4j-1:7687: offline (Unable to start database, store copy "failed")`,
		testOnlineTimeout.String(),
		"--neo4j-cluster-online-timeout",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestWaitForNeo4jClusterDatabaseOnlineLogsTheReasonForEachServer(t *testing.T) {
	backend := &neo4jClusterBackend{statusOutputs: []string{
		clusterStatusHeader +
			`"online", "neo4j-0:7687", ""` + "\n" +
			`"offline", "neo4j-1:7687", "No store found on any of the seeders [ServerId{c2f29e54}]."` + "\n" +
			`"store copying", "neo4j-2:7687", ""` + "\n",
	}}
	iops := newNeo4jClusterTestOps(backend)

	logs := captureLogrus(t, func() {
		_ = iops.waitForNeo4jClusterDatabaseOnline(testOnlineTimeout, testPollInterval)
	})

	for _, want := range []string{
		`level=warning msg="Database neo4j is not online on every server yet`,
		`level=error msg="Database neo4j is not online on this server" reason="No store found on any of the seeders [ServerId{c2f29e54}]." server="neo4j-1:7687" status=offline`,
		`level=error msg="Database neo4j is not online on this server" reason="Neo4j gave no status message" server="neo4j-2:7687" status="store copying"`,
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs do not contain %q:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, `server="neo4j-0:7687"`) {
		t.Errorf("logs report the server where the database is online as failing:\n%s", logs)
	}
	if count := strings.Count(logs, "is not online on every server yet"); count != 1 {
		t.Errorf("the unchanged status was logged %d times, want once:\n%s", count, logs)
	}
}

func TestWaitForNeo4jClusterDatabaseOnlineReportsTheFailedStatusQuery(t *testing.T) {
	queryFailure := errors.New("exit status 1")
	backend := &neo4jClusterBackend{statusErr: queryFailure}
	iops := newNeo4jClusterTestOps(backend)

	var err error
	logs := captureLogrus(t, func() {
		err = iops.waitForNeo4jClusterDatabaseOnline(testOnlineTimeout, testPollInterval)
	})

	if !errors.Is(err, queryFailure) {
		t.Fatalf("error = %v, want it to wrap %v", err, queryFailure)
	}
	if !strings.Contains(err.Error(), "Unable to get a routing table") {
		t.Errorf("error = %q, want it to include the cypher-shell output", err)
	}
	if count := strings.Count(logs, "Failed to read the status of database neo4j"); count != 1 {
		t.Errorf("the unchanged status query failure was logged %d times, want once:\n%s", count, logs)
	}
}

func TestWaitForNeo4jClusterDatabaseOnlineFailsWhenNoServerHostsTheDatabase(t *testing.T) {
	backend := &neo4jClusterBackend{statusOutputs: []string{clusterStatusHeader}}
	iops := newNeo4jClusterTestOps(backend)

	err := iops.waitForNeo4jClusterDatabaseOnline(testOnlineTimeout, testPollInterval)

	if err == nil || !strings.Contains(err.Error(), "no server hosts the database") {
		t.Fatalf("error = %v, want one saying no server hosts the database", err)
	}
}

// TestRestoreNeo4jClusterFailsWhenTheDatabaseStaysOffline covers the restore reporting success
// while the restored database was still unavailable, which left the next step to fail on it.
func TestRestoreNeo4jClusterFailsWhenTheDatabaseStaysOffline(t *testing.T) {
	backend := &neo4jClusterBackend{statusOutputs: []string{
		clusterStatusHeader + `"starting", "neo4j-0:7687", ""` + "\n",
	}}
	iops := newNeo4jClusterTestOps(backend)

	err := iops.restoreNeo4jCluster(nil)

	if err == nil {
		t.Fatalf("restoreNeo4jCluster() = nil, want an error")
	}
	if !strings.Contains(err.Error(), "neo4j-0:7687: starting") {
		t.Errorf("error = %q, want it to name the server status", err)
	}
}

// TestRestoreNeo4jClusterSeedsFromTheServerThatHoldsTheRestoredStore covers seeding from the
// system database leader, which has no store when the restore ran on another server.
func TestRestoreNeo4jClusterSeedsFromTheServerThatHoldsTheRestoredStore(t *testing.T) {
	backend := &neo4jClusterBackend{statusOutputs: []string{allOnlineStatus()}}
	iops := newNeo4jClusterTestOps(backend)

	if err := iops.restoreNeo4jCluster(nil); err != nil {
		t.Fatalf("restoreNeo4jCluster() = %v, want nil", err)
	}
	if len(backend.creates) != 1 {
		t.Fatalf("CREATE DATABASE ran %d times, want once: %v", len(backend.creates), backend.creates)
	}
	if !strings.Contains(backend.creates[0], "'"+restoredServerID+"'") {
		t.Errorf("CREATE DATABASE = %q, want it to seed from %s", backend.creates[0], restoredServerID)
	}
}

func TestCreateSeededNeo4jDatabaseKeepsTheLegacyOptionWhenNeo4jAcceptsIt(t *testing.T) {
	backend := &neo4jClusterBackend{}
	iops := newNeo4jClusterTestOps(backend)

	if err := iops.createSeededNeo4jDatabase(restoredServerID); err != nil {
		t.Fatalf("createSeededNeo4jDatabase() = %v, want nil", err)
	}
	if len(backend.creates) != 1 || !strings.Contains(backend.creates[0], neo4jLegacySeedServerOption+": '"+restoredServerID+"'") {
		t.Errorf("CREATE DATABASE calls = %v, want one using %s", backend.creates, neo4jLegacySeedServerOption)
	}
}

func TestCreateSeededNeo4jDatabaseUsesTheNewOptionWhenCypher25RefusesTheLegacyOne(t *testing.T) {
	backend := &neo4jClusterBackend{cypher25: true}
	iops := newNeo4jClusterTestOps(backend)

	if err := iops.createSeededNeo4jDatabase(restoredServerID); err != nil {
		t.Fatalf("createSeededNeo4jDatabase() = %v, want nil", err)
	}
	if len(backend.creates) != 2 {
		t.Fatalf("CREATE DATABASE ran %d times, want twice: %v", len(backend.creates), backend.creates)
	}
	if !strings.Contains(backend.creates[1], neo4jSeedServerOption+": '"+restoredServerID+"'") {
		t.Errorf("second CREATE DATABASE = %q, want it to use %s", backend.creates[1], neo4jSeedServerOption)
	}
}

func TestCreateSeededNeo4jDatabaseReportsOtherFailuresWithoutRetrying(t *testing.T) {
	createFailure := errors.New("exit status 1")
	backend := &neo4jClusterBackend{
		createErr:    createFailure,
		createOutput: "Failed to create the specified database 'neo4j': Database already exists.",
	}
	iops := newNeo4jClusterTestOps(backend)

	err := iops.createSeededNeo4jDatabase(restoredServerID)

	if !errors.Is(err, createFailure) {
		t.Fatalf("error = %v, want it to wrap %v", err, createFailure)
	}
	if !strings.Contains(err.Error(), "Database already exists") {
		t.Errorf("error = %q, want it to include the cypher-shell output", err)
	}
	if len(backend.creates) != 1 {
		t.Errorf("CREATE DATABASE ran %d times, want once: %v", len(backend.creates), backend.creates)
	}
}
