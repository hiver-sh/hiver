package controller

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	gen "github.com/sandbox-platform/agent-sandbox/internal/api/gen/controller"
	sandboxgen "github.com/sandbox-platform/agent-sandbox/internal/api/gen/sandbox"
	"sigs.k8s.io/yaml"
)

const (
	composeProject      = "hive"
	defaultSandboxImage = "hive-sandbox-bundle"
	sandboxAPIPort      = 8080
)

// DockerRuntime implements SandboxRuntime using local Docker commands.
type DockerRuntime struct{}

func newDockerRuntime() *DockerRuntime {
	return &DockerRuntime{}
}

func (r *DockerRuntime) Lookup(id string) (bool, string, error) {
	name := containerNameFor(id)
	_, running, err := containerState(name)
	if err != nil {
		return false, "", err
	}
	if !running {
		return false, "", nil
	}
	hostPort, err := lookupHostPort(name, sandboxAPIPort)
	if err != nil {
		return false, "", err
	}
	return true, fmt.Sprintf("http://127.0.0.1:%s", hostPort), nil
}

func (r *DockerRuntime) Start(id string, cfg sandboxgen.SandboxConfig) (gen.Sandbox, error) {
	specBytes, err := yaml.Marshal(cfg)
	if err != nil {
		return gen.Sandbox{}, fmt.Errorf("marshal spec: %w", err)
	}
	specPath := filepath.Join(os.TempDir(), "hive-spec-"+id+".yaml")
	if err := os.WriteFile(specPath, specBytes, 0o644); err != nil {
		return gen.Sandbox{}, fmt.Errorf("write spec: %w", err)
	}
	defer os.Remove(specPath)

	containerName := containerNameFor(id)
	// Clear any lingering container of the same name (e.g. one that exited
	// but wasn't auto-removed) so `docker create --name` below doesn't fail
	// with a name conflict. No-op if nothing matches.
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

	var image = defaultSandboxImage
	if cfg.Image != nil && *cfg.Image != "" {
		image = *cfg.Image
	}

	createArgs = append(createArgs,
		image,
		"--spec", "/mnt/spec.yaml",
	)
	if out, err := exec.Command("docker", createArgs...).CombinedOutput(); err != nil {
		return gen.Sandbox{}, fmt.Errorf("docker create: %v: %s", err, out)
	}

	if out, err := exec.Command("docker", "cp", specPath, containerName+":/mnt/spec.yaml").CombinedOutput(); err != nil {
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
		return gen.Sandbox{}, fmt.Errorf("docker cp spec: %v: %s", err, out)
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

func (r *DockerRuntime) Shutdown(id string) error {
	name := containerNameFor(id)
	exists, running, err := containerState(name)
	if err != nil {
		return err
	}
	if !exists {
		return ErrSandboxNotFound
	}
	if running {
		if out, err := exec.Command("docker", "stop", name).CombinedOutput(); err != nil {
			return fmt.Errorf("docker stop %s: %v: %s", name, err, out)
		}
	}
	if out, err := exec.Command("docker", "rm", name).CombinedOutput(); err != nil {
		return fmt.Errorf("docker rm %s: %v: %s", name, err, out)
	}
	return nil
}

func containerNameFor(id string) string {
	return composeProject + "-sandbox-" + id
}

// containerState returns whether the named container exists and whether it is
// running. A missing container is (false, false, nil) rather than an error.
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

// lookupHostPort returns the host-side port docker bound to container:port.
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
