package controller

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"encoding/json"

	gen "github.com/blasten/hive/internal/api/gen/controller"
	sandboxgen "github.com/blasten/hive/internal/api/gen/sandbox"
	"github.com/blasten/hive/internal/spec"
)

const (
	composeProject      = "hive"
	defaultSandboxImage = "hive-sandbox-bundle"
	sandboxAPIPort      = 8080
	sandboxTCPProxyPort = 8081
	labelSandboxID      = "hive.sandbox.id"
)

// DockerRuntime implements SandboxRuntime using local Docker commands.
type DockerRuntime struct{}

func newDockerRuntime() *DockerRuntime {
	return &DockerRuntime{}
}

func (r *DockerRuntime) Lookup(id string) (bool, gen.Sandbox, error) {
	name := containerNameFor(id)
	_, running, err := containerState(name)
	if err != nil {
		return false, gen.Sandbox{}, err
	}
	if !running {
		return false, gen.Sandbox{}, nil
	}
	hostPort, err := lookupHostPort(name, sandboxAPIPort)
	if err != nil {
		return false, gen.Sandbox{}, withContainerLogs(err, name)
	}
	sb := gen.Sandbox{
		Id:              id,
		Endpoint:        fmt.Sprintf("http://127.0.0.1:%s", hostPort),
		ExposedEndpoint: lookupTCPProxyEndpoint(name),
	}
	return true, sb, nil
}

func (r *DockerRuntime) List() ([]gen.Sandbox, error) {
	out, err := exec.Command("docker", "ps", "--filter", "label="+labelSandboxID, "--format", "{{.Names}}").Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}
	names := strings.Fields(strings.TrimSpace(string(out)))
	prefix := composeProject + "-sandbox-"
	sandboxes := make([]gen.Sandbox, 0, len(names))
	for _, name := range names {
		id := strings.TrimPrefix(name, prefix)
		hostPort, err := lookupHostPort(name, sandboxAPIPort)
		if err != nil {
			return nil, withContainerLogs(err, name)
		}
		sandboxes = append(sandboxes, gen.Sandbox{
			Id:              id,
			Endpoint:        fmt.Sprintf("http://127.0.0.1:%s", hostPort),
			ExposedEndpoint: lookupTCPProxyEndpoint(name),
		})
	}
	return sandboxes, nil
}

func (r *DockerRuntime) Get(id string) (gen.SandboxDetail, error) {
	name := containerNameFor(id)
	_, running, err := containerState(name)
	if err != nil {
		return gen.SandboxDetail{}, err
	}
	if !running {
		return gen.SandboxDetail{}, ErrSandboxNotFound
	}
	hostPort, err := lookupHostPort(name, sandboxAPIPort)
	if err != nil {
		return gen.SandboxDetail{}, withContainerLogs(err, name)
	}
	cmd := fmt.Sprintf("docker exec -it %s sandbox-exec", name)
	return gen.SandboxDetail{
		Id:              id,
		Endpoint:        fmt.Sprintf("http://127.0.0.1:%s", hostPort),
		ExposedEndpoint: lookupTCPProxyEndpoint(name),
		TerminalCmd:     &cmd,
	}, nil
}

