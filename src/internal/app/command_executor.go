package app

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
	"sync"

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

func (ce *CommandExecutor) runCommandQuiet(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	return cmd.Run()
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
