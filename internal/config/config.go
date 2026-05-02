package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Nickname         string   `yaml:"nickname"`
	DataDir          string   `yaml:"data_dir"`
	ContributedBytes int64    `yaml:"contributed_bytes"`
	ListenAddrs      []string `yaml:"listen_addrs"`
	AnnounceAddrs    []string `yaml:"announce_addrs,omitempty"`
	APIListen        string   `yaml:"api_listen"`

	// HeartbeatSeconds: how often we ping every peer. Production default
	// 30s; tests override to ~5s for faster offline detection.
	HeartbeatSeconds int `yaml:"heartbeat_seconds,omitempty"`

	// MountPath: if non-empty, the daemon mounts the unified resource
	// library as a read-only FUSE filesystem at this path. Other apps on
	// the host (飞牛影视, Plex, file managers…) can then read fnshare
	// content like normal local files.
	MountPath string `yaml:"mount_path,omitempty"`

	// PublicHost: the DDNS hostname (or static IP) reachable from the
	// public internet — used to auto-fill the bootstrap multiaddr in
	// new invite links. Survives IP changes (DDNS resolves), so an
	// invite generated today still works tomorrow when your home IP
	// rotates. Format: bare hostname like "myhome.dyn.example.com".
	PublicHost string `yaml:"public_host,omitempty"`

	// PublicPort overrides the port advertised in the public bootstrap
	// multiaddr. Defaults to 4001 (the libp2p listen port). Only set
	// this if your router maps a different external port to 4001.
	PublicPort int `yaml:"public_port,omitempty"`
}

func Default(dataDir string) Config {
	return Config{
		Nickname:         hostnameOr("fnshare-node"),
		DataDir:          dataDir,
		ContributedBytes: 100 * 1024 * 1024 * 1024, // 100 GiB
		ListenAddrs: []string{
			"/ip4/0.0.0.0/tcp/4001",
			"/ip4/0.0.0.0/udp/4001/quic-v1",
			"/ip6/::/tcp/4001",
			"/ip6/::/udp/4001/quic-v1",
		},
		// 0.0.0.0 by default so the embedded Web UI is reachable from any
		// host on the LAN (typical fnOS use). For paranoid setups, override
		// to 127.0.0.1 in config.yaml.
		APIListen:        "0.0.0.0:4101",
		HeartbeatSeconds: 30,
	}
}

func Path(dataDir string) string {
	return filepath.Join(dataDir, "config.yaml")
}

func Load(dataDir string) (Config, error) {
	p := Path(dataDir)
	raw, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("config not found at %s — run `fnshare init` first", p)
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return Config{}, err
	}
	if c.DataDir == "" {
		c.DataDir = dataDir
	}
	return c, nil
}

func Save(c Config) error {
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		return err
	}
	raw, err := yaml.Marshal(&c)
	if err != nil {
		return err
	}
	return os.WriteFile(Path(c.DataDir), raw, 0o600)
}

func hostnameOr(fallback string) string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return fallback
	}
	return h
}
