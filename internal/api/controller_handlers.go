package api

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
	gen "github.com/sandbox-platform/agent-sandbox/internal/api/gen/controller"
	sandboxgen "github.com/sandbox-platform/agent-sandbox/internal/api/gen/sandbox"
	"sigs.k8s.io/yaml"
)

const (
	composeProject = "hive"
	sandboxImage   = "sandbox-runtime"
	sandboxAPIPort = 8080
)

type ControllerHandlers struct {
	// mu serializes container lifecycle operations so two requests for
	// the same id can't both decide "not running" and race on
	// `docker create`. Held for the whole handler — startSandbox is
	// short and docker itself serializes the host's daemon anyway.
	mu   sync.Mutex
	tars *tarCache
}

func NewControllerHandlers() *ControllerHandlers {
	return &ControllerHandlers{
		tars: newTarCache(os.TempDir(), tarCacheMaxBytes),
	}
}

// GetOrCreateSandbox is idempotent on `id`: if a container for `id` is
// running, its existing endpoint is returned (200); otherwise (no
// container, or one that exited / was removed out-of-band) a new
// sandbox is booted (201). Docker is the single source of truth — we
// don't cache anything that could go stale.
func (h *ControllerHandlers) GetOrCreateSandbox(c *gin.Context, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	name := containerNameFor(id)
	_, running, err := containerState(name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, sandboxgen.Error{Error: err.Error()})
		return
	}
	if running {
		hostPort, err := lookupHostPort(name, sandboxAPIPort)
		if err != nil {
			c.JSON(http.StatusInternalServerError, sandboxgen.Error{Error: err.Error()})
			return
		}
		c.JSON(http.StatusOK, gen.Sandbox{
			Id:       id,
			Endpoint: fmt.Sprintf("http://127.0.0.1:%s", hostPort),
		})
		return
	}

	var cfg sandboxgen.SandboxConfig
	if err := c.ShouldBindBodyWith(&cfg, binding.JSON); err != nil {
		c.JSON(http.StatusBadRequest, sandboxgen.Error{Error: err.Error()})
		return
	}

	sb, err := h.startSandbox(id, cfg)
	if err != nil {
		c.JSON(http.StatusInternalServerError, sandboxgen.Error{Error: err.Error()})
		return
	}
	c.JSON(http.StatusCreated, sb)
}

func containerNameFor(id string) string {
	return composeProject + "-sandbox-" + id
}

// containerState returns whether the named container exists at all and,
// if so, whether it's running. A missing container is `(false, false, nil)`
// rather than an error so callers can branch on "not found" vs "docker
// is broken".
func containerState(name string) (exists, running bool, err error) {
	cmd := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", name)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.Contains(string(ee.Stderr), "No such object") {
			return false, false, nil
		}
		return false, false, fmt.Errorf("docker inspect %s: %w", name, err)
	}
	return true, strings.TrimSpace(string(out)) == "true", nil
}

// ShutdownSandbox stops and removes the container backing `id`. The
// stop uses docker's default graceful path (SIGTERM, then SIGKILL after
// the timeout), giving sandboxd's signal handler room to run its own
// shutdown cascade (sidecars → agent → drain) before the container is
// torn down. An already-exited container is simply removed.
func (h *ControllerHandlers) ShutdownSandbox(c *gin.Context, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	name := containerNameFor(id)
	exists, running, err := containerState(name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, sandboxgen.Error{Error: err.Error()})
		return
	}
	if !exists {
		c.JSON(http.StatusNotFound, sandboxgen.Error{Error: fmt.Sprintf("sandbox %q does not exist", id)})
		return
	}
	if running {
		if out, err := exec.Command("docker", "stop", name).CombinedOutput(); err != nil {
			c.JSON(http.StatusInternalServerError, sandboxgen.Error{Error: fmt.Sprintf("docker stop %s: %v: %s", name, err, out)})
			return
		}
	}
	if out, err := exec.Command("docker", "rm", name).CombinedOutput(); err != nil {
		c.JSON(http.StatusInternalServerError, sandboxgen.Error{Error: fmt.Sprintf("docker rm %s: %v: %s", name, err, out)})
		return
	}
	c.Status(http.StatusNoContent)
}

