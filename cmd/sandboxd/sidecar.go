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

	"github.com/hiver-sh/hiver/internal/events"

	gen "github.com/hiver-sh/hiver/internal/api/gen/sandbox"
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
	cmd, _, err := startChild(ctx, wg, name, bin, args, env, []*os.File{child}, nil)
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
// and hands the event to `handle`, which decides what (if anything)
// to publish to the broker.
//
// The handler owns its own state — typically a correlator that lets
// a response event reference the SSE id its paired request was given
// — so it's per-sidecar, not shared across them.
func sidecarOnEvent(
	formatLog func(map[string]any) string,
	handle func(raw map[string]any),
) func(map[string]any) {
	return func(raw map[string]any) {
		if s := formatLog(raw); s != "" {
			log.Printf("sandboxd: agent op | %s", s)
		}
		handle(raw)
	}
}

// correlator maps a sidecar's internal request_id to the SSE event id
// the broker assigned to the paired request event.
type correlator struct {
	mu sync.Mutex
	m  map[string]int64
}

func newCorrelator() *correlator { return &correlator{m: map[string]int64{}} }

func (c *correlator) put(internalID string, sseID int64) {
	c.mu.Lock()
	c.m[internalID] = sseID
	c.mu.Unlock()
}

// peek returns the SSE id for internalID without removing it.
func (c *correlator) peek(internalID string) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sseID, ok := c.m[internalID]
	return sseID, ok
}

// take returns the SSE id for internalID and removes the entry —
// pair-once semantics.
func (c *correlator) take(internalID string) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sseID, ok := c.m[internalID]
	if ok {
		delete(c.m, internalID)
	}
	return sseID, ok
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

// formatProxyEvent renders an internal/proxy.AuditEvent.
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
	if phase == "response_chunk" {
		return ""
	}
	body, _ := ev["body"].(string)
	if body != "" {
		if len(body) > 60 {
			body = body[:60] + "…"
		}
		return fmt.Sprintf("proxy %-5s %s %s%s %q", verdict, method, host, path, body)
	}
	return fmt.Sprintf("proxy %-5s %s %s%s", verdict, method, host, path)
}

// requestIDKey normalises the `request_id` field of an audit event to
// a stable string for the correlator. JSON decodes numbers as
// float64, so int-typed RequestID fields (proxy.AuditEvent) need a
// trip through strconv; string-typed ones (fusefs.AuditEvent) pass
// through unchanged.
func requestIDKey(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	default:
		return ""
	}
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

// formatFuseEvent renders an internal/fusefs.AuditEvent map.
func formatFuseEvent(ev map[string]any) string {
	op, _ := ev["op"].(string)
	path, _ := ev["path"].(string)
	phase, _ := ev["phase"].(string)
	if phase == "response" {
		durMs := intField(ev, "duration_ms")
		if errStr, ok := ev["err"].(string); ok && errStr != "" {
			return fmt.Sprintf("fuse  resp  %-10s %s %dms err=%s", op, path, durMs, errStr)
		}
		return fmt.Sprintf("fuse  resp  %-10s %s %dms", op, path, durMs)
	}
	verdict, _ := ev["verdict"].(string)
	return fmt.Sprintf("fuse  %-5s %-10s %s", verdict, op, path)
}

// proxyTranslator turns proxy AuditEvents into egress.request /
// egress.response SandboxEvents and publishes them to the broker.
//
// Phase semantics:
//   - phase=request, verdict in {allow, deny} → egress.request; the
//     SSE event id is stored in the correlator for both verdicts.
//   - phase=response, verdict in {allow, error, deny} → egress.response
//     with `request_id` set to the SSE event id of the paired request
//     (looked up via the correlator). The proxy emits a synthetic
//     phase=response immediately after every deny so consumers always
//     see a paired egress.response regardless of verdict or upstream
//     failure.
type proxyTranslator struct {
	broker *events.Broker
	corr   *correlator
}

func newProxyTranslator(broker *events.Broker) *proxyTranslator {
	return &proxyTranslator{broker: broker, corr: newCorrelator()}
}

