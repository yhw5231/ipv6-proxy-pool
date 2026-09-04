package socks5

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"

	"ipv6-proxy-pool/internal/lease"
)

type recordingPool struct {
	acquiredID   string
	acquiredPort int
	persistent   bool
	recordedID   string
	selected     lease.Lease
}

func (p *recordingPool) Acquire(id string, persistent bool) (lease.Lease, error) {
	p.acquiredID = id
	p.persistent = persistent
	return p.selected, nil
}

func (p *recordingPool) AcquirePort(id string, port int, persistent bool) (lease.Lease, error) {
	p.acquiredID = id
	p.acquiredPort = port
	p.persistent = persistent
	return p.selected, nil
}

func (p *recordingPool) RecordRequest(id string) (lease.Lease, error) {
	p.recordedID = id
	return p.selected, nil
}

func TestServeConnSelectsLeaseFromUsernameAndBindsIPv6(t *testing.T) {
	pool := &recordingPool{selected: lease.Lease{ID: "alice", IPv6: "2001:db8::42"}}
	server := NewServer(pool)

	var gotNetwork string
	var gotAddress string
	var gotLocalIP net.IP
	server.Dial = func(_ context.Context, network, address string, localIP net.IP) (net.Conn, error) {
		gotNetwork = network
		gotAddress = address
		gotLocalIP = append(net.IP(nil), localIP...)
		client, upstream := net.Pipe()
		go upstream.Close()
		return client, nil
	}

	client, proxy := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- server.ServeConn(context.Background(), proxy, "", 0)
	}()

	if _, err := client.Write([]byte{version5, 1, methodPassword}); err != nil {
		t.Fatalf("write greeting: %v", err)
	}
	methodReply := make([]byte, 2)
	if _, err := io.ReadFull(client, methodReply); err != nil {
		t.Fatalf("read method reply: %v", err)
	}
	if methodReply[0] != version5 || methodReply[1] != methodPassword {
		t.Fatalf("method reply = %v", methodReply)
	}

	username := []byte("alice")
	password := []byte("secret")
	auth := []byte{1, byte(len(username))}
	auth = append(auth, username...)
	auth = append(auth, byte(len(password)))
	auth = append(auth, password...)
	if _, err := client.Write(auth); err != nil {
		t.Fatalf("write authentication: %v", err)
	}
	authReply := make([]byte, 2)
	if _, err := io.ReadFull(client, authReply); err != nil {
		t.Fatalf("read authentication reply: %v", err)
	}
	if authReply[1] != 0 {
		t.Fatalf("authentication reply = %v", authReply)
	}

	domain := []byte("example.test")
	request := []byte{version5, commandConnect, 0, addressDomain, byte(len(domain))}
	request = append(request, domain...)
	port := make([]byte, 2)
	binary.BigEndian.PutUint16(port, 443)
	request = append(request, port...)
	if _, err := client.Write(request); err != nil {
		t.Fatalf("write CONNECT request: %v", err)
	}

	reply := make([]byte, 10)
	if _, err := io.ReadFull(client, reply); err != nil {
		t.Fatalf("read CONNECT reply: %v", err)
	}
	if reply[1] != 0 {
		t.Fatalf("CONNECT reply code = %d", reply[1])
	}

	_ = client.Close()
	if err := <-done; err != nil {
		t.Fatalf("ServeConn: %v", err)
	}
	if pool.acquiredID != "user:alice" {
		t.Fatalf("acquired identity = %q, want user:alice", pool.acquiredID)
	}
	if pool.persistent {
		t.Fatal("multiplexed lease was marked persistent")
	}
	if pool.recordedID != "user:alice" {
		t.Fatalf("recorded identity = %q, want user:alice", pool.recordedID)
	}
	if gotNetwork != "tcp" || gotAddress != "example.test:443" {
		t.Fatalf("dial = %q %q", gotNetwork, gotAddress)
	}
	if !gotLocalIP.Equal(net.ParseIP("2001:db8::42")) {
		t.Fatalf("bound local IP = %s", gotLocalIP)
	}
}

func TestServeConnFixedLeaseReacquiresListenerPort(t *testing.T) {
	pool := &recordingPool{selected: lease.Lease{ID: "port-20000", IPv6: "2001:db8::7"}}
	server := NewServer(pool)
	server.Dial = func(_ context.Context, _, _ string, _ net.IP) (net.Conn, error) {
		client, upstream := net.Pipe()
		go upstream.Close()
		return client, nil
	}

	client, proxy := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- server.ServeConn(context.Background(), proxy, "port-20000", 20000)
	}()

	_, _ = client.Write([]byte{version5, 1, methodNone})
	methodReply := make([]byte, 2)
	_, _ = io.ReadFull(client, methodReply)
	request := []byte{version5, commandConnect, 0, addressIPv4, 127, 0, 0, 1, 0, 80}
	_, _ = client.Write(request)
	reply := make([]byte, 10)
	_, _ = io.ReadFull(client, reply)
	_ = client.Close()
	_ = <-done

	if pool.acquiredID != "port-20000" || pool.acquiredPort != 20000 {
		t.Fatalf("AcquirePort called with id=%q port=%d", pool.acquiredID, pool.acquiredPort)
	}
	if pool.persistent {
		t.Fatal("fixed lease was forced persistent; dynamic leases must stay idle-releasable")
	}
}

func TestServeConnFixedLeaseRejectsMissingPort(t *testing.T) {
	pool := &recordingPool{}
	server := NewServer(pool)
	client, proxy := net.Pipe()
	defer client.Close()
	done := make(chan error, 1)
	go func() {
		done <- server.ServeConn(context.Background(), proxy, "port-20000", 0)
	}()
	_, _ = client.Write([]byte{version5, 1, methodNone})
	methodReply := make([]byte, 2)
	_, _ = io.ReadFull(client, methodReply)
	if err := <-done; err == nil {
		t.Fatal("fixed lease without a port did not fail")
	}
}
