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
func LaunchComposeBackup(project, dbService, repoPath, uri string, opts map[string]string, tags []string, mountDBVolumes bool) (string, error) {
	args, err := composeRunnerArgs(project, dbService, repoPath, mountDBVolumes)
	if err != nil {
		return "", err
	}
	repoArg := repoArgFor(repoPath)
	args = append(args, "__run-connector", "backup", repoArg, uri)
	for k, v := range opts {
		args = append(args, "--opt", k+"="+v)
	}
	for _, t := range tags {
		args = append(args, "--tag", t)
	}
	return runDockerCapture(args)
}

// LaunchComposeRestore runs ONE restore connector op in a co-located runner.
func LaunchComposeRestore(project, dbService, repoPath, destURI, snapshot string, opts map[string]string, mountDBVolumes bool) error {
	args, err := composeRunnerArgs(project, dbService, repoPath, mountDBVolumes)
	if err != nil {
		return err
	}
	repoArg := repoArgFor(repoPath)
	args = append(args, "__run-connector", "restore", repoArg, destURI, snapshot)
	for k, v := range opts {
		args = append(args, "--opt", k+"="+v)
	}
	_, err = runDockerCapture(args)
	return err
}

// composeRunnerArgs builds the `docker run …` prefix up to (but not including)
// the in-container command: image, network, mounts, env.
func composeRunnerArgs(project, dbService, repoPath string, mountDBVolumes bool) ([]string, error) {
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

	args := []string{
		"run", "--rm",
		"--network", network,
		"--user", "root", // neo4j-admin/pg tools; online backup tolerates root
		"-e", "HOME=/tmp",
		"-w", "/tmp", // kloset writes a relative "<ver>/store" cache under CWD — keep it writable
		"-v", bin + ":/usr/local/bin/infrahub-backup:ro",
	}
	if !strings.Contains(repoPath, "://") {
		// Local fs:// repo — bind-mount the host directory into the runner.
		args = append(args, "-v", repoPath+":/repo")
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

// repoArgFor maps the configured repo to the path the in-container worker uses:
// a local fs:// repo is bind-mounted at /repo; an s3:// URI is passed through.
func repoArgFor(repoPath string) string {
	if strings.Contains(repoPath, "://") {
		return repoPath
	}
	return "/repo"
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

func runDockerCapture(args []string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.Command("docker", args...)
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
