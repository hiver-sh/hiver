// Command sbxfuse is the per-sandbox FUSE filesystem daemon.
//
// Prototype scope (T69, T71, T75, T79): Linux-only passthrough mount over a
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
	"time"

	"github.com/sandbox-platform/agent-sandbox/internal/fusefs"
	"github.com/sandbox-platform/agent-sandbox/internal/remotefs"
)

func main() {
	var (
		mountPoint   = flag.String("mount", "/workspace", "FUSE mount point (agent-visible path root)")
		backendDir   = flag.String("backend", "", "host directory backing the workspace (required)")
		aclPath      = flag.String("acls", "", "path to JSON file containing ACL rules")
		auditPath    = flag.String("audit", "", "audit log file (default: stderr)")
		auditReads   = flag.Bool("audit-reads", false, "audit each FUSE Read request (chatty: many events per user-level read)")
		remoteName   = flag.String("remote", "", "remote backend name; blank = local-only. Supported: gdrive. Unimplemented: s3, gcs, onedrive.")
		remoteConfig = flag.String("remote-config", "", "JSON config consumed by the remote backend (per-impl schema; see remotefs.GoogleDriveConfig).")
		oplogDepth   = flag.Int("oplog-depth", 1024, "oplog queue size; Enqueue blocks when full")
	)
	flag.Parse()

	if *backendDir == "" {
		log.Fatalf("-backend is required (host directory)")
	}

	rules := loadACLs(*aclPath)
	var auditOut io.Writer = os.Stderr
	if *auditPath != "" {
		f, err := os.OpenFile(*auditPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			log.Fatalf("audit log: %v", err)
		}
		defer f.Close()
		auditOut = f
	}

	cfg := fusefs.Config{
		MountPoint: *mountPoint,
		Backend:    *backendDir,
		ACLs:       fusefs.Compile(rules),
		Audit:      auditOut,
		AuditReads: *auditReads,
	}

	store, err := buildStore(context.Background(), *remoteName, []byte(*remoteConfig))
	if err != nil {
		log.Fatalf("remote: %v", err)
	}
	if store != nil {
		cfg.Oplog = fusefs.NewOplog(store, *oplogDepth)
		ctxBoot, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if err := fusefs.Bootstrap(ctxBoot, store, *backendDir, *mountPoint); err != nil {
			cancel()
			log.Fatalf("bootstrap: %v", err)
		}
		cancel()
		log.Printf("sbxfuse: bootstrapped from %q into %s", *remoteName, *backendDir)
	}

	srv, err := fusefs.Mount(cfg)
	if err != nil {
		log.Fatalf("fusefs.Mount: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	remoteDesc := *remoteName
	if remoteDesc == "" {
		remoteDesc = "local-only"
	}
	log.Printf("sbxfuse: mounted %s -> %s with %d ACL rules (remote=%s)", *mountPoint, *backendDir, len(rules), remoteDesc)
	if err := srv.Serve(ctx); err != nil {
		log.Fatalf("fusefs.Serve: %v", err)
	}
}

func buildStore(ctx context.Context, name string, configJSON []byte) (remotefs.Store, error) {
	switch name {
	case "":
		return nil, nil
	case "gdrive":
		cfg, err := remotefs.ParseGoogleDriveConfig(configJSON)
		if err != nil {
			return nil, err
		}
		return remotefs.NewGoogleDrive(ctx, cfg)
	case "s3", "gcs", "onedrive":
		return nil, fmt.Errorf("backend %q is not implemented yet", name)
	}
	return nil, fmt.Errorf("unknown -remote backend %q", name)
}

func loadACLs(p string) []fusefs.Rule {
	if p == "" {
		// Sensible default for the prototype: workspace is rw, everything else denied.
		return []fusefs.Rule{{Path: "/", Access: fusefs.AccessRW}}
	}
	data, err := os.ReadFile(p)
	if err != nil {
		log.Fatalf("read ACL file: %v", err)
	}
	var rules []fusefs.Rule
	if err := json.Unmarshal(data, &rules); err != nil {
		log.Fatalf("parse ACL file: %v", err)
	}
	return rules
}
