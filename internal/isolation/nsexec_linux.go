//go:build linux

package isolation

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// MaybeRunNSExec checks whether this process was re-executed as the namespace-launch
// helper (argv[1] == nsExecArg, built by nsExecArgs) and, if so, enters the requested
// cgroup / network namespace / private mount namespace, performs the bind mounts, and
// execs the real command in place. It does NOT return on the helper path — it either
// execs (replacing this process) or log.Fatals; on a normal sandboxd invocation it
// returns immediately. Call it as the first line of main(), before flag parsing (the
// helper's argv is not the normal sandboxd flag set).
//
// This replaces the former sh→ip netns exec→unshare→sh wrapper (4 forks) with a
// single fork: the parent forks this helper, the helper does the setup and execs
// firecracker in place (same PID, so the supervisor tracks firecracker and signals
// reach it; stdio fds and env are inherited across the exec). setns(CLONE_NEWNET) and
// unshare(CLONE_NEWNS) are both legal in a multithreaded program, so no cgo
// pre-runtime constructor is needed — we lock the OS thread so the entered netns and
// unshared mount ns belong to the thread we exec from (execve keeps that thread).
func MaybeRunNSExec() {
	if len(os.Args) < 2 || os.Args[1] != nsExecArg {
		return
	}
	// Pin to this thread: setns/unshare below are thread-scoped, and execve keeps the
	// calling thread's namespaces, so all setup and the exec must stay on one thread.
	runtime.LockOSThread()

	fs := flag.NewFlagSet(nsExecArg, flag.ExitOnError)
	cgroup := fs.String("cgroup", "", "cgroup dir to join")
	netnsName := fs.String("netns", "", "named network namespace to enter")
	var binds nsBinds
	fs.Var(&binds, "bind", "src=dst bind mount (repeatable); implies a private mount ns")
	if err := fs.Parse(os.Args[2:]); err != nil {
		log.Fatalf("nsexec: parse args: %v", err)
	}
	cmd := fs.Args() // everything after "--": the real bin + its args
	if len(cmd) == 0 {
		log.Fatalf("nsexec: no command to exec")
	}

	// cgroup: join by writing our PID (which becomes firecracker's after exec), so the
	// VMM's threads are accounted under the sandbox cgroup. Done first — it's
	// independent of the namespaces and reads the host /sys/fs/cgroup.
	if *cgroup != "" {
		if err := os.MkdirAll(*cgroup, 0o755); err != nil {
			log.Fatalf("nsexec: mkdir cgroup %s: %v", *cgroup, err)
		}
		if err := os.WriteFile(*cgroup+"/cgroup.procs", []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
			log.Fatalf("nsexec: join cgroup %s: %v", *cgroup, err)
		}
	}

	// netns: enter the VM's network namespace so firecracker opens its tap there.
	if *netnsName != "" {
		fd, err := openNamedNetns(*netnsName)
		if err != nil {
			log.Fatalf("nsexec: open netns %s: %v", *netnsName, err)
		}
		if err := unix.Setns(fd, unix.CLONE_NEWNET); err != nil {
			log.Fatalf("nsexec: setns %s: %v", *netnsName, err)
		}
		_ = unix.Close(fd)
	}

	// mount ns: a private mount ns + the per-VM binds over the canonical paths the
	// base snapshot recorded. After the netns so the binds nest inside it; make-
	// rprivate stops them propagating back to the host and other VMs.
	if len(binds) > 0 {
		if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
			log.Fatalf("nsexec: unshare mount ns: %v", err)
		}
		if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
			log.Fatalf("nsexec: make-rprivate /: %v", err)
		}
		for _, b := range binds {
			src, dst, ok := strings.Cut(b, "=")
			if !ok {
				log.Fatalf("nsexec: malformed -bind %q (want src=dst)", b)
			}
			if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
				log.Fatalf("nsexec: bind %s -> %s: %v", src, dst, err)
			}
		}
	}

	// Resolve the binary: syscall.Exec needs a path (no PATH search), but fcBin may be
	// a bare name ("firecracker") that the old `sh` would have found via PATH. Use the
	// same (inherited) PATH the shell would have.
	bin := cmd[0]
	if !filepath.IsAbs(bin) {
		resolved, err := exec.LookPath(bin)
		if err != nil {
			log.Fatalf("nsexec: resolve %s: %v", bin, err)
		}
		bin = resolved
	}

	// Exec the real command in place: same PID, inheriting our stdio fds and env.
	if err := syscall.Exec(bin, cmd, os.Environ()); err != nil {
		log.Fatalf("nsexec: exec %s: %v", bin, err)
	}
}

// nsBinds collects repeated -bind src=dst flags.
type nsBinds []string

func (b *nsBinds) String() string     { return strings.Join(*b, ",") }
func (b *nsBinds) Set(v string) error { *b = append(*b, v); return nil }

// openNamedNetns opens a named network namespace (a bind mount created by
// `ip netns add` / vishvananda netns) under whichever of the conventional dirs holds
// it on this distro.
func openNamedNetns(name string) (int, error) {
	for _, base := range []string{"/var/run/netns/", "/run/netns/"} {
		if fd, err := unix.Open(base+name, unix.O_RDONLY|unix.O_CLOEXEC, 0); err == nil {
			return fd, nil
		}
	}
	return -1, fmt.Errorf("named netns %q not found under /var/run/netns or /run/netns", name)
}
