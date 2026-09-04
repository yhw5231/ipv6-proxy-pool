package listener

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

type recordingServer struct {
	mu       sync.Mutex
	leaseIDs []string
	started  chan struct{}
}

func (s *recordingServer) Serve(ctx context.Context, listener net.Listener, leaseID string) error {
	s.mu.Lock()
	s.leaseIDs = append(s.leaseIDs, leaseID)
	s.mu.Unlock()
	if s.started != nil {
		select {
		case s.started <- struct{}{}:
		default:
		}
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		_ = conn.Close()
	}
}

func TestManagerStartGetStop(t *testing.T) {
	server := &recordingServer{started: make(chan struct{}, 1)}
	manager := NewManager(context.Background(), server)
	t.Cleanup(func() { _ = manager.Close() })

	info, err := manager.Start("lease-1", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if info.LeaseID != "lease-1" {
		t.Fatalf("lease ID = %q, want lease-1", info.LeaseID)
	}
	if info.Address == "" {
		t.Fatal("listener address is empty")
	}

	select {
	case <-server.started:
	case <-time.After(time.Second):
		t.Fatal("Serve was not started")
	}

	got, ok := manager.Get("lease-1")
	if !ok {
		t.Fatal("Get did not find active listener")
	}
	if got != info {
		t.Fatalf("Get = %+v, want %+v", got, info)
	}

	if err := manager.Stop("lease-1"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, ok := manager.Get("lease-1"); ok {
		t.Fatal("listener remains after Stop")
	}

	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.leaseIDs) != 1 || server.leaseIDs[0] != "lease-1" {
		t.Fatalf("Serve lease IDs = %v", server.leaseIDs)
	}
}

func TestManagerRejectsDuplicateLease(t *testing.T) {
	manager := NewManager(context.Background(), &recordingServer{})
	t.Cleanup(func() { _ = manager.Close() })

	first, err := manager.Start("lease-1", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if _, err := manager.Start("lease-1", "127.0.0.1:0"); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("duplicate Start error = %v, want ErrAlreadyStarted", err)
	}
	if got, ok := manager.Get("lease-1"); !ok || got != first {
		t.Fatalf("active listener changed after duplicate Start: %+v, %v", got, ok)
	}
}

func TestManagerReportsPortInUse(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy port: %v", err)
	}
	defer occupied.Close()

	manager := NewManager(context.Background(), &recordingServer{})
	t.Cleanup(func() { _ = manager.Close() })

	if _, err := manager.Start("lease-1", occupied.Addr().String()); err == nil {
		t.Fatal("Start succeeded on occupied port")
	}
	if _, ok := manager.Get("lease-1"); ok {
		t.Fatal("failed Start registered a listener")
	}
}

func TestManagerConcurrentClose(t *testing.T) {
	manager := NewManager(context.Background(), &recordingServer{})
	for i, leaseID := range []string{"lease-1", "lease-2", "lease-3"} {
		if _, err := manager.Start(leaseID, "127.0.0.1:0"); err != nil {
			t.Fatalf("Start %d: %v", i, err)
		}
	}

	const callers = 16
	var wg sync.WaitGroup
	errorsSeen := make(chan error, callers)
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			errorsSeen <- manager.Close()
		}()
	}
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	for _, leaseID := range []string{"lease-1", "lease-2", "lease-3"} {
		if _, ok := manager.Get(leaseID); ok {
			t.Fatalf("%s remains after Close", leaseID)
		}
	}
	if _, err := manager.Start("lease-4", "127.0.0.1:0"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Close error = %v, want ErrClosed", err)
	}
}

func TestManagerContextCancellationClosesListeners(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	manager := NewManager(ctx, &recordingServer{})

	info, err := manager.Start("lease-1", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	cancel()

	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := manager.Get("lease-1"); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("listener remains after context cancellation")
		}
		time.Sleep(time.Millisecond)
	}

	connection, err := net.DialTimeout("tcp", info.Address, 50*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		t.Fatal("listener still accepts connections after context cancellation")
	}
	if _, err := manager.Start("lease-2", "127.0.0.1:0"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after cancellation error = %v, want ErrClosed", err)
	}
	if err := manager.Close(); err != nil {
		t.Fatalf("Close after cancellation: %v", err)
	}
}

func TestManagerStopMissingLease(t *testing.T) {
	manager := NewManager(context.Background(), &recordingServer{})
	t.Cleanup(func() { _ = manager.Close() })

	if err := manager.Stop("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Stop error = %v, want ErrNotFound", err)
	}
}
