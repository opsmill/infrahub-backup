package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// defaultPrefectPaginationSize is the pagination size used for the running-tasks
// check when the task-manager cap cannot be discovered. It matches Prefect's
// built-in PREFECT_API_DEFAULT_LIMIT default.
const defaultPrefectPaginationSize = 200

type tasksOutput struct {
	Id   string `json:"id"`
	Name string `json:"title"`
}

// prefectLimitRe extracts the pagination cap from a task-manager 422 response,
// e.g. "Invalid limit: must be less than or equal to 200.". The captured number
// is Prefect's effective PREFECT_API_DEFAULT_LIMIT.
var prefectLimitRe = regexp.MustCompile(`must be less than or equal to (\d+)`)

// parsePrefectMaxLimit returns the server-enforced pagination cap reported in a
// task-manager limit error, scanning each provided string (error text, command
// output).
func parsePrefectMaxLimit(parts ...string) (int, bool) {
	for _, part := range parts {
		if match := prefectLimitRe.FindStringSubmatch(part); match != nil {
			if limit, err := strconv.Atoi(match[1]); err == nil && limit > 0 {
				return limit, true
			}
		}
	}
	return 0, false
}

// discoverPrefectPaginationLimit reads PREFECT_API_DEFAULT_LIMIT from the
// task-manager container so the running-tasks check can request a valid
// pagination size on the first try. Returns false when the value is unset,
// invalid, or the container cannot be reached (the caller then falls back to the
// default and the reactive retry).
func (iops *InfrahubOps) discoverPrefectPaginationLimit() (int, bool) {
	output, err := iops.Exec("task-manager", []string{"sh", "-c", `printf %s "$PREFECT_API_DEFAULT_LIMIT"`}, nil)
	if err != nil {
		logrus.Debugf("Could not read PREFECT_API_DEFAULT_LIMIT from task-manager: %v", err)
		return 0, false
	}
	value := strings.TrimSpace(output)
	if value == "" {
		return 0, false
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit <= 0 {
		logrus.Debugf("Ignoring invalid PREFECT_API_DEFAULT_LIMIT value %q from task-manager", value)
		return 0, false
	}
	return limit, true
}

func (iops *InfrahubOps) waitForRunningTasks() error {
	useInfrahubctl := true
	var scriptContent string

	// Layer 1 (proactive): discover the task-manager cap so the first request is
	// valid. Layer 2 (default): fall back to Prefect's built-in default otherwise.
	// The value never exceeds the default, since a larger page is unnecessary for an
	// existence check and must stay within the server cap.
	paginationSize := defaultPrefectPaginationSize
	if discovered, ok := iops.discoverPrefectPaginationLimit(); ok && discovered < paginationSize {
		paginationSize = discovered
		logrus.Debugf("Using task-manager pagination cap of %d for the running-tasks check", discovered)
	}

	loadScriptContent := func() error {
		if scriptContent != "" {
			return nil
		}
		scriptBytes, err := readEmbeddedScript("get_running_tasks.py")
		if err != nil {
			return fmt.Errorf("could not retrieve get_running_tasks.py: %w", err)
		}
		scriptContent = string(scriptBytes)
		return nil
	}

	isCommandNotFound := func(err error, output string) bool {
		if err == nil {
			return false
		}
		errMsg := strings.ToLower(err.Error())
		if strings.Contains(errMsg, "no such command") {
			return true
		}
		outputLower := strings.ToLower(output)
		return strings.Contains(outputLower, "no such command")
	}

	// adaptPaginationLimit (Layer 3, reactive): when the task-manager rejects the
	// request, lower the pagination size to the cap reported in the error and signal
	// a retry. Only lowers the size, so retries converge and cannot loop.
	adaptPaginationLimit := func(parts ...string) bool {
		maxLimit, ok := parsePrefectMaxLimit(parts...)
		if !ok || maxLimit >= paginationSize {
			return false
		}
		logrus.Warnf("task-manager rejected pagination size %d; retrying with %d", paginationSize, maxLimit)
		paginationSize = maxLimit
		return true
	}

	for {
		var (
			output string
			err    error
		)

		// Layer 0 (clamp): inject the bounded pagination size into the exec so the
		// deployment's INFRAHUB_PAGINATION_SIZE cannot exceed the server cap.
		execOpts := iops.buildTaskWorkerExecOpts(&ExecOptions{
			Env: map[string]string{"INFRAHUB_PAGINATION_SIZE": strconv.Itoa(paginationSize)},
		})

		if useInfrahubctl {
			output, err = iops.Exec("task-worker", []string{"infrahubctl", "task", "list", "--json", "--state", "running", "--state", "pending"}, execOpts)
			if err != nil {
				if isCommandNotFound(err, output) {
					logrus.Infof("infrahubctl task list command not available in task-worker, falling back to embedded script")
					useInfrahubctl = false
					if loadErr := loadScriptContent(); loadErr != nil {
						return loadErr
					}
					continue
				}
				if adaptPaginationLimit(err.Error(), output) {
					continue
				}
				return fmt.Errorf("failed to check running tasks: %w\n%s", err, output)
			}
		} else {
			if err := loadScriptContent(); err != nil {
				return err
			}
			output, err = iops.executeScriptWithOpts("task-worker", scriptContent, "/tmp/get_running_tasks.py", execOpts, "python", "-u", "/tmp/get_running_tasks.py")
			if err != nil {
				if adaptPaginationLimit(err.Error(), output) {
					continue
				}
				return fmt.Errorf("failed to check running tasks: %w", err)
			}
		}

		output = strings.TrimSpace(output)
		var tasks []tasksOutput
		if output != "" {
			if err := json.Unmarshal([]byte(output), &tasks); err != nil {
				return fmt.Errorf("could not parse json: %w\n%v", err, output)
			}
		}
		if len(tasks) == 0 {
			logrus.Info("No running tasks detected. Proceeding with backup.")
			return nil
		}

		logrus.Warnf("There are running %v tasks: %v", len(tasks), tasks)
		logrus.Warnf("Waiting for them to complete... (use --force to override)")
		time.Sleep(5 * time.Second)
	}
}

// appServicesStoppedForBackup are the services a run stops for an offline
// capture, and the only services IsServiceRunning is ever asked about.
//
// It is named rather than inlined because that second fact is load-bearing:
// the running check and the internal-versus-external location decision read a
// namespace by the same rule but differ on what an unanchored name means (see
// deploymentClaimsResource), and this list is why those two can never be asked
// about the same service. No database appears here, and locateService is asked
// about nothing else.
//
// That says nothing about the workload resolver, which is a different caller of
// the same rule and *is* asked about a database: a restore starts
// `task-manager-db`. So the resolver does not take its policy from this list —
// it takes it from the service (see unanchoredNamePolicyFor), because what
// holds of the services stopped here does not hold of everything that gets
// scaled.
var appServicesStoppedForBackup = []string{
	"infrahub-server", "task-worker", "task-manager",
	"task-manager-background-svc", "cache", "message-queue",
}

func (iops *InfrahubOps) stopAppContainers() ([]string, error) {
	logrus.Info("Stopping Infrahub application services...")

	stopped := []string{}

	for _, service := range appServicesStoppedForBackup {
		// A status this run could not determine is not a reason to stop
		// anything, and it is not a reason to carry on either. Stopping on a
		// guess is what scaled another product's `cache` or `message-queue` to
		// zero, and constitution Principle II allows a backup no destructive
		// effect beyond the documented stop/start of Infrahub's own containers
		// — so this does not stop the workload. But every caller of this
		// function is about to do something that requires the services to be
		// down: an offline capture, or a restore. Warning and continuing turned
		// "we do not know whether infrahub-server is running" into a cold
		// `neo4j-admin dump` taken with writers still attached, reported as a
		// successful backup. So the undetermined status fails the run instead,
		// leaving the deployment as it was found and naming the service the
		// query could not answer for.
		running, err := iops.IsServiceRunning(service)
		if err != nil {
			return stopped, fmt.Errorf("cannot determine whether %s is running, so the deployment cannot be confirmed quiesced and this run will not read or write data as if it were: %w", service, err)
		}

		if running {
			logrus.Infof("Stopping %s...", service)
			if err := iops.StopServices(service); err != nil {
				return stopped, fmt.Errorf("failed to stop %s: %w", service, err)
			}
			stopped = append(stopped, service)
		}
	}

	if len(stopped) == 0 {
		logrus.Info("No application services were running")
	} else {
		logrus.Info("Application services stopped")
	}

	return stopped, nil
}

const (
	// appQuiesceTimeout bounds the wait for the services a run stopped to
	// actually be down. It is generous because a scale-down is asynchronous: on
	// Kubernetes the replica count changes at once and the pods terminate
	// afterwards, at whatever pace their own grace periods and finalizers
	// allow.
	appQuiesceTimeout = 5 * time.Minute

	// appScaleTimeout bounds the wait for them to be back up, which is a pull
	// and a start rather than a stop.
	appScaleTimeout = 10 * time.Minute

	// appScalePollInterval is how often the deployment is asked. It decides how
	// many status queries a wait spends, not how long it waits.
	appScalePollInterval = 3 * time.Second
)

// confirmAppContainersQuiesced waits until every service this run stopped
// reports that it is down, and fails otherwise (FR-026).
//
// This is the difference between asking for a scale-down and having one. The
// two are not the same on either backend and are furthest apart on Kubernetes,
// where `kubectl scale` returns as soon as the replica count is recorded: the
// pods keep running while they terminate, and an Infrahub server still holding
// a Bolt session is a writer attached to a database the next step is about to
// overwrite. What that produces is not a failed restore, it is a restore that
// reports success over a database some other process was writing to as it
// landed — and constitution Principle II is written against exactly that class
// of quiet corruption.
//
// It takes the list the run actually stopped rather than the whole service set.
// A service that reported itself not running before the stop has already
// answered this question, and waiting on one that was never up would fail
// restores on deployments that legitimately run without a task manager.
//
// A status the deployment cannot answer for fails the run, which is the same
// answer stopAppContainers gives to the same uncertainty and for the same
// reason: a destructive step must not proceed on a guess about whether the
// writers are gone.
func (iops *InfrahubOps) confirmAppContainersQuiesced(stopped []string) error {
	return iops.confirmAppContainersQuiescedWithin(stopped, appQuiesceTimeout, appScalePollInterval)
}

// confirmAppContainersQuiescedWithin is the confirmation under an explicit
// window, which is what makes a deployment that will not quiesce assertable
// without waiting out the real one.
func (iops *InfrahubOps) confirmAppContainersQuiescedWithin(stopped []string, window, interval time.Duration) error {
	if len(stopped) == 0 {
		return nil
	}

	logrus.Info("Confirming Infrahub application services have stopped...")

	deadline := time.Now().Add(window)
	for _, service := range stopped {
		for {
			running, err := iops.IsServiceRunning(service)
			if err != nil {
				return fmt.Errorf("cannot confirm that %s has stopped, so the deployment cannot be confirmed quiesced and this run will not write data as if it were: %w", service, err)
			}
			if !running {
				break
			}

			if sleepUntilNextPoll(deadline, interval) {
				return fmt.Errorf(
					"%s is still running %v after it was stopped, so the deployment is not quiesced and this run will not write to a database something else may still be writing to. "+
						"Check for a pod that will not terminate, or a controller scaling it back up",
					service, window)
			}
		}
	}

	logrus.Info("Application services confirmed stopped")

	return nil
}

// returnAppContainersToScale gives back everything a run took down (FR-013).
//
// It is the failure path's counterpart of the ordinary restart at the end of a
// restore, and it exists because that restart is only reached by a run that
// succeeded. A restore that failed after quiescing used to leave the deployment
// scaled to zero with the operator's data still in place and nothing serving
// it — the outage lasting until somebody noticed, rather than until the run
// ended.
//
// It reports what it could not do rather than returning it. The caller is on
// its way out with a failure of its own, and replacing that failure with "and
// also the restart did not work" would hide the reason the restore failed.
func (iops *InfrahubOps) returnAppContainersToScale(stopped []string) {
	if len(stopped) == 0 {
		return
	}

	logrus.Infof("Returning %d Infrahub service(s) this run stopped to their prior scale...", len(stopped))

	if err := iops.startAppContainers(stopped); err != nil {
		logrus.Errorf("Could not return every Infrahub service this run stopped to its prior scale; %v was quiesced for the restore and needs starting by hand: %v", stopped, err)

		return
	}

	iops.reportAppContainersRunning(stopped)
}

// reportAppContainersRunning waits on the default window; see
// reportAppContainersRunningWithin.
func (iops *InfrahubOps) reportAppContainersRunning(services []string) {
	iops.reportAppContainersRunningWithin(services, appScaleTimeout, appScalePollInterval)
}

// reportAppContainersRunning waits for the services a run put back to report
// themselves up, and says so (FR-026's second half).
//
// It reports rather than fails, and the asymmetry with
// confirmAppContainersQuiesced is deliberate. That check guards a destructive
// step, so uncertainty there has to stop the run. This one runs after the data
// is already restored, where turning "the cluster is still pulling an image"
// into a non-zero exit would report a successful restore as a failed one — and
// a scheduled restore's exit status is what somebody gets paged by. What the
// operator needs here is to be told which service has not come back, which is
// what this says.
//
// Whether Infrahub then *serves* the restored data is not something this layer
// can observe: the tool speaks to the deployment and the databases, never to
// Infrahub's own API. That assertion belongs to the end-to-end suite.
func (iops *InfrahubOps) reportAppContainersRunningWithin(services []string, window, interval time.Duration) {
	deadline := time.Now().Add(window)
	pending := []string{}

	for _, service := range services {
		for {
			running, err := iops.IsServiceRunning(service)
			if err == nil && running {
				break
			}

			if sleepUntilNextPoll(deadline, interval) {
				pending = append(pending, service)

				break
			}
		}
	}

	if len(pending) > 0 {
		logrus.Warnf("The data is restored, but %v has not reported itself running again within %v; Infrahub will not serve until it does, so check those workloads in the deployment", pending, window)

		return
	}

	logrus.Info("Every Infrahub service this run stopped is running again")
}

func (iops *InfrahubOps) startAppContainers(services []string) error {
	if len(services) == 0 {
		return nil
	}

	logrus.Info("Starting Infrahub application services...")

	preferredOrder := []string{
		"cache",
		"message-queue",
		"task-manager",
		"task-manager-background-svc",
		"infrahub-server",
		"task-worker",
	}

	serviceSet := make(map[string]struct{}, len(services))
	for _, svc := range services {
		serviceSet[svc] = struct{}{}
	}

	ordered := make([]string, 0, len(serviceSet))
	for _, svc := range preferredOrder {
		if _, ok := serviceSet[svc]; ok {
			ordered = append(ordered, svc)
			delete(serviceSet, svc)
		}
	}
	for svc := range serviceSet {
		ordered = append(ordered, svc)
	}

	// Every service is attempted, and the failures are reported together
	// (FR-013).
	//
	// It used to return on the first one, and the order above is what made that
	// costly: `cache` is started first, so a single transient failure there left
	// the other five at zero replicas — a whole deployment down on one service's
	// error, reported as one line from returnAppContainersToScale. The services
	// do not depend on each other for *starting*; preferredOrder is about the
	// order they come up in, so one that will not start is not a reason to leave
	// the rest stopped.
	startErrs := []error{}
	for _, svc := range ordered {
		logrus.Infof("Starting %s...", svc)
		if err := iops.StartServices(svc); err != nil {
			startErrs = append(startErrs, fmt.Errorf("failed to start %s: %w", svc, err))
		}
	}
	if len(startErrs) > 0 {
		return errors.Join(startErrs...)
	}

	logrus.Info("Application services started")
	return nil
}

func (iops *InfrahubOps) wipeTransientData() error {
	logrus.Info("Wiping cache and message queue data...")

	if _, err := iops.Exec("message-queue", []string{"find", "/var/lib/rabbitmq", "-mindepth", "1", "-delete"}, nil); err != nil {
		logrus.Warnf("Failed to wipe message queue data: %v", err)
	}
	if _, err := iops.Exec("cache", []string{"find", "/data", "-mindepth", "1", "-delete"}, nil); err != nil {
		logrus.Warnf("Failed to wipe cache data: %v", err)
	}
	logrus.Info("Transient data wiped")
	return nil
}
