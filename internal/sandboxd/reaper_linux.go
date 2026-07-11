//go:build linux

package sandboxd

import (
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// reapInterval is how often reapOrphans sweeps for orphan zombies. A zombie that
// survives a full interval is treated as an orphan — long enough that sandboxd's
// own cmd.Wait() (which reaps the processes it launches within milliseconds) has
// already collected its children, so the sweep never steals a reap from it (and
// never corrupts, e.g., an exec's exit code, which comes from cmd.Wait()).
const reapInterval = 5 * time.Second

// reapOrphans reaps orphaned zombie processes when sandboxd is the pod's init
// (PID 1). Processes that reparent to it — a sandbox entrypoint whose runc
// launcher detached, or an exec grandchild whose parent exited — become zombies
// that nothing Wait()s, each holding a PID slot until they exhaust the pod's PID
// space. No-op unless PID 1.
//
// It is race-free with sandboxd's own process supervision: it reaps only zombie
// children that persist across two consecutive sweeps, and identifies them by a
// targeted wait4(pid) (never wait4(-1)), so a process sandboxd is itself about to
// cmd.Wait() is left alone (its zombie is gone well before the next sweep).
func reapOrphans(ctx context.Context) {
	if os.Getpid() != 1 {
		return
	}
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	prev := map[int]bool{} // zombie children seen on the previous sweep
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cur := map[int]bool{}
		for _, pid := range zombieChildren() {
			cur[pid] = true
			if prev[pid] {
				// Survived a full interval → an orphan nobody else will reap.
				var ws syscall.WaitStatus
				_, _ = syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
			}
		}
		prev = cur
	}
}

// zombieChildren returns the pids of zombie (state Z) processes whose parent is
// this process — i.e. orphans reparented to the pod init.
func zombieChildren() []int {
	self := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		state, ppid, ok := procStat(pid)
		if ok && ppid == self && state == 'Z' {
			out = append(out, pid)
		}
	}
	return out
}

// procStat reads the state byte and ppid from /proc/<pid>/stat. The comm field
// (2nd) is wrapped in parens and may contain spaces, so it's skipped by scanning
// past the last ')'.
func procStat(pid int) (state byte, ppid int, ok bool) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	s := string(data)
	rparen := strings.LastIndexByte(s, ')')
	if rparen < 0 || rparen+2 >= len(s) {
		return 0, 0, false
	}
	fields := strings.Fields(s[rparen+2:]) // state ppid pgrp ...
	if len(fields) < 2 || len(fields[0]) == 0 {
		return 0, 0, false
	}
	pp, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], pp, true
}
