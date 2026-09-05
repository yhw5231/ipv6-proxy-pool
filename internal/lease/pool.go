package lease

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"ipv6-proxy-pool/internal/ipv6addr"
)

var (
	ErrCapacity        = errors.New("lease pool capacity reached")
	ErrPortUnavailable = errors.New("lease port unavailable")
	ErrNotFound        = errors.New("lease not found")
	ErrPersistent      = errors.New("lease is persistent")
)

// Lease roles. "standby" leases form the resident floor (常驻保底): they hold a
// port and an unused IPv6 and are handed to clients on request. "client" leases
// are assigned to a concrete client identity.
const (
	RoleClient  = "client"
	RoleStandby = "standby"
)

// Options controls lease allocation, expiration, rotation, and optional ports.
type Options struct {
	Prefix string
	// MinLeases is the resident standby floor. NewPool seeds this many standby
	// leases, and any release below the floor converts the released lease into
	// a fresh standby instead of destroying it.
	MinLeases      int
	MaxLeases      int
	IdleTimeout    time.Duration
	RotateAfter    time.Duration
	RotateRequests uint64
	PortStart      int
	PortEnd        int
	Now            func() time.Time
	// OnRelease is invoked after a lease has been removed from the pool for any
	// reason (explicit release, idle release, recycle). It lets callers tear
	// down resources attached to the lease, such as per-IPv6 listeners. It must
	// not block on pool methods; it is called without the pool mutex held. A
	// nil value disables the callback.
	OnRelease func(id string)
}

