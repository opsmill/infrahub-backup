// Package neo4jconn parses the connector configuration / location URI for the
// Neo4j Plakar integration and locates the neo4j-admin binary.
package neo4jconn

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
)

// DefaultBackupPort is the default Neo4j Enterprise online-backup service port.
const DefaultBackupPort = "6362"

// ConnConfig holds the parsed connection parameters for a Neo4j backup/restore.
type ConnConfig struct {
	// Proto is the registered protocol: "neo4j" (Enterprise online backup) or
	// "neo4j+offline" (Community offline dump).
	Proto string
	Host  string
	Port  string // backup-service port for online; unused for offline
	// Username/Password are parsed from the URI for completeness but are NOT
	// passed to neo4j-admin: online backup/restore authenticate over the backup
	// service (port 6362), not via Bolt credentials, and the connector issues no
	// Bolt queries. Reserved for a future Bolt-based metadata enhancement.
	Username string
	Password string
	Database string
	// DataDir is the path to the Neo4j home/data directory (offline only).
	DataDir string
	// AdminBin overrides the neo4j-admin binary name.
	AdminBin string
	// BinDir overrides the directory used to locate neo4j-admin (else $PATH).
	BinDir string
}

func (cc ConnConfig) adminBin() string {
	if cc.AdminBin != "" {
		return cc.AdminBin
	}
	return "neo4j-admin"
}

// BinPath resolves the neo4j-admin binary against BinDir, or returns it for $PATH lookup.
func (cc ConnConfig) BinPath() string {
	if cc.BinDir != "" {
		return filepath.Join(cc.BinDir, cc.adminBin())
	}
	return cc.adminBin()
}

// Offline reports whether this is the Community offline-dump protocol.
func (cc ConnConfig) Offline() bool { return cc.Proto == "neo4j+offline" }

// Origin returns a stable identity for the backup source. Online is keyed on
// host; offline (which has no host) is keyed on the data directory, so two
// distinct offline datadirs backing up the same database name do not collide.
func (cc ConnConfig) Origin() string {
	loc := cc.Host
	if cc.Offline() {
		loc = cc.DataDir
	}
	return cc.Proto + "://" + loc + "/" + cc.Database
}

// ParseConnConfig builds a ConnConfig from the connector configuration map.
// Standalone keys take precedence over the location URI.
func ParseConnConfig(proto string, config map[string]string) (ConnConfig, error) {
	cc := ConnConfig{Proto: proto, Port: DefaultBackupPort}

	if location, ok := config["location"]; ok && location != "" {
		if err := parseURI(location, &cc); err != nil {
			return cc, fmt.Errorf("parsing location URI: %w", err)
		}
	}
	if v := config["host"]; v != "" {
		cc.Host = v
	}
	if v := config["port"]; v != "" {
		cc.Port = v
	}
	if v := config["username"]; v != "" {
		cc.Username = v
	}
	if v := config["password"]; v != "" {
		cc.Password = v
	}
	if v := config["database"]; v != "" {
		cc.Database = v
	}
	if v := config["data_dir"]; v != "" {
		cc.DataDir = v
	}
	if v := config["neo4j_admin_path"]; v != "" {
		cc.AdminBin = v
	}
	if v := config["neo4j_bin_dir"]; v != "" {
		cc.BinDir = v
	}
	if cc.Database == "" {
		cc.Database = "neo4j" // Neo4j's default database name
	}
	return cc, nil
}

func parseURI(uri string, cc *ConnConfig) error {
	if !strings.HasPrefix(uri, "neo4j") {
		return fmt.Errorf("unsupported URI scheme in %q: expected neo4j:// or neo4j+offline://", uri)
	}
	// url.Parse accepts "+" in the scheme (RFC 3986), so neo4j+offline parses directly.
	u, err := url.Parse(uri)
	if err != nil {
		return fmt.Errorf("invalid URI %q: %w", uri, err)
	}
	if h := u.Hostname(); h != "" {
		cc.Host = h
	}
	if p := u.Port(); p != "" {
		cc.Port = p
	}
	if u.User != nil {
		if name := u.User.Username(); name != "" {
			cc.Username = name
		}
		if pass, ok := u.User.Password(); ok && pass != "" {
			cc.Password = pass
		}
	}
	if cc.Offline() {
		if u.Path != "" {
			cc.DataDir = u.Path
		}
		if db := u.Query().Get("database"); db != "" {
			cc.Database = db
		}
	} else if p := strings.TrimPrefix(u.Path, "/"); p != "" {
		cc.Database = p
	}
	return nil
}

// Ping verifies the neo4j-admin binary is available (a cheap pre-flight).
//
// NOTE (behavior verification pending): a stronger online Ping would probe the
// backup service at Host:Port; a stronger offline Ping would stat DataDir and
// confirm the database is stopped. Implement once a testcontainers harness exists.
func (cc ConnConfig) Ping(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, cc.BinPath(), "--version")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("neo4j-admin not available (%s): %w: %s", cc.BinPath(), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ServerVersion returns the neo4j-admin --version string (best-effort, "" on error).
func (cc ConnConfig) ServerVersion(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, cc.BinPath(), "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
