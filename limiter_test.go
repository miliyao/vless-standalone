package main

import (
	"fmt"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheck_MaxConnPerIP(t *testing.T) {
	cfg := &Config{
		MaxConnPerIP:          2,
		MaxNewConnPerIPPerMin: 0,
	}
	limiter := NewLimiter(cfg)

	ip := netip.MustParseAddr("192.168.1.1")
	now := time.Now()

	meta1 := ConnMeta{ConnID: "1", SourceIP: ip, StartedAt: now}
	dec1 := limiter.Check(meta1, now)
	if !dec1.Allow {
		t.Fatalf("connection 1 should be allowed, got %s", dec1.Reason)
	}
	limiter.Register(meta1)

	meta2 := ConnMeta{ConnID: "2", SourceIP: ip, StartedAt: now}
	dec2 := limiter.Check(meta2, now)
	if !dec2.Allow {
		t.Fatalf("connection 2 should be allowed, got %s", dec2.Reason)
	}
	limiter.Register(meta2)

	meta3 := ConnMeta{ConnID: "3", SourceIP: ip, StartedAt: now}
	dec3 := limiter.Check(meta3, now)
	if dec3.Allow {
		t.Fatal("connection 3 should be rejected")
	}
}

func TestCheck_MaxNewConnPerMin(t *testing.T) {
	cfg := &Config{
		MaxConnPerIP:          0,
		MaxNewConnPerIPPerMin: 2,
	}
	limiter := NewLimiter(cfg)

	ip := netip.MustParseAddr("192.168.1.1")
	now := time.Now()

	meta1 := ConnMeta{ConnID: "1", SourceIP: ip, StartedAt: now}
	dec1 := limiter.Check(meta1, now)
	if !dec1.Allow {
		t.Fatalf("new connection 1 should be allowed, got %s", dec1.Reason)
	}
	limiter.Register(meta1)

	meta2 := ConnMeta{ConnID: "2", SourceIP: ip, StartedAt: now}
	dec2 := limiter.Check(meta2, now)
	if !dec2.Allow {
		t.Fatalf("new connection 2 should be allowed, got %s", dec2.Reason)
	}
	limiter.Register(meta2)

	meta3 := ConnMeta{ConnID: "3", SourceIP: ip, StartedAt: now}
	dec3 := limiter.Check(meta3, now)
	if dec3.Allow {
		t.Fatal("new connection 3 should be rejected by rate limit")
	}

	future := now.Add(time.Minute + time.Second)
	meta4 := ConnMeta{ConnID: "4", SourceIP: ip, StartedAt: future}
	dec4 := limiter.Check(meta4, future)
	if !dec4.Allow {
		t.Fatalf("new connection 4 should be allowed after window reset, got %s", dec4.Reason)
	}
}

func TestCheck_ReleaseOnUnregister(t *testing.T) {
	cfg := &Config{
		MaxConnPerIP:          1,
		MaxNewConnPerIPPerMin: 0,
	}
	limiter := NewLimiter(cfg)

	ip := netip.MustParseAddr("192.168.1.1")
	now := time.Now()

	meta1 := ConnMeta{ConnID: "1", SourceIP: ip, StartedAt: now}
	limiter.Check(meta1, now)
	limiter.Register(meta1)

	meta2 := ConnMeta{ConnID: "2", SourceIP: ip, StartedAt: now}
	if limiter.Check(meta2, now).Allow {
		t.Fatal("connection 2 should be rejected before unregister")
	}

	limiter.Unregister(meta1)

	if !limiter.Check(meta2, now).Allow {
		t.Fatal("connection 2 should be allowed after connection 1 unregisters")
	}
}

func TestCheck_AllowsZeroLimit(t *testing.T) {
	cfg := &Config{
		MaxConnPerIP:          0,
		MaxNewConnPerIPPerMin: 0,
	}
	limiter := NewLimiter(cfg)

	ip := netip.MustParseAddr("192.168.1.1")
	now := time.Now()

	for i := 0; i < 100; i++ {
		meta := ConnMeta{ConnID: string(rune(i)), SourceIP: ip, StartedAt: now}
		if !limiter.Check(meta, now).Allow {
			t.Fatal("zero limits should always allow connections")
		}
		limiter.Register(meta)
	}
}

func TestConcurrent_RegisterUnregister(t *testing.T) {
	cfg := &Config{
		MaxConnPerIP:          1000,
		MaxNewConnPerIPPerMin: 1000,
	}
	limiter := NewLimiter(cfg)

	ip1 := netip.MustParseAddr("192.168.1.1")
	ip2 := netip.MustParseAddr("192.168.1.2")
	now := time.Now()

	var wg sync.WaitGroup
	workers := 10
	loops := 100

	wg.Add(workers * 2)

	for i := 0; i < workers; i++ {
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < loops; j++ {
				connID := fmt.Sprintf("w1-%d-%d", workerID, j)
				meta := ConnMeta{ConnID: connID, SourceIP: ip1, StartedAt: now}
				limiter.Check(meta, now)
				limiter.Register(meta)
				limiter.Unregister(meta)
			}
		}(i)

		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < loops; j++ {
				connID := fmt.Sprintf("w2-%d-%d", workerID, j)
				meta := ConnMeta{ConnID: connID, SourceIP: ip2, StartedAt: now}
				limiter.Check(meta, now)
				limiter.Register(meta)
				limiter.Unregister(meta)
			}
		}(i)
	}

	wg.Wait()
}

