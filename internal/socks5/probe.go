package socks5

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// ProbeProxy verifies a live proxy endpoint end to end:
//
//  1. TCP-connects to proxyAddr and completes the SOCKS5 greeting. When
//     leaseID is set, the probe offers USERPASS and identifies as
//     "lease:<id>", so multiplex mode targets that lease instead of minting a
//     new one from the probe's remote address; per-IPv6 fixed leases take the
//     NO_AUTH path.
//  2. When egressURL is non-empty (http scheme only), issues a CONNECT to the
//     egress host and performs a plain HTTP GET. The response body is parsed
//     as the observed exit IPv6 address (ipify-style services return the raw
//     address) and returned; with an empty egressURL the probe stops after
//     the handshake, which already exercises lease acquisition.
//
// ctx controls the whole probe; its deadline, when present, is applied to the
// connection.
func ProbeProxy(ctx context.Context, proxyAddr, egressURL, leaseID string) (exitIPv6 string, err error) {
	if proxyAddr == "" {
		return "", errors.New("proxy address must not be empty")
	}

	egressHost, egressPort, err := parseEgress(egressURL)
	if err != nil {
		return "", err
	}

	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return "", fmt.Errorf("connect to proxy %s: %w", proxyAddr, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	// Greeting: NO_AUTH always, plus USERPASS when a specific lease is wanted.
	methods := []byte{methodNone}
	if leaseID != "" {
		methods = append(methods, methodPassword)
	}
	greeting := []byte{version5, byte(len(methods))}
	greeting = append(greeting, methods...)
	if err := writeAll(conn, greeting); err != nil {
		return "", err
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return "", fmt.Errorf("read SOCKS method reply: %w", err)
	}
	if reply[0] != version5 || reply[1] == methodRejected {
		return "", fmt.Errorf("proxy rejected the handshake (method %#x)", reply[1])
	}
	if leaseID != "" && reply[1] == methodPassword {
		username := []byte("lease:" + leaseID)
		auth := []byte{0x01, byte(len(username))}
		auth = append(auth, username...)
		auth = append(auth, 0x00)
		if err := writeAll(conn, auth); err != nil {
			return "", err
		}
		status := make([]byte, 2)
		if _, err := io.ReadFull(conn, status); err != nil {
			return "", fmt.Errorf("read SOCKS auth reply: %w", err)
		}
		if status[1] != 0x00 {
			return "", fmt.Errorf("proxy rejected credentials for lease %q", leaseID)
		}
	}
	if egressHost == "" {
		return "", nil // handshake-only probe
	}

	request := []byte{version5, commandConnect, 0x00, addressDomain, byte(len(egressHost))}
	request = append(request, []byte(egressHost)...)
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, uint16(egressPort))
	request = append(request, portBytes...)
	if err := writeAll(conn, request); err != nil {
		return "", err
	}

	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		return "", fmt.Errorf("read SOCKS connect reply: %w", err)
	}
	if header[1] != 0x00 {
		return "", fmt.Errorf("egress connect to %s failed (reply %#x)", net.JoinHostPort(egressHost, strconv.Itoa(egressPort)), header[1])
	}
	if err := drainReplyAddress(conn, header[3]); err != nil {
		return "", err
	}

	httpRequest := "GET / HTTP/1.0\r\nHost: " + egressHost + "\r\nConnection: close\r\n\r\n"
	if err := writeAll(conn, []byte(httpRequest)); err != nil {
		return "", err
	}
	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read egress HTTP response: %w", err)
	}
	if !strings.HasPrefix(statusLine, "HTTP/") || len(statusLine) < 12 || statusLine[9] != '2' {
		return "", fmt.Errorf("egress HTTP status line: %s", strings.TrimSpace(statusLine))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("read egress HTTP headers: %w", err)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		return "", fmt.Errorf("read egress response body: %w", err)
	}
	address := net.ParseIP(strings.TrimSpace(string(body)))
	if address == nil {
		return "", fmt.Errorf("egress response is not an IP address: %q", strings.TrimSpace(string(body)))
	}
	return address.String(), nil
}

// parseEgress extracts the host and port from an optional egress URL. Only
// plain http is supported because the probe speaks raw HTTP; an empty URL
// means handshake-only probing.
func parseEgress(egressURL string) (host string, port int, err error) {
	if strings.TrimSpace(egressURL) == "" {
		return "", 0, nil
	}
	parsed, err := url.Parse(egressURL)
	if err != nil {
		return "", 0, fmt.Errorf("parse egress URL: %w", err)
	}
	if parsed.Scheme != "http" {
		return "", 0, errors.New("egress URL must use the http scheme")
	}
	host = parsed.Hostname()
	if host == "" {
		return "", 0, errors.New("egress URL has no host")
	}
	port = 80
	if parsedPort := parsed.Port(); parsedPort != "" {
		port, err = strconv.Atoi(parsedPort)
		if err != nil {
			return "", 0, fmt.Errorf("egress URL port: %w", err)
		}
	}
	return host, port, nil
}

// drainReplyAddress consumes the variable-length address+port tail of a
// successful SOCKS5 reply so the remaining stream is the egress data.
func drainReplyAddress(conn net.Conn, addressType byte) error {
	var length int
	switch addressType {
	case addressIPv4:
		length = net.IPv4len
	case addressIPv6:
		length = net.IPv6len
	case addressDomain:
		lengthBytes := []byte{0}
		if _, err := io.ReadFull(conn, lengthBytes); err != nil {
			return fmt.Errorf("read reply domain length: %w", err)
		}
		length = int(lengthBytes[0])
	default:
		return fmt.Errorf("unexpected reply address type %#x", addressType)
	}
	buffer := make([]byte, length+2)
	if _, err := io.ReadFull(conn, buffer); err != nil {
		return fmt.Errorf("read reply address: %w", err)
	}
	return nil
}

func writeAll(conn net.Conn, data []byte) error {
	if _, err := conn.Write(data); err != nil {
		return fmt.Errorf("write SOCKS probe data: %w", err)
	}
	return nil
}
