package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// installFakeCommand puts a shell script named name first on PATH for the
// test and returns the directory where the script writes its call log.
func installFakeCommand(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\ndir=\"$(dirname \"$0\")\"\necho \"$*\" >> \"$dir/calls\"\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func readCalls(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func TestPublishedAddress(t *testing.T) {
	tests := []struct {
		output  string
		want    string
		wantErr error
	}{
		{output: "0.0.0.0:3200", want: "127.0.0.1:3200"},
		{output: "[::]:3200", want: "127.0.0.1:3200"},
		{output: "192.0.2.10:13200", want: "192.0.2.10:13200"},
		{output: "", wantErr: errTempoNotFound},
		{output: ":0", wantErr: errTempoNotFound},
	}
	for _, tt := range tests {
		got, err := publishedAddress(tt.output)
		if tt.wantErr != nil {
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("publishedAddress(%q) error = %v, want %v", tt.output, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("publishedAddress(%q) = %q, %v, want %q", tt.output, got, err, tt.want)
		}
	}
}

func TestDockerLocateTempo(t *testing.T) {
	t.Run("published port", func(t *testing.T) {
		dir := installFakeCommand(t, "docker", `echo "0.0.0.0:3200"`+"\n")
		d := &DockerBackend{executor: NewCommandExecutor(), project: "infrahub"}
		url, stop, err := d.LocateTempo(context.Background())
		if err != nil || url != "http://127.0.0.1:3200" {
			t.Fatalf("LocateTempo() = %q, %v", url, err)
		}
		stop()
		if calls := readCalls(t, dir); !strings.Contains(calls, "compose -p infrahub port infrahub-tempo 3200") {
			t.Errorf("calls = %q", calls)
		}
	})

	t.Run("no tempo service", func(t *testing.T) {
		installFakeCommand(t, "docker", "echo 'no such service: infrahub-tempo' >&2\nexit 1\n")
		d := &DockerBackend{executor: NewCommandExecutor(), project: "infrahub"}
		if _, _, err := d.LocateTempo(context.Background()); !errors.Is(err, errTempoNotFound) {
			t.Fatalf("error = %v, want errTempoNotFound", err)
		}
	})

	t.Run("docker failure is an error", func(t *testing.T) {
		installFakeCommand(t, "docker", "echo 'Cannot connect to the Docker daemon' >&2\nexit 1\n")
		d := &DockerBackend{executor: NewCommandExecutor(), project: "infrahub"}
		_, _, err := d.LocateTempo(context.Background())
		if err == nil || errors.Is(err, errTempoNotFound) {
			t.Fatalf("error = %v, want a failure", err)
		}
	})
}

func TestDockerExecStdinContext(t *testing.T) {
	dir := installFakeCommand(t, "docker", "cat > \"$dir/stdin\"\necho out\necho err >&2\n")
	d := &DockerBackend{executor: NewCommandExecutor(), project: "infrahub"}
	stdout, stderr, err := d.ExecStdinContext(context.Background(), time.Minute, "infrahub-server", []string{"python", "-c", "x"}, []byte("secret\n"))
	if err != nil || stdout != "out" || stderr != "err" {
		t.Fatalf("ExecStdinContext() = %q, %q, %v", stdout, stderr, err)
	}
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if string(stdin) != "secret\n" {
		t.Errorf("stdin = %q", stdin)
	}
	if calls := readCalls(t, dir); strings.Contains(calls, "secret") || !strings.Contains(calls, "exec -T infrahub-server python -c x") {
		t.Errorf("calls = %q", calls)
	}
}

func TestExecStdinContextTimeLimit(t *testing.T) {
	installFakeCommand(t, "docker", "sleep 5\n")
	d := &DockerBackend{executor: NewCommandExecutor(), project: "infrahub"}
	start := time.Now()
	_, _, err := d.ExecStdinContext(context.Background(), 200*time.Millisecond, "infrahub-server", []string{"true"}, nil)
	var timeout *timeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("error = %v, want *timeoutError", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Errorf("the command was not stopped at the time limit")
	}
}

// fakeKubectlTempoScript answers the service lookup and the port-forward of
// the Kubernetes locator. The files "forbid-list" and "forbid-forward" make
// the matching call answer Forbidden; the file "services" holds the service
// names the lookup prints.
const fakeKubectlTempoScript = `case "$1 $2" in
"get service")
	if [ -f "$dir/forbid-list" ]; then
		echo 'Error from server (Forbidden): services is forbidden: User "x" cannot list resource "services"' >&2
		exit 1
	fi
	if [ -f "$dir/services" ]; then cat "$dir/services"; fi ;;
"port-forward -n")
	if [ -f "$dir/forbid-forward" ]; then
		echo 'error: error upgrading connection: pods "tempo-0" is forbidden: User "x" cannot create resource "pods/portforward"' >&2
		exit 1
	fi
	echo "Forwarding from 127.0.0.1:43210 -> 3100"
	echo "Forwarding from [::1]:43210 -> 3100"
	echo $$ > "$dir/pid"
	exec sleep 30 ;;
"exec -i")
	cat > "$dir/stdin"
	echo '{"status": 200, "body": "{}"}' ;;
"get pods")
	echo infrahub-server-0 ;;
esac
`

func newKubernetesTempoBackend(t *testing.T) (*KubernetesBackend, string) {
	t.Helper()
	dir := installFakeCommand(t, "kubectl", fakeKubectlTempoScript)
	k := NewKubernetesBackend(&Configuration{}, NewCommandExecutor())
	k.namespace = "infrahub"
	return k, dir
}

func TestKubernetesLocateTempo(t *testing.T) {
	t.Run("service found and port forwarded", func(t *testing.T) {
		k, dir := newKubernetesTempoBackend(t)
		os.WriteFile(filepath.Join(dir, "services"), []byte("tempo-headless\nobservability-tempo\n"), 0o644)
		url, stop, err := k.LocateTempo(context.Background())
		if err != nil || url != "http://127.0.0.1:43210" {
			t.Fatalf("LocateTempo() = %q, %v", url, err)
		}
		calls := readCalls(t, dir)
		if !strings.Contains(calls, "get service -n infrahub -l app.kubernetes.io/name=tempo") ||
			!strings.Contains(calls, "port-forward -n infrahub service/observability-tempo :3100") {
			t.Errorf("calls = %q", calls)
		}

		stop()
		pid, _ := os.ReadFile(filepath.Join(dir, "pid"))
		if alive := exec.Command("kill", "-0", strings.TrimSpace(string(pid))).Run() == nil; alive {
			t.Error("the port-forward process still runs after stop")
		}
	})

	t.Run("no service", func(t *testing.T) {
		k, _ := newKubernetesTempoBackend(t)
		if _, _, err := k.LocateTempo(context.Background()); !errors.Is(err, errTempoNotFound) {
			t.Fatalf("error = %v, want errTempoNotFound", err)
		}
	})

	t.Run("cannot list services", func(t *testing.T) {
		k, dir := newKubernetesTempoBackend(t)
		os.WriteFile(filepath.Join(dir, "forbid-list"), nil, 0o644)
		_, _, err := k.LocateTempo(context.Background())
		var permission *tempoPermissionError
		if !errors.As(err, &permission) || !strings.Contains(err.Error(), "list services") {
			t.Fatalf("error = %v, want a permission error for services", err)
		}
	})

	t.Run("cannot forward a port", func(t *testing.T) {
		k, dir := newKubernetesTempoBackend(t)
		os.WriteFile(filepath.Join(dir, "services"), []byte("tempo\n"), 0o644)
		os.WriteFile(filepath.Join(dir, "forbid-forward"), nil, 0o644)
		_, _, err := k.LocateTempo(context.Background())
		var permission *tempoPermissionError
		if !errors.As(err, &permission) || !strings.Contains(err.Error(), "pods/portforward") {
			t.Fatalf("error = %v, want a permission error for pods/portforward", err)
		}
	})
}

func TestKubernetesExecStdinContext(t *testing.T) {
	k, dir := newKubernetesTempoBackend(t)
	stdout, _, err := k.ExecStdinContext(context.Background(), time.Minute, "infrahub-server", []string{"python", "-c", "x"}, []byte("secret\n"))
	if err != nil || !strings.Contains(stdout, `"status": 200`) {
		t.Fatalf("ExecStdinContext() = %q, %v", stdout, err)
	}
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if string(stdin) != "secret\n" {
		t.Errorf("stdin = %q", stdin)
	}
	if calls := readCalls(t, dir); strings.Contains(calls, "secret") || !strings.Contains(calls, "exec -i -n infrahub infrahub-server-0 -- python -c x") {
		t.Errorf("calls = %q", calls)
	}
}

func TestPickTempoService(t *testing.T) {
	if got := pickTempoService([]string{"tempo-headless", "tempo-memberlist", "tempo"}); got != "tempo" {
		t.Errorf("pickTempoService() = %q", got)
	}
	if got := pickTempoService(nil); got != "" {
		t.Errorf("pickTempoService(nil) = %q", got)
	}
}
