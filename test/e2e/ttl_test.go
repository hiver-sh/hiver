package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/blasten/hive/internal/spec"
	"github.com/blasten/hive/test/e2e/setup"
)

// TestTTLE2E exercises the /v1/ping keepalive contract against the ttl
// fixture. The test runs both directions in one docker run:
//
//  1. While pings flow every ttl/3, the pod must stay up — each ping
//     resets the deadline.
//  2. Once pings stop, the pod must shut itself down on its own,
//     within ttl + sandboxd's graceful chain.
func TestTTLE2E(t *testing.T) {
	setup.RequireDocker(t)

	fixtureDir, err := filepath.Abs(filepath.Join(moduleRoot, "test/e2e/fixtures/ttl"))
	if err != nil {
		t.Fatalf("abs fixture: %v", err)
	}
	specPath := filepath.Join(fixtureDir, "spec.json")
	sp, err := spec.Load(specPath)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	if sp.Ttl == nil {
		t.Fatalf("ttl fixture is missing top-level ttl")
	}
	ttl := time.Duration(*sp.Ttl) * time.Second

	agentDir, err := filepath.Abs(filepath.Join(fixtureDir, sp.Image))
	if err != nil {
		t.Fatalf("abs agent dir: %v", err)
	}
	moduleAbs, err := filepath.Abs(moduleRoot)
	if err != nil {
		t.Fatalf("abs module root: %v", err)
	}
	agentImage := "sandbox-ttl:e2e"
	bundleImage := "sandbox-bundle-ttl:e2e"
	setup.BuildImages(t, agentDir, moduleAbs, agentImage)
	setup.BuildSandboxBundle(t, agentImage, bundleImage)

	containerName := fmt.Sprintf("sandbox-pod-ttl-e2e-%d", time.Now().UnixNano())
	args := []string{
		"run", "--rm", "--name", containerName,
		"--device", "/dev/fuse",
		"--cap-add", "SYS_ADMIN", "--cap-add", "NET_ADMIN", "--cap-add", "MKNOD",
		"--cap-add", "SYS_CHROOT", "--cap-add", "SETPCAP", "--cap-add", "SETFCAP",
		"--cap-add", "SETUID", "--cap-add", "SETGID",
		"--cap-add", "DAC_READ_SEARCH", "--cap-add", "FOWNER", "--cap-add", "CHOWN",
		"--security-opt", "apparmor=unconfined",
		"--security-opt", "seccomp=unconfined",
		"-v", "/sys/fs/cgroup:/sys/fs/cgroup:rw",
		"-p", "8080:8080",
		"-v", specPath + ":/mnt/spec.json:ro",
		bundleImage,
		"--spec", "/mnt/spec.json",
	}

	var podOut bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout, cmd.Stderr = &podOut, &podOut
	if err := cmd.Start(); err != nil {
		t.Fatalf("docker run: %v", err)
	}
	containerDone := make(chan error, 1)
	go func() { containerDone <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = exec.Command("docker", "kill", containerName).Run()
		select {
		case <-containerDone:
		case <-time.After(10 * time.Second):
		}
		if t.Failed() {
			t.Logf("pod output:\n%s", podOut.String())
		}
	})

	// Pinger fires from t=0 so the first successful ping lands as soon
	// as docker's port publish is wired up — minimises the chance
	// startup latency eats the initial deadline. Pings before the API
	// binds get connection-refused and are simply discarded.
	apiURL := "http://127.0.0.1:8080"
	pingerCtx, stopPinger := context.WithCancel(context.Background())
	pingerDone := make(chan struct{})
	pingedOK := make(chan struct{}, 1)
	go func() {
		defer close(pingerDone)
		tk := time.NewTicker(ttl / 3)
		defer tk.Stop()
		for {
			select {
			case <-pingerCtx.Done():
				return
			case <-tk.C:
				resp, err := http.Get(apiURL + "/v1/ping")
				if err != nil {
					continue
				}
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					select {
					case pingedOK <- struct{}{}:
					default:
					}
				}
			}
		}
	}()

	// Wait for the first successful ping. Doubles as readiness for the
	// API server and a smoke test for the /v1/ping endpoint itself.
	const apiDeadline = 60 * time.Second
	select {
	case <-pingedOK:
	case <-time.After(apiDeadline):
		stopPinger()
		<-pingerDone
		t.Fatalf("no successful /v1/ping within %v\n%s", apiDeadline, podOut.String())
	case err := <-containerDone:
		stopPinger()
		<-pingerDone
		t.Fatalf("container exited before first ping succeeded: %v\n%s", err, podOut.String())
	}

	// Phase 1: keepalive. Pod must not exit while pings are flowing.
	keepaliveWindow := 3 * ttl
	select {
	case err := <-containerDone:
		stopPinger()
		<-pingerDone
		t.Fatalf("pod exited while being pinged (window=%v): %v\n%s", keepaliveWindow, err, podOut.String())
	case <-time.After(keepaliveWindow):
	}

	// Phase 2: stop pinging — pod must self-terminate. Lifetime ticks
	// every second, then sandboxd's graceful chain (sbxfuse drain,
	// child reap) runs on top; budget ttl + 30s.
	stopPinger()
	<-pingerDone
	graceful := ttl + 30*time.Second
	select {
	case <-containerDone:
	case <-time.After(graceful):
		t.Fatalf("pod did not exit within %v after pings stopped\n%s", graceful, podOut.String())
	}

	if !strings.Contains(podOut.String(), "TTL elapsed since last /v1/ping") {
		t.Errorf("pod output missing TTL shutdown log line:\n%s", podOut.String())
	}
}
