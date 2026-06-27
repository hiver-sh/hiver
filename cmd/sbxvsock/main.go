// Command sbxvsock bridges a single exec session from the host to the in-guest
// agent over the per-VM netns network (TCP). It is the host end of the vsockexec
// framed protocol: it dials the guest exec port, sends a Start frame describing
// the command, relays the local stdin/stdout/stderr to/from the guest, and exits
// with the workload's exit code. (The name predates the vsock→TCP move; kept for
// compatibility with the exec call site.)
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

	"github.com/creack/pty"
	"github.com/hiver-sh/hiver/internal/vsockexec"
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
		addr    = flag.String("addr", "", "guest exec address host:port (required)")
		mark    = flag.Int("mark", 0, "SO_MARK stamped on the dial (netns egress bypass)")
		command = flag.String("command", "", "command to run in the guest (required)")
		cwd     = flag.String("cwd", "", "working directory")
		tty     = flag.Bool("tty", false, "allocate a tty in the guest")
		session = flag.String("session", "", "detachable session id: the guest keeps the process alive across a dropped connection and re-attaches it on reconnect (empty = one-shot exec)")
		env     envFlag
	)
	flag.Var(&env, "env", "environment KEY=VALUE (repeatable)")
	flag.Parse()

	if *addr == "" || *command == "" {
		flag.Usage()
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	conn, err := dialGuestTCP(ctx, *addr, *mark)
	if err != nil {
		log.Fatalf("connect guest exec %s: %v", *addr, err)
	}
	defer conn.Close()

	// Serialise all host→guest frame writes: the stdin pump and the SIGWINCH
	// resize watcher both write to the same vsock stream, and one frame's
	// header+payload must not interleave with another's.
	var writeMu sync.Mutex
	writeFrame := func(t vsockexec.FrameType, payload []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return vsockexec.WriteFrame(conn, t, payload)
	}
	writeJSON := func(t vsockexec.FrameType, v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return vsockexec.WriteJSON(conn, t, v)
	}

	start := vsockexec.Start{
		Command:   *command,
		Cwd:       *cwd,
		Env:       envMap(env),
		TTY:       *tty,
		SessionID: *session,
	}
	// Seed the guest pty with the current window size so the program starts at
	// the right dimensions; SIGWINCH (below) carries later changes. os.Stdin is
	// the pty slave the exec handler wired up.
	if *tty {
		if rows, cols, err := pty.Getsize(os.Stdin); err == nil {
			start.Rows, start.Cols = uint16(rows), uint16(cols)
		}
	}
	if err := writeJSON(vsockexec.FrameStart, start); err != nil {
		log.Fatalf("send start: %v", err)
	}

	// Relay terminal resizes: the exec handler resizes the outer pty master,
	// the kernel delivers SIGWINCH here (we hold its controlling tty), and we
	// forward the new size to the guest, which applies it to the in-guest pty.
	// Without this the guest pty is stuck at its startup size and clears leave
	// stale content — the same failure the container tty path avoids via SIGWINCH.
	if *tty {
		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for range winch {
				rows, cols, err := pty.Getsize(os.Stdin)
				if err != nil {
					continue
				}
				_ = writeJSON(vsockexec.FrameResize, vsockexec.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
			}
		}()
	}

	// Pump local stdin → guest as Stdin frames; signal EOF with StdinClose.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, rerr := os.Stdin.Read(buf)
			if n > 0 {
				if werr := writeFrame(vsockexec.FrameStdin, buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				_ = writeFrame(vsockexec.FrameStdinClose, nil)
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
