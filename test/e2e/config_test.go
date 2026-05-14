package e2e_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sandbox-platform/agent-sandbox/internal/api/gen"
	"github.com/sandbox-platform/agent-sandbox/test/e2e/setup"
)

// TestConfigE2E exercises `GET /v1/config` and `PUT /v1/config` end-to-end.
// Runs inside the agent-node fixture's live window so we can also
// confirm that each successful PUT publishes a `config.apply`
// SandboxEvent on the same pod's `/v1/events` stream.
func TestConfigE2E(t *testing.T) {
	setup.RunFixtureE2EHook(t, "agent-node", func(t *testing.T, baseURL string) {
		// GET returns the spec.yaml the pod was launched with, after
		// the spec.Spec → gen.SandboxConfig round-trip sandboxd does on
		// startup.
		initial := getConfig(t, baseURL)
		assertMountPresent(t, initial, "/workspace", gen.Local)
		assertEgressHostPresent(t, initial, "upstream-allowed")
		assertEgressHostPresent(t, initial, "go.dev")

		// Idempotent PUT: re-applying the same document succeeds with
		// no diff.
		noop := putConfig(t, baseURL, initial)
		if !noop.Applied {
			t.Fatalf("idempotent PUT: applied=false (error=%q)", derefString(noop.Error))
		}
		if noop.Changes.Fs != nil || noop.Changes.Egress != nil {
			t.Errorf("idempotent PUT produced changes: %+v", noop.Changes)
		}

		// Add a new egress rule. The diff must surface it as an
		// addition, the response config must include it, and a
		// subsequent GET must reflect the new on-disk state.
		const newHost = "config-e2e.example.com"
		modified := withExtraEgressRule(initial, gen.EgressRule{Host: newHost})
		add := putConfig(t, baseURL, modified)
		if !add.Applied {
			t.Fatalf("add PUT: applied=false (error=%q)", derefString(add.Error))
		}
		assertEgressAdded(t, add.Changes, newHost)
		assertEgressHostPresent(t, add.Config, newHost)
		assertEgressHostPresent(t, getConfig(t, baseURL), newHost)

		// Remove the rule we just added by PUT'ing the original config
		// back. The diff must surface it as a removal and the
		// post-apply state must drop it.
		remove := putConfig(t, baseURL, initial)
		if !remove.Applied {
			t.Fatalf("remove PUT: applied=false (error=%q)", derefString(remove.Error))
		}
		assertEgressRemoved(t, remove.Changes, newHost)
		if egressHasHost(getConfig(t, baseURL), newHost) {
			t.Errorf("after remove PUT: %q still present in egress.allow", newHost)
		}

		// Schema-invalid bodies are rejected with 400 by the OpenAPI
		// validator before the handler runs.
		assertPutStatus(t, baseURL, `{"fs": []}`, http.StatusBadRequest)
		assertPutStatus(t, baseURL, `{"fs": [{"mount": "relative", "backend": "local"}]}`, http.StatusBadRequest)
		assertPutStatus(t, baseURL, `{"fs": [{"mount": "/x", "backend": "bogus"}]}`, http.StatusBadRequest)

		// Every successful PUT must publish a config.apply SandboxEvent
		// on /v1/events. Drain a replay from id=0 and check we saw at
		// least three (idempotent + add + remove), all success=true.
		events := setup.FetchEvents(t, baseURL, setup.FetchOpts{
			LastEventID: "0",
			IdleTimeout: 750 * time.Millisecond,
		})
		var applies []map[string]any
		for _, e := range events {
			if typ, _ := e["type"].(string); typ == "config.apply" {
				applies = append(applies, e)
			}
		}
		if len(applies) < 3 {
			t.Errorf("expected ≥3 config.apply events on /v1/events; got %d", len(applies))
		}
		for _, e := range applies {
			if s, ok := e["success"].(bool); !ok || !s {
				t.Errorf("config.apply event with success != true: %v", e)
			}
		}
	})
}

