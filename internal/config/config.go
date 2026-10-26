// Package config defines the wsd daemon configuration struct.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Config is the wsd daemon configuration, decoded from wsd.json.
type Config struct {
	DataDir        string         `json:"data_dir"`
	JailerDir      string         `json:"jailer_dir"`
	FirecrackerBin string         `json:"firecracker_bin"`
	JailerBin      string         `json:"jailer_bin"`
	UIDRange       [2]int         `json:"uid_range"`
	VM             VMConfig       `json:"vm"`
	Net            NetConfig      `json:"net"`
	Egress         EgressCfg      `json:"egress"`
	IdleTimeout    string         `json:"idle_timeout"`
	Domain         string         `json:"domain"`
	ImageID        string         `json:"image_id"`
	SocketPath     string         `json:"socket_path"`
	Opencode       OpencodeConfig `json:"opencode"`
}

// OpencodeConfig controls how the opencode config file is generated and pushed.
type OpencodeConfig struct {
	ConfigTemplate string `json:"config_template"` // path to Go text/template, e.g. /etc/wsd/opencode.json.tmpl
	SecretsFile    string `json:"secrets_file"`    // path to JSON secrets file passed as template data
}

// VMConfig holds per-VM resource limits.
type VMConfig struct {
	VCPUs            int    `json:"vcpus"`
	MemMiB           int    `json:"mem_mib"`
	WorkspaceDiskGiB int    `json:"workspace_disk_gib"`
	CPUMax           string `json:"cpu_max"` // cgroup v2 cpu.max value, e.g. "200000 100000"
}

// NetConfig describes the host-side network configuration.
type NetConfig struct {
	Pool        string `json:"pool"`
	EgressIface string `json:"egress_iface"`
	DNS         string `json:"dns"`
}

// EgressCfg lists the allowed egress rules.
type EgressCfg struct {
	Allow []EgressRule `json:"allow"`
}

// EgressRule allows traffic to a CIDR on given ports.
type EgressRule struct {
	CIDR  string `json:"cidr"`
	Ports []int  `json:"ports"`
}

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config parse: %w", err)
	}
	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (cfg *Config) applyDefaults() {
	if cfg.SocketPath == "" {
		cfg.SocketPath = "/run/wsd/wsd.sock"
	}
	if cfg.VM.VCPUs == 0 {
		cfg.VM.VCPUs = 2
	}
	if cfg.VM.MemMiB == 0 {
		cfg.VM.MemMiB = 2048
	}
	if cfg.VM.WorkspaceDiskGiB == 0 {
		cfg.VM.WorkspaceDiskGiB = 20
	}
	if cfg.IdleTimeout == "" {
		cfg.IdleTimeout = "30m"
	}
	if cfg.VM.CPUMax == "" {
		cfg.VM.CPUMax = fmt.Sprintf("%d 100000", cfg.VM.VCPUs*100000)
	}
}

func (cfg *Config) validate() error {
	switch {
	case cfg.DataDir == "":
		return fmt.Errorf("config: data_dir requis")
	case cfg.JailerDir == "":
		return fmt.Errorf("config: jailer_dir requis")
	case cfg.FirecrackerBin == "":
		return fmt.Errorf("config: firecracker_bin requis")
	case cfg.JailerBin == "":
		return fmt.Errorf("config: jailer_bin requis")
	case cfg.UIDRange[0] == 0 || cfg.UIDRange[1] <= cfg.UIDRange[0]:
		return fmt.Errorf("config: uid_range invalide")
	case cfg.Net.Pool == "":
		return fmt.Errorf("config: net.pool requis")
	}
	return nil
}

// IdleTimeoutDuration parses IdleTimeout as a Duration.
func (cfg *Config) IdleTimeoutDuration() time.Duration {
	d, err := time.ParseDuration(cfg.IdleTimeout)
	if err != nil {
		return 30 * time.Minute
	}
	return d
}

// UIDForBlock returns the uid and gid to assign to a workspace by IP block index.
func (cfg *Config) UIDForBlock(blockIdx int) (uid, gid int) {
	uid = cfg.UIDRange[0] + blockIdx
	return uid, uid
}

// ImageDir returns the path to the directory for a given imageID.
func (cfg *Config) ImageDir(imageID string) string {
	return cfg.DataDir + "/images/" + imageID
}

// WorkspaceDir returns the workspace data directory path.
func (cfg *Config) WorkspaceDir(wsID string) string {
	return cfg.DataDir + "/workspaces/" + wsID
}

// GoldenDir returns the directory for a golden snapshot.
func (cfg *Config) GoldenDir(goldenID string) string {
	return cfg.DataDir + "/goldens/" + goldenID
}
