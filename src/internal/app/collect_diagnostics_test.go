package app

import (
	"strings"
	"testing"
)

func TestCollectPlanOrdering(t *testing.T) {
	iops := NewInfrahubOps()
	plan := iops.collectPlan(CollectOptions{})

	want := []string{
		"logs/infrahub-server",
		"logs/task-worker",
		"logs/database",
		"logs/message-queue",
		"logs/cache",
		"logs/task-manager",
		"logs/task-manager-db",
		"logs/task-manager-background-svc",
		"database-logs",
		"message-queue-status",
		"cache-status",
		"task-worker-state",
		"task-manager-state",
		"server-info",
		"metrics",
		// Opt-in extras stay last: the benchmark generates load, and the
		// include-backup collector may stop/restart application containers
		// (inherited backup behavior), so backup stays last of all.
		"benchmark",
		"backup",
	}

	if len(plan) != len(want) {
		names := make([]string, len(plan))
		for i, c := range plan {
			names[i] = c.name
		}
		t.Fatalf("plan has %d collectors %v, want %d %v", len(plan), names, len(want), want)
	}
	for i, name := range want {
		if plan[i].name != name {
			t.Errorf("plan[%d].name = %q, want %q", i, plan[i].name, name)
		}
		if plan[i].run == nil {
			t.Errorf("plan[%d] (%s) has no run function", i, name)
		}
	}

	// Every service-bound collector must carry an absent-service skip
	// precondition; metrics applies to the whole deployment and has none.
	for i, c := range plan {
		wantSkip := c.name != "metrics"
		if (c.skip != nil) != wantSkip {
			t.Errorf("plan[%d] (%s) skip precondition presence = %v, want %v", i, c.name, c.skip != nil, wantSkip)
		}
	}
}

func TestSkipWhenServiceAbsent(t *testing.T) {
	tests := []struct {
		name       string
		cc         *collectContext
		service    string
		wantSkip   bool
		wantReason string
	}{
		{
			name:       "service with replicas is not skipped",
			cc:         &collectContext{backend: newFakeCollectBackend()},
			service:    "infrahub-server",
			wantSkip:   false,
			wantReason: "",
		},
		{
			name:       "service without replicas is skipped",
			cc:         &collectContext{backend: newFakeCollectBackend()},
			service:    "task-manager-background-svc",
			wantSkip:   true,
			wantReason: "service not deployed",
		},
		{
			name:       "backend without collect primitives defers to run",
			cc:         &collectContext{backend: &bareBackend{name: "docker"}},
			service:    "cache",
			wantSkip:   false,
			wantReason: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skip, reason := skipWhenServiceAbsent(tt.service)(tt.cc)
			if skip != tt.wantSkip || reason != tt.wantReason {
				t.Errorf("skipWhenServiceAbsent(%q) = (%v, %q), want (%v, %q)",
					tt.service, skip, reason, tt.wantSkip, tt.wantReason)
			}
		})
	}
}

func TestMessageQueueDumps(t *testing.T) {
	dumps := messageQueueDumps()

	wantFiles := []string{
		"queues.txt", "exchanges.txt", "bindings.txt",
		"connections.txt", "channels.txt", "status.txt", "environment.txt",
	}
	if len(dumps) != len(wantFiles) {
		t.Fatalf("messageQueueDumps() has %d dumps, want %d", len(dumps), len(wantFiles))
	}

	masked := map[string]bool{"status.txt": true, "environment.txt": true}
	for i, dump := range dumps {
		if dump.filename != wantFiles[i] {
			t.Errorf("dumps[%d].filename = %q, want %q", i, dump.filename, wantFiles[i])
		}
		if dump.command[0] != "rabbitmqctl" {
			t.Errorf("dumps[%d] (%s) command = %v, want a rabbitmqctl invocation", i, dump.filename, dump.command)
		}
		if masked[dump.filename] {
			if dump.mask == nil {
				t.Errorf("dumps[%d] (%s) has no mask, want Erlang-config masking (research R5)", i, dump.filename)
				continue
			}
			got := dump.mask(`{default_pass,<<"guest">>}`)
			if want := "{default_pass," + maskedValue + "}"; got != want {
				t.Errorf("dumps[%d] (%s) mask output = %q, want %q", i, dump.filename, got, want)
			}
		} else if dump.mask != nil {
			t.Errorf("dumps[%d] (%s) is masked, want raw output", i, dump.filename)
		}
	}
}

