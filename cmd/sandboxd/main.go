// Command sandboxd is the runtime agent that wires together the MITM proxy,
// FUSE daemon, and agent workloads as a sandbox "pod". This binary owns the
// command surface — the microvm namespace re-exec entrypoint and flag parsing —
// and hands the parsed options to internal/sandboxd, where all the runtime logic
// lives.
package main

import (
	"flag"

	"github.com/hiver-sh/hiver/internal/isolation"
	"github.com/hiver-sh/hiver/internal/sandboxd"
)

func main() {
	// If this process was re-executed as the microvm namespace-launch helper, enter
	// the requested cgroup/netns/mount-ns, bind the per-VM overlay, and exec the VMM
	// in place — a single fork in place of the old sh→ip netns exec→unshare→sh chain.
	// Returns immediately on a normal sandboxd start; never returns on the helper path
	// (it execs or fatals). Must precede flag parsing — its argv isn't the flag set.
	isolation.MaybeRunNSExec()

	var opts sandboxd.Options
	flag.StringVar(&opts.APIServerPort, "api-server-port", "8099", "port of the API server")
	flag.BoolVar(&opts.Pack, "pack", false, "run as a multi-tenant pack host: park and serve N same-image sandboxes created on demand via POST /v1/<key>, outliving any single sandbox. When omitted, sandboxd hosts a single sandbox and its own lifecycle follows that sandbox's — the process exits once the sandbox is shut down or killed.")
	flag.StringVar(&opts.SnapshotDir, "snapshot-dir", "", "directory where files and VM-state snapshots are stored on local disk (skip the pod overlay — point it at NVMe); optional — when unset, files snapshots only work for configs that route them to a FUSE drive via snapshot.files.mount, and VM snapshots are disabled")
	flag.IntVar(&opts.PreallocPool, "prealloc-pool", 10, "number of sandbox network slots (netns/veth/iptables + DNS sink) to preallocate and keep warm so a create claims one instead of wiring it on the request path; 0 disables")
	flag.IntVar(&opts.MaxConcurrentLaunches, "max-concurrent-launches", 10, "cap on concurrent sandbox creates in flight, so a burst doesn't oversubscribe the node's cores during the CPU-bound resume/convergence phases; set near the node's vCPU count; 0 disables (unbounded)")
	flag.Parse()

	sandboxd.Run(opts)
}
