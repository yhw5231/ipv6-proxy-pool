package socks5

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"ipv6-proxy-pool/internal/lease"
)

// startEgressServer runs a local fake ipify-style HTTP endpoint that answers
// with the given IP address as plain text.
func startEgressServer(t *testing.T, address string) (addr string, close func()) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen egress: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				// 读完请求头（HTTP/1.0 GET 无请求体）即可响应；不能等 EOF，
				// 因为客户端会保持连接等待响应。
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if line == "\r\n" || line == "\n" {
						break
					}
				}
				_, _ = conn.Write([]byte("HTTP/1.0 200 OK\r\nConnection: close\r\n\r\n" + address + "\n"))
			}()
		}
	}()
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	return listener.Addr().String(), func() { cancel(); _ = listener.Close() }
}

// startProbeServer runs a real SOCKS5 server bound to loopback. Its dialer
// ignores the lease source address and connects straight to the target, so
// the tests work without any IPv6 routing.
func startProbeServer(t *testing.T, pool Pool) (addr string, close func()) {
	t.Helper()
	proxy := NewServer(pool)
	proxy.Dial = func(ctx context.Context, network, target string, _ net.IP) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, target)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen proxy: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = proxy.Serve(ctx, listener, "") }()
	return listener.Addr().String(), func() { cancel(); _ = listener.Close() }
}

func newProbePool(t *testing.T, min, max int) *lease.Pool {
	t.Helper()
	pool, err := lease.NewPool(lease.Options{
		Prefix:    "2001:db8::/64",
		MinLeases: min,
		MaxLeases: max,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return pool
}

func TestProbeProxyHandshakeOnly(t *testing.T) {
	pool := newProbePool(t, 0, 4)
	proxyAddr, close := startProbeServer(t, pool)
	defer close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exit, err := ProbeProxy(ctx, proxyAddr, "", "")
	if err != nil {
		t.Fatalf("ProbeProxy handshake-only: %v", err)
	}
	if exit != "" {
		t.Fatalf("exit = %q, want empty for handshake-only probe", exit)
	}
}

func TestProbeProxyEgressReturnsExitIP(t *testing.T) {
	pool := newProbePool(t, 0, 4)
	proxyAddr, closeProxy := startProbeServer(t, pool)
	defer closeProxy()

	const observed = "2001:db8:dead::1"
	egressAddr, closeEgress := startEgressServer(t, observed)
	defer closeEgress()

	egressURL := "http://" + egressAddr
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exit, err := ProbeProxy(ctx, proxyAddr, egressURL, "")
	if err != nil {
		t.Fatalf("ProbeProxy egress: %v", err)
	}
	if exit != observed {
		t.Fatalf("exit = %q, want %q", exit, observed)
	}
}

func TestProbeProxyMultiplexTargetsNamedLease(t *testing.T) {
	pool := newProbePool(t, 0, 4)
	if _, err := pool.Acquire("client-x", false); err != nil {
		t.Fatalf("Acquire client-x: %v", err)
	}
	proxyAddr, close := startProbeServer(t, pool)
	defer close()

	// 以 lease:client-x 身份探测：不应新建任何租约（池内仍只有 client-x 与备用）。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := ProbeProxy(ctx, proxyAddr, "", "client-x"); err != nil {
		t.Fatalf("ProbeProxy with lease id: %v", err)
	}
	entries := pool.ListAll()
	for _, item := range entries {
		if item.Role == "client" && item.ID != "client-x" {
			t.Fatalf("probe minted an unexpected client lease %q", item.ID)
		}
	}

	// 不带身份的握手探测会以探测连接地址为身份创建租约（multiplex 语义），
	// 这也是为什么测试按钮总是显式指定租约。
	if _, err := ProbeProxy(ctx, proxyAddr, "", ""); err != nil {
		t.Fatalf("ProbeProxy anonymous handshake: %v", err)
	}
	clients := 0
	for _, item := range pool.ListAll() {
		if item.Role == "client" && item.ID != "client-x" {
			clients++
		}
	}
	if clients != 1 {
		t.Fatalf("anonymous probe created %d client lease(s), want 1", clients)
	}
}

func TestProbeProxyReportsUnreachableProxy(t *testing.T) {
	pool := newProbePool(t, 0, 4)
	proxyAddr, close := startProbeServer(t, pool)
	close() // 先关闭，探测应失败。

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := ProbeProxy(ctx, proxyAddr, "", ""); err == nil {
		t.Fatal("ProbeProxy succeeded against a closed proxy")
	} else if !strings.Contains(err.Error(), "connect to proxy") {
		t.Fatalf("unexpected error: %v", err)
	}
}
