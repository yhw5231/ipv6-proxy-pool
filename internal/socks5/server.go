package socks5

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"

	"ipv6-proxy-pool/internal/lease"
)

const (
	version5       = 0x05
	methodNone     = 0x00
	methodPassword = 0x02
	methodRejected = 0xff
	commandConnect = 0x01
	addressIPv4    = 0x01
	addressDomain  = 0x03
	addressIPv6    = 0x04
)

// DialContextFunc allows tests to inject a dialer without using external IPv6.
type DialContextFunc func(ctx context.Context, network, address string, localIP net.IP) (net.Conn, error)

// Pool is the lease behavior required by the SOCKS server.
type Pool interface {
	Acquire(id string, persistent bool) (lease.Lease, error)
	AcquirePort(id string, port int, persistent bool) (lease.Lease, error)
	RecordRequest(id string) (lease.Lease, error)
}

// Server implements SOCKS5 CONNECT with lease-based source IPv6 selection.
type Server struct {
	Pool Pool
	Dial DialContextFunc
}

// NewServer creates a SOCKS5 server. The default dialer binds LocalAddr to the
// selected lease IPv6 before connecting to the requested destination.
func NewServer(pool Pool) *Server {
	return &Server{Pool: pool, Dial: dialBound}
}

// Serve accepts connections until the listener is closed or context expires.
// fixedLeaseID is non-empty for one-port-per-IPv6 mode; the listener's bound
// port is passed to ServeConn so the lease is re-acquired on that exact port.
func (s *Server) Serve(ctx context.Context, listener net.Listener, fixedLeaseID string) error {
	if s.Pool == nil {
		return errors.New("lease pool is required")
	}
	if s.Dial == nil {
		s.Dial = dialBound
	}

	fixedPort := 0
	if fixedLeaseID != "" {
		if tcpAddress, ok := listener.Addr().(*net.TCPAddr); ok {
			fixedPort = tcpAddress.Port
		}
	}

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accept SOCKS connection: %w", err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer conn.Close()
			_ = s.ServeConn(ctx, conn, fixedLeaseID, fixedPort)
		}()
	}
}

// ServeConn handles one SOCKS5 connection. In multiplex mode the lease
// identity is selected from username, password, or an explicit lease id in
// username form "lease:<id>". With no authentication data, the remote address
// is used as a stable identity.
//
// In per-IPv6 mode fixedLeaseID is non-empty and fixedPort must be the
// listener's TCP port. The lease is re-acquired on that exact port without
// forcing persistence, so idle release can still reclaim dynamic leases; the
// port keeps matching the running listener.
func (s *Server) ServeConn(ctx context.Context, client net.Conn, fixedLeaseID string, fixedPort int) error {
	if s.Pool == nil {
		return errors.New("lease pool is required")
	}
	if s.Dial == nil {
		s.Dial = dialBound
	}

	identity, err := negotiate(client, fixedLeaseID)
	if err != nil {
		return err
	}
	if identity == "" {
		identity = client.RemoteAddr().String()
	}

	var selected lease.Lease
	if fixedLeaseID != "" {
		if fixedPort <= 0 {
			return errors.New("fixed lease requires a listener port")
		}
		selected, err = s.Pool.AcquirePort(identity, fixedPort, false)
	} else {
		selected, err = s.Pool.Acquire(identity, false)
	}
	if err != nil {
		_ = writeReply(client, 0x01, nil)
		return fmt.Errorf("acquire lease: %w", err)
	}
	localIP := net.ParseIP(selected.IPv6)
	if localIP == nil || localIP.To4() != nil {
		_ = writeReply(client, 0x01, nil)
		return fmt.Errorf("lease %q has invalid IPv6 address %q", selected.ID, selected.IPv6)
	}

	destination, err := readConnectRequest(client)
	if err != nil {
		return err
	}
	upstream, err := s.Dial(ctx, "tcp", destination, localIP)
	if err != nil {
		_ = writeReply(client, 0x05, nil)
		return fmt.Errorf("connect %s from %s: %w", destination, localIP, err)
	}
	defer upstream.Close()

	if err := writeReply(client, 0x00, upstream.LocalAddr()); err != nil {
		return err
	}
	if _, err := s.Pool.RecordRequest(identity); err != nil {
		return fmt.Errorf("record lease request: %w", err)
	}

	return relay(client, upstream)
}

func dialBound(ctx context.Context, network, address string, localIP net.IP) (net.Conn, error) {
	dialer := &net.Dialer{LocalAddr: &net.TCPAddr{IP: localIP}}
	return dialer.DialContext(ctx, network, address)
}

