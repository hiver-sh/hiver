package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/sandbox-platform/agent-sandbox/internal/api/gen"
	"github.com/sandbox-platform/agent-sandbox/internal/events"
)

// newEventsPair makes a connected socketpair for streaming JSON-line
// events from a sidecar to sandboxd. The child end is handed to the
// sidecar via cmd.ExtraFiles (becomes fd 3 in the child); the parent
// end stays here for ingestEvents to read. The buffer is in-kernel,
// so the hop costs no IOPS.
func newEventsPair() (parent, child *os.File, err error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("socketpair: %w", err)
	}
	parent = os.NewFile(uintptr(fds[0]), "events:parent")
	child = os.NewFile(uintptr(fds[1]), "events:child")
	return parent, child, nil
}

// eventsFD is the fd the events socketpair lands on in the sidecar:
// Go's os/exec guarantees ExtraFiles[i] becomes fd 3+i, and the
// socketpair is the only entry we add to ExtraFiles.
const eventsFD = 3

// startSidecar spawns a sidecar that emits JSON-line audit events on
// fd `eventsFD`. It owns the events-pipe lifecycle:
//
//  1. create a socketpair,
//  2. append `-events-fd <eventsFD>` to args,
//  3. start the child with the child-side fd in ExtraFiles,
//  4. close sandboxd's own copy of the child-side fd (so the read end
//     sees EOF when the child exits),
//  5. spawn a goroutine that decodes events and calls onEvent for each.
//
// `onEvent` is invoked synchronously per event from a single goroutine,
// so callers don't need their own serialization.
func startSidecar(
	ctx context.Context,
	wg *sync.WaitGroup,
	name, bin string,
	args, env []string,
	onEvent func(map[string]any),
) (*exec.Cmd, error) {
	parent, child, err := newEventsPair()
	if err != nil {
		return nil, err
	}
	args = append(args, "-events-fd", strconv.Itoa(eventsFD))
	// nil onStdio: sidecar stdout/stderr is operational logging
	// (mount messages, http request log), not workload output worth
	// surfacing as a SandboxEvent.
	cmd, err := startChild(ctx, wg, name, bin, args, env, []*os.File{child}, nil)
	if err != nil {
		_ = parent.Close()
		_ = child.Close()
		return nil, err
	}
	_ = child.Close()
	wg.Add(1)
	go func() {
		defer wg.Done()
		ingestEvents(ctx, parent, name, onEvent)
	}()
	return cmd, nil
}

// ingestEvents reads JSON objects (one per audit event) from r and
// invokes onEvent for each, until EOF or context cancel. r is closed
// on return.
func ingestEvents(ctx context.Context, r io.ReadCloser, name string, onEvent func(map[string]any)) {
	defer r.Close()
	dec := json.NewDecoder(r)
	for {
		var ev map[string]any
		if err := dec.Decode(&ev); err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				log.Printf("sandboxd: events(%s): decode: %v", name, err)
			}
			return
		}
		onEvent(ev)
	}
}

// sidecarOnEvent builds the per-event callback every sidecar uses.
// For each raw audit record it emits a `sandboxd: agent op | …`
// summary line (the e2e fixture grep-extracts these from docker logs)
// and, when the translation produces a SandboxEvent variant, publishes
// it to the broker so it fans out to SSE subscribers.
//
// Translation returning nil drops the event from the broker but keeps
// the log line (useful for verdict="error" or ops we haven't mapped
// onto the SSE schema yet).
func sidecarOnEvent(
	broker *events.Broker,
	formatLog func(map[string]any) string,
	translate func(map[string]any) []events.Factory,
) func(map[string]any) {
	return func(raw map[string]any) {
		log.Printf("sandboxd: agent op | %s", formatLog(raw))
		for _, build := range translate(raw) {
			broker.Publish(build)
		}
	}
}

// publishAgentStdio returns the per-line callback for the agent
// child's stdout/stderr. Each line is published as a `stdio`
// SandboxEvent with the matching field set; the trailing newline is
// preserved to match the schema's example shape.
func publishAgentStdio(broker *events.Broker) func(stream, line string) {
	return func(stream, line string) {
		broker.Publish(func(id int64, ts time.Time) gen.SandboxEvent {
			var ev gen.SandboxEvent
			stdio := gen.StdioEvent{Id: int(id), Timestamp: ts}
			if stream == "stdout" {
				stdio.Stdout = &line
			} else {
				stdio.Stderr = &line
			}
			_ = ev.FromStdioEvent(stdio)
			return ev
		})
	}
}

// formatProxyEvent renders an internal/proxy.AuditEvent map for the
// "agent op | …" log line. Schema (see internal/proxy.AuditEvent):
//
//	{at, type:"network", phase, request_id, method, host, path, verdict,
//	 status?, duration_ms?, reason?}
func formatProxyEvent(ev map[string]any) string {
	verdict, _ := ev["verdict"].(string)
	method, _ := ev["method"].(string)
	host, _ := ev["host"].(string)
	path, _ := ev["path"].(string)
	if path == "" {
		path = "/"
	}
	phase, _ := ev["phase"].(string)
	if phase == "response" {
		// On the response side `verdict` is allow/error from upstream;
		// status carries the wire result. Show that instead.
		status := intField(ev, "status")
		durMs := intField(ev, "duration_ms")
		return fmt.Sprintf("proxy resp  %d %dms %s %s%s", status, durMs, method, host, path)
	}
	return fmt.Sprintf("proxy %-5s %s %s%s", verdict, method, host, path)
}

