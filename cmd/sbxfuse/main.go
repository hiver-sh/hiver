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
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/hiver-sh/hiver/internal/fusefs"
	"github.com/hiver-sh/hiver/internal/remotefs"
)

func main() {
	var (
		mountPoint   = flag.String("mount", "/workspace", "FUSE mount point (agent-visible path root)")
		backendDir   = flag.String("backend", "", "host directory backing the workspace (required)")
		aclPath      = flag.String("acls", "", "path to JSON file containing ACL rules")
		eventsFD     = flag.Int("events-fd", 0, "inherited fd for streaming audit events; overrides -audit when >0 (set by sandboxd)")
		remoteName   = flag.String("remote", "", "remote backend name; blank = local-only. Supported: gdrive, gcs. Unimplemented: s3, onedrive.")
		remoteConfig = flag.String("remote-config", "", "JSON config consumed by the remote backend (per-impl schema; see remotefs.GoogleDriveConfig).")
		oplogDepth   = flag.Int("oplog-depth", 1024, "oplog queue size; Enqueue blocks when full")
		outboundMark = flag.Int("mark", 0, "SO_MARK to stamp on outbound TCP from the remote backend's HTTP client; needed inside the sandbox-pod to escape iptables OUTPUT REDIRECT.")
	)
	flag.Parse()

	if *backendDir == "" {
		log.Fatalf("-backend is required (host directory)")
	}

	rules := loadACLs(*aclPath)
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

	cfg := fusefs.Config{
		MountPoint: *mountPoint,
		Backend:    *backendDir,
		ACLs:       fusefs.Compile(rules),
		Audit:      auditOut,
	}

	log.Printf("sbxfuse: starting (remote=%q, mount=%s, backend=%s, mark=0x%x)",
		*remoteName, *mountPoint, *backendDir, *outboundMark)

	// Remote-backend HTTP traffic (Drive API + OAuth) is logged to
	// sbxfuse's stdout as one JSON line per request; sandboxd's
	// streamPrefixed will re-emit each line as `[sbxfuse:<slug>:out]`,
	// so the user can grep the pod log instead of tailing a file.
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
	case "s3", "onedrive":
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