func (r *DockerRuntime) Start(id string, cfg sandboxgen.SandboxConfig) (gen.Sandbox, error) {
	specBytes, err := json.Marshal(cfg)
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

	var image = defaultSandboxImage
	if cfg.Image != nil && *cfg.Image != "" {
		image = *cfg.Image
	}

	serviceLabel := "sandbox-" + id
	createArgs := []string{
		"create",
		"--name", containerName,
		"--label", "com.docker.compose.project=" + composeProject,
		"--label", "com.docker.compose.service=" + serviceLabel,
		"--label", labelSandboxID + "=" + id,
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
		"-p", fmt.Sprintf("%d", sandboxAPIPort),
		"-p", fmt.Sprintf("%d", sandboxTCPProxyPort),
	}
	if cfg.Env != nil {
		for k, v := range *cfg.Env {
			createArgs = append(createArgs, "-e", k+"="+v)
		}
	}

	// Mount volumes
	createArgs = append(createArgs, "-v", "/sys/fs/cgroup:/sys/fs/cgroup:rw")
	for _, fs := range cfg.Fs {
		local, err := fs.AsLocalFileSystem()
		if err != nil || local.Origin == nil {
			continue
		}
		createArgs = append(createArgs, "-v", *local.Origin+":"+local.Mount+spec.BackendSuffix)
	}

	createArgs = append(createArgs, "-v", composeProject+"_snapshots:/snapshots")

	createArgs = append(createArgs,
		image,
		"--spec", "/mnt/spec.json",
		"--snapshot-dir", "/snapshots",
	)
	if out, err := exec.Command("docker", createArgs...).CombinedOutput(); err != nil {
		return gen.Sandbox{}, fmt.Errorf("docker create %s: %v: %s", image, err, out)
	}

	if out, err := exec.Command("docker", "cp", specPath, containerName+":/mnt/spec.json").CombinedOutput(); err != nil {
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
		return gen.Sandbox{}, fmt.Errorf("docker cp spec: %v: %s", err, out)
	}

	if out, err := exec.Command("docker", "start", containerName).CombinedOutput(); err != nil {
		_ = exec.Command("docker", "rm", "-f", containerName).Run()
		return gen.Sandbox{}, fmt.Errorf("docker start %s: %v: %s", image, err, out)
	}

	hostPort, err := lookupHostPort(containerName, sandboxAPIPort)
	if err != nil {
		defer exec.Command("docker", "rm", "-f", containerName).Run()
		return gen.Sandbox{}, withContainerLogs(err, containerName)
	}
	return gen.Sandbox{
		Id:              id,
		Endpoint:        fmt.Sprintf("http://127.0.0.1:%s", hostPort),
		ExposedEndpoint: lookupTCPProxyEndpoint(containerName),
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
		// Allow 60 s for sandboxd to capture the snapshot before SIGKILL.
		// `-t` (not `--timeout`) — the Debian Bookworm `docker.io` package
		// ships CLI 20.10, which predates the `--timeout` long form.
		if out, err := exec.Command("docker", "stop", "-t", "60", name).CombinedOutput(); err != nil {
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

// containerImage returns the image name a container was started from.
func containerImage(name string) string {
	out, _ := exec.Command("docker", "inspect", "-f", "{{.Config.Image}}", name).Output()
	return strings.TrimSpace(string(out))
}

// containerLogs returns recent log output for a container, or empty string if
// unavailable (e.g. the container was already removed).
func containerLogs(name string) string {
	out, _ := exec.Command("docker", "logs", "--tail", "100", name).CombinedOutput()
	return strings.TrimSpace(string(out))
}

// withContainerLogs appends the container's recent logs to err, if any exist.
func withContainerLogs(err error, container string) error {
	logs := containerLogs(container)
	return fmt.Errorf("%w\n\ncontainer logs:\n%s\n", err, logs)

}

// lookupHostPort returns the host-side port docker bound to container:port.
func lookupHostPort(container string, containerPort int) (string, error) {
	out, err := exec.Command("docker", "port", container, fmt.Sprintf("%d/tcp", containerPort)).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("container %s: port %d/tcp is not published — ensure the image `%s` was built with `hive image`", container, containerPort, containerImage(container))
	}
	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if !strings.HasPrefix(line, "0.0.0.0:") {
			continue
		}
		_, port, err := net.SplitHostPort(line)
		if err != nil {
			return "", fmt.Errorf("parse %q: %w", line, err)
		}
		return port, nil
	}
	return "", fmt.Errorf("container %s: port %d is not published — ensure the image `%s` was built with `hive image`", container, containerPort, containerImage(container))
}

// lookupTCPProxyEndpoint returns "localhost:<hostPort>" for the container's
// published sandboxTCPProxyPort, or nil if the port isn't mapped yet.
func lookupTCPProxyEndpoint(container string) *string {
	port, err := lookupHostPort(container, sandboxTCPProxyPort)
	if err != nil {
		return nil
	}
	s := "localhost:" + port
	return &s
}
