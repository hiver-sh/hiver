// Command sbxfuse is the per-sandbox FUSE filesystem daemon.
//
// Scope (T69, T71, T75, T79): Linux-only passthrough mount over a
// host directory backend, with longest-prefix-match ACL evaluation (deny → ENOENT)
// and JSON-line audit logging.
//
// Out of scope for v0: COW upper layer (T76), quotas (T77), inline scanning (T78),
// CSI integration (T81–T82).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"

	"github.com/hiver-sh/hiver/internal/fusefs"
	"github.com/hiver-sh/hiver/internal/remotefs"
)

func main() {
	defer func() {
		if r := recover(); r != nil {
			buf := make([]byte, 1<<16)
			n := runtime.Stack(buf, true)
			log.Fatalf("sbxfuse: panic: %v\n%s", r, buf[:n])
		}
	}()
	var (
		mountPoint   = flag.String("mount", "/workspace", "FUSE mount point (agent-visible path root)")
		backendDir   = flag.String("backend", "", "host directory backing the workspace (required)")
		aclPath      = flag.String("acls", "", "path to JSON file containing ACL rules")
		eventsFD     = flag.Int("events-fd", 0, "inherited fd for streaming audit events; overrides -audit when >0 (set by sandboxd)")
		remoteName   = flag.String("remote", "", "remote backend name; blank = local-only. Supported: gdrive, gcs, s3, external. Unimplemented: onedrive.")
		remoteConfig = flag.String("remote-config", "", "JSON config consumed by the remote backend (per-impl schema; see remotefs.GoogleDriveConfig).")
		oplogDepth   = flag.Int("oplog-depth", 1024, "oplog queue size; Enqueue blocks when full")
		outboundMark = flag.Int("mark", 0, "SO_MARK to stamp on outbound TCP from the remote backend's HTTP client; needed inside the sandbox-pod to escape iptables OUTPUT REDIRECT.")
		control      = flag.Bool("control", false, "control mode: serve N workspaces in one process, driven by mount/unmount/reacl commands read as JSON lines on stdin (set by sandboxd; -mount/-backend/-acls ignored)")
	)
	flag.Parse()

	// -events-fd is the sandboxd-driven path: a socketpair fd inherited
	// from the parent. Stream audit events there instead of a file —
	// no disk I/O, backpressure is the kernel socket buffer.
	var auditOut io.Writer = os.Stderr
	if *eventsFD > 0 {
		f := os.NewFile(uintptr(*eventsFD), "events")
		if f == nil {
			log.Fatalf("events-fd=%d not open", *eventsFD)
		}
		defer f.Close()
		auditOut = f
	}

	// Control mode: one sbxfuse process per pod serving every sandbox's
	// workspaces, so the pod has a single shared FUSE daemon (design §9). sandboxd
	// adds/removes mounts over the stdin command channel as keyed sandboxes are
	// created and destroyed; this process owns N independent fusefs.Servers.
	if *control {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer cancel()
		runControl(ctx, auditOut, *oplogDepth)
		return
	}

	if *backendDir == "" {
		log.Fatalf("-backend is required (host directory)")
	}

	rules := loadACLs(*aclPath)

	cfg := fusefs.Config{
		MountPoint: *mountPoint,
		Backend:    *backendDir,
		ACLs:       fusefs.Compile(rules),
		Audit:      auditOut,
	}

	log.Printf("sbxfuse: starting (remote=%q, mount=%s, backend=%s, mark=0x%x)",
		*remoteName, *mountPoint, *backendDir, *outboundMark)

	// Remote-backend HTTP traffic (Drive API + OAuth) is logged to
	// sbxfuse's stdout as one JSON line per request; sandboxd forwards
	// each line verbatim to the pod log (see streamLines), so the user
	// can grep the pod log instead of tailing a file.
	log.Printf("sbxfuse: building remote store…")
	store, err := buildStore(context.Background(), *remoteName, []byte(*remoteConfig), *outboundMark, os.Stdout)
	if err != nil {
		log.Fatalf("remote: %v", err)
	}
	if store != nil {
		// Wire the store as both write-back target (Oplog) and read-side
		// authority (Config.Remote). The local backend dir is now a write
		// buffer only — no Bootstrap pre-fetch, no read cache. Reads on
		// remote-backed mounts always consult the upstream store via the
		// fusefs handlers (see Lookup/Attr/ReadDirAll/Open).
		cfg.Oplog = fusefs.NewOplog(store, *oplogDepth)
		cfg.Remote = store
		log.Printf("sbxfuse: remote-backed mount (%q) — local %s is a write buffer; reads go to upstream",
			*remoteName, *backendDir)
	}

	srv, err := fusefs.Mount(cfg)
	if err != nil {
		log.Fatalf("fusefs.Mount: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// SIGHUP → reload ACLs from -acls. sandboxd signals this after a
	// /v1/config PUT rewrites the file. Read failures keep the
	// current policy in place — a half-written file can't relax
	// access by accident.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				newRules, err := readACLs(*aclPath)
				if err != nil {
					log.Printf("sbxfuse: SIGHUP reload failed (keeping current ACLs): %v", err)
					continue
				}
				srv.SetACLs(fusefs.Compile(newRules))
				log.Printf("sbxfuse: reloaded %d ACL rules from %s", len(newRules), *aclPath)
			}
		}
	}()

	remoteDesc := *remoteName
	if remoteDesc == "" {
		remoteDesc = "local-only"
	}
	log.Printf("sbxfuse: mounted %s -> %s with %d ACL rules (remote=%s)", *mountPoint, *backendDir, len(rules), remoteDesc)
	if err := srv.Serve(ctx); err != nil {
		log.Fatalf("fusefs.Serve: %v", err)
	}
}

