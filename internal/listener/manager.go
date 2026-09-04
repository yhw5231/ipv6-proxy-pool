package listener

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
)

var (
	ErrAlreadyStarted = errors.New("listener already started")
	ErrNotFound       = errors.New("listener not found")
	ErrClosed         = errors.New("listener manager closed")
)

// Server serves SOCKS5 connections for a fixed lease on listener.
type Server interface {
	Serve(ctx context.Context, listener net.Listener, leaseID string) error
}

// Info describes a running listener.
type Info struct {
	LeaseID string
	Address string
}

type managedListener struct {
	listener net.Listener
	done     chan struct{}
}

// Manager owns dynamically created SOCKS5 listeners keyed by lease ID.
type Manager struct {
	ctx    context.Context
	cancel context.CancelFunc
	server Server

	mu        sync.RWMutex
	listeners map[string]*managedListener
	closed    bool
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// NewManager creates a listener manager and closes it when ctx expires.
func NewManager(ctx context.Context, server Server) *Manager {
	managerCtx, cancel := context.WithCancel(ctx)
	m := &Manager{
		ctx:       managerCtx,
		cancel:    cancel,
		server:    server,
		listeners: make(map[string]*managedListener),
	}
	go func() {
		<-managerCtx.Done()
		_ = m.Close()
	}()
	return m
}

// Start creates and serves a listener for leaseID. A lease can have at most one
// active listener.
func (m *Manager) Start(leaseID, address string) (Info, error) {
	if leaseID == "" {
		return Info{}, errors.New("lease ID is required")
	}
	if m.server == nil {
		return Info{}, errors.New("SOCKS5 server is required")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return Info{}, ErrClosed
	}
	if _, exists := m.listeners[leaseID]; exists {
		return Info{}, fmt.Errorf("%w: %s", ErrAlreadyStarted, leaseID)
	}

	ln, err := net.Listen("tcp", address)
	if err != nil {
		return Info{}, fmt.Errorf("listen for lease %q on %q: %w", leaseID, address, err)
	}
	entry := &managedListener{listener: ln, done: make(chan struct{})}
	m.listeners[leaseID] = entry
	m.wg.Add(1)
	go m.serve(leaseID, entry)

	return Info{LeaseID: leaseID, Address: ln.Addr().String()}, nil
}

func (m *Manager) serve(leaseID string, entry *managedListener) {
	defer m.wg.Done()
	defer close(entry.done)
	_ = m.server.Serve(m.ctx, entry.listener, leaseID)

	m.mu.Lock()
	if current, exists := m.listeners[leaseID]; exists && current == entry {
		delete(m.listeners, leaseID)
	}
	m.mu.Unlock()
}

// Get returns information about the active listener for leaseID.
func (m *Manager) Get(leaseID string) (Info, bool) {
	m.mu.RLock()
	entry, exists := m.listeners[leaseID]
	m.mu.RUnlock()
	if !exists {
		return Info{}, false
	}
	return Info{LeaseID: leaseID, Address: entry.listener.Addr().String()}, true
}

// Stop closes and waits for the listener associated with leaseID.
func (m *Manager) Stop(leaseID string) error {
	m.mu.Lock()
	entry, exists := m.listeners[leaseID]
	if exists {
		delete(m.listeners, leaseID)
	}
	m.mu.Unlock()
	if !exists {
		return fmt.Errorf("%w: %s", ErrNotFound, leaseID)
	}

	_ = entry.listener.Close()
	<-entry.done
	return nil
}

// Close closes all listeners and waits for their Serve calls to return. It is
// safe to call concurrently and more than once.
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		entries := make([]*managedListener, 0, len(m.listeners))
		for leaseID, entry := range m.listeners {
			entries = append(entries, entry)
			delete(m.listeners, leaseID)
		}
		m.mu.Unlock()

		m.cancel()
		for _, entry := range entries {
			_ = entry.listener.Close()
		}
		m.wg.Wait()
	})
	return nil
}