func getConfig(t *testing.T, baseURL string) gen.SandboxConfig {
	t.Helper()
	resp, err := http.Get(baseURL + "/v1/config")
	if err != nil {
		t.Fatalf("GET /v1/config: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /v1/config: status %d, body %s", resp.StatusCode, body)
	}
	var cfg gen.SandboxConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatalf("GET /v1/config decode: %v", err)
	}
	return cfg
}

func putConfig(t *testing.T, baseURL string, cfg gen.SandboxConfig) gen.ApplyResult {
	t.Helper()
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	req, err := http.NewRequest(http.MethodPut, baseURL+"/v1/config", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("PUT request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /v1/config: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT /v1/config: status %d, body %s\nreq body: %s", resp.StatusCode, respBody, body)
	}
	var result gen.ApplyResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("PUT /v1/config decode: %v", err)
	}
	return result
}

func assertPutStatus(t *testing.T, baseURL, jsonBody string, want int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, baseURL+"/v1/config", strings.NewReader(jsonBody))
	if err != nil {
		t.Fatalf("PUT request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /v1/config: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != want {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("PUT %s: status %d, want %d (body=%s)", jsonBody, resp.StatusCode, want, body)
	}
}

// withExtraEgressRule returns a copy of cfg with rule appended to the
// allow list. cfg is left untouched so callers can reuse it as the
// "before" snapshot when undoing a change.
func withExtraEgressRule(cfg gen.SandboxConfig, rule gen.EgressRule) gen.SandboxConfig {
	out := cfg
	var allow []gen.EgressRule
	if cfg.Egress != nil && cfg.Egress.Allow != nil {
		allow = append(allow, *cfg.Egress.Allow...)
	}
	allow = append(allow, rule)
	out.Egress = &gen.Egress{Allow: &allow}
	return out
}

func assertMountPresent(t *testing.T, cfg gen.SandboxConfig, mount string, backend gen.Backend) {
	t.Helper()
	for _, f := range cfg.Fs {
		if f.Mount == mount {
			if f.Backend != backend {
				t.Errorf("mount %q: backend=%q, want %q", mount, f.Backend, backend)
			}
			return
		}
	}
	t.Errorf("mount %q not present in fs (got %+v)", mount, cfg.Fs)
}

func assertEgressHostPresent(t *testing.T, cfg gen.SandboxConfig, host string) {
	t.Helper()
	if egressHasHost(cfg, host) {
		return
	}
	var hosts []string
	if cfg.Egress != nil && cfg.Egress.Allow != nil {
		for _, r := range *cfg.Egress.Allow {
			hosts = append(hosts, r.Host)
		}
	}
	t.Errorf("egress host %q not in allow list (got %v)", host, hosts)
}

func egressHasHost(cfg gen.SandboxConfig, host string) bool {
	if cfg.Egress == nil || cfg.Egress.Allow == nil {
		return false
	}
	for _, r := range *cfg.Egress.Allow {
		if r.Host == host {
			return true
		}
	}
	return false
}

func assertEgressAdded(t *testing.T, ch gen.Changes, host string) {
	t.Helper()
	if ch.Egress == nil || ch.Egress.Added == nil {
		t.Errorf("changes.egress.added is empty; want host=%q", host)
		return
	}
	for _, r := range *ch.Egress.Added {
		if r.Host == host {
			return
		}
	}
	t.Errorf("changes.egress.added missing host %q (got %+v)", host, *ch.Egress.Added)
}

func assertEgressRemoved(t *testing.T, ch gen.Changes, host string) {
	t.Helper()
	if ch.Egress == nil || ch.Egress.Removed == nil {
		t.Errorf("changes.egress.removed is empty; want host=%q", host)
		return
	}
	for _, r := range *ch.Egress.Removed {
		if r.Host == host {
			return
		}
	}
	t.Errorf("changes.egress.removed missing host %q (got %+v)", host, *ch.Egress.Removed)
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
