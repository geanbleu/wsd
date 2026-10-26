// Package fc is a minimal Firecracker HTTP API client over a Unix socket.
// Field names follow the Firecracker swagger at src/firecracker/swagger/firecracker.yaml (v1.17.0).
package fc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
)

// Client sends requests to the Firecracker API socket.
type Client struct {
	socket string
	http   *http.Client
}

// New creates a client for the Firecracker Unix socket at socketPath.
func New(socketPath string) *Client {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{socket: socketPath, http: &http.Client{Transport: tr}}
}

// ── request / response types ─────────────────────────────────────────────────

// MachineConfig corresponds to MachineConfiguration in the swagger.
type MachineConfig struct {
	VcpuCount  int  `json:"vcpu_count"`
	MemSizeMiB int  `json:"mem_size_mib"`
	SMT        bool `json:"smt,omitempty"`
}

// BootSource corresponds to BootSource in the swagger.
type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
	InitrdPath      string `json:"initrd_path,omitempty"`
}

// Drive corresponds to Drive in the swagger.
type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

// NetworkInterface corresponds to NetworkInterface in the swagger.
type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac,omitempty"`
}

// Vsock corresponds to Vsock in the swagger.
type Vsock struct {
	GuestCID uint32 `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
}

// VMState is the body of PATCH /vm.
type VMState struct {
	State string `json:"state"` // "Paused" or "Resumed"
}

// SnapshotCreateParams corresponds to SnapshotCreateParams in the swagger.
type SnapshotCreateParams struct {
	SnapshotPath     string `json:"snapshot_path"`
	MemFilePath      string `json:"mem_file_path"`
	SnapshotType     string `json:"snapshot_type,omitempty"`      // "Full" (default) or "Diff"
	SyncSnapshotFiles *bool `json:"sync_snapshot_files,omitempty"` // default true
}

// MemoryBackend corresponds to MemoryBackend in the swagger.
// Use BackendType "File" for the golden-share pattern.
type MemoryBackend struct {
	BackendType string `json:"backend_type"` // "File" or "Uffd"
	BackendPath string `json:"backend_path"`
}

// SnapshotLoadParams corresponds to SnapshotLoadParams in the swagger.
// mem_file_path is deprecated; use MemBackend.
type SnapshotLoadParams struct {
	SnapshotPath    string        `json:"snapshot_path"`
	MemBackend      MemoryBackend `json:"mem_backend"`
	ResumeVM        bool          `json:"resume_vm,omitempty"`
	TrackDirtyPages bool          `json:"track_dirty_pages,omitempty"`
}

// Balloon corresponds to Balloon in the swagger.
type Balloon struct {
	AmountMiB            int  `json:"amount_mib"`
	DeflateOnOOM         bool `json:"deflate_on_oom"`
	StatsPollingIntervalS int  `json:"stats_polling_interval_s,omitempty"`
}

// ── low-level helpers ─────────────────────────────────────────────────────────

func (c *Client) do(ctx context.Context, method, path string, body any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("fc marshal %s %s: %w", method, path, err)
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, r)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("fc %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("fc %s %s: HTTP %d: %s", method, path, resp.StatusCode, b)
	}
	return nil
}

// ── API methods ───────────────────────────────────────────────────────────────

// PutMachineConfig sets vCPU count and memory.
func (c *Client) PutMachineConfig(ctx context.Context, cfg MachineConfig) error {
	return c.do(ctx, http.MethodPut, "/machine-config", cfg)
}

// PutBootSource sets the kernel image and boot arguments.
func (c *Client) PutBootSource(ctx context.Context, bs BootSource) error {
	return c.do(ctx, http.MethodPut, "/boot-source", bs)
}

// PutDrive attaches or updates a block device.
func (c *Client) PutDrive(ctx context.Context, d Drive) error {
	return c.do(ctx, http.MethodPut, "/drives/"+d.DriveID, d)
}

// PutNetworkInterface attaches or updates a network interface.
func (c *Client) PutNetworkInterface(ctx context.Context, ni NetworkInterface) error {
	return c.do(ctx, http.MethodPut, "/network-interfaces/"+ni.IfaceID, ni)
}

// PutVsock configures the vsock device.
func (c *Client) PutVsock(ctx context.Context, v Vsock) error {
	return c.do(ctx, http.MethodPut, "/vsock", v)
}

// PutBalloon configures the balloon device.
func (c *Client) PutBalloon(ctx context.Context, b Balloon) error {
	return c.do(ctx, http.MethodPut, "/balloon", b)
}

// Start sends the InstanceStart action to boot the VM.
func (c *Client) Start(ctx context.Context) error {
	return c.do(ctx, http.MethodPut, "/actions", map[string]string{"action_type": "InstanceStart"})
}

// Pause transitions the VM to the Paused state (required before snapshot).
func (c *Client) Pause(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", VMState{State: "Paused"})
}

// Resume transitions the VM from Paused back to running.
func (c *Client) Resume(ctx context.Context) error {
	return c.do(ctx, http.MethodPatch, "/vm", VMState{State: "Resumed"})
}

// CreateSnapshot creates a full snapshot (vmstate + mem).
func (c *Client) CreateSnapshot(ctx context.Context, p SnapshotCreateParams) error {
	if p.SnapshotType == "" {
		p.SnapshotType = "Full"
	}
	return c.do(ctx, http.MethodPut, "/snapshot/create", p)
}

// LoadSnapshot restores a VM from a snapshot.
// Set p.ResumeVM = true to atomically resume after load.
func (c *Client) LoadSnapshot(ctx context.Context, p SnapshotLoadParams) error {
	return c.do(ctx, http.MethodPut, "/snapshot/load", p)
}
