// Command sbxvsock bridges a single exec session from the host to the
// in-guest agent over a Firecracker vsock stream. It is the host end of the
// vsockexec protocol: it dials the guest exec port, sends a Start frame
// describing the command, relays the local stdin/stdout/stderr to/from the
// guest, and exits with the workload's exit code.
//
// The microvm isolation backend returns `sbxvsock ...` as the exec command,
// so sandboxd's exec handlers drive it exactly like `runc exec`: they wire
// pipes (or a pty slave) to its stdio and read the exit status from Wait().
package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/blasten/hive/internal/firecracker"
	"github.com/blasten/hive/internal/vsockexec"
)

type envFlag []string

func (e *envFlag) String() string { return strings.Join(*e, ",") }
func (e *envFlag) Set(v string) error {
	*e = append(*e, v)
	return nil
}

func main() {
	log.SetFlags(0)
	log.SetPrefix("sbxvsock: ")

	var (
		uds     = flag.String("uds", "", "firecracker vsock host unix socket (required)")
		port    = flag.Int("port", int(firecracker.GuestExecPort), "guest vsock exec port")
		command = flag.String("command", "", "command to run in the guest (required)")
		cwd     = flag.String("cwd", "", "working directory")
		tty     = flag.Bool("tty", false, "allocate a tty in the guest")
		env     envFlag
	)
	flag.Var(&env, "env", "environment KEY=VALUE (repeatable)")
	flag.Parse()

	if *uds == "" || *command == "" {
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	conn, err := firecracker.DialGuest(ctx, *uds, uint32(*port))
	if err != nil {
		log.Fatalf("connect guest exec port: %v", err)
	}
	defer conn.Close()

	start := vsockexec.Start{
		Command: *command,
		Cwd:     *cwd,
		Env:     envMap(env),
		TTY:     *tty,
	}
	if err := vsockexec.WriteJSON(conn, vsockexec.FrameStart, start); err != nil {
		log.Fatalf("send start: %v", err)
	}

	// Pump local stdin → guest as Stdin frames; signal EOF with StdinClose.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := os.Stdin.Read(buf)
			if n > 0 {
				if werr := vsockexec.WriteFrame(conn, vsockexec.FrameStdin, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				_ = vsockexec.WriteFrame(conn, vsockexec.FrameStdinClose, nil)
				return
			}
		}
	}()

	// Read guest frames → local stdout/stderr; FrameExit ends the session.
	exitCode := 0
	var stdoutMu sync.Mutex
	for {
		t, payload, rerr := vsockexec.ReadFrame(conn)
		if rerr != nil {
			if rerr != io.EOF {
				log.Printf("read guest: %v", rerr)
			}
			break
		}
		switch t {
		case vsockexec.FrameStdout:
			stdoutMu.Lock()
			_, _ = os.Stdout.Write(payload)
			stdoutMu.Unlock()
		case vsockexec.FrameStderr:
			_, _ = os.Stderr.Write(payload)
		case vsockexec.FrameExit:
			exitCode = decodeExit(payload)
		}
		if t == vsockexec.FrameExit {
			break
		}
	}
	os.Exit(exitCode)
}

func envMap(entries []string) map[string]string {
	if len(entries) == 0 {
		return nil
	}
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		if k, v, ok := strings.Cut(e, "="); ok {
			m[k] = v
		}
	}
	return m
}

func decodeExit(payload []byte) int {
	var ex vsockexec.Exit
	if err := json.Unmarshal(payload, &ex); err != nil {
		return 0
	}
	return ex.Code
}
