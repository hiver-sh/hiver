// Package firecracker is a dependency-free host-side driver for the
// Firecracker VMM. It speaks Firecracker's REST API over its API unix
// socket and models the boot configuration (machine, kernel, drives,
// network, vsock) so the microvm isolation backend can boot and control a
// guest without pulling in the upstream Go SDK.
//
// Scope: everything here runs on the host. Assembling the overlay/FUSE
// stack, the in-guest firewall, and running exec'd commands happen inside
// the guest and are handled by the in-guest agent (cmd/sbxguest) reached
// over vsock; see internal/vsockexec for the wire protocol.
package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"time"
)

// Host/guest vsock parameters. The host CID is always 2 (VMADDR_CID_HOST);
// the guest CID is assigned by us and must be >= 3. The guest agent listens
// on GuestExecPort for exec sessions.
//
// GuestReadyPort is the reverse direction: a host-listening port (mapped to
// the unix socket ${uds}_<port>) that the guest dials once its exec listener
// is up, so the host's WaitReady blocks on Accept instead of polling.
const (
	HostCID        uint32 = 2
	GuestCID       uint32 = 3
	GuestExecPort  uint32 = 1024
	GuestReadyPort uint32 = 1026
)

// Client talks to a running Firecracker process over its API unix socket
// (the --api-sock path). Firecracker serves a small REST API there.
type Client struct {
	sock string
	http *http.Client
}

// NewClient returns a Client bound to the Firecracker API socket at sock.
// The socket need not exist yet; requests block (up to their context
// deadline) until Firecracker creates and listens on it.
func NewClient(sock string) *Client {
	return &Client{
		sock: sock,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, "unix", sock)
				},
			},
		},
	}
}

// MachineConfig is the body of PUT /machine-config.
type MachineConfig struct {
	VcpuCount  int  `json:"vcpu_count"`
	MemSizeMib int  `json:"mem_size_mib"`
	Smt        bool `json:"smt"`
}

// BootSource is the body of PUT /boot-source.
type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
	InitrdPath      string `json:"initrd_path,omitempty"`
}

// Drive is the body of PUT /drives/{drive_id}.
type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

// NetworkInterface is the body of PUT /network-interfaces/{iface_id}.
type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac,omitempty"`
}