func TestCacheDumps(t *testing.T) {
	dumps := cacheDumps()

	wantFiles := []string{"info.txt", "clients.txt", "config.txt", "slowlog.txt", "dbsize.txt"}
	if len(dumps) != len(wantFiles) {
		t.Fatalf("cacheDumps() has %d dumps, want %d", len(dumps), len(wantFiles))
	}

	for i, dump := range dumps {
		if dump.filename != wantFiles[i] {
			t.Errorf("dumps[%d].filename = %q, want %q", i, dump.filename, wantFiles[i])
		}
		if dump.command[0] != "redis-cli" {
			t.Errorf("dumps[%d] (%s) command = %v, want a redis-cli invocation", i, dump.filename, dump.command)
		}
		if dump.filename == "config.txt" {
			if dump.mask == nil {
				t.Errorf("config.txt has no mask, want key/value-pair masking (research R5)")
				continue
			}
			got := dump.mask("requirepass\nhunter2")
			if want := "requirepass\n" + maskedValue; got != want {
				t.Errorf("config.txt mask output = %q, want %q", got, want)
			}
		} else if dump.mask != nil {
			t.Errorf("dumps[%d] (%s) is masked, want raw output", i, dump.filename)
		}
	}
}

func TestTaskWorkerDumps(t *testing.T) {
	dumps := taskWorkerDumps()

	wantFiles := []string{"version.txt", "config.txt", "work-pools.txt"}
	if len(dumps) != len(wantFiles) {
		t.Fatalf("taskWorkerDumps() has %d dumps, want %d", len(dumps), len(wantFiles))
	}

	for i, dump := range dumps {
		if dump.filename != wantFiles[i] {
			t.Errorf("dumps[%d].filename = %q, want %q", i, dump.filename, wantFiles[i])
		}
		if dump.command[0] != "prefect" {
			t.Errorf("dumps[%d] (%s) command = %v, want a prefect invocation", i, dump.filename, dump.command)
		}
	}

	// The worker configuration can carry Prefect API credentials.
	config := dumps[1]
	if config.mask == nil {
		t.Fatal("config.txt has no mask, want env-style masking (research R5)")
	}
	got := config.mask("PREFECT_API_KEY='abc123'")
	if want := "PREFECT_API_KEY=" + maskedValue; got != want {
		t.Errorf("config.txt mask output = %q, want %q", got, want)
	}
}

func TestPrefectServerCommand(t *testing.T) {
	cmd := prefectServerCommand("prefect", "work-pool", "ls")

	if len(cmd) != 7 {
		t.Fatalf("prefectServerCommand produced %d elements %v, want 7", len(cmd), cmd)
	}
	if cmd[0] != "sh" || cmd[1] != "-c" {
		t.Errorf("command prefix = %v, want a sh -c wrapper", cmd[:2])
	}
	script := cmd[2]
	if !strings.Contains(script, `${PREFECT_API_URL:-`+prefectServerAPIFallback+`}`) {
		t.Errorf("wrapper script %q does not default PREFECT_API_URL to %s", script, prefectServerAPIFallback)
	}
	if !strings.Contains(script, `exec "$@"`) {
		t.Errorf("wrapper script %q does not exec the wrapped command", script)
	}
	wantTail := []string{"sh", "prefect", "work-pool", "ls"}
	for i, arg := range wantTail {
		if cmd[3+i] != arg {
			t.Errorf("cmd[%d] = %q, want %q", 3+i, cmd[3+i], arg)
		}
	}
}

