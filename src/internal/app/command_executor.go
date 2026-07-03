package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// CommandExecutor handles command execution
type CommandExecutor struct{}

func NewCommandExecutor() *CommandExecutor {
	return &CommandExecutor{}
}

type lineLogger struct {
	buf     bytes.Buffer
	logFunc func(string)
}

func newLineLogger(logFunc func(string)) *lineLogger {
	return &lineLogger{logFunc: logFunc}
}

func (l *lineLogger) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		if idx := bytes.IndexByte(p, '\n'); idx >= 0 {
			l.buf.Write(p[:idx])
			l.flush()
			p = p[idx+1:]
			continue
		}
		l.buf.Write(p)
		break
	}
	return total, nil
}

func (l *lineLogger) flush() {
	if l.buf.Len() == 0 {
		return
	}
	l.logFunc(l.buf.String())
	l.buf.Reset()
}

func (l *lineLogger) Flush() {
	l.flush()
}

func (ce *CommandExecutor) runCommand(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	output, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

// timeoutError marks a command that exceeded its allotted execution time. Its
// message is exactly "timed out after <duration>" so orchestrators can surface
// it verbatim (e.g. as a bundle manifest failure reason).
type timeoutError struct {
	timeout time.Duration
}

func (e *timeoutError) Error() string {
	return "timed out after " + formatCommandTimeout(e.timeout)
}

// formatCommandTimeout renders whole-second durations as plain seconds
// ("60s", "300s") to match the documented reason format; sub-second
// durations fall back to Go's default formatting.
func formatCommandTimeout(d time.Duration) string {
	if d == d.Truncate(time.Second) {
		return fmt.Sprintf("%ds", int64(d/time.Second))
	}
	return d.String()
}

// runCommandContext is the timeout-bounded variant of runCommand: the command
// is killed once timeout elapses (or ctx is cancelled) and the returned error
// is a *timeoutError when the timeout expired.
func (ce *CommandExecutor) runCommandContext(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, name, args...)
	output, err := cmd.CombinedOutput()
	if err != nil && errors.Is(cctx.Err(), context.DeadlineExceeded) {
		return strings.TrimSpace(string(output)), &timeoutError{timeout: timeout}
	}
	return strings.TrimSpace(string(output)), err
}

// runCommandPipeContext is the timeout-bounded variant of runCommandPipe. The
// caller must read from stdout and then call wait() to get the exit status;
// wait() returns a *timeoutError when the timeout expired before the command
// finished. The internal context is released when wait() is called.
func (ce *CommandExecutor) runCommandPipeContext(ctx context.Context, timeout time.Duration, name string, args ...string) (io.ReadCloser, func() error, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)

	cmd := exec.CommandContext(cctx, name, args...)
	logrus.Debugf("exec pipe (timeout %s): %s %s", timeout, name, strings.Join(args, " "))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, nil, err
	}

	// Capture stderr for error reporting
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		cancel()
		return nil, nil, err
	}

	wait := func() error {
		defer cancel()
		if err := cmd.Wait(); err != nil {
			if errors.Is(cctx.Err(), context.DeadlineExceeded) {
				return &timeoutError{timeout: timeout}
			}
			stderrStr := strings.TrimSpace(stderrBuf.String())
			if stderrStr != "" {
				return fmt.Errorf("%w: %s", err, stderrStr)
			}
			return err
		}
		return nil
	}

	return stdout, wait, nil
}

func (ce *CommandExecutor) runCommandQuiet(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Run()
}

// runCommandPipe starts a command and returns the stdout pipe, a wait function, and any startup error.
// The caller must read from stdout and then call wait() to get the exit status.
func (ce *CommandExecutor) runCommandPipe(name string, args ...string) (io.ReadCloser, func() error, error) {
	cmd := exec.Command(name, args...)
	logrus.Debugf("exec pipe: %s %s", name, strings.Join(args, " "))

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}

	// Capture stderr for error reporting
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}

	wait := func() error {
		if err := cmd.Wait(); err != nil {
			stderrStr := strings.TrimSpace(stderrBuf.String())
			if stderrStr != "" {
				return fmt.Errorf("%w: %s", err, stderrStr)
			}
			return err
		}
		return nil
	}

	return stdout, wait, nil
}

// runCommandWritePipe starts a command with stdin connected to the provided reader.
// The caller must call wait() after the reader is fully consumed to get the exit status.
func (ce *CommandExecutor) runCommandWritePipe(stdin io.Reader, name string, args ...string) (func() error, error) {
	cmd := exec.Command(name, args...)
	logrus.Debugf("exec write-pipe: %s %s", name, strings.Join(args, " "))

	cmd.Stdin = stdin

	// Capture stderr for error reporting
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	wait := func() error {
		if err := cmd.Wait(); err != nil {
			stderrStr := strings.TrimSpace(stderrBuf.String())
			if stderrStr != "" {
				return fmt.Errorf("%w: %s", err, stderrStr)
			}
			return err
		}
		return nil
	}

	return wait, nil
}

func (ce *CommandExecutor) runCommandWithStream(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}

	if err := cmd.Start(); err != nil {
		return "", err
	}

	var stdoutBuf bytes.Buffer
	stdoutLogger := newLineLogger(func(line string) {
		logrus.Info(line)
	})
	stderrLogger := newLineLogger(func(line string) {
		logrus.Info(line)
	})

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		if _, copyErr := io.Copy(io.MultiWriter(&stdoutBuf, stdoutLogger), stdout); copyErr != nil {
			logrus.WithError(copyErr).Warn("failed reading command stdout")
		}
		stdoutLogger.Flush()
	}()

	go func() {
		defer wg.Done()
		if _, copyErr := io.Copy(stderrLogger, stderr); copyErr != nil {
			logrus.WithError(copyErr).Warn("failed reading command stderr")
		}
		stderrLogger.Flush()
	}()

	wg.Wait()

	err = cmd.Wait()
	return stdoutBuf.String(), err
}