func TestAcquire_AtomicUnderConcurrency(t *testing.T) {
	cfg := &Config{
		MaxConnPerIP:          1,
		MaxNewConnPerIPPerMin: 0,
	}
	limiter := NewLimiter(cfg)

	ip := netip.MustParseAddr("192.168.1.1")
	now := time.Now()

	const workers = 32
	var wg sync.WaitGroup
	var allowed int32

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			meta := ConnMeta{
				ConnID:    fmt.Sprintf("conn-%d", i),
				SourceIP:  ip,
				StartedAt: now,
			}
			if limiter.Acquire(meta, now).Allow {
				atomic.AddInt32(&allowed, 1)
			}
		}(i)
	}
	wg.Wait()

	if allowed != 1 {
		t.Fatalf("expected exactly 1 acquired connection, got %d", allowed)
	}
}

func TestSnapshot_ReadLock(t *testing.T) {
	cfg := &Config{
		MaxConnPerIP:          100,
		MaxNewConnPerIPPerMin: 100,
	}
	limiter := NewLimiter(cfg)
	ip := netip.MustParseAddr("192.168.1.1")
	now := time.Now()

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			meta := ConnMeta{ConnID: string(rune(i)), SourceIP: ip, StartedAt: now}
			limiter.Register(meta)
			limiter.Unregister(meta)
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			limiter.Snapshot()
			time.Sleep(time.Microsecond)
		}
	}()

	wg.Wait()
}

func TestUpdateConfig(t *testing.T) {
	cfg := &Config{
		MaxConnPerIP: 1,
	}
	limiter := NewLimiter(cfg)
	ip := netip.MustParseAddr("192.168.1.1")
	now := time.Now()

	meta1 := ConnMeta{ConnID: "1", SourceIP: ip, StartedAt: now}
	limiter.Register(meta1)

	meta2 := ConnMeta{ConnID: "2", SourceIP: ip, StartedAt: now}
	if limiter.Check(meta2, now).Allow {
		t.Fatal("second connection should be rejected before config update")
	}

	limiter.UpdateConfig(&Config{MaxConnPerIP: 5})

	if !limiter.Check(meta2, now).Allow {
		t.Fatal("second connection should be allowed after config update")
	}
}

func TestCheck_UDPIdleJanitor(t *testing.T) {
	cfg := &Config{
		MaxConnPerIP:          2,
		MaxNewConnPerIPPerMin: 0,
	}
	limiter := NewLimiter(cfg)

	ip := netip.MustParseAddr("192.168.1.1")
	now := time.Now()

	lastActive := now.UnixNano()
	var closed bool
	var mu sync.Mutex

	meta := ConnMeta{
		ConnID:     "udp-1",
		SourceIP:   ip,
		StartedAt:  now,
		IsUDP:      true,
		LastActive: &lastActive,
		CloseFunc: func() error {
			mu.Lock()
			closed = true
			mu.Unlock()
			limiter.Unregister(ConnMeta{ConnID: "udp-1", SourceIP: ip})
			return nil
		},
	}

	limiter.Register(meta)

	snap := limiter.Snapshot()
	if snap["active_connections"].(int) != 1 {
		t.Fatalf("expected 1 initial UDP connection, got %v", snap["active_connections"])
	}
	if snap["active_udp_connections"].(int) != 1 {
		t.Fatalf("expected 1 initial active UDP connection, got %v", snap["active_udp_connections"])
	}

	atomic.StoreInt64(&lastActive, time.Now().Add(-40*time.Second).UnixNano())
	limiter.cleanupIdleUDP()
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	if !closed {
		mu.Unlock()
		t.Fatal("UDP connection should be closed by janitor")
	}
	mu.Unlock()

	snap = limiter.Snapshot()
	if snap["active_connections"].(int) != 0 {
		t.Fatalf("expected 0 active connections after cleanup, got %v", snap["active_connections"])
	}
}