func negotiate(conn net.Conn, fixedLeaseID string) (string, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", fmt.Errorf("read SOCKS greeting: %w", err)
	}
	if header[0] != version5 || header[1] == 0 {
		return "", errors.New("invalid SOCKS5 greeting")
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return "", fmt.Errorf("read SOCKS methods: %w", err)
	}

	method := byte(methodRejected)
	if fixedLeaseID != "" && containsMethod(methods, methodNone) {
		method = methodNone
	} else if containsMethod(methods, methodPassword) {
		method = methodPassword
	} else if containsMethod(methods, methodNone) {
		method = methodNone
	}
	if _, err := conn.Write([]byte{version5, method}); err != nil {
		return "", fmt.Errorf("write SOCKS method: %w", err)
	}
	if method == methodRejected {
		return "", errors.New("no supported SOCKS authentication method")
	}
	if fixedLeaseID != "" {
		if method == methodPassword {
			if _, _, err := readCredentials(conn); err != nil {
				return "", err
			}
		}
		return fixedLeaseID, nil
	}
	if method == methodNone {
		return "", nil
	}

	username, password, err := readCredentials(conn)
	if err != nil {
		return "", err
	}
	if username != "" {
		if len(username) > 6 && username[:6] == "lease:" {
			return username[6:], nil
		}
		return "user:" + username, nil
	}
	if password != "" {
		return "password:" + password, nil
	}
	return "", errors.New("empty SOCKS credentials")
}

func readCredentials(conn net.Conn) (string, string, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", "", fmt.Errorf("read authentication header: %w", err)
	}
	if header[0] != 0x01 || header[1] == 0 {
		return "", "", errors.New("invalid username/password authentication")
	}
	username := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, username); err != nil {
		return "", "", fmt.Errorf("read username: %w", err)
	}
	length := []byte{0}
	if _, err := io.ReadFull(conn, length); err != nil {
		return "", "", fmt.Errorf("read password length: %w", err)
	}
	password := make([]byte, int(length[0]))
	if _, err := io.ReadFull(conn, password); err != nil {
		return "", "", fmt.Errorf("read password: %w", err)
	}
	if _, err := conn.Write([]byte{0x01, 0x00}); err != nil {
		return "", "", fmt.Errorf("write authentication result: %w", err)
	}
	return string(username), string(password), nil
}

func readConnectRequest(conn net.Conn) (string, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", fmt.Errorf("read SOCKS request: %w", err)
	}
	if header[0] != version5 || header[1] != commandConnect || header[2] != 0 {
		_ = writeReply(conn, 0x07, nil)
		return "", errors.New("unsupported SOCKS request")
	}

	var host string
	switch header[3] {
	case addressIPv4:
		address := make([]byte, net.IPv4len)
		if _, err := io.ReadFull(conn, address); err != nil {
			return "", fmt.Errorf("read IPv4 destination: %w", err)
		}
		host = net.IP(address).String()
	case addressIPv6:
		address := make([]byte, net.IPv6len)
		if _, err := io.ReadFull(conn, address); err != nil {
			return "", fmt.Errorf("read IPv6 destination: %w", err)
		}
		host = net.IP(address).String()
	case addressDomain:
		length := []byte{0}
		if _, err := io.ReadFull(conn, length); err != nil {
			return "", fmt.Errorf("read domain length: %w", err)
		}
		if length[0] == 0 {
			return "", errors.New("empty SOCKS destination domain")
		}
		name := make([]byte, int(length[0]))
		if _, err := io.ReadFull(conn, name); err != nil {
			return "", fmt.Errorf("read destination domain: %w", err)
		}
		host = string(name)
	default:
		_ = writeReply(conn, 0x08, nil)
		return "", errors.New("unsupported SOCKS address type")
	}

	portBytes := make([]byte, 2)
	if _, err := io.ReadFull(conn, portBytes); err != nil {
		return "", fmt.Errorf("read destination port: %w", err)
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(portBytes)))), nil
}

func writeReply(conn net.Conn, reply byte, address net.Addr) error {
	ip := net.IPv4zero
	port := 0
	if tcpAddress, ok := address.(*net.TCPAddr); ok {
		ip = tcpAddress.IP
		port = tcpAddress.Port
	}

	response := []byte{version5, reply, 0x00}
	if ipv4 := ip.To4(); ipv4 != nil {
		response = append(response, addressIPv4)
		response = append(response, ipv4...)
	} else {
		response = append(response, addressIPv6)
		response = append(response, ip.To16()...)
	}
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, uint16(port))
	response = append(response, portBytes...)
	if _, err := conn.Write(response); err != nil {
		return fmt.Errorf("write SOCKS reply: %w", err)
	}
	return nil
}

func relay(client, upstream net.Conn) error {
	result := make(chan error, 2)
	copyConnection := func(destination, source net.Conn) {
		_, err := io.Copy(destination, source)
		if closeWriter, ok := destination.(interface{ CloseWrite() error }); ok {
			_ = closeWriter.CloseWrite()
		}
		result <- err
	}
	go copyConnection(upstream, client)
	go copyConnection(client, upstream)
	first := <-result
	second := <-result
	if first != nil && !errors.Is(first, net.ErrClosed) {
		return first
	}
	if second != nil && !errors.Is(second, net.ErrClosed) {
		return second
	}
	return nil
}

func containsMethod(methods []byte, wanted byte) bool {
	for _, method := range methods {
		if method == wanted {
			return true
		}
	}
	return false
}
