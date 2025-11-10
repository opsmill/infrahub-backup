package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

var (
	pidFile   = flag.String("pid-file", "/var/lib/neo4j/run/neo4j.pid", "Path to the neo4j pid file")
	readyFile = flag.String("ready-file", "", "Optional path to write once watcher is initialized")
)

func main() {
	flag.Parse()

	pid, err := readPID(*pidFile)
	if err != nil {
		log.Fatalf("failed to read pid: %v", err)
	}

	if err := watchForDelete(*pidFile, pid, *readyFile); err != nil {
		log.Fatalf("watcher error: %v", err)
	}
}

func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	trimmed := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, fmt.Errorf("invalid pid in %s: %w", path, err)
	}
	return pid, nil
}

func watchForDelete(path string, pid int, readyFile string) error {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC)
	if err != nil {
		return fmt.Errorf("inotify init: %w", err)
	}
	defer unix.Close(fd)

	wd, err := unix.InotifyAddWatch(fd, path, unix.IN_DELETE_SELF|unix.IN_MOVE_SELF)
	if err != nil {
		return fmt.Errorf("inotify add watch: %w", err)
	}
	defer unix.InotifyRmWatch(fd, uint32(wd))

	if readyFile != "" {
		if err := os.WriteFile(readyFile, []byte("ready"), 0644); err != nil {
			return fmt.Errorf("write ready file: %w", err)
		}
	}

	buf := make([]byte, 4096)
	for {
		n, err := unix.Read(fd, buf)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return fmt.Errorf("read inotify: %w", err)
		}
		if n < unix.SizeofInotifyEvent {
			continue
		}

		offset := 0
		for offset < n {
			raw := (*unix.InotifyEvent)(unsafe.Pointer(&buf[offset]))
			mask := uint32(raw.Mask)
			if mask&(unix.IN_DELETE_SELF|unix.IN_MOVE_SELF) != 0 {
				if err := syscall.Kill(pid, syscall.SIGSTOP); err != nil {
					return fmt.Errorf("failed to SIGSTOP pid %d: %w", pid, err)
				}
				return nil
			}
			offset += unix.SizeofInotifyEvent + int(raw.Len)
		}
	}
}
