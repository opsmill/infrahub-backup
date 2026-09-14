package app

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// portForwardStartTimeout bounds how long a `kubectl port-forward` may take to
// announce its local port. It is a local process opening a listener and an API
// connection, so a slow answer means something is wrong rather than busy.
const portForwardStartTimeout = 30 * time.Second

// portForwardReady matches the line kubectl prints once the listener is up:
//
//	Forwarding from 127.0.0.1:54321 -> 5432
//
// The port is parsed rather than chosen because kubectl is asked for ":<remote>",
// which lets it pick a free local port. Choosing one here would mean either
// racing another process for a fixed port or reimplementing the search kubectl
// already does.
var portForwardReady = regexp.MustCompile(`Forwarding from (?:127\.0\.0\.1|\[::1\]):(\d+) ->`)

// portForward is a running `kubectl port-forward` to one pod.
type portForward struct {
	// LocalPort is the loopback port on this machine that reaches the pod.
	LocalPort int
	cancel    context.CancelFunc
	done      chan struct{}
}

// Close tears the forward down and waits for kubectl to exit, so the listener is
// gone by the time it returns.
func (p *portForward) Close() {
	if p == nil {
		return
	}
	p.cancel()
	<-p.done
}

// PortForward starts a `kubectl port-forward` to service's pod and returns it
// once the local listener is accepting connections.
//
// It exists for the task-manager database. Unlike Neo4j — whose neo4j-admin has
// to run beside the data directory, so the tool execs it in the pod — pg_dump and
// pg_restore speak the wire protocol, so the upstream PostgreSQL connector can
// run in this process and needs only a route to the server. A port-forward is
// that route, and it keeps the snapshot the real connector's by construction
// rather than by a second implementation of its layout.
//
// The listener is on loopback only, and lives for the length of one component's
// backup or restore.
func (k *KubernetesBackend) PortForward(service string, remotePort int) (*portForward, error) {
	pod, err := k.getPodForService(service)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	args := []string{"port-forward", "-n", k.namespace, "pod/" + pod, fmt.Sprintf(":%d", remotePort)}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	// kubectl kills the forward when its parent goes away; make the reverse true
	// too, so a cancelled context ends the process rather than orphaning a
	// listener that outlives the backup.
	cmd.Cancel = func() error { return cmd.Process.Kill() }

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("port-forwarding %s: %w", service, err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	logrus.Debugf("exec: kubectl %s", strings.Join(args, " "))
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("starting the port-forward to %s: %w", service, err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := cmd.Wait(); err != nil && ctx.Err() == nil {
			logrus.Warnf("The port-forward to %s ended unexpectedly: %v: %s", service, err, strings.TrimSpace(stderr.String()))
		}
	}()

	ports := make(chan int, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		announced := false
		for scanner.Scan() {
			line := scanner.Text()
			logrus.Debugf("kubectl port-forward: %s", line)
			if announced {
				continue
			}
			if m := portForwardReady.FindStringSubmatch(line); m != nil {
				if port, err := strconv.Atoi(m[1]); err == nil {
					ports <- port
					announced = true
				}
			}
		}
		if !announced {
			close(ports)
		}
	}()

	stop := func() {
		cancel()
		<-done
	}

	select {
	case port, ok := <-ports:
		if !ok {
			stop()
			return nil, fmt.Errorf("the port-forward to %s exited without opening a listener: %s",
				service, strings.TrimSpace(stderr.String()))
		}
		logrus.Debugf("Port-forwarding %s:%d to 127.0.0.1:%d", service, remotePort, port)
		return &portForward{LocalPort: port, cancel: cancel, done: done}, nil
	case <-time.After(portForwardStartTimeout):
		stop()
		return nil, fmt.Errorf("the port-forward to %s did not open a listener within %s: %s",
			service, portForwardStartTimeout, strings.TrimSpace(stderr.String()))
	}
}