// Vsock is the body of PUT /vsock. UDSPath is the host-side unix socket
// Firecracker creates to multiplex guest vsock connections.
type Vsock struct {
	GuestCID int    `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// Logger is the body of PUT /logger.
type Logger struct {
	LogPath string `json:"log_path,omitempty"`
	Level   string `json:"level,omitempty"`
}

// InstanceInfo is the body of GET /.
type InstanceInfo struct {
	ID         string `json:"id"`
	State      string `json:"state"` // "Not started", "Running", "Paused"
	VmmVersion string `json:"vmm_version"`
}

// apiError mirrors Firecracker's error response shape.
type apiError struct {
	FaultMessage string `json:"fault_message"`
}

func (c *Client) put(ctx context.Context, path string, body any) error {
	return c.do(ctx, http.MethodPut, path, body, nil)
}

// do issues a request to the API socket. The host portion of the URL is a
// throwaway — the transport always dials the unix socket — so any value
// works as long as it's a valid URL.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal %s %s: %w", method, path, err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var ae apiError
		_ = json.NewDecoder(resp.Body).Decode(&ae)
		if ae.FaultMessage != "" {
			return fmt.Errorf("%s %s: firecracker %s: %s", method, path, resp.Status, ae.FaultMessage)
		}
		return fmt.Errorf("%s %s: firecracker %s", method, path, resp.Status)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) PutMachineConfig(ctx context.Context, m MachineConfig) error {
	return c.put(ctx, "/machine-config", m)
}

func (c *Client) PutBootSource(ctx context.Context, b BootSource) error {
	return c.put(ctx, "/boot-source", b)
}

func (c *Client) PutDrive(ctx context.Context, d Drive) error {
	return c.put(ctx, "/drives/"+d.DriveID, d)
}

func (c *Client) PutNetworkInterface(ctx context.Context, n NetworkInterface) error {
	return c.put(ctx, "/network-interfaces/"+n.IfaceID, n)
}

func (c *Client) PutVsock(ctx context.Context, v Vsock) error {
	return c.put(ctx, "/vsock", v)
}

func (c *Client) PutLogger(ctx context.Context, l Logger) error {
	return c.put(ctx, "/logger", l)
}

// Start issues the InstanceStart action, booting the guest.
func (c *Client) Start(ctx context.Context) error {
	return c.put(ctx, "/actions", map[string]string{"action_type": "InstanceStart"})
}

// Pause/Resume transition the VM via PATCH /vm; Pause is used before a
// snapshot so guest memory is quiesced.
func (c *Client) Pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Paused"}, nil)
}

func (c *Client) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", map[string]string{"state": "Resumed"}, nil)
}

// SnapshotCreate is the body of PUT /snapshot/create.
type SnapshotCreate struct {
	SnapshotType string `json:"snapshot_type"`
	SnapshotPath string `json:"snapshot_path"`
	MemFilePath  string `json:"mem_file_path"`
}

// memBackend is the mem_backend object of PUT /snapshot/load — the current
// Firecracker API addresses guest memory through a backend descriptor rather
// than the legacy flat mem_file_path field.
type memBackend struct {
	BackendType string `json:"backend_type"`
	BackendPath string `json:"backend_path"`
}

// NetworkOverride is one entry of SnapshotLoad.NetworkOverrides: it repoints a
// snapshotted network interface at a different host tap. Required for pack-mode
// resume, where every VM loads the same base snapshot (which recorded the base
// builder's tap) but must egress on its own per-sandbox tap. Added in Firecracker
// v1.12.0; see docker/core.Dockerfile's version pin.
type NetworkOverride struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
}

// SnapshotLoad is the body of PUT /snapshot/load.
type SnapshotLoad struct {
	SnapshotPath        string            `json:"snapshot_path"`
	MemBackend          memBackend        `json:"mem_backend"`
	EnableDiffSnapshots bool              `json:"enable_diff_snapshots"`
	ResumeVM            bool              `json:"resume_vm"`
	NetworkOverrides    []NetworkOverride `json:"network_overrides,omitempty"`
}

// CreateSnapshot writes a full VM snapshot — guest memory to memFilePath and
// device/vCPU state to snapshotPath. The VM must be Paused first (Firecracker
// rejects snapshotting a running VM); the caller pauses, snapshots, then stops
// the VM.
func (c *Client) CreateSnapshot(ctx context.Context, snapshotPath, memFilePath string) error {
	return c.put(ctx, "/snapshot/create", SnapshotCreate{
		SnapshotType: "Full",
		SnapshotPath: snapshotPath,
		MemFilePath:  memFilePath,
	})
}

// LoadSnapshot restores a VM from the snapshotPath/memFilePath pair produced by
// CreateSnapshot. It must be the first state-changing call on a *fresh*
// Firecracker process — one started with no --config-file and not yet booted.
// When resume is true the guest is resumed as part of the load; otherwise it
// stays Paused until Resume. netOverrides repoints snapshotted interfaces at
// per-VM host taps (nil for an in-place resume that reuses the recorded tap).
func (c *Client) LoadSnapshot(ctx context.Context, snapshotPath, memFilePath string, resume bool, netOverrides ...NetworkOverride) error {
	return c.put(ctx, "/snapshot/load", SnapshotLoad{
		SnapshotPath:        snapshotPath,
		MemBackend:          memBackend{BackendType: "File", BackendPath: memFilePath},
		EnableDiffSnapshots: false,
		ResumeVM:            resume,
		NetworkOverrides:    netOverrides,
	})
}

// InstanceInfo reads GET / for the current VM state.
func (c *Client) InstanceInfo(ctx context.Context) (InstanceInfo, error) {
	var info InstanceInfo
	err := c.do(ctx, http.MethodGet, "/", nil, &info)
	return info, err
}

// WaitAPIReady blocks until the API socket accepts a request (Firecracker
// has created it and is serving) or ctx is cancelled.
func (c *Client) WaitAPIReady(ctx context.Context) error {
	for {
		if _, err := c.InstanceInfo(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
