package ipv6addr

import (
	"net"
	"testing"
)

func TestGeneratorFromIndex(t *testing.T) {
	generator, err := NewGenerator("2001:db8:1234:5678::/64")
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}

	tests := []struct {
		index uint64
		want  string
	}{
		{index: 0, want: "2001:db8:1234:5678::"},
		{index: 1, want: "2001:db8:1234:5678::1"},
		{index: 0x1020304050607080, want: "2001:db8:1234:5678:1020:3040:5060:7080"},
	}

	for _, tt := range tests {
		got, err := generator.FromIndex(tt.index)
		if err != nil {
			t.Fatalf("FromIndex(%d): %v", tt.index, err)
		}
		if got.String() != tt.want {
			t.Errorf("FromIndex(%d) = %s, want %s", tt.index, got, tt.want)
		}
		if !generator.Contains(got) {
			t.Errorf("generated address %s is outside prefix", got)
		}
	}
}

func TestGeneratorRejectsOutOfRangeIndex(t *testing.T) {
	generator, err := NewGenerator("2001:db8::/127")
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}

	if _, err := generator.FromIndex(2); err == nil {
		t.Fatal("FromIndex(2) succeeded for /127 prefix")
	}
}

func TestGeneratorRandomStaysWithinPrefix(t *testing.T) {
	generator, err := NewGenerator("fd00:abcd::/80")
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}

	for i := 0; i < 100; i++ {
		ip, err := generator.Random()
		if err != nil {
			t.Fatalf("Random: %v", err)
		}
		if !generator.Contains(ip) {
			t.Fatalf("Random returned address outside prefix: %s", ip)
		}
	}
}

func TestGeneratorValidation(t *testing.T) {
	invalid := []string{
		"",
		"192.0.2.0/24",
		"2001:db8::/128",
		"not-a-prefix",
	}

	for _, prefix := range invalid {
		if _, err := NewGenerator(prefix); err == nil {
			t.Errorf("NewGenerator(%q) succeeded, want error", prefix)
		}
	}
}

func TestGeneratorContainsRejectsUnrelatedAddresses(t *testing.T) {
	generator, err := NewGenerator("2001:db8::/64")
	if err != nil {
		t.Fatalf("NewGenerator: %v", err)
	}

	if generator.Contains(net.ParseIP("2001:db9::1")) {
		t.Fatal("Contains accepted address outside prefix")
	}
	if generator.Contains(net.ParseIP("127.0.0.1")) {
		t.Fatal("Contains accepted IPv4 address")
	}
	if generator.Contains(nil) {
		t.Fatal("Contains accepted nil address")
	}
}