// ctrlCmd is one line on the stdin command channel in control mode. sandboxd
// emits these as keyed sandboxes' workspaces come and go; the JSON shape mirrors
// the single-mount flags plus an op selector.
type ctrlCmd struct {
	Op           string `json:"op"`             // "mount" | "unmount" | "reacl"
	Mount        string `json:"mount"`          // host FUSE mount point (the key in the live set)
	Backend      string `json:"backend"`        // host directory backing the workspace (mount)
	ACLs         string `json:"acls"`           // path to the mount's ACL JSON file (mount/reacl)
	Remote       string `json:"remote"`         // remote backend name; blank = local-only
	RemoteConfig string `json:"remote_config"`  // remote backend JSON config
	Mark         int    `json:"mark"`           // SO_MARK for the remote backend's HTTP client
	OplogDepth   int    `json:"oplog_depth"`    // oplog queue size; 0 = use default
}

// lockedWriter serializes concurrent writes from the N fusefs.Servers that share
// one audit sink in control mode, so their JSON event lines never interleave.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// ctrlMount is one live workspace served by the control-mode process.
type ctrlMount struct {
	srv     *fusefs.Server
	cancel  context.CancelFunc
	aclPath string
}

// runControl is the control-mode event loop: it reads ctrlCmds from stdin and
// maintains a set of independent FUSE mounts, one fusefs.Server each, all sharing
// the single audit sink. Stdin EOF (sandboxd closing the pipe or exiting) tears
// every mount down. A failure on one command is logged and never aborts the loop
// — one bad mount must not take down the pod's other sandboxes.
func runControl(ctx context.Context, audit io.Writer, defaultOplogDepth int) {
	aw := &lockedWriter{w: audit}
	var mu sync.Mutex
	mounts := map[string]*ctrlMount{}

	// On SIGTERM/SIGINT (ctx cancel) close stdin so the blocking Decode below
	// returns promptly, rather than waiting for sandboxd's WaitDelay kill.
	go func() {
		<-ctx.Done()
		_ = os.Stdin.Close()
	}()

	log.Printf("sbxfuse: control mode — awaiting mount commands on stdin")
	dec := json.NewDecoder(os.Stdin)
	for {
		var cmd ctrlCmd
		if err := dec.Decode(&cmd); err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				log.Printf("sbxfuse: control: decode: %v", err)
			}
			break
		}
		switch cmd.Op {
		case "mount":
			if err := controlMount(ctx, aw, defaultOplogDepth, cmd, &mu, mounts); err != nil {
				log.Printf("sbxfuse: control: mount %s: %v", cmd.Mount, err)
			} else {
				log.Printf("sbxfuse: control: mounted %s -> %s", cmd.Mount, cmd.Backend)
			}
		case "unmount":
			mu.Lock()
			in := mounts[cmd.Mount]
			delete(mounts, cmd.Mount)
			mu.Unlock()
			if in != nil {
				in.cancel()
				log.Printf("sbxfuse: control: unmounted %s", cmd.Mount)
			}
		case "reacl":
			mu.Lock()
			in := mounts[cmd.Mount]
			mu.Unlock()
			if in == nil {
				continue
			}
			rules, err := readACLs(in.aclPath)
			if err != nil {
				log.Printf("sbxfuse: control: reacl %s (keeping current): %v", cmd.Mount, err)
				continue
			}
			in.srv.SetACLs(fusefs.Compile(rules))
			log.Printf("sbxfuse: control: reloaded %d ACL rules for %s", len(rules), cmd.Mount)
		default:
			log.Printf("sbxfuse: control: unknown op %q", cmd.Op)
		}
	}

	mu.Lock()
	for _, in := range mounts {
		in.cancel()
	}
	mu.Unlock()
}

