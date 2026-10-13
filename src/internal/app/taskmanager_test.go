package app

import (
	"reflect"
	"testing"
)

// Help texts of `infrahub tasks flush flow-runs`, printed with COLUMNS=100 by the Infrahub 1.11.4 image and by a
// build of the change that ships in Infrahub 1.13.
const (
	flowRunsHelpBefore113 = ` Usage: infrahub tasks flush flow-runs [OPTIONS] [CONFIG_FILE]

 Flush old task runs.

╭─ Arguments ──────────────────────────────────────────────────────────────────────────────────────╮
│   config_file      [CONFIG_FILE]  [env var: INFRAHUB_CONFIG] [default: infrahub.toml]            │
╰──────────────────────────────────────────────────────────────────────────────────────────────────╯
╭─ Options ────────────────────────────────────────────────────────────────────────────────────────╮
│ --days-to-keep        INTEGER  [default: 30]                                                     │
│ --batch-size          INTEGER  [default: 100]                                                    │
│ --help                         Show this message and exit.                                       │
╰──────────────────────────────────────────────────────────────────────────────────────────────────╯`

	flowRunsHelpFrom113 = ` Usage: infrahub tasks flush flow-runs [OPTIONS] [CONFIG_FILE]

 Delete the finished task runs older than the task history retention, with their logs and
 artifacts.

 Raises:     typer.Exit: With code 1 when the cleanup fails.

╭─ Arguments ──────────────────────────────────────────────────────────────────────────────────────╮
│   config_file      [CONFIG_FILE]  [env var: INFRAHUB_CONFIG] [default: infrahub.toml]            │
╰──────────────────────────────────────────────────────────────────────────────────────────────────╯
╭─ Options ────────────────────────────────────────────────────────────────────────────────────────╮
│ --rewrite          After the deletes, rewrite each task history and activity log table whose     │
│                    live rows hold less than half of its disk space, to return that space to the  │
│                    disk. Tasks that write to a table wait while it is rewritten.                 │
│ --help             Show this message and exit.                                                   │
╰──────────────────────────────────────────────────────────────────────────────────────────────────╯`
)

func TestTaskManagerOwnsRetention(t *testing.T) {
	cases := []struct {
		name string
		help string
		want bool
	}{
		{name: "before 1.13", help: flowRunsHelpBefore113, want: false},
		{name: "from 1.13", help: flowRunsHelpFrom113, want: true},
		{name: "no help", help: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := taskManagerOwnsRetention(tc.help); got != tc.want {
				t.Fatalf("taskManagerOwnsRetention() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFlowRunsCommand(t *testing.T) {
	cases := []struct {
		name    string
		request FlowRunsRequest
		want    []string
	}{
		{
			name:    "without rewrite",
			request: FlowRunsRequest{DaysToKeep: 7, BatchSize: 200, ArgumentsGiven: true},
			want:    []string{"infrahub", "tasks", "flush", "flow-runs"},
		},
		{
			name:    "with rewrite",
			request: FlowRunsRequest{DaysToKeep: 30, BatchSize: 200, Rewrite: true},
			want:    []string{"infrahub", "tasks", "flush", "flow-runs", "--rewrite"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := flowRunsCommand(tc.request); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("flowRunsCommand() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIgnoredArgumentsWarning(t *testing.T) {
	cases := []struct {
		name    string
		request FlowRunsRequest
		want    string
	}{
		{
			name:    "days given",
			request: FlowRunsRequest{DaysToKeep: 7, BatchSize: 200, ArgumentsGiven: true},
			want: "This Infrahub release deletes the flow runs older than the task history retention set on the task " +
				"manager, not the 7 days given; to keep 7 days, set INFRAHUB_TASK_MANAGER_RETENTION_TASK_HISTORY=7d " +
				"on the task manager",
		},
		{
			name:    "nothing given",
			request: FlowRunsRequest{DaysToKeep: 30, BatchSize: 200},
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ignoredArgumentsWarning(tc.request); got != tc.want {
				t.Fatalf("ignoredArgumentsWarning() = %q, want %q", got, tc.want)
			}
		})
	}
}
