package api

import (
	"fmt"
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
	mu        sync.Mutex
	sandboxes map[string]gen.Sandbox
	tars      *tarCache
}

func NewControllerHandlers() *ControllerHandlers {
	return &ControllerHandlers{
		sandboxes: make(map[string]gen.Sandbox),
		tars:      newTarCache(os.TempDir(), tarCacheMaxBytes),
	}
}

// GetOrCreateSandbox is idempotent on `id`: a known id returns the prior
// record (200); a fresh id boots a new sandbox container in the `hive`
// compose project and records it (201). Concurrent callers serialize on
// the handler mutex so racers see a single creation.
func (h *ControllerHandlers) GetOrCreateSandbox(c *gin.Context, id string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if sb, ok := h.sandboxes[id]; ok {
		c.JSON(http.StatusOK, sb)
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
	h.sandboxes[id] = sb
	c.JSON(http.StatusCreated, sb)
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

	containerName := composeProject + "-sandbox-" + id
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
