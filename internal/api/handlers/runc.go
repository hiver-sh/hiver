package handlers

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// waitForContainer polls `runc state` until the container is in the "running"
// state or ctx is cancelled. It returns an error if the deadline is exceeded
// or the container reaches a terminal state before running.
func waitForContainer(ctx context.Context, containerID string) error {
	type runcState struct {
		Status string `json:"status"`
	}
	for {
		out, err := exec.CommandContext(ctx, "runc", "state", containerID).Output()
		if err == nil {
			var s runcState
			if json.Unmarshal(out, &s) == nil && s.Status == "running" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// buildRuncExecArgs constructs the argument slice for `runc exec`.
// When tty is set, --tty puts runc in interactive terminal mode (it proxies
// the container pty through its own stdio, which the caller supplies as a
// pty slave).
//
// env entries are passed as `--env KEY=VALUE` flags. runc seeds the exec
// process with the container's configured environment (i.e. the sandbox
// config's `env`) and merges these on top, so callers that omit env inherit
// the sandbox config environment unchanged.
//
// pidFile, when set, becomes `--pid-file`: runc writes the host-namespace PID
// of the spawned process there so the caller can kill the whole process tree
// on teardown (SIGKILL of the runc process alone does not reliably reap the
// in-container workload).
func buildRuncExecArgs(command string, cwd *string, tty bool, env *map[string]string, pidFile string) []string {
	args := []string{"exec"}
	if tty {
		args = append(args, "--tty")
	}
	if cwd != nil && *cwd != "" {
		args = append(args, "--cwd", *cwd)
	}
	if pidFile != "" {
		args = append(args, "--pid-file", pidFile)
	}
	if env != nil {
		// Sort keys so the flag order is deterministic.
		keys := make([]string, 0, len(*env))
		for k := range *env {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "--env", k+"="+(*env)[k])
		}
	}
	args = append(args, agentContainerID, "sh", "-c", command)
	return args
}

// newExecPIDFile creates an empty temp file for `runc exec --pid-file`. runc
// overwrites it with the spawned process's PID. The caller is responsible for
// removing it.
func newExecPIDFile() (string, error) {
	f, err := os.CreateTemp("", "hive-exec-*.pid")
	if err != nil {
		return "", err
	}
	name := f.Name()
	f.Close()
	return name, nil
}

// killExecTree reads the PID runc wrote to pidPath and SIGKILLs that process
// together with every descendant. Killing the runc process does not reliably
// reap the in-container workload (runc sets no parent-death signal for exec'd
// processes), so we kill the tree explicitly to guarantee teardown.
//
// Call this only on an aborted exec (client disconnect): on normal completion
// the process has already exited and its PID could have been recycled by an
// unrelated process.
func killExecTree(pidPath string) {
	pid, ok := readExecPID(pidPath)
	if !ok {
		return
	}
	killProcessTree(pid)
}

// readExecPID reads and parses the PID runc wrote to pidPath. runc writes the
// file right after spawning the process, so on a very early abort it may not
// exist yet; retry briefly to cover that window.
func readExecPID(pidPath string) (int, bool) {
	for i := 0; i < 10; i++ {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 1 {
				return pid, true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return 0, false
}

// killProcessTree SIGKILLs rootPID and all of its descendants. It snapshots
// the parent→child relationships from /proc first and then signals every
// member of the subtree, so descendants survive being re-parented to the
// container's init (which a naive live parent-walk would lose) and are still
// killed. PIDs are interpreted in sandboxd's PID namespace, which is where
// runc reports them and where /proc lists the in-container processes.
func killProcessTree(rootPID int) {
	if rootPID <= 1 {
		return
	}
	children := map[int][]int{}
	if entries, err := os.ReadDir("/proc"); err == nil {
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err != nil {
				continue
			}
			if ppid, ok := readPPID(pid); ok {
				children[ppid] = append(children[ppid], pid)
			}
		}
	}

	// Breadth-first collection of the whole subtree before signaling anything.
	victims := []int{rootPID}
	seen := map[int]bool{rootPID: true}
	for i := 0; i < len(victims); i++ {
		for _, c := range children[victims[i]] {
			if !seen[c] {
				seen[c] = true
				victims = append(victims, c)
			}
		}
	}
	for _, pid := range victims {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// readPPID returns the parent PID from /proc/<pid>/stat.
func readPPID(pid int) (int, bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, false
	}
	return parsePPIDStat(string(data))
}

// parsePPIDStat extracts the parent PID (the 4th field) from the contents of
// /proc/<pid>/stat. The comm field (2nd) is wrapped in parentheses and may
// itself contain spaces and parentheses, so the remaining space-separated
// fields are parsed after the final ')'.
func parsePPIDStat(s string) (int, bool) {
	rparen := strings.LastIndexByte(s, ')')
	if rparen < 0 || rparen+2 > len(s) {
		return 0, false
	}
	fields := strings.Fields(s[rparen+2:]) // state, ppid, ...
	if len(fields) < 2 {
		return 0, false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, false
	}
	return ppid, true
}
