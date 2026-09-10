package app

import (
	"strings"
	"testing"
)

// The local port is read back from kubectl's own announcement rather than
// chosen here, so the pattern that reads it is what decides whether the
// task-manager component works on Kubernetes at all. A forward that opened but
// was never recognised would time out and be reported as unreachable.
func TestPortForwardReadyParsesKubectlsAnnouncement(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{
			name: "IPv4 loopback, the usual case",
			line: "Forwarding from 127.0.0.1:54321 -> 5432",
			want: "54321",
		},
		{
			name: "IPv6 loopback, which kubectl also binds",
			line: "Forwarding from [::1]:54321 -> 5432",
			want: "54321",
		},
		{
			name: "a line kubectl prints per connection, not on start",
			line: "Handling connection for 54321",
			want: "",
		},
		{
			name: "the error a taken port produces",
			line: "Unable to listen on port 5432: address already in use",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := portForwardReady.FindStringSubmatch(tc.line)
			got := ""
			if m != nil {
				got = m[1]
			}
			if got != tc.want {
				t.Errorf("port from %q = %q, want %q", tc.line, got, tc.want)
			}
		})
	}
}

// A missing PostgreSQL client has to be reported before anything is stopped or
// written, and the message has to say what to install: the connector itself
// would otherwise surface it as an exec failure partway through a restore, with
// the deployment already quiesced.
func TestRequirePostgresClientNamesWhatIsMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	err := requirePostgresClient()
	if err == nil {
		t.Fatal("an empty PATH produced no error")
	}
	for _, bin := range postgresClientBinaries {
		if !strings.Contains(err.Error(), bin) {
			t.Errorf("the error does not name %s: %v", bin, err)
		}
	}
	if !strings.Contains(err.Error(), "postgresql-client") {
		t.Errorf("the error does not say what to install: %v", err)
	}
	if !strings.Contains(err.Error(), "--exclude-task-manager") {
		t.Errorf("the error does not offer the way past it: %v", err)
	}
}