func (h *ControllerHandlers) startSandbox(id string, cfg sandboxgen.SandboxConfig) (gen.Sandbox, error) {
	if cfg.Image == nil || *cfg.Image == "" {
		return gen.Sandbox{}, fmt.Errorf("image is required")
	}
	specBytes, err := yaml.Marshal(cfg)
	if err != nil {
		return gen.Sandbox{}, fmt.Errorf("marshal spec: %w", err)
	}
	specPath := filepath.Join(os.TempDir(), "hive-spec-"+id+".yaml")
	if err := os.WriteFile(specPath, specBytes, 0o644); err != nil {
		return gen.Sandbox{}, fmt.Errorf("write spec: %w", err)
	}
	defer os.Remove(specPath)

	tarPath, err := h.tars.getOrSave(*cfg.Image)
	if err != nil {
		return gen.Sandbox{}, err
	}

	containerName := containerNameFor(id)
	// Clear any lingering container of the same name (e.g. one that
	// exited but wasn't auto-removed) so `docker create --name` below
	// doesn't fail with a name conflict. No-op if nothing matches.
	_ = exec.Command("docker", "rm", "-f", containerName).Run()

	log.Println("docker rm ", containerName)
	serviceLabel := "sandbox-" + id
	createArgs := []string{
		"create",
		"--name", containerName,
		"--label", "com.docker.compose.project=" + composeProject,
		"--label", "com.docker.compose.service=" + serviceLabel,
		"--label", "hive.sandbox.id=" + id,
		"--network", composeProject + "_default",
		"--device", "/dev/fuse",
		"--cap-add", "SYS_ADMIN",
		"--cap-add", "NET_ADMIN",
		"--cap-add", "MKNOD",
		"--cap-add", "SYS_CHROOT",
		"--cap-add", "SETPCAP",
		"--cap-add", "SETFCAP",
		"--cap-add", "SETUID",
		"--cap-add", "SETGID",
		"--cap-add", "DAC_READ_SEARCH",
		"--cap-add", "FOWNER",
		"--cap-add", "CHOWN",
		"--security-opt", "apparmor=unconfined",
		"--security-opt", "seccomp=unconfined",
		"-v", "/sys/fs/cgroup:/sys/fs/cgroup:rw",
		"-p", fmt.Sprintf("%d", sandboxAPIPort),
	}
	if cfg.Env != nil {
		for _, kv := range *cfg.Env {
			createArgs = append(createArgs, "-e", kv)
		}
	}
	createArgs = append(createArgs,
		sandboxImage,
		"--spec", "/mnt/spec.yaml",
	)
	if out, err := exec.Command("docker", createArgs...).CombinedOutput(); err != nil {
		return gen.Sandbox{}, fmt.Errorf("docker create: %v: %s", err, out)
	}

	if out, err := exec.Command("docker", "cp", specPath, containerName+":/mnt/spec.yaml").CombinedOutput(); err != nil {
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
		return gen.Sandbox{}, fmt.Errorf("docker cp spec: %v: %s", err, out)
	}

	if out, err := exec.Command("docker", "cp", tarPath, containerName+":/mnt/sandbox.tar").CombinedOutput(); err != nil {
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
		return gen.Sandbox{}, fmt.Errorf("docker cp tar: %v: %s", err, out)
	}

	if out, err := exec.Command("docker", "start", containerName).CombinedOutput(); err != nil {
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
		return gen.Sandbox{}, fmt.Errorf("docker start: %v: %s", err, out)
	}

	hostPort, err := lookupHostPort(containerName, sandboxAPIPort)
	if err != nil {
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
		return gen.Sandbox{}, err
	}
	return gen.Sandbox{
		Id:       id,
		Endpoint: fmt.Sprintf("http://127.0.0.1:%s", hostPort),
	}, nil
}

// lookupHostPort returns the host-side port docker bound to
// container:port.
func lookupHostPort(container string, containerPort int) (string, error) {
	out, err := exec.Command("docker", "port", container, fmt.Sprintf("%d/tcp", containerPort)).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker port %s: %v: %s", container, err, out)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if !strings.HasPrefix(line, "0.0.0.0:") {
			continue
		}
		_, port, err := net.SplitHostPort(line)
		if err != nil {
			return "", fmt.Errorf("parse %q: %w", line, err)
		}
		return port, nil
	}
	return "", fmt.Errorf("no IPv4 host mapping for %s:%d in %q", container, containerPort, out)
}