// Lease is an immutable snapshot of a pool lease.
type Lease struct {
	ID         string    `json:"id"`
	IPv6       string    `json:"ipv6"`
	Port       int       `json:"port,omitempty"`
	Persistent bool      `json:"persistent"`
	Role       string    `json:"role"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	Requests   uint64    `json:"requests"`
}

type entry struct {
	Lease
	generation uint64
}

// Pool is a concurrency-safe collection of identity-keyed IPv6 leases with a
// resident standby floor. Standbys make client allocation instant and keep the
// pool self-healing: a released lease becomes a fresh standby (new IPv6) while
// the standby count is below the floor, and is destroyed once the floor is
// satisfied.
type Pool struct {
	mu           sync.Mutex
	generator    *ipv6addr.Generator
	options      Options
	leases       map[string]*entry
	nextIndex    uint64
	freePorts    []int
	standbyCount int
	standbySeq   uint64
}

// NewPool constructs a lease pool and seeds the resident standby floor.
func NewPool(options Options) (*Pool, error) {
	if options.MaxLeases <= 0 {
		return nil, errors.New("max leases must be greater than zero")
	}
	if options.MinLeases < 0 || options.MinLeases > options.MaxLeases {
		return nil, errors.New("min leases must be between zero and max leases")
	}
	if options.IdleTimeout < 0 || options.RotateAfter < 0 {
		return nil, errors.New("lease timeouts must not be negative")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.PortStart != 0 || options.PortEnd != 0 {
		if options.PortStart < 1 || options.PortEnd < options.PortStart || options.PortEnd > 65535 {
			return nil, errors.New("invalid lease port range")
		}
		if options.PortEnd-options.PortStart+1 < options.MaxLeases {
			return nil, errors.New("lease port range is smaller than max leases")
		}
	}

	generator, err := ipv6addr.NewGenerator(options.Prefix)
	if err != nil {
		return nil, err
	}

	pool := &Pool{
		generator: generator,
		options:   options,
		leases:    make(map[string]*entry),
	}
	for port := options.PortStart; port != 0 && port <= options.PortEnd; port++ {
		pool.freePorts = append(pool.freePorts, port)
	}
	pool.ensureStandbysLocked(options.Now())
	return pool, nil
}

// notifyRelease runs the configured OnRelease callback for each released lease
// id. It must be called without the pool mutex held.
func (p *Pool) notifyRelease(ids []string) {
	if p.options.OnRelease == nil {
		return
	}
	for _, id := range ids {
		p.options.OnRelease(id)
	}
}

// Acquire returns the current lease for id, claiming a standby, rotating, or
// creating a lease as needed. A non-empty id provides stable identity selection
// for SOCKS credentials and explicit lease identifiers.
func (p *Pool) Acquire(id string, persistent bool) (Lease, error) {
	if id == "" {
		return Lease{}, errors.New("lease id must not be empty")
	}

	var released []string
	p.mu.Lock()
	defer func() {
		p.mu.Unlock()
		p.notifyRelease(released)
	}()

	now := p.options.Now()
	released = p.releaseIdleLocked(now)

	if current, ok := p.leases[id]; ok {
		if current.Role == RoleStandby {
			p.claimLocked(current, id, persistent, now)
		} else if persistent {
			current.Persistent = true
		}
		if p.shouldRotateLocked(current, now) {
			if err := p.rotateLocked(current, now); err != nil {
				return Lease{}, err
			}
		}
		current.LastUsedAt = now
		return current.Lease, nil
	}

	if len(p.leases) >= p.options.MaxLeases {
		return Lease{}, ErrCapacity
	}

	if standby := p.takeStandbyLocked(); standby != nil {
		p.claimLocked(standby, id, persistent, now)
		return standby.Lease, nil
	}

	created, err := p.newEntryLocked(id, RoleClient, persistent, now)
	if err != nil {
		return Lease{}, err
	}
	p.leases[id] = created
	return created.Lease, nil
}

// AcquirePort returns the current lease for id or creates a lease using the
// requested port. The port must be available within the configured range. A
// standby already holding the requested port is claimed instead.
func (p *Pool) AcquirePort(id string, port int, persistent bool) (Lease, error) {
	if id == "" {
		return Lease{}, errors.New("lease id must not be empty")
	}

	var released []string
	p.mu.Lock()
	defer func() {
		p.mu.Unlock()
		p.notifyRelease(released)
	}()

	now := p.options.Now()
	released = p.releaseIdleLocked(now)

	if current, ok := p.leases[id]; ok {
		if current.Port != port {
			return Lease{}, fmt.Errorf("%w: port %d is not assigned to lease %q", ErrPortUnavailable, port, id)
		}
		if current.Role == RoleStandby {
			p.claimLocked(current, id, persistent, now)
		} else if persistent {
			current.Persistent = true
		}
		if p.shouldRotateLocked(current, now) {
			if err := p.rotateLocked(current, now); err != nil {
				return Lease{}, err
			}
		}
		current.LastUsedAt = now
		return current.Lease, nil
	}

	if len(p.leases) >= p.options.MaxLeases {
		return Lease{}, ErrCapacity
	}

	if standby := p.takeStandbyOnPortLocked(port); standby != nil {
		p.claimLocked(standby, id, persistent, now)
		return standby.Lease, nil
	}

	portIndex := sort.SearchInts(p.freePorts, port)
	if portIndex >= len(p.freePorts) || p.freePorts[portIndex] != port {
		return Lease{}, fmt.Errorf("%w: %d", ErrPortUnavailable, port)
	}

	ip, err := p.nextAddressLocked()
	if err != nil {
		return Lease{}, err
	}
	created := &entry{Lease: Lease{
		ID:         id,
		IPv6:       ip.String(),
		Port:       port,
		Persistent: persistent,
		Role:       RoleClient,
		CreatedAt:  now,
		LastUsedAt: now,
	}}
	p.freePorts = append(p.freePorts[:portIndex], p.freePorts[portIndex+1:]...)
	p.leases[id] = created
	return created.Lease, nil
}

// RecordRequest increments usage after a successful proxy request and rotates
// immediately when the configured request threshold is reached.
func (p *Pool) RecordRequest(id string) (Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	current, ok := p.leases[id]
	if !ok {
		return Lease{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}

	now := p.options.Now()
	current.Requests++
	current.LastUsedAt = now
	if p.options.RotateRequests > 0 && current.Requests >= p.options.RotateRequests {
		if err := p.rotateLocked(current, now); err != nil {
			return Lease{}, err
		}
	}
	return current.Lease, nil
}

// Get returns a lease snapshot without updating its activity time.
func (p *Pool) Get(id string) (Lease, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	current, ok := p.leases[id]
	if !ok {
		return Lease{}, false
	}
	return current.Lease, true
}

// List returns client lease snapshots sorted by lease identifier. Standby
// leases are excluded; use ListAll to include them.
func (p *Pool) List() []Lease {
	return p.listLocked(false)
}

// ListAll returns every lease snapshot, including resident standbys.
func (p *Pool) ListAll() []Lease {
	return p.listLocked(true)
}

func (p *Pool) listLocked(includeStandby bool) []Lease {
	p.mu.Lock()
	defer p.mu.Unlock()

	result := make([]Lease, 0, len(p.leases))
	for _, current := range p.leases {
		if !includeStandby && current.Role == RoleStandby {
			continue
		}
		result = append(result, current.Lease)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

// StandbyCount reports how many resident standby leases are currently
// unassigned and ready for clients.
func (p *Pool) StandbyCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.standbyCount
}

// Release recycles a lease. While the standby count is below the floor the
// lease becomes a fresh standby (new IPv6, same port); otherwise it is removed
// and its port returns to the free pool.
func (p *Pool) Release(id string) bool {
	p.mu.Lock()
	recycled := p.recycleLocked(id, p.options.Now())
	p.mu.Unlock()
	if recycled {
		p.notifyRelease([]string{id})
	}
	return recycled
}

// ReleaseIdle recycles non-persistent client leases that exceeded IdleTimeout.
// It only runs while the total lease count exceeds the resident floor, so the
// standing leases within the floor are never reclaimed for idleness.
func (p *Pool) ReleaseIdle() int {
	var released []string
	p.mu.Lock()
	released = p.releaseIdleLocked(p.options.Now())
	p.mu.Unlock()
	p.notifyRelease(released)
	return len(released)
}

// Rotate forces the lease to a fresh IPv6 address without changing its port.
// This lets a client explicitly request a new source IP ("更换IP").
func (p *Pool) Rotate(id string) (Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	current, ok := p.leases[id]
	if !ok {
		return Lease{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	if err := p.rotateLocked(current, p.options.Now()); err != nil {
		return Lease{}, err
	}
	return current.Lease, nil
}

// Recycle releases a client lease and immediately re-acquires a lease under the
// same id ("释放并自动重新获取"): the old lease returns to the standby pool and
// the client is handed a slot with a different port and a fresh IPv6. Persistent
// leases are rejected because they are anchored to their always-on port.
func (p *Pool) Recycle(id string) (Lease, error) {
	p.mu.Lock()
	current, ok := p.leases[id]
	if !ok {
		p.mu.Unlock()
		return Lease{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	if current.Persistent {
		p.mu.Unlock()
		return Lease{}, fmt.Errorf("%w: %q; use rotate instead", ErrPersistent, id)
	}
	p.recycleLocked(id, p.options.Now())
	p.mu.Unlock()

	p.notifyRelease([]string{id})

	return p.Acquire(id, false)
}

// SetPersistent changes whether a lease is exempt from idle release.
func (p *Pool) SetPersistent(id string, persistent bool) (Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	current, ok := p.leases[id]
	if !ok {
		return Lease{}, fmt.Errorf("%w: %q", ErrNotFound, id)
	}
	current.Persistent = persistent
	return current.Lease, nil
}

// claimLocked converts a standby lease into a client lease under a new id. The
// port and the (still unused) IPv6 are kept, so allocation is instant. The
// standby must already have been removed from the lease map by the caller.
func (p *Pool) claimLocked(standby *entry, id string, persistent bool, now time.Time) {
	p.standbyCount--
	standby.ID = id
	standby.Role = RoleClient
	standby.Persistent = persistent
	standby.CreatedAt = now
	standby.LastUsedAt = now
	standby.Requests = 0
	standby.generation++
	p.leases[id] = standby
}

// takeStandbyLocked removes and returns an arbitrary standby lease, preferring
// the lowest port so per-IPv6 slots are handed out in order.
func (p *Pool) takeStandbyLocked() *entry {
	var chosen *entry
	for _, current := range p.leases {
		if current.Role != RoleStandby {
			continue
		}
		if chosen == nil || current.Port < chosen.Port ||
			(current.Port == chosen.Port && current.ID < chosen.ID) {
			chosen = current
		}
	}
	if chosen == nil {
		return nil
	}
	delete(p.leases, chosen.ID)
	return chosen
}

// takeStandbyOnPortLocked removes and returns the standby holding the given
// port, if any.
func (p *Pool) takeStandbyOnPortLocked(port int) *entry {
	for id, current := range p.leases {
		if current.Role == RoleStandby && current.Port == port {
			delete(p.leases, id)
			return current
		}
	}
	return nil
}

// recycleLocked removes the lease with id and keeps the resident floor filled:
// while standbys are short the lease is converted into a fresh standby (new
// IPv6, port kept), otherwise it is destroyed. Reports whether the lease
// existed.
func (p *Pool) recycleLocked(id string, now time.Time) bool {
	current, ok := p.leases[id]
	if !ok {
		return false
	}
	if current.Role != RoleStandby && p.standbyCount < p.options.MinLeases {
		if ip, err := p.nextAddressLocked(); err == nil {
			delete(p.leases, id)
			p.standbySeq++
			standby := &entry{Lease: Lease{
				ID:         fmt.Sprintf("pool-%d", p.standbySeq),
				IPv6:       ip.String(),
				Port:       current.Port,
				Role:       RoleStandby,
				CreatedAt:  now,
				LastUsedAt: now,
			}}
			p.leases[standby.ID] = standby
			p.standbyCount++
			return true
		}
	}
	p.releaseLocked(id)
	p.ensureStandbysLocked(now)
	return true
}

// releaseIdleLocked recycles idle client leases once the pool exceeds the
// resident floor. Standby and persistent leases are always exempt.
func (p *Pool) releaseIdleLocked(now time.Time) []string {
	if p.options.IdleTimeout <= 0 {
		return nil
	}
	if len(p.leases) <= p.options.MinLeases {
		return nil
	}
	released := make([]string, 0)
	for id, current := range p.leases {
		if current.Role == RoleStandby || current.Persistent {
			continue
		}
		if now.Sub(current.LastUsedAt) >= p.options.IdleTimeout {
			if p.recycleLocked(id, now) {
				released = append(released, id)
			}
		}
	}
	return released
}

// ensureStandbysLocked tops the standby pool up to the configured floor. It is
// best effort: in per_ipv6 mode it stops when no free port remains, and it
// stops if the prefix cannot yield a fresh address.
func (p *Pool) ensureStandbysLocked(now time.Time) {
	for p.standbyCount < p.options.MinLeases {
		if !p.newStandbyLocked(now) {
			return
		}
	}
}

// newStandbyLocked creates one standby lease and reports success.
func (p *Pool) newStandbyLocked(now time.Time) bool {
	if p.options.PortStart != 0 && len(p.freePorts) == 0 {
		return false
	}
	ip, err := p.nextAddressLocked()
	if err != nil {
		return false
	}
	port := 0
	if p.options.PortStart != 0 {
		port = p.freePorts[0]
		p.freePorts = p.freePorts[1:]
	}
	p.standbySeq++
	standby := &entry{Lease: Lease{
		ID:         fmt.Sprintf("pool-%d", p.standbySeq),
		IPv6:       ip.String(),
		Port:       port,
		Role:       RoleStandby,
		CreatedAt:  now,
		LastUsedAt: now,
	}}
	p.leases[standby.ID] = standby
	p.standbyCount++
	return true
}

func (p *Pool) newEntryLocked(id string, role string, persistent bool, now time.Time) (*entry, error) {
	ip, err := p.nextAddressLocked()
	if err != nil {
		return nil, err
	}
	port := 0
	if len(p.freePorts) > 0 {
		port = p.freePorts[0]
		p.freePorts = p.freePorts[1:]
	}
	return &entry{Lease: Lease{
		ID:         id,
		IPv6:       ip.String(),
		Port:       port,
		Persistent: persistent,
		Role:       role,
		CreatedAt:  now,
		LastUsedAt: now,
	}}, nil
}

func (p *Pool) nextAddressLocked() (net.IP, error) {
	for attempts := 0; attempts <= p.options.MaxLeases; attempts++ {
		p.nextIndex++
		candidate, err := p.generator.FromIndex(p.nextIndex)
		if err != nil {
			return nil, err
		}
		used := false
		for _, current := range p.leases {
			if current.IPv6 == candidate.String() {
				used = true
				break
			}
		}
		if !used {
			return candidate, nil
		}
	}
	return nil, ErrCapacity
}

func (p *Pool) shouldRotateLocked(current *entry, now time.Time) bool {
	return p.options.RotateAfter > 0 && now.Sub(current.CreatedAt) >= p.options.RotateAfter
}

func (p *Pool) rotateLocked(current *entry, now time.Time) error {
	ip, err := p.nextAddressLocked()
	if err != nil {
		return err
	}
	current.IPv6 = ip.String()
	current.CreatedAt = now
	current.LastUsedAt = now
	current.Requests = 0
	current.generation++
	return nil
}

// Reassign switches every lease (clients, standbys and always-on ports) to an
// address derived from the new prefix. IDs, ports, persistence and roles stay
// unchanged, so listeners bound to host:port keep working and the whole pool
// moves to the new prefix without a restart. Allocation is planned against a
// local copy first, so an invalid or too-small prefix leaves the pool
// untouched.
func (p *Pool) Reassign(prefix string) error {
	generator, err := ipv6addr.NewGenerator(prefix)
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	// Plan the new addresses first. The current addresses stay reserved so a
	// new prefix overlapping the old one (shared base, larger mask) cannot
	// hand out an address that is still in use.
	allocated := make(map[string]struct{}, len(p.leases))
	for _, current := range p.leases {
		allocated[current.IPv6] = struct{}{}
	}
	plan := make(map[string]string, len(p.leases))
	next := uint64(0)
	for _, current := range p.leases {
		var candidate net.IP
		for attempts := 0; ; attempts++ {
			if attempts > p.options.MaxLeases {
				return ErrCapacity
			}
			next++
			candidate, err = generator.FromIndex(next)
			if err != nil {
				return err
			}
			key := candidate.String()
			if _, taken := allocated[key]; taken {
				continue
			}
			allocated[key] = struct{}{}
			break
		}
		plan[current.ID] = candidate.String()
	}

	p.generator = generator
	p.options.Prefix = prefix
	p.nextIndex = next
	now := p.options.Now()
	for id, address := range plan {
		current := p.leases[id]
		current.IPv6 = address
		current.generation++
		if current.Role == RoleClient {
			current.CreatedAt = now
			current.LastUsedAt = now
			current.Requests = 0
		}
	}
	p.ensureStandbysLocked(now)
	return nil
}

func (p *Pool) releaseLocked(id string) bool {
	current, ok := p.leases[id]
	if !ok {
		return false
	}
	if current.Role == RoleStandby {
		p.standbyCount--
	}
	delete(p.leases, id)
	if current.Port != 0 {
		p.freePorts = append(p.freePorts, current.Port)
		sort.Ints(p.freePorts)
	}
	return true
}
