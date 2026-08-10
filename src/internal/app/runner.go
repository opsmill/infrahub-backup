package app

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runnerBinary returns the path to the tool binary to mount into the runner
// container. In production the tool runs on Linux, so its own executable is a
// valid Linux binary; INFRAHUB_RUNNER_BINARY overrides it (e.g. for a cross-built
// binary during development on a non-Linux host).
func runnerBinary() (string, error) {
	if b := os.Getenv("INFRAHUB_RUNNER_BINARY"); b != "" {
		return b, nil
	}
	return os.Executable()
}

// LaunchComposeBackup runs ONE backup connector op in a one-shot runner container
// co-located with the target Docker Compose database service, and returns the
// created snapshot id.
//
// Validated model (2026-06-30, live Infrahub): the runner uses the DB service's
// OWN image (so neo4j-admin / pg client tools and the matching version are
// present), joins the DB's compose network (reach it by service name), and mounts
// the tool binary + the kloset repo. This needs no separately-built runner image
// and resolves fs:// repo reachability (the host repo dir is bind-mounted in).
func LaunchComposeBackup(project, dbService, repoPath, uri, passphrase string, opts map[string]string, tags []string, mountDBVolumes bool) (string, error) {
	args, err := composeRunnerArgs(project, dbService, repoPath, mountDBVolumes, passphrase != "")
	if err != nil {
		return "", err
	}
	repoArg := repoArgFor(repoPath)
	args = append(args, "__run-connector", "backup", repoArg, uri)
	if passphrase != "" {
		args = append(args, "--passphrase-stdin")
	}
	for k, v := range opts {
		args = append(args, "--opt", k+"="+v)
	}
	for _, t := range tags {
		args = append(args, "--tag", t)
	}
	return runDockerCapture(args, passphrase)
}

// LaunchComposeRestore runs ONE restore connector op in a co-located runner.
func LaunchComposeRestore(project, dbService, repoPath, destURI, snapshot, passphrase string, opts map[string]string, mountDBVolumes bool) error {
	args, err := composeRunnerArgs(project, dbService, repoPath, mountDBVolumes, passphrase != "")
	if err != nil {
		return err
	}
	repoArg := repoArgFor(repoPath)
	args = append(args, "__run-connector", "restore", repoArg, destURI, snapshot)
	if passphrase != "" {
		args = append(args, "--passphrase-stdin")
	}
	for k, v := range opts {
		args = append(args, "--opt", k+"="+v)
	}
	_, err = runDockerCapture(args, passphrase)
	return err
}

// composeRunnerArgs builds the `docker run …` prefix up to (but not including)
// the in-container command: image, network, mounts, env. When withStdin is set,
// the container keeps stdin open (`-i`) so the passphrase can be piped in.
func composeRunnerArgs(project, dbService, repoPath string, mountDBVolumes, withStdin bool) ([]string, error) {
	cid, err := composeContainerID(project, dbService)
	if err != nil {
		return nil, err
	}
	image, err := dockerInspect(cid, "{{.Config.Image}}")
	if err != nil {
		return nil, fmt.Errorf("inspecting image of %s: %w", dbService, err)
	}
	network, err := firstNetwork(cid)
	if err != nil {
		return nil, err
	}
	bin, err := runnerBinary()
	if err != nil {
		return nil, fmt.Errorf("resolving runner binary: %w", err)
	}

	args := []string{"run", "--rm"}
	if withStdin {
		// Keep stdin open so the orchestrator can pipe the passphrase in. The
		// passphrase is never an arg or -e env var (it would leak via docker
		// inspect / the process list).
		args = append(args, "-i")
	}
	args = append(args,
		"--network", network,
		"--user", "root", // neo4j-admin/pg tools; online backup tolerates root
		"-e", "HOME=/tmp",
		"-w", "/tmp", // kloset writes a relative "<ver>/store" cache under CWD — keep it writable
		"-v", bin+":/usr/local/bin/infrahub-backup:ro",
	)
	if local, hostPath := parseRepoLocation(repoPath); local {
		// Local repo, spelled either /path or fs:///path — bind-mount the host
		// directory into the runner, where repoArgFor points the worker at /repo.
		args = append(args, "-v", hostPath+":/repo")
	} else if strings.HasPrefix(repoPath, "s3://") {
		for _, e := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "INFRAHUB_S3_ENDPOINT"} {
			if v := os.Getenv(e); v != "" {
				args = append(args, "-e", e+"="+v)
			}
		}
	}
	if mountDBVolumes {
		// Neo4j community offline dump / restore: share the DB's data volume.
		args = append(args, "--volumes-from", cid)
	}
	args = append(args, image, "/usr/local/bin/infrahub-backup")
	return args, nil
}

// parseRepoLocation classifies a --repo value and, for a local repository, returns
// its filesystem path with any fs:// scheme stripped.
//
// A local repository has two spellings — a bare path (/backups/infra) and the fs://
// URI the documentation and README use (fs:///backups/infra) — and every decision
// that turns on "is this local" has to treat them as the same thing. Testing only
// for "://" does not: fs:// contains it, so the documented spelling was classified
// as remote. The runner then neither bind-mounted the directory nor rewrote the
// path, and the in-container worker looked for the repository at a host path that
// does not exist inside the container, failing with a missing CONFIG. Only the bare
// spelling worked, which is why the local test recipes never caught it.
func parseRepoLocation(repoPath string) (local bool, path string) {
	if rest, ok := strings.CutPrefix(repoPath, "fs://"); ok {
		return true, rest
	}
	if strings.Contains(repoPath, "://") {
		return false, repoPath
	}
	return true, repoPath
}

// repoArgFor maps the configured repo to the path the in-container worker uses:
// a local repo is bind-mounted at /repo; a remote URI is passed through.
func repoArgFor(repoPath string) string {
	if local, _ := parseRepoLocation(repoPath); local {
		return "/repo"
	}
	return repoPath
}

func composeContainerID(project, service string) (string, error) {
	// -a so a stopped container is still found (restore stops the writer first).
	out, err := exec.Command("docker", "ps", "-aq",
		"--filter", "label=com.docker.compose.project="+project,
		"--filter", "label=com.docker.compose.service="+service).Output()
	if err != nil {
		return "", fmt.Errorf("locating compose service %q: %w", service, err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return "", fmt.Errorf("no running container for compose service %q in project %q", service, project)
	}
	return fields[0], nil
}

func dockerInspect(cid, format string) (string, error) {
	out, err := exec.Command("docker", "inspect", "--format", format, cid).Output()
	if err != nil {
		return "", fmt.Errorf("docker inspect %s: %w", cid, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func firstNetwork(cid string) (string, error) {
	s, err := dockerInspect(cid, "{{range $k,$_ := .NetworkSettings.Networks}}{{$k}} {{end}}")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "", fmt.Errorf("container %s has no network", cid)
	}
	return fields[0], nil
}

// runDockerCapture runs `docker <args>` and returns the last stdout token. When
// stdin is non-empty it is written to the container (one line) and stdin closed,
// used to pipe the repository passphrase without exposing it on argv/env.
func runDockerCapture(args []string, stdin string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.Command("docker", args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin + "\n")
	}
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("runner launch failed: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	fields := strings.Fields(strings.TrimSpace(out.String()))
	if len(fields) == 0 {
		return "", nil
	}
	return fields[len(fields)-1], nil // snapshot id is the last stdout token
}
