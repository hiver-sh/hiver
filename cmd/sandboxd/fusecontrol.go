package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
	"github.com/hiver-sh/hiver/internal/events"
)

// fuseControl drives the single, pod-wide sbxfuse process (design §9). Instead of
// one sbxfuse per workspace, the pod runs one `sbxfuse -control`; sandboxd adds
// and removes a sandbox's workspaces over the process's stdin command channel as
// keyed sandboxes are created and destroyed. Audit events from every mount stream
// back over one events fd and are demultiplexed by host-mount prefix to the
// owning sandbox's broker (see sharedFuseTranslator).
type fuseControl struct {
	mu    sync.Mutex
	enc   *json.Encoder // guarded by mu: serializes command writes to stdin
	stdin io.WriteCloser
	cmd   *exec.Cmd
	trans *sharedFuseTranslator
}

// fuseMountSpec is one command on the control channel. Its JSON shape must match
// sbxfuse's ctrlCmd.
type fuseMountSpec struct {
	Op           string `json:"op"`
	Mount        string `json:"mount"`
	Backend      string `json:"backend,omitempty"`
	ACLs         string `json:"acls,omitempty"`
	Remote       string `json:"remote,omitempty"`
	RemoteConfig string `json:"remote_config,omitempty"`
	Mark         int    `json:"mark,omitempty"`
	OplogDepth   int    `json:"oplog_depth,omitempty"`
}

// startFuseControl spawns the pod's shared `sbxfuse -control` process. It wires an
// events socketpair (fd 3 in the child) whose stream is demultiplexed per mount,
// and a stdin pipe over which Mount/Unmount/ReACL issue commands. The process
// runs on ctx; cancelling it (e.g. cancelFS at shutdown) unmounts everything.
func startFuseControl(ctx context.Context, wg *sync.WaitGroup, fuseBin string) (*fuseControl, error) {
	parent, child, err := newEventsPair()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, fuseBin, "-control", "-events-fd", strconv.Itoa(eventsFD))
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	cmd.ExtraFiles = []*os.File{child}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = parent.Close()
		_ = child.Close()
		return nil, err
	}
	fc := &fuseControl{stdin: stdin, enc: json.NewEncoder(stdin), cmd: cmd, trans: newSharedFuseTranslator()}
	// superviseStdio starts the process; sbxfuse's stdout/stderr is operational
	// logging (mount messages), surfaced as prefixed pod-log lines, not events.
	if _, err := superviseStdio(wg, "sbxfuse", cmd, nil); err != nil {
		_ = parent.Close()
		_ = child.Close()
		return nil, err
	}
	_ = child.Close()
	wg.Add(1)
	go func() {
		defer wg.Done()
		ingestEvents(ctx, parent, "sbxfuse", sidecarOnEvent(formatFuseEvent, fc.trans.handle))
	}()
	return fc, nil
}

func (fc *fuseControl) send(spec fuseMountSpec) error {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.enc.Encode(spec)
}

// Mount adds a workspace to the shared process. The caller must register the
// mount with the translator first so its audit events resolve.
func (fc *fuseControl) Mount(spec fuseMountSpec) error {
	spec.Op = "mount"
	return fc.send(spec)
}

// Unmount removes a workspace. The caller unregisters from the translator after.
func (fc *fuseControl) Unmount(hostMount string) error {
	return fc.send(fuseMountSpec{Op: "unmount", Mount: hostMount})
}

// ReACL tells the process to reload a mount's ACL file (already rewritten on disk).
func (fc *fuseControl) ReACL(hostMount string) error {
	return fc.send(fuseMountSpec{Op: "reacl", Mount: hostMount})
}

// fuseMountReg is the per-mount routing state the translator needs to turn a raw
// audit event back into a keyed sandbox's fs.request/fs.response SandboxEvent.
type fuseMountReg struct {
	guestMount string
	backend    gen.Backend
	broker     *events.Broker
	corr       *correlator
}

// sharedFuseTranslator demultiplexes the single audit stream from the pod-wide
// sbxfuse: each event's path is matched (longest host-mount prefix) to its owning
// sandbox, the path is rewritten host→guest, and the event is published to that
// sandbox's broker via the standard fuseTranslator logic with a per-mount
// correlator (so request/response ids never collide across mounts).
type sharedFuseTranslator struct {
	mu     sync.RWMutex
	mounts map[string]*fuseMountReg // host mount path → registration
}

func newSharedFuseTranslator() *sharedFuseTranslator {
	return &sharedFuseTranslator{mounts: map[string]*fuseMountReg{}}
}

func (s *sharedFuseTranslator) register(hostMount, guestMount string, backend gen.Backend, broker *events.Broker) {
	s.mu.Lock()
	s.mounts[hostMount] = &fuseMountReg{guestMount: guestMount, backend: backend, broker: broker, corr: newCorrelator()}
	s.mu.Unlock()
}

func (s *sharedFuseTranslator) unregister(hostMount string) {
	s.mu.Lock()
	delete(s.mounts, hostMount)
	s.mu.Unlock()
}

// lookup returns the registered mount (and its host path) covering path, choosing
// the longest matching host-mount prefix.
func (s *sharedFuseTranslator) lookup(path string) (string, *fuseMountReg) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var bestHost string
	var best *fuseMountReg
	for h, reg := range s.mounts {
		if path == h || strings.HasPrefix(path, strings.TrimRight(h, "/")+"/") {
			if best == nil || len(h) > len(bestHost) {
				bestHost, best = h, reg
			}
		}
	}
	return bestHost, best
}

func (s *sharedFuseTranslator) handle(raw map[string]any) {
	path, _ := raw["path"].(string)
	hostMount, reg := s.lookup(path)
	if reg == nil {
		return // event for an already-unregistered mount
	}
	// Rewrite the host FUSE path to the guest-visible path the agent used, so the
	// SSE event reports /workspace/… rather than /run/sandboxd/<key>/mnt/….
	raw["path"] = reg.guestMount + strings.TrimPrefix(path, hostMount)
	t := &fuseTranslator{broker: reg.broker, mount: reg.guestMount, backend: reg.backend, corr: reg.corr}
	t.handle(raw)
}