func (t *proxyTranslator) handle(raw map[string]any) {
	phase, _ := raw["phase"].(string)
	internalID := requestIDKey(raw["request_id"])
	switch phase {
	case "request":
		verdict, _ := raw["verdict"].(string)
		f := proxyRequestFactory(raw)
		if f == nil {
			return
		}
		sseID := t.broker.Publish(f)
		// Both allow and deny requests get a paired response event from the
		// proxy (DNS sink queries included — the sink answers locally and
		// emits the response immediately), so store the SSE id for both.
		if verdict == "allow" || verdict == "deny" {
			t.corr.put(internalID, sseID)
		}
	case "response_chunk":
		sseID, ok := t.corr.peek(internalID)
		if !ok {
			return
		}
		f := proxyStreamChunkFactory(raw, sseID)
		if f != nil {
			t.broker.Publish(f)
		}
	case "response":
		verdict, _ := raw["verdict"].(string)
		if verdict != "allow" && verdict != "error" && verdict != "deny" {
			return
		}
		// Allow responses are emitted at headers-time, before the body
		// finishes streaming; subsequent response_chunks still need the
		// correlation. Peek (keep the entry) for allow; take (clean up)
		// for deny/error and DNS since those are terminal — no chunks follow.
		method, _ := raw["method"].(string)
		var sseID int64
		var ok bool
		if verdict == "allow" && method != "DNS" {
			sseID, ok = t.corr.peek(internalID)
		} else {
			sseID, ok = t.corr.take(internalID)
		}
		if !ok {
			return // paired request was filtered out (shouldn't happen for proxy)
		}
		f := proxyResponseFactory(raw, sseID)
		if f != nil {
			t.broker.Publish(f)
		}
	}
}

// proxyRouter routes proxy audit events to the per-sandbox broker that owns the
// workload whose source IP generated the event. Used in pack mode where one
// shared sbxproxy serves N sandboxes (each with a distinct source IP).
type proxyRouter struct {
	mu sync.RWMutex
	m  map[string]*proxyTranslator // srcIP → translator for that sandbox's broker
}

func newProxyRouter() *proxyRouter {
	return &proxyRouter{m: map[string]*proxyTranslator{}}
}

func (r *proxyRouter) register(srcIP string, broker *events.Broker) {
	r.mu.Lock()
	r.m[srcIP] = newProxyTranslator(broker)
	r.mu.Unlock()
}

func (r *proxyRouter) unregister(srcIP string) {
	r.mu.Lock()
	delete(r.m, srcIP)
	r.mu.Unlock()
}

func (r *proxyRouter) handle(raw map[string]any) {
	srcIP, _ := raw["src_ip"].(string)
	r.mu.RLock()
	t := r.m[srcIP]
	r.mu.RUnlock()
	if t != nil {
		t.handle(raw)
	}
}

func proxyStreamChunkFactory(raw map[string]any, requestID int64) events.Factory {
	body, _ := raw["body"].(string)
	if body == "" {
		return nil
	}
	label, _ := raw["label"].(string)
	return func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		chunk := gen.EgressChunkEvent{
			Id:        int(id),
			Timestamp: ts,
			RequestId: int(requestID),
			Body:      body,
		}
		if label != "" {
			chunk.Label = &label
		}
		_ = ev.FromEgressChunkEvent(chunk)
		return ev
	}
}

func proxyRequestFactory(raw map[string]any) events.Factory {
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
	var query *string
	if q, ok := raw["query"].(string); ok && q != "" {
		query = &q
	}
	headers := rawToStringMap(raw["headers"])
	var body *string
	if b, ok := raw["body"].(string); ok && b != "" {
		body = &b
	}
	var upstream *string
	if u, ok := raw["upstream"].(string); ok && u != "" {
		upstream = &u
	}
	return func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		_ = ev.FromEgressRequestEvent(gen.EgressRequestEvent{
			Id:        int(id),
			Timestamp: ts,
			Access:    access,
			Host:      host,
			Method:    method,
			Path:      path,
			Query:     query,
			Headers:   headers,
			Body:      body,
			Upstream:  upstream,
		})
		return ev
	}
}

func proxyResponseFactory(raw map[string]any, requestID int64) events.Factory {
	status := intField(raw, "status")
	durationMs := intField(raw, "duration_ms")
	headers := rawToStringMap(raw["headers"])
	return func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		_ = ev.FromEgressResponseEvent(gen.EgressResponseEvent{
			Id:         int(id),
			Timestamp:  ts,
			RequestId:  int(requestID),
			Status:     status,
			DurationMs: durationMs,
			Headers:    headers,
		})
		return ev
	}
}

