package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	ModeMultiplex = "multiplex"
	ModePerIPv6   = "per_ipv6"
)

type Config struct {
	IPv6Prefix string `json:"ipv6_prefix"`
	// MinLeases is the resident standby floor. The pool keeps this many
	// unassigned standby leases ready at all times; leases claimed from it are
	// protected from idle release while the total stays at or below the floor.
	MinLeases      int           `json:"min_leases"`
	MaxLeases      int           `json:"max_leases"`
	IdleTimeout    time.Duration `json:"idle_timeout"`
	RotateAfter    time.Duration `json:"rotate_after"`
	RotateRequests uint64        `json:"rotate_requests"`
	SOCKS          SOCKSConfig   `json:"socks"`
	Admin          AdminConfig   `json:"admin"`
}

type SOCKSConfig struct {
	Mode          string `json:"mode"`
	ListenAddress string `json:"listen_address"`
	PortStart     int    `json:"port_start"`
	PortEnd       int    `json:"port_end"`
	AlwaysOnPorts []int  `json:"always_on_ports,omitempty"`
}

type AdminConfig struct {
	ListenAddress string `json:"listen_address"`
	// Token, when set, requires `Authorization: Bearer <token>` on every /v1/*
	// request. It lets remote clients safely request and manage proxy leases.
	Token string `json:"token,omitempty"`
}

func Default() Config {
	return Config{
		IPv6Prefix:     "2001:db8::/64",
		MinLeases:      1024,
		MaxLeases:      2048,
		IdleTimeout:    time.Hour,
		RotateAfter:    0,
		RotateRequests: 0,
		SOCKS: SOCKSConfig{
			Mode:          ModeMultiplex,
			ListenAddress: "127.0.0.1:1080",
			PortStart:     20000,
			PortEnd:       21023,
		},
		Admin: AdminConfig{ListenAddress: "127.0.0.1:8080"},
	}
}

func (c Config) Validate() error {
	ip, network, err := net.ParseCIDR(c.IPv6Prefix)
	if err != nil || ip.To4() != nil {
		return fmt.Errorf("ipv6_prefix must be a valid IPv6 CIDR: %q", c.IPv6Prefix)
	}
	ones, bits := network.Mask.Size()
	if bits != 128 || ones < 1 || ones > 127 {
		return fmt.Errorf("ipv6_prefix length must be between 1 and 127")
	}
	if c.MaxLeases <= 0 {
		return errors.New("max_leases must be greater than zero")
	}
	if c.MinLeases < 0 {
		return errors.New("min_leases must not be negative")
	}
	if c.MinLeases > c.MaxLeases {
		return errors.New("min_leases must not exceed max_leases")
	}
	if c.IdleTimeout < 0 {
		return errors.New("idle_timeout must not be negative")
	}
	if c.RotateAfter < 0 {
		return errors.New("rotate_after must not be negative")
	}
	if c.SOCKS.Mode != ModeMultiplex && c.SOCKS.Mode != ModePerIPv6 {
		return fmt.Errorf("socks.mode must be %q or %q", ModeMultiplex, ModePerIPv6)
	}
	if err := validateListenAddress("socks.listen_address", c.SOCKS.ListenAddress); err != nil {
		return err
	}
	if err := validateListenAddress("admin.listen_address", c.Admin.ListenAddress); err != nil {
		return err
	}
	if c.SOCKS.PortStart < 1 || c.SOCKS.PortStart > 65535 {
		return errors.New("socks.port_start must be between 1 and 65535")
	}
	if c.SOCKS.PortEnd < c.SOCKS.PortStart || c.SOCKS.PortEnd > 65535 {
		return errors.New("socks.port_end must be between port_start and 65535")
	}
	if len(c.SOCKS.AlwaysOnPorts) > 0 && c.SOCKS.Mode != ModePerIPv6 {
		return errors.New("socks.always_on_ports is only supported in per_ipv6 mode")
	}
	seenAlwaysOnPorts := make(map[int]struct{}, len(c.SOCKS.AlwaysOnPorts))
	for _, port := range c.SOCKS.AlwaysOnPorts {
		if port < c.SOCKS.PortStart || port > c.SOCKS.PortEnd {
			return fmt.Errorf("socks.always_on_ports contains port %d outside the configured range %d-%d", port, c.SOCKS.PortStart, c.SOCKS.PortEnd)
		}
		if _, exists := seenAlwaysOnPorts[port]; exists {
			return fmt.Errorf("socks.always_on_ports contains duplicate port %d", port)
		}
		seenAlwaysOnPorts[port] = struct{}{}
	}
	if len(c.SOCKS.AlwaysOnPorts) > c.MaxLeases {
		return errors.New("socks.always_on_ports must not contain more entries than max_leases")
	}
	if c.SOCKS.Mode == ModePerIPv6 && c.SOCKS.PortEnd-c.SOCKS.PortStart+1 < c.MaxLeases {
		return errors.New("per_ipv6 mode requires a port range at least as large as max_leases")
	}
	if len(c.Admin.Token) > 0 && len(c.Admin.Token) < 8 {
		return errors.New("admin.token must be at least 8 characters when set")
	}
	return nil
}

func validateListenAddress(name, address string) error {
	if address == "" {
		return fmt.Errorf("%s must not be empty", name)
	}
	_, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		return fmt.Errorf("%s must be a valid host:port address", name)
	}
	return nil
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	cfg := Default()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, errors.New("decode config: trailing JSON value")
		}
		return Config{}, fmt.Errorf("decode config trailing content: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func SaveAtomic(path string, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary config: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("set temporary config permissions: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write temporary config: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return fmt.Errorf("sync temporary config: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary config: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}