func TestTaskManagerDumps(t *testing.T) {
	dumps := taskManagerDumps()

	wantFiles := []string{"work-pools.txt", "work-queues.txt", "flow-runs.txt", "automations.txt"}
	if len(dumps) != len(wantFiles) {
		t.Fatalf("taskManagerDumps() has %d dumps, want %d", len(dumps), len(wantFiles))
	}

	for i, dump := range dumps {
		if dump.filename != wantFiles[i] {
			t.Errorf("dumps[%d].filename = %q, want %q", i, dump.filename, wantFiles[i])
		}
		if dump.command[0] != "sh" || !contains(dump.command, "prefect") {
			t.Errorf("dumps[%d] (%s) command = %v, want a wrapped prefect invocation", i, dump.filename, dump.command)
		}
	}
}

func TestServerAPITargets(t *testing.T) {
	targets := serverAPITargets()

	want := []struct {
		filename string
		path     string
		masked   bool
	}{
		{"info.json", "/api/info", false},
		{"config.json", "/api/config", true},
		{"schema.json", "/api/schema", false},
	}
	if len(targets) != len(want) {
		t.Fatalf("serverAPITargets() has %d targets, want %d", len(targets), len(want))
	}

	for i, tt := range want {
		target := targets[i]
		if target.filename != tt.filename || target.path != tt.path {
			t.Errorf("targets[%d] = {%q %q}, want {%q %q}", i, target.filename, target.path, tt.filename, tt.path)
		}
		if (target.mask != nil) != tt.masked {
			t.Errorf("targets[%d] (%s) mask presence = %v, want %v", i, tt.filename, target.mask != nil, tt.masked)
			continue
		}
		if tt.masked {
			got := target.mask(`{"security":{"secret_key":"abc123"}}`)
			if strings.Contains(got, "abc123") || !strings.Contains(got, maskedValue) {
				t.Errorf("config mask output %q still leaks the secret", got)
			}
		}
	}
}

func TestServerAPIFetchCommand(t *testing.T) {
	url := "http://infrahub-server:8000/api/config"
	cmd := serverAPIFetchCommand(url)

	if len(cmd) != 4 || cmd[0] != "python" || cmd[1] != "-c" {
		t.Fatalf("serverAPIFetchCommand = %v, want a python -c invocation with the URL argument", cmd)
	}
	if cmd[3] != url {
		t.Errorf("URL argument = %q, want %q", cmd[3], url)
	}
	script := cmd[2]
	for _, fragment := range []string{"httpx", "sys.argv[1]", "resp.status_code >= 400", "sys.exit(1)"} {
		if !strings.Contains(script, fragment) {
			t.Errorf("fetch script missing %q:\n%s", fragment, script)
		}
	}
}

func TestReplicaBaseName(t *testing.T) {
	tests := []struct {
		name           string
		replica        Replica
		multiContainer bool
		want           string
	}{
		{
			name:    "docker container name",
			replica: Replica{Service: "task-worker", Container: "infrahub-task-worker-1"},
			want:    "infrahub-task-worker-1",
		},
		{
			name:    "kubernetes single-container pod",
			replica: Replica{Service: "task-worker", Pod: "task-worker-abc", Container: "task-worker"},
			want:    "task-worker-abc",
		},
		{
			name:           "kubernetes multi-container pod",
			replica:        Replica{Service: "database", Pod: "database-0", Container: "sidecar"},
			multiContainer: true,
			want:           "database-0_sidecar",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := replicaBaseName(tt.replica, tt.multiContainer); got != tt.want {
				t.Errorf("replicaBaseName(%+v, %v) = %q, want %q", tt.replica, tt.multiContainer, got, tt.want)
			}
		})
	}
}