// rawToStringMap converts a JSON-decoded map[string]any (from the audit
// socketpair) into a *map[string]string suitable for the gen event types.
// Returns nil if the value is absent, nil, or not a map.
func rawToStringMap(v any) *map[string]string {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		if s, ok := val.(string); ok {
			out[k] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return &out
}

// fuseTranslator turns fuse AuditEvents into fs.request / fs.response
// SandboxEvents and publishes them to the broker.
//
// The fuse source emits one event per kernel callback (attr, lookup,
// open, read, write, …) but the kernel fans out auxiliary callbacks
// around every user-level op — a single agent read(2) becomes
// lookup + open + read. We collapse to one SSE event per user-visible
// operation:
//
//   - allow path: emit only the "concrete" ops (read, readdir, write,
//     mkdir, remove, rename). attr/lookup/open are kernel scaffolding,
//     and create/truncate are preludes to Write — the agent's
//     fs.writeFileSync produces Create+Write (new file) or
//     Truncate+Write (overwrite); the Write captures the intent.
//   - deny path: emit every op, because a denied lookup/attr/open IS
//     the user-visible failure (the kernel short-circuits before
//     reaching the concrete op).
//   - response phase: only for concrete ops; for each one its
//     request_id is the SSE event id of the paired fs.request,
//     looked up via the correlator.
type fuseTranslator struct {
	broker  *events.Broker
	mount   string
	backend gen.Backend
	corr    *correlator
}

// fuseTranslator instances are built by sharedFuseTranslator per registered
// mount (with a persistent per-mount correlator), since the pod's single sbxfuse
// multiplexes every workspace's audit events onto one stream.

func (t *fuseTranslator) handle(raw map[string]any) {
	phase, _ := raw["phase"].(string)
	op, _ := raw["op"].(string)
	internalID, _ := raw["request_id"].(string)
	switch phase {
	case "request":
		verdict, _ := raw["verdict"].(string)
		if verdict == "allow" && !isConcreteFuseOp(op) {
			return
		}
		f := fuseRequestFactory(raw, t.mount)
		if f == nil {
			return
		}
		sseID := t.broker.Publish(f)
		if verdict == "allow" || verdict == "deny" {
			t.corr.put(internalID, sseID)
		}
	case "response":
		verdict, _ := raw["verdict"].(string)
		// allow responses: only concrete ops are surfaced (non-concrete request
		// events were filtered out above, so their correlator entries are absent).
		// deny responses: always pass through — the request was published for
		// every denied op regardless of concrete/non-concrete.
		if verdict != "deny" && !isConcreteFuseOp(op) {
			return
		}
		sseID, ok := t.corr.take(internalID)
		if !ok {
			return // paired request was filtered out
		}
		f := fuseResponseFactory(raw, t.backend, int(sseID))
		if f != nil {
			t.broker.Publish(f)
		}
	}
}

// isConcreteFuseOp reports whether the op is a user-visible file
// operation. The kernel decomposes one user-level call into multiple
// FUSE callbacks; the rest of them are kernel scaffolding that we
// elide on the allow path:
//
//   - attr / lookup / open: metadata probes around every read/write
//     — already covered by the read/write event that follows.
//   - create / truncate: preludes to Write — the agent's
//     fs.writeFileSync produces Create+Write (new file) or
//     Truncate+Write (overwrite); the Write captures the intent.
func isConcreteFuseOp(op string) bool {
	switch op {
	case "read", "readdir", "write", "mkdir", "remove", "rename":
		return true
	}
	return false
}

func fuseRequestFactory(raw map[string]any, mount string) events.Factory {
	verdict, _ := raw["verdict"].(string)
	var access gen.FSRequestEventAccess
	switch verdict {
	case "allow":
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

// fuseResponseFactory builds an fs.response event. `requestID` is the
// stringified SSE event id of the paired fs.request — looked up by
// the translator's correlator, NOT the fuse-internal counter that
// rides on the source AuditEvent. Local backends emit the minimum
// shape (request_id + backend + duration_ms, plus err on failure);
// remote-HTTP backends would also carry method/url/status, which the
// fuse audit shape doesn't surface today.
func fuseResponseFactory(raw map[string]any, backend gen.Backend, requestID int) events.Factory {
	durationMs := intField(raw, "duration_ms")
	errStr, _ := raw["err"].(string)
	return func(id int64, ts time.Time) gen.SandboxEvent {
		var ev gen.SandboxEvent
		out := gen.FSResponseEvent{
			Id:         int(id),
			Timestamp:  ts,
			RequestId:  requestID,
			Backend:    backend,
			DurationMs: durationMs,
		}
		if errStr != "" {
			out.Error = &errStr
		}
		_ = ev.FromFSResponseEvent(out)
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
	case "open-write", "write", "create", "mkdir", "truncate":
		return gen.Write
	case "remove":
		return gen.Delete
	}
	return ""
}
