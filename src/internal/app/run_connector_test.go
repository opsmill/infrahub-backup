package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withStdin replaces os.Stdin with a file holding content for the duration of the
// test, which is how the worker's end of the credentials channel is exercised.
func withStdin(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdin
	os.Stdin = f
	t.Cleanup(func() {
		os.Stdin = original
		_ = f.Close()
	})
}

// The worker has to read back exactly what the orchestrator sent, and must not
// echo the payload into an error message when it cannot.
func TestCredentialsInputRead(t *testing.T) {
	t.Run("credentials arrive intact", func(t *testing.T) {
		sent := runnerCredentials{
			Passphrase:  "correct horse battery staple",
			DBPassword:  "pa=ss word",
			S3AccessKey: "AKIA",
			S3SecretKey: "s3cr3t",
		}
		encoded, err := sent.encode()
		if err != nil {
			t.Fatal(err)
		}
		withStdin(t, encoded)

		in := &credentialsInput{stdin: true}
		got, err := in.read()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if got != sent {
			t.Errorf("read = %+v, want %+v", got, sent)
		}
	})

	t.Run("stdin is not read at all without the flag", func(t *testing.T) {
		withStdin(t, `{"passphrase":"should not be read"}`)
		in := &credentialsInput{}
		got, err := in.read()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if got != (runnerCredentials{}) {
			t.Errorf("read = %+v, want the zero credentials", got)
		}
	})

	t.Run("an empty stdin is not an error", func(t *testing.T) {
		withStdin(t, "")
		in := &credentialsInput{stdin: true}
		got, err := in.read()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if got != (runnerCredentials{}) {
			t.Errorf("read = %+v, want the zero credentials", got)
		}
	})

	t.Run("a malformed payload fails without echoing the secrets", func(t *testing.T) {
		withStdin(t, `{"passphrase": "super-secret-value"`) // truncated JSON
		in := &credentialsInput{stdin: true}
		_, err := in.read()
		if err == nil {
			t.Fatal("a malformed payload was accepted")
		}
		if strings.Contains(err.Error(), "super-secret-value") {
			t.Errorf("error echoes the payload: %v", err)
		}
	})
}

// The database password no longer travels in the connector URI, so the worker is
// what reunites it with the connection. Both pinned connectors document standalone
// keys as overriding the location URI, which is why `password` is the right seam.
func TestConnectorConfigAppliesTheDatabasePassword(t *testing.T) {
	uri := "postgres://prefect@task-manager-db:5432/prefect"

	config := connectorConfig(uri, runnerCredentials{DBPassword: "s3cret"}, map[string]string{"clean": "true"})
	if config["location"] != uri {
		t.Errorf("location = %q, want %q", config["location"], uri)
	}
	if config["password"] != "s3cret" {
		t.Errorf("password = %q, want it applied from the credentials", config["password"])
	}
	if config["clean"] != "true" {
		t.Errorf("clean = %q, want the caller's options preserved", config["clean"])
	}

	// The offline Neo4j paths authenticate through the filesystem and send no
	// password; the option must then be absent rather than empty.
	if _, ok := connectorConfig("neo4j+offline:///data", runnerCredentials{}, nil)["password"]; ok {
		t.Error("password was set with no credential to set it from")
	}

	// An explicit --opt still wins, so nothing here silently overrides an operator.
	config = connectorConfig(uri, runnerCredentials{DBPassword: "from-stdin"}, map[string]string{"password": "from-opt"})
	if config["password"] != "from-opt" {
		t.Errorf("password = %q, want the explicit option to win", config["password"])
	}
}

// The repository configuration the worker opens comes entirely from the
// credentials channel and the one non-secret flag, never from its environment.
func TestPlakarConfigFromCredentials(t *testing.T) {
	creds := runnerCredentials{Passphrase: "pass", S3AccessKey: "AKIA", S3SecretKey: "secret"}
	cfg := plakarConfigFrom("s3://minio:9000/bucket", creds, true)

	if cfg.RepoPath != "s3://minio:9000/bucket" {
		t.Errorf("RepoPath = %q", cfg.RepoPath)
	}
	if cfg.Passphrase != "pass" || cfg.S3AccessKey != "AKIA" || cfg.S3SecretKey != "secret" {
		t.Errorf("credentials not carried onto the config: %+v", cfg)
	}
	if !cfg.S3Insecure {
		t.Error("S3Insecure = false, want the flag honoured")
	}

	// And storeConfig has to use them: the location carries no userinfo any more, so
	// the credentials can only come from the config.
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("INFRAHUB_S3_ENDPOINT", "")
	sc := storeConfig(cfg)
	if sc["access_key"] != "AKIA" || sc["secret_access_key"] != "secret" {
		t.Errorf("storeConfig credentials = %q/%q, want them from the config", sc["access_key"], sc["secret_access_key"])
	}
	if sc["use_tls"] != "false" {
		t.Errorf("use_tls = %q, want \"false\" — stripping URI credentials must not flip a plain-HTTP store to TLS", sc["use_tls"])
	}
}
