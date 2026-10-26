// Package agentproto defines the newline-delimited JSON protocol between host and wsagent (vsock port 1024).
package agentproto

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"strings"
)

// Port is the vsock port the agent listens on inside the guest.
const Port = 1024

// Op constants for the op field of requests.
const (
	OpPing            = "ping"
	OpPostRestore     = "post_restore"
	OpPrepareSnapshot = "prepare_snapshot"
	OpStats           = "stats"
)

// Req is a generic request header used for dispatch.
type Req struct {
	ID string `json:"id,omitempty"`
	Op string `json:"op"`
}

// FileSpec describes a file to be written by the agent during post-restore.
type FileSpec struct {
	Path       string `json:"path"`
	Mode       int    `json:"mode"`
	Owner      string `json:"owner"`
	ContentB64 string `json:"content_b64"`
}

// PostRestoreReq is sent after a snapshot restore.
type PostRestoreReq struct {
	ID             string     `json:"id,omitempty"`
	Op             string     `json:"op"` // OpPostRestore
	TimeUnixNano   int64      `json:"time_unix_nano"`
	MachineID      string     `json:"machine_id"`
	Hostname       string     `json:"hostname"`
	RandomBytes    []byte     `json:"random_bytes,omitempty"`
	Files          []FileSpec `json:"files,omitempty"`
	MountWorkspace bool       `json:"mount_workspace"`
	RestartUnits   []string   `json:"restart_units,omitempty"`
}

// PrepareSnapshotReq asks the agent to sync and drop_caches before a snapshot.
type PrepareSnapshotReq struct {
	ID string `json:"id,omitempty"`
	Op string `json:"op"` // OpPrepareSnapshot
}

// PingResp is the response to a ping request.
type PingResp struct {
	ID    string `json:"id,omitempty"`
	OK    bool   `json:"ok"`
	Ready bool   `json:"ready"`
}

// OKResp is the generic success/error response.
type OKResp struct {
	ID    string `json:"id,omitempty"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Conn is a host-side connection to the agent over the Firecracker vsock UDS.
type Conn struct {
	conn net.Conn
	br   *bufio.Reader
}

// Dial opens the Firecracker vsock UDS at udsPath and performs the CONNECT handshake
// to reach vsock port 1024 inside the guest.
func Dial(udsPath string) (*Conn, error) {
	c, err := net.Dial("unix", udsPath)
	if err != nil {
		return nil, fmt.Errorf("vsock dial %s: %w", udsPath, err)
	}
	conn := &Conn{conn: c, br: bufio.NewReader(c)}
	if err := conn.handshake(); err != nil {
		c.Close()
		return nil, err
	}
	return conn, nil
}

func (c *Conn) handshake() error {
	if _, err := fmt.Fprintf(c.conn, "CONNECT %d\n", Port); err != nil {
		return fmt.Errorf("vsock handshake write: %w", err)
	}
	resp, err := c.br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("vsock handshake read: %w", err)
	}
	if !strings.HasPrefix(resp, "OK") {
		return fmt.Errorf("vsock handshake: unexpected response %q", resp)
	}
	return nil
}

func (c *Conn) Close() error { return c.conn.Close() }

func (c *Conn) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = c.conn.Write(b)
	return err
}

func (c *Conn) recv(v any) error {
	line, err := c.br.ReadBytes('\n')
	if err != nil {
		return err
	}
	return json.Unmarshal(line, v)
}

// Ping sends a ping and returns the response.
func (c *Conn) Ping() (*PingResp, error) {
	if err := c.send(Req{Op: OpPing}); err != nil {
		return nil, fmt.Errorf("ping send: %w", err)
	}
	var resp PingResp
	if err := c.recv(&resp); err != nil {
		return nil, fmt.Errorf("ping recv: %w", err)
	}
	return &resp, nil
}

// PostRestore sends the post-restore payload and waits for acknowledgement.
func (c *Conn) PostRestore(req PostRestoreReq) (*OKResp, error) {
	req.Op = OpPostRestore
	if err := c.send(req); err != nil {
		return nil, fmt.Errorf("post_restore send: %w", err)
	}
	var resp OKResp
	if err := c.recv(&resp); err != nil {
		return nil, fmt.Errorf("post_restore recv: %w", err)
	}
	return &resp, nil
}

// PrepareSnapshot asks the agent to sync and drop caches before a snapshot.
func (c *Conn) PrepareSnapshot() (*OKResp, error) {
	if err := c.send(PrepareSnapshotReq{Op: OpPrepareSnapshot}); err != nil {
		return nil, fmt.Errorf("prepare_snapshot send: %w", err)
	}
	var resp OKResp
	if err := c.recv(&resp); err != nil {
		return nil, fmt.Errorf("prepare_snapshot recv: %w", err)
	}
	return &resp, nil
}
