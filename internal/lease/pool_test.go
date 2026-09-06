package lease

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPoolCapacityAndPortReuse(t *testing.T) {
	now := time.Date(2026, 8, 26, 9, 30, 0, 0, time.UTC)
	pool, err := NewPool(Options{
		Prefix:    "2001:db8::/64",
		MaxLeases: 2,
		PortStart: 20000,
		PortEnd:   20001,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	first, err := pool.Acquire("first", false)
	if err != nil {
		t.Fatalf("Acquire first: %v", err)
	}
	second, err := pool.Acquire("second", false)
	if err != nil {
		t.Fatalf("Acquire second: %v", err)
	}
	if first.Port != 20000 || second.Port != 20001 {
		t.Fatalf("ports = %d, %d; want 20000, 20001", first.Port, second.Port)
	}
	if _, err := pool.Acquire("third", false); !errors.Is(err, ErrCapacity) {
		t.Fatalf("Acquire above capacity error = %v, want ErrCapacity", err)
	}

	if !pool.Release("first") {
		t.Fatal("Release(first) returned false")
	}
	third, err := pool.Acquire("third", false)
	if err != nil {
		t.Fatalf("Acquire third after release: %v", err)
	}
	if third.Port != 20000 {
		t.Fatalf("reused port = %d, want 20000", third.Port)
	}
}

func TestPoolSeedsResidentStandbys(t *testing.T) {
	now := time.Date(2026, 8, 26, 9, 30, 0, 0, time.UTC)
	pool, err := NewPool(Options{
		Prefix:    "2001:db8::/64",
		MinLeases: 3,
		MaxLeases: 8,
		PortStart: 20000,
		PortEnd:   20007,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if got := pool.StandbyCount(); got != 3 {
		t.Fatalf("StandbyCount = %d, want 3", got)
	}
	standbys := pool.ListAll()
	if len(standbys) != 3 {
		t.Fatalf("ListAll = %d entries, want 3", len(standbys))
	}
	for _, entry := range standbys {
		if entry.Role != RoleStandby {
			t.Fatalf("standby %q role = %q, want %q", entry.ID, entry.Role, RoleStandby)
		}
		if entry.Port == 0 || entry.IPv6 == "" {
			t.Fatalf("standby %q missing port or IPv6: %+v", entry.ID, entry)
		}
	}
	// Standbys must not appear in the client-facing list.
	if clients := pool.List(); len(clients) != 0 {
		t.Fatalf("List = %d entries, want 0 (standbys excluded)", len(clients))
	}
}

func TestAcquireClaimsStandbyInstantly(t *testing.T) {
	now := time.Date(2026, 8, 26, 9, 30, 0, 0, time.UTC)
	pool, err := NewPool(Options{
		Prefix:    "2001:db8::/64",
		MinLeases: 4,
		MaxLeases: 8,
		PortStart: 20000,
		PortEnd:   20007,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	leased, err := pool.Acquire("client-a", false)
	if err != nil {
		t.Fatalf("Acquire client-a: %v", err)
	}
	if leased.Role != RoleClient {
		t.Fatalf("leased role = %q, want %q", leased.Role, RoleClient)
	}
	if leased.Port != 20000 {
		t.Fatalf("claimed port = %d, want lowest standby port 20000", leased.Port)
	}
	if got := pool.StandbyCount(); got != 3 {
		t.Fatalf("StandbyCount after claim = %d, want 3", got)
	}
	if _, ok := pool.Get("client-a"); !ok {
		t.Fatal("claimed lease is not registered under the client id")
	}
	if all := pool.ListAll(); len(all) != 4 {
		t.Fatalf("ListAll = %d entries, want 4 (3 standbys + 1 client)", len(all))
	}

	// Releasing a claimed lease above the floor hands it to the standby pool
	// with a fresh IPv6 instead of destroying it.
	if !pool.Release("client-a") {
		t.Fatal("Release(client-a) returned false")
	}
	if got := pool.StandbyCount(); got != 4 {
		t.Fatalf("StandbyCount after release = %d, want 4", got)
	}
	if _, ok := pool.Get("client-a"); ok {
		t.Fatal("released client lease still registered under client id")
	}

	reborn, err := pool.Acquire("client-b", false)
	if err != nil {
		t.Fatalf("Acquire client-b: %v", err)
	}
	if reborn.IPv6 == leased.IPv6 {
		t.Fatal("recycled standby kept the old IPv6 address")
	}
	if reborn.Port != leased.Port {
		t.Fatalf("recycled standby port = %d, want %d", reborn.Port, leased.Port)
	}
}

func TestIdleReleaseRespectsResidentFloor(t *testing.T) {
	now := time.Date(2026, 8, 26, 9, 30, 0, 0, time.UTC)
	pool, err := NewPool(Options{
		Prefix:      "2001:db8::/64",
		MinLeases:   3,
		MaxLeases:   6,
		IdleTimeout: 3 * time.Hour,
		PortStart:   20000,
		PortEnd:     20005,
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	// Claim two of the three standbys and let them go idle. Total stays at
	// the floor, so nothing may be released.
	for _, id := range []string{"c1", "c2"} {
		if _, err := pool.Acquire(id, false); err != nil {
			t.Fatalf("Acquire %s: %v", id, err)
		}
	}
	now = now.Add(time.Hour)
	if released := pool.ReleaseIdle(); released != 0 {
		t.Fatalf("ReleaseIdle at floor = %d, want 0", released)
	}
	if _, ok := pool.Get("c1"); !ok {
		t.Fatal("lease inside the resident floor was idle-released")
	}

	// Claim the last standby (total still 3), then create a genuinely new
	// lease so the total exceeds the floor.
	if _, err := pool.Acquire("c3", false); err != nil {
		t.Fatalf("Acquire c3: %v", err)
	}
	if _, err := pool.Acquire("c4", false); err != nil {
		t.Fatalf("Acquire c4: %v", err)
	}

	// Two hours later c1/c2 are idle for 3h (expired) but c3/c4 only 2h
	// (still fresh). The floor is exceeded, so exactly the idle ones go.
	now = now.Add(2 * time.Hour)
	released := pool.ReleaseIdle()
	if released != 2 {
		t.Fatalf("ReleaseIdle above floor = %d, want 2", released)
	}
	if _, ok := pool.Get("c1"); ok {
		t.Fatal("idle lease above the floor survived ReleaseIdle")
	}
	if _, ok := pool.Get("c3"); !ok {
		t.Fatal("active lease was idle-released")
	}
	if got := pool.StandbyCount(); got != 2 {
		t.Fatalf("StandbyCount = %d, want 2 (released leases refilled the floor)", got)
	}
}

func TestRecycleReplacesPortAndIPv6UnderSameID(t *testing.T) {
	now := time.Date(2026, 8, 26, 9, 30, 0, 0, time.UTC)
	pool, err := NewPool(Options{
		Prefix:    "2001:db8::/64",
		MinLeases: 2,
		MaxLeases: 4,
		PortStart: 20000,
		PortEnd:   20003,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	original, err := pool.Acquire("client-a", false)
	if err != nil {
		t.Fatalf("Acquire client-a: %v", err)
	}

	recycled, err := pool.Recycle("client-a")
	if err != nil {
		t.Fatalf("Recycle client-a: %v", err)
	}
	if recycled.ID != "client-a" {
		t.Fatalf("recycled id = %q, want client-a", recycled.ID)
	}
	if recycled.IPv6 == original.IPv6 {
		t.Fatal("recycle kept the old IPv6 address")
	}
	// Recycle is "释放后自动重新获取" and behaves like an IP change: the slot
	// returns to the standby pool and the client immediately claims one —
	// typically the same port with a fresh IPv6. The old lease became a
	// standby and was re-claimed, so the count is back to 1.
	if recycled.Port != original.Port {
		t.Fatalf("recycle port = %d, want %d (same port, fresh IPv6)", recycled.Port, original.Port)
	}
	if got := pool.StandbyCount(); got != 1 {
		t.Fatalf("StandbyCount after recycle = %d, want 1", got)
	}

	if _, err := pool.Recycle("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Recycle missing error = %v, want ErrNotFound", err)
	}

	// Persistent leases are anchored to their always-on port and cannot be
	// recycled.
	if _, err := pool.Acquire("pinned", true); err != nil {
		t.Fatalf("Acquire pinned: %v", err)
	}
	if _, err := pool.Recycle("pinned"); !errors.Is(err, ErrPersistent) {
		t.Fatalf("Recycle persistent error = %v, want ErrPersistent", err)
	}
}

func TestReleaseBelowFloorRefillsStandby(t *testing.T) {
	now := time.Date(2026, 8, 26, 9, 30, 0, 0, time.UTC)
	var mu sync.Mutex
	released := make(map[string]int)
	pool, err := NewPool(Options{
		Prefix:    "2001:db8::/64",
		MinLeases: 2,
		MaxLeases: 4,
		PortStart: 20000,
		PortEnd:   20003,
		Now:       func() time.Time { return now },
		OnRelease: func(id string) {
			mu.Lock()
			defer mu.Unlock()
			released[id]++
		},
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	// Draining standbys below the floor must refill it.
	if _, err := pool.Acquire("a", false); err != nil {
		t.Fatalf("Acquire a: %v", err)
	}
	if _, err := pool.Acquire("b", false); err != nil {
		t.Fatalf("Acquire b: %v", err)
	}
	if got := pool.StandbyCount(); got != 0 {
		t.Fatalf("StandbyCount = %d, want 0 after claiming two", got)
	}
	if !pool.Release("a") {
		t.Fatal("Release(a) returned false")
	}
	if got := pool.StandbyCount(); got != 1 {
		t.Fatalf("StandbyCount after release = %d, want 1 (refilled)", got)
	}
	if got := pool.ListAll(); len(got) != 2 {
		t.Fatalf("ListAll = %d, want 2", len(got))
	}
}

func TestPoolIdleReleasePreservesPersistentLease(t *testing.T) {
	now := time.Date(2026, 8, 26, 9, 30, 0, 0, time.UTC)
	pool, err := NewPool(Options{
		Prefix:      "2001:db8::/64",
		MaxLeases:   2,
		IdleTimeout: time.Minute,
		Now:         func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if _, err := pool.Acquire("temporary", false); err != nil {
		t.Fatalf("Acquire temporary: %v", err)
	}
	if _, err := pool.Acquire("persistent", true); err != nil {
		t.Fatalf("Acquire persistent: %v", err)
	}

	now = now.Add(time.Minute)
	if released := pool.ReleaseIdle(); released != 1 {
		t.Fatalf("ReleaseIdle = %d, want 1", released)
	}
	if _, ok := pool.Get("temporary"); ok {
		t.Fatal("temporary lease remains after idle timeout")
	}
	if _, ok := pool.Get("persistent"); !ok {
		t.Fatal("persistent lease was released")
	}
}

func TestPoolTimeAndRequestRotation(t *testing.T) {
	now := time.Date(2026, 8, 26, 9, 30, 0, 0, time.UTC)
	pool, err := NewPool(Options{
		Prefix:         "2001:db8::/64",
		MaxLeases:      2,
		RotateAfter:    time.Minute,
		RotateRequests: 2,
		Now:            func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	initial, err := pool.Acquire("client", false)
	if err != nil {
		t.Fatalf("Acquire initial: %v", err)
	}
	if _, err := pool.RecordRequest("client"); err != nil {
		t.Fatalf("RecordRequest first: %v", err)
	}
	requestRotated, err := pool.RecordRequest("client")
	if err != nil {
		t.Fatalf("RecordRequest second: %v", err)
	}
	if requestRotated.IPv6 == initial.IPv6 {
		t.Fatal("request-count rotation did not change IPv6 address")
	}
	if requestRotated.Requests != 0 {
		t.Fatalf("requests after rotation = %d, want 0", requestRotated.Requests)
	}

	now = now.Add(time.Minute)
	timeRotated, err := pool.Acquire("client", false)
	if err != nil {
		t.Fatalf("Acquire for time rotation: %v", err)
	}
	if timeRotated.IPv6 == requestRotated.IPv6 {
		t.Fatal("time-based rotation did not change IPv6 address")
	}
}

func TestPoolConcurrentAcquireUsesSingleLease(t *testing.T) {
	pool, err := NewPool(Options{
		Prefix:    "2001:db8::/64",
		MaxLeases: 8,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	const workers = 32
	addresses := make(chan string, workers)
	errorsCh := make(chan error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entry, acquireErr := pool.Acquire("shared", false)
			if acquireErr != nil {
				errorsCh <- acquireErr
				return
			}
			addresses <- entry.IPv6
		}()
	}
	wg.Wait()
	close(addresses)
	close(errorsCh)

	for acquireErr := range errorsCh {
		t.Errorf("concurrent Acquire: %v", acquireErr)
	}
	var expected string
	for address := range addresses {
		if expected == "" {
			expected = address
		}
		if address != expected {
			t.Errorf("concurrent address = %s, want %s", address, expected)
		}
	}
	if leases := pool.List(); len(leases) != 1 {
		t.Fatalf("lease count = %d, want 1", len(leases))
	}
}

func TestPoolRotateKeepsPortAndChangesIPv6(t *testing.T) {
	now := time.Date(2026, 8, 26, 9, 30, 0, 0, time.UTC)
	pool, err := NewPool(Options{
		Prefix:    "2001:db8::/64",
		MaxLeases: 4,
		PortStart: 20000,
		PortEnd:   20003,
		Now:       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if _, err := pool.Acquire("client", false); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	initial, _ := pool.Get("client")
	if _, err := pool.RecordRequest("client"); err != nil {
		t.Fatalf("RecordRequest: %v", err)
	}

	rotated, err := pool.Rotate("client")
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if rotated.IPv6 == initial.IPv6 {
		t.Fatal("Rotate did not change the IPv6 address")
	}
	if rotated.Port != initial.Port {
		t.Fatalf("Rotate changed port %d -> %d", initial.Port, rotated.Port)
	}
	if rotated.Requests != 0 {
		t.Fatalf("requests after rotate = %d, want 0", rotated.Requests)
	}
	if _, err := pool.Rotate("missing"); err == nil {
		t.Fatal("Rotate accepted a missing lease")
	}

	// Manual rotation must not flip persistence.
	if _, err := pool.SetPersistent("client", false); err != nil {
		t.Fatalf("SetPersistent: %v", err)
	}
	again, err := pool.Rotate("client")
	if err != nil {
		t.Fatalf("Rotate second: %v", err)
	}
	if again.Persistent {
		t.Fatal("Rotate marked the lease persistent")
	}
}

func TestPoolOnReleaseHookFiresOnEveryReleasePath(t *testing.T) {
	now := time.Date(2026, 8, 26, 9, 30, 0, 0, time.UTC)
	var mu sync.Mutex
	released := make(map[string]int)
	hook := func(id string) {
		mu.Lock()
		defer mu.Unlock()
		released[id]++
	}

	pool, err := NewPool(Options{
		Prefix:      "2001:db8::/64",
		MaxLeases:   8,
		IdleTimeout: time.Minute,
		Now:         func() time.Time { return now },
		OnRelease:   hook,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	for _, id := range []string{"explicit", "idle-a", "idle-b", "persistent"} {
		if _, err := pool.Acquire(id, id == "persistent"); err != nil {
			t.Fatalf("Acquire %s: %v", id, err)
		}
	}
	if !pool.Release("explicit") {
		t.Fatal("Release(explicit) returned false")
	}

	// Explicit ReleaseIdle path.
	now = now.Add(time.Minute)
	if count := pool.ReleaseIdle(); count != 2 {
		t.Fatalf("ReleaseIdle = %d, want 2", count)
	}

	// Acquire-triggered idle release path.
	for _, id := range []string{"via-acquire-a", "via-acquire-b", "touch"} {
		if _, err := pool.Acquire(id, false); err != nil {
			t.Fatalf("Acquire %s: %v", id, err)
		}
	}
	now = now.Add(time.Minute)
	if _, err := pool.Acquire("trigger", false); err != nil {
		t.Fatalf("Acquire trigger: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for id, want := range map[string]int{
		"explicit":      1,
		"idle-a":        1,
		"idle-b":        1,
		"via-acquire-a": 1,
		"via-acquire-b": 1,
		"touch":         1,
	} {
		if got := released[id]; got != want {
			t.Fatalf("hook(%q) called %d times, want %d", id, got, want)
		}
	}
	for _, absent := range []string{"persistent", "trigger"} {
		if got := released[absent]; got != 0 {
			t.Fatalf("hook(%q) called %d times, want 0", absent, got)
		}
	}
}

func TestPoolOnReleaseHookSkipsPersistentLease(t *testing.T) {
	now := time.Date(2026, 8, 26, 9, 30, 0, 0, time.UTC)
	pool, err := NewPool(Options{
		Prefix:      "2001:db8::/64",
		MaxLeases:   4,
		IdleTimeout: time.Minute,
		Now:         func() time.Time { return now },
		OnRelease: func(id string) {
			t.Fatalf("persistent lease %q should never be released", id)
		},
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if _, err := pool.Acquire("fixed", true); err != nil {
		t.Fatalf("Acquire fixed: %v", err)
	}
	now = now.Add(time.Hour)
	if released := pool.ReleaseIdle(); released != 0 {
		t.Fatalf("ReleaseIdle = %d, want 0 for persistent leases", released)
	}
	if _, ok := pool.Get("fixed"); !ok {
		t.Fatal("persistent lease was released")
	}
}

func TestPoolSetPersistentAndValidation(t *testing.T) {
	pool, err := NewPool(Options{
		Prefix:    "2001:db8::/64",
		MaxLeases: 1,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	if _, err := pool.Acquire("", false); err == nil {
		t.Fatal("Acquire accepted empty lease id")
	}
	if _, err := pool.Acquire("client", false); err != nil {
		t.Fatalf("Acquire client: %v", err)
	}
	updated, err := pool.SetPersistent("client", true)
	if err != nil {
		t.Fatalf("SetPersistent: %v", err)
	}
	if !updated.Persistent {
		t.Fatal("SetPersistent did not update lease")
	}
	if _, err := pool.SetPersistent("missing", true); err == nil {
		t.Fatal("SetPersistent accepted missing lease")
	}
	if _, err := pool.RecordRequest("missing"); err == nil {
		t.Fatal("RecordRequest accepted missing lease")
	}
}

func TestAcquirePortUsesRequestedPortAndReusesAfterRelease(t *testing.T) {
	pool, err := NewPool(Options{
		Prefix:    "2001:db8::/64",
		MaxLeases: 3,
		PortStart: 20000,
		PortEnd:   20002,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	entry, err := pool.AcquirePort("fixed", 20001, true)
	if err != nil {
		t.Fatalf("AcquirePort: %v", err)
	}
	if entry.Port != 20001 {
		t.Fatalf("AcquirePort port = %d, want 20001", entry.Port)
	}
	if !entry.Persistent {
		t.Fatal("AcquirePort did not create a persistent lease")
	}

	same, err := pool.AcquirePort("fixed", 20001, false)
	if err != nil {
		t.Fatalf("AcquirePort existing lease: %v", err)
	}
	if same.ID != entry.ID || same.Port != entry.Port || same.IPv6 != entry.IPv6 {
		t.Fatalf("AcquirePort existing lease = %+v, want %+v", same, entry)
	}

	if !pool.Release("fixed") {
		t.Fatal("Release(fixed) returned false")
	}
	reused, err := pool.AcquirePort("replacement", 20001, false)
	if err != nil {
		t.Fatalf("AcquirePort after release: %v", err)
	}
	if reused.Port != 20001 {
		t.Fatalf("reused port = %d, want 20001", reused.Port)
	}
}

func TestAcquireUsesRandomAddressesByDefault(t *testing.T) {
	pool, err := NewPool(Options{
		Prefix:    "2001:db8::/64",
		MinLeases: 2,
		MaxLeases: 8,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	seen := make(map[string]struct{}, 6)
	for i := 0; i < 6; i++ {
		item, err := pool.Acquire(fmt.Sprintf("client-%d", i), false)
		if err != nil {
			t.Fatalf("Acquire client-%d: %v", i, err)
		}
		if !strings.HasPrefix(item.IPv6, "2001:db8:") {
			t.Fatalf("address %s is outside the prefix", item.IPv6)
		}
		if _, dup := seen[item.IPv6]; dup {
			t.Fatalf("duplicate address %s handed out twice", item.IPv6)
		}
		seen[item.IPv6] = struct{}{}
	}
	if len(seen) != 6 {
		t.Fatalf("expected 6 distinct addresses, got %d", len(seen))
	}
}

func TestSequentialAddressesOption(t *testing.T) {
	pool, err := NewPool(Options{
		Prefix:              "2001:db8::/64",
		MaxLeases:           8,
		SequentialAddresses: true,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	first, err := pool.Acquire("first", false)
	if err != nil {
		t.Fatalf("Acquire first: %v", err)
	}
	second, err := pool.Acquire("second", false)
	if err != nil {
		t.Fatalf("Acquire second: %v", err)
	}
	third, err := pool.Acquire("third", false)
	if err != nil {
		t.Fatalf("Acquire third: %v", err)
	}
	if first.IPv6 != "2001:db8::1" || second.IPv6 != "2001:db8::2" || third.IPv6 != "2001:db8::3" {
		t.Fatalf("sequential addresses = %s, %s, %s; want ::1, ::2, ::3",
			first.IPv6, second.IPv6, third.IPv6)
	}
}

func TestAcquirePortRejectsUnavailablePortAndMismatchedLease(t *testing.T) {
	pool, err := NewPool(Options{
		Prefix:    "2001:db8::/64",
		MaxLeases: 2,
		PortStart: 20000,
		PortEnd:   20001,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}

	if _, err := pool.AcquirePort("", 20000, false); err == nil {
		t.Fatal("AcquirePort accepted an empty lease id")
	}
	if _, err := pool.AcquirePort("outside", 19999, false); !errors.Is(err, ErrPortUnavailable) {
		t.Fatalf("AcquirePort outside range error = %v, want ErrPortUnavailable", err)
	}
	if _, err := pool.AcquirePort("first", 20000, false); err != nil {
		t.Fatalf("AcquirePort first: %v", err)
	}
	if _, err := pool.AcquirePort("second", 20000, false); !errors.Is(err, ErrPortUnavailable) {
		t.Fatalf("AcquirePort duplicate port error = %v, want ErrPortUnavailable", err)
	}
	if _, err := pool.AcquirePort("first", 20001, false); !errors.Is(err, ErrPortUnavailable) {
		t.Fatalf("AcquirePort mismatched existing lease error = %v, want ErrPortUnavailable", err)
	}
}

func TestReassignMovesEveryLeaseToNewPrefix(t *testing.T) {
	pool, err := NewPool(Options{
		Prefix:    "2001:db8::/64",
		MinLeases: 1,
		MaxLeases: 4,
		PortStart: 20000,
		PortEnd:   20003,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	clientA, err := pool.Acquire("client-a", true)
	if err != nil {
		t.Fatalf("Acquire client-a: %v", err)
	}
	clientB, err := pool.AcquirePort("client-b", 20001, false)
	if err != nil {
		t.Fatalf("AcquirePort client-b: %v", err)
	}
	before := pool.ListAll()

	if err := pool.Reassign("2001:db8:beef::/64"); err != nil {
		t.Fatalf("Reassign: %v", err)
	}

	after := pool.ListAll()
	if len(after) < len(before) {
		t.Fatalf("lease count after reassign = %d, want at least %d", len(after), len(before))
	}
	byID := make(map[string]Lease, len(after))
	for _, item := range after {
		byID[item.ID] = item
	}
	prefix := "2001:db8:beef:"
	for _, original := range before {
		updated, ok := byID[original.ID]
		if !ok {
			t.Fatalf("lease %q vanished after reassign", original.ID)
		}
		if updated.ID != original.ID || updated.Port != original.Port || updated.Persistent != original.Persistent || updated.Role != original.Role {
			t.Fatalf("lease %q identity changed: %+v -> %+v", original.ID, original, updated)
		}
		if updated.IPv6 == original.IPv6 {
			t.Fatalf("lease %q kept its old address %s", original.ID, updated.IPv6)
		}
		if !strings.HasPrefix(updated.IPv6, prefix) {
			t.Fatalf("lease %q address %s is not under the new prefix %s", original.ID, updated.IPv6, prefix)
		}
	}
	// 新租约应使用新前缀下的地址。
	clientC, err := pool.Acquire("client-c", false)
	if err != nil {
		t.Fatalf("Acquire after reassign: %v", err)
	}
	if !strings.HasPrefix(clientC.IPv6, prefix) {
		t.Fatalf("new lease address %s is not under the new prefix", clientC.IPv6)
	}
	if clientA.Port == 0 || clientB.Port == 0 {
		t.Fatal("expected leased ports to survive reassign")
	}
}

func TestReassignRejectsInvalidPrefixWithoutChanges(t *testing.T) {
	pool, err := NewPool(Options{
		Prefix:    "2001:db8::/64",
		MinLeases: 1,
		MaxLeases: 2,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	original := pool.ListAll()

	if err := pool.Reassign("not-a-cidr"); err == nil {
		t.Fatal("Reassign accepted an invalid prefix")
	}
	unchanged := pool.ListAll()
	if len(unchanged) != len(original) {
		t.Fatalf("lease count changed after failed reassign")
	}
	for _, item := range unchanged {
		var match *Lease
		for i := range original {
			if original[i].ID == item.ID {
				match = &original[i]
				break
			}
		}
		if match == nil || match.IPv6 != item.IPv6 {
			t.Fatalf("lease %q address changed after failed reassign", item.ID)
		}
	}
}

func TestReassignOverlappingPrefixSkipsInUseAddresses(t *testing.T) {
	pool, err := NewPool(Options{
		Prefix:    "2001:db8:1::/64",
		MinLeases: 0,
		MaxLeases: 2,
	})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	first, err := pool.Acquire("first", false)
	if err != nil {
		t.Fatalf("Acquire first: %v", err)
	}
	if _, err := pool.Acquire("second", false); err != nil {
		t.Fatalf("Acquire second: %v", err)
	}

	// /62 与旧 /64 共享基地址段：新分配必须避开旧地址。
	if err := pool.Reassign("2001:db8:1::/62"); err != nil {
		t.Fatalf("Reassign overlapping prefix: %v", err)
	}
	after := pool.ListAll()
	seen := make(map[string]struct{}, len(after))
	for _, item := range after {
		if _, dup := seen[item.IPv6]; dup {
			t.Fatalf("duplicate address %s after reassign", item.IPv6)
		}
		seen[item.IPv6] = struct{}{}
	}
	// 原地址集合仍应整体属于新前缀覆盖范围（这里只验证无重复与全部变化）。
	if after[0].IPv6 == first.IPv6 {
		t.Fatalf("lease kept its old address %s despite overlapping prefix", after[0].IPv6)
	}
}
