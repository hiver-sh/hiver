// Command sbxfuse is the per-sandbox FUSE filesystem daemon.
//
// Prototype scope (T69, T71, T75, T79): Linux-only passthrough mount over a
// host directory backend, with longest-prefix-match ACL evaluation (deny → ENOENT)
// and JSON-line audit logging.
//
// Out of scope for v0: COW upper layer (T76), quotas (T77), inline scanning (T78),
// cloud / encrypted backends (T72–T74), CSI integration (T81–T82).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/sandbox-platform/agent-sandbox/internal/fusefs"
)

func main() {
	var (
		mountPoint = flag.String("mount", "/workspace", "FUSE mount point (agent-visible path root)")
		backend    = flag.String("backend", "", "host directory backing the workspace (required)")
		aclPath    = flag.String("acls", "", "path to JSON file containing ACL rules")
		auditPath  = flag.String("audit", "", "audit log file (default: stderr)")
		auditReads = flag.Bool("audit-reads", false, "audit each FUSE Read request (chatty: many events per user-level read)")
	)
	flag.Parse()

	if *backend == "" {
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

	srv, err := fusefs.Mount(fusefs.Config{
		MountPoint: *mountPoint,
		Backend:    *backend,
		ACLs:       fusefs.Compile(rules),
		Audit:      auditOut,
		AuditReads: *auditReads,
	})
	if err != nil {
		log.Fatalf("fusefs.Mount: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Printf("sbxfuse: mounted %s -> %s with %d ACL rules", *mountPoint, *backend, len(rules))
	if err := srv.Serve(ctx); err != nil {
		log.Fatalf("fusefs.Serve: %v", err)
	}
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