func intField(ev map[string]any, key string) int {
	switch v := ev[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

// formatFuseEvent renders an internal/fusefs.AuditEvent map. Schema:
//
//	{at, type:"filesystem", op, path, verdict, err?}
func formatFuseEvent(ev map[string]any) string {
	verdict, _ := ev["verdict"].(string)
	op, _ := ev["op"].(string)
	path, _ := ev["path"].(string)
	return fmt.Sprintf("fuse  %-5s %-10s %s", verdict, op, path)
}

// translateProxyEvent maps the proxy's raw AuditEvent shape onto a
// SandboxEvent factory. The proxy emits two events per HTTP-like flow
// (phase=request with the access decision, then phase=response with
// upstream status + duration_ms); host-only flows (CONNECT, raw-forward
// TLS) emit phase=request only.
//
//   - phase=request, verdict in {allow, deny} → egress.request
//   - phase=response, verdict=allow           → egress.response
//   - phase=response, verdict=error           → dropped (no upstream
//     status to report)
//
// Returns a slice for parity with [translateFuseEvent], which emits
// two SSE events per source record. Proxy is always one-in-one-out.
func translateProxyEvent(raw map[string]any) []events.Factory {
	phase, _ := raw["phase"].(string)
	var f events.Factory
	switch phase {
	case "request":
		f = translateProxyRequest(raw)
	case "response":
		f = translateProxyResponse(raw)
	}
	if f == nil {
		return nil
	}
	return []events.Factory{f}
}

func translateProxyRequest(raw map[string]any) events.Factory {
	verdict, _ := raw["verdict"].(string)
	var access gen.EgressRequestEventAccess
	switch verdict {
	case "allow":
		access = gen.EgressRequestEventAccessAllowed
	case "deny":
		access = gen.EgressRequestEventAccessDenied
	default:
		return nil
	}
	method, _ := raw["method"].(string)
	host, _ := raw["host"].(string)
	path, _ := raw["path"].(string)
	if path == "" {
		path = "/"
	}
	return func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		_ = ev.FromEgressRequestEvent(gen.EgressRequestEvent{
			Id:        int(id),
			Timestamp: ts,
			Access:    access,
			Host:      host,
			Method:    gen.HttpMethod(method),
			Path:      path,
		})
		return ev
	}
}

func translateProxyResponse(raw map[string]any) events.Factory {
	if v, _ := raw["verdict"].(string); v != "allow" {
		return nil
	}
	requestID, _ := raw["request_id"].(string)
	status := intField(raw, "status")
	durationMs := intField(raw, "duration_ms")
	return func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		_ = ev.FromEgressResponseEvent(gen.EgressResponseEvent{
			Id:         int(id),
			Timestamp:  ts,
			RequestId:  requestID,
			Status:     status,
			DurationMs: durationMs,
		})
		return ev
	}
}

// translateFuseEvent maps the fuse AuditEvent shape onto fs.request /
// fs.response SandboxEvent factories. mount and backend are closed
// over by the caller (per-FS context that the audit event doesn't
// carry).
//
// Each source record produces:
//   - fs.request, always (with the access decision),
//   - fs.response, only when the op reached the backend (verdict in
//     {allow, error}) — the schema models this as "FUSE got a response
//     from a storage backend". Deny short-circuits before the backend.
//
// Op kinds are bucketed into read/write per FSRequestEventOperation;
// unknown ops drop out of fs.request but still get an fs.response when
// the backend was reached. Local backends omit method/url/status (those
// are HTTP-shaped fields for remote backends like gdrive).
func translateFuseEvent(mount string, backend gen.Backend) func(map[string]any) []events.Factory {
	return func(raw map[string]any) []events.Factory {
		verdict, _ := raw["verdict"].(string)
		var out []events.Factory
		if f := fuseRequestFactory(raw, mount, verdict); f != nil {
			out = append(out, f)
		}
		if verdict == "allow" || verdict == "error" {
			if f := fuseResponseFactory(raw, backend); f != nil {
				out = append(out, f)
			}
		}
		return out
	}
}

func fuseRequestFactory(raw map[string]any, mount, verdict string) events.Factory {
	var access gen.FSRequestEventAccess
	switch verdict {
	case "allow", "error":
		// "error" is fusefs-speak for "ACL allowed but the backend
		// errored". The ACL decision was allowed; the fs.response
		// event (which the caller emits for both allow and error)
		// carries the failure.
		access = gen.FSRequestEventAccessAllowed
	case "deny":
		access = gen.FSRequestEventAccessDenied
	default:
		return nil
	}
	op, _ := raw["op"].(string)
	operation := fuseOpKind(op)
	if operation == "" {
		return nil
	}
	path, _ := raw["path"].(string)
	return func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		_ = ev.FromFSRequestEvent(gen.FSRequestEvent{
			Id:        int(id),
			Timestamp: ts,
			Access:    access,
			Mount:     mount,
			Path:      path,
			Operation: operation,
		})
		return ev
	}
}

// fuseResponseFactory builds an fs.response event. Local backends emit
// the minimum shape (backend + duration_ms); remote-HTTP backends
// could carry method/url/status too but the fuse audit shape doesn't
// surface those today.
func fuseResponseFactory(raw map[string]any, backend gen.Backend) events.Factory {
	durationMs := intField(raw, "duration_ms")
	return func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		_ = ev.FromFSResponseEvent(gen.FSResponseEvent{
			Id:         int(id),
			Timestamp:  ts,
			Backend:    backend,
			DurationMs: durationMs,
		})
		return ev
	}
}

// fuseOpKind buckets a fuse Op into read/write for the SSE schema.
// Returns "" for ops that don't map (e.g. metadata-only ops we choose
// not to surface yet).
func fuseOpKind(op string) gen.FSRequestEventOperation {
	switch op {
	case "attr", "lookup", "readdir", "open", "read":
		return gen.Read
	case "open-write", "write", "create", "mkdir", "remove", "rename", "truncate":
		return gen.Write
	}
	return ""
}
