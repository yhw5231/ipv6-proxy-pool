package config

import (
	"strings"
	"testing"
	"time"
)

func TestAlwaysOnPortsValidation(t *testing.T) {
	tests := []struct {
		name      string
		ports     []int
		mode      string
		wantError string
	}{
		{
			name:  "valid ports",
			ports: []int{20000, 20001, 21023},
			mode:  ModePerIPv6,
		},
		{
			name:      "port below configured range",
			ports:     []int{19999},
			mode:      ModePerIPv6,
			wantError: "outside the configured range",
		},
		{
			name:      "port above configured range",
			ports:     []int{21024},
			mode:      ModePerIPv6,
			wantError: "outside the configured range",
		},
		{
			name:      "duplicate port",
			ports:     []int{20000, 20000},
			mode:      ModePerIPv6,
			wantError: "duplicate port",
		},
		{
			name:      "unsupported in multiplex mode",
			ports:     []int{20000},
			mode:      ModeMultiplex,
			wantError: "only supported in per_ipv6 mode",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Default()
			cfg.MaxLeases = 1024
			cfg.SOCKS.Mode = test.mode
			cfg.SOCKS.PortStart = 20000
			cfg.SOCKS.PortEnd = 21023
			cfg.SOCKS.AlwaysOnPorts = test.ports

			err := cfg.Validate()
			if test.wantError == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() succeeded, want error containing %q", test.wantError)
			}
			if !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Validate() error = %q, want substring %q", err, test.wantError)
			}
		})
	}
}

func TestAlwaysOnPortsCannotExceedMaxLeases(t *testing.T) {
	cfg := Default()
	cfg.MinLeases = 1
	cfg.MaxLeases = 1
	cfg.SOCKS.Mode = ModePerIPv6
	cfg.SOCKS.PortStart = 20000
	cfg.SOCKS.PortEnd = 20001
	cfg.SOCKS.AlwaysOnPorts = []int{20000, 20001}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() succeeded, want max_leases error")
	}
	if !strings.Contains(err.Error(), "must not contain more entries than max_leases") {
		t.Fatalf("Validate() error = %q", err)
	}
}

func TestAdminTokenValidation(t *testing.T) {
	good := Default()
	good.Admin.Token = "secret-token-123"
	if err := good.Validate(); err != nil {
		t.Fatalf("Validate() with long token error = %v", err)
	}

	weak := Default()
	weak.Admin.Token = "short"
	if err := weak.Validate(); err == nil {
		t.Fatal("Validate() accepted a token shorter than 8 characters")
	}

	longToken := Default()
	longToken.Admin.Token = "k7#pQv2$eR9!sT5@"
	if err := longToken.Validate(); err != nil {
		t.Fatalf("Validate() with special-character token error = %v", err)
	}
}

func TestMinLeasesValidation(t *testing.T) {
	defaults := Default()
	if defaults.MinLeases != 1024 || defaults.MaxLeases != 2048 {
		t.Fatalf("defaults min/max = %d/%d, want 1024/2048", defaults.MinLeases, defaults.MaxLeases)
	}
	if defaults.IdleTimeout != time.Hour {
		t.Fatalf("default idle_timeout = %v, want 1h", defaults.IdleTimeout)
	}
	if err := defaults.Validate(); err != nil {
		t.Fatalf("Validate() defaults error = %v", err)
	}

	over := Default()
	over.MinLeases = over.MaxLeases + 1
	if err := over.Validate(); err == nil || !strings.Contains(err.Error(), "min_leases must not exceed") {
		t.Fatalf("Validate() min>max error = %v", err)
	}

	negative := Default()
	negative.MinLeases = -1
	if err := negative.Validate(); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("Validate() negative min error = %v", err)
	}

	zero := Default()
	zero.MinLeases = 0
	if err := zero.Validate(); err != nil {
		t.Fatalf("Validate() min=0 error = %v", err)
	}
}