// controlMount builds and starts one FUSE server for a mount command, registering
// it in the live set. The server runs on its own child context so a later
// "unmount" (or the parent shutting down) cancels just this mount.
func controlMount(ctx context.Context, audit io.Writer, defaultOplogDepth int, cmd ctrlCmd, mu *sync.Mutex, mounts map[string]*ctrlMount) error {
	if cmd.Mount == "" || cmd.Backend == "" {
		return errors.New("mount and backend required")
	}
	rules, err := readACLs(cmd.ACLs)
	if err != nil {
		return fmt.Errorf("acls: %w", err)
	}
	cfg := fusefs.Config{
		MountPoint: cmd.Mount,
		Backend:    cmd.Backend,
		ACLs:       fusefs.Compile(rules),
		Audit:      audit,
	}
	store, err := buildStore(ctx, cmd.Remote, []byte(cmd.RemoteConfig), cmd.Mark, os.Stdout)
	if err != nil {
		return fmt.Errorf("remote: %w", err)
	}
	if store != nil {
		depth := cmd.OplogDepth
		if depth == 0 {
			depth = defaultOplogDepth
		}
		cfg.Oplog = fusefs.NewOplog(store, depth)
		cfg.Remote = store
	}
	srv, err := fusefs.Mount(cfg)
	if err != nil {
		return err
	}
	mctx, cancel := context.WithCancel(ctx)
	go func() {
		if err := srv.Serve(mctx); err != nil && mctx.Err() == nil {
			log.Printf("sbxfuse: control: serve %s: %v", cmd.Mount, err)
		}
	}()
	mu.Lock()
	mounts[cmd.Mount] = &ctrlMount{srv: srv, cancel: cancel, aclPath: cmd.ACLs}
	mu.Unlock()
	return nil
}

func buildStore(ctx context.Context, name string, configJSON []byte, outboundMark int, requestLog io.Writer) (remotefs.Store, error) {
	switch name {
	case "":
		return nil, nil
	case "gdrive":
		cfg, err := remotefs.ParseGoogleDriveConfig(configJSON)
		if err != nil {
			return nil, err
		}
		return remotefs.NewGoogleDrive(ctx, cfg, outboundMark, requestLog)
	case "gcs":
		cfg, err := remotefs.ParseGoogleCloudStorageConfig(configJSON)
		if err != nil {
			return nil, err
		}
		return remotefs.NewGoogleCloudStorage(ctx, cfg, outboundMark, requestLog)
	case "s3":
		cfg, err := remotefs.ParseS3Config(configJSON)
		if err != nil {
			return nil, err
		}
		return remotefs.NewS3(ctx, cfg, outboundMark, requestLog)
	case "external":
		cfg, err := remotefs.ParseExternalConfig(configJSON)
		if err != nil {
			return nil, err
		}
		return remotefs.NewExternal(ctx, cfg, outboundMark, requestLog)
	case "onedrive":
		return nil, fmt.Errorf("backend %q is not implemented yet", name)
	}
	return nil, fmt.Errorf("unknown -remote backend %q", name)
}

func loadACLs(p string) []fusefs.Rule {
	rules, err := readACLs(p)
	if err != nil {
		log.Fatalf("%v", err)
	}
	return rules
}

// readACLs is the non-fatal sibling of loadACLs; SIGHUP reloads use it
// so a transient on-disk error doesn't crash the FUSE daemon.
func readACLs(p string) ([]fusefs.Rule, error) {
	if p == "" {
		// Default: workspace is rw, everything else denied.
		return []fusefs.Rule{{Path: "/", Access: fusefs.AccessRW}}, nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, fmt.Errorf("read ACL file %s: %w", p, err)
	}
	var rules []fusefs.Rule
	if err := json.Unmarshal(data, &rules); err != nil {
		return nil, fmt.Errorf("parse ACL file %s: %w", p, err)
	}
	return rules, nil
}
