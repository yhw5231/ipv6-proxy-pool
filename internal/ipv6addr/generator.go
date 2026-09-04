package ipv6addr

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net"
)

// Generator creates IPv6 addresses contained within a configured prefix.
type Generator struct {
	network *net.IPNet
	base    *big.Int
	hostMax *big.Int
}

// NewGenerator validates prefix and creates an IPv6 address generator.
func NewGenerator(prefix string) (*Generator, error) {
	ip, network, err := net.ParseCIDR(prefix)
	if err != nil || ip.To4() != nil {
		return nil, fmt.Errorf("invalid IPv6 prefix %q", prefix)
	}

	ones, bits := network.Mask.Size()
	if bits != 128 || ones < 1 || ones > 127 {
		return nil, errors.New("IPv6 prefix length must be between 1 and 127")
	}

	baseIP := ip.Mask(network.Mask).To16()
	if baseIP == nil {
		return nil, fmt.Errorf("invalid IPv6 prefix %q", prefix)
	}

	hostMax := new(big.Int).Lsh(big.NewInt(1), uint(128-ones))
	return &Generator{
		network: network,
		base:    new(big.Int).SetBytes(baseIP),
		hostMax: hostMax,
	}, nil
}

// Random returns a cryptographically random address inside the prefix.
func (g *Generator) Random() (net.IP, error) {
	host, err := rand.Int(rand.Reader, g.hostMax)
	if err != nil {
		return nil, fmt.Errorf("generate IPv6 host bits: %w", err)
	}
	return g.fromHost(host), nil
}

// FromIndex deterministically maps a non-negative index into the prefix.
func (g *Generator) FromIndex(index uint64) (net.IP, error) {
	host := new(big.Int).SetUint64(index)
	if host.Cmp(g.hostMax) >= 0 {
		return nil, fmt.Errorf("index %d exceeds IPv6 prefix capacity", index)
	}
	return g.fromHost(host), nil
}

// Contains reports whether ip belongs to the configured prefix.
func (g *Generator) Contains(ip net.IP) bool {
	return ip != nil && ip.To4() == nil && g.network.Contains(ip)
}

func (g *Generator) fromHost(host *big.Int) net.IP {
	value := new(big.Int).Or(new(big.Int).Set(g.base), host)
	bytes := value.Bytes()
	ip := make(net.IP, net.IPv6len)
	copy(ip[net.IPv6len-len(bytes):], bytes)
	return ip
}
