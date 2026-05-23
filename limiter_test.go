package main

import (
	"fmt"
	"net/netip"
	"sync"
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

	// 模拟连接 1
	meta1 := ConnMeta{ConnID: "1", SourceIP: ip, StartedAt: now}
	dec1 := limiter.Check(meta1, now)
	if !dec1.Allow {
		t.Fatalf("连接 1 应被允许，但被拒绝: %s", dec1.Reason)
	}
	limiter.Register(meta1)

	// 模拟连接 2
	meta2 := ConnMeta{ConnID: "2", SourceIP: ip, StartedAt: now}
	dec2 := limiter.Check(meta2, now)
	if !dec2.Allow {
		t.Fatalf("连接 2 应被允许，但被拒绝: %s", dec2.Reason)
	}
	limiter.Register(meta2)

	// 模拟连接 3 (并发上限到达 2，应拒绝)
	meta3 := ConnMeta{ConnID: "3", SourceIP: ip, StartedAt: now}
	dec3 := limiter.Check(meta3, now)
	if dec3.Allow {
		t.Fatalf("连接 3 应当被拦截，但被允许了")
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

	// 新建 1
	meta1 := ConnMeta{ConnID: "1", SourceIP: ip, StartedAt: now}
	dec1 := limiter.Check(meta1, now)
	if !dec1.Allow {
		t.Fatalf("新建 1 应允许: %s", dec1.Reason)
	}
	limiter.Register(meta1)

	// 新建 2
	meta2 := ConnMeta{ConnID: "2", SourceIP: ip, StartedAt: now}
	dec2 := limiter.Check(meta2, now)
	if !dec2.Allow {
		t.Fatalf("新建 2 应允许: %s", dec2.Reason)
	}
	limiter.Register(meta2)

	// 新建 3 (超速，拒绝)
	meta3 := ConnMeta{ConnID: "3", SourceIP: ip, StartedAt: now}
	dec3 := limiter.Check(meta3, now)
	if dec3.Allow {
		t.Fatalf("新建 3 应当超速被拦截")
	}

	// 1 分钟后，限制重置
	future := now.Add(time.Minute + time.Second)
	meta4 := ConnMeta{ConnID: "4", SourceIP: ip, StartedAt: future}
	dec4 := limiter.Check(meta4, future)
	if !dec4.Allow {
		t.Fatalf("1分钟后新建 4 应重新允许，但被拒绝: %s", dec4.Reason)
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

	// 超限
	meta2 := ConnMeta{ConnID: "2", SourceIP: ip, StartedAt: now}
	if limiter.Check(meta2, now).Allow {
		t.Fatalf("连接 2 应当因超限被拦截")
	}

	// 注销 1
	limiter.Unregister(meta1)

	// 现在应该允许了
	if !limiter.Check(meta2, now).Allow {
		t.Fatalf("连接 1 注销后，连接 2 应被允许")
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
			t.Fatalf("限制设为 0（即不限制）时，应当始终允许")
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

	// 并发执行 Register / Unregister / Check
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

	// 协程 1: 并发写入
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			meta := ConnMeta{ConnID: string(rune(i)), SourceIP: ip, StartedAt: now}
			limiter.Register(meta)
			limiter.Unregister(meta)
		}
	}()

	// 协程 2: 并发 Snapshot() 读取
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

	// 默认限额 1，第二个连接应当拒绝
	meta2 := ConnMeta{ConnID: "2", SourceIP: ip, StartedAt: now}
	if limiter.Check(meta2, now).Allow {
		t.Fatalf("第二个连接应当拒绝")
	}

	// 动态更新配置将上限调大至 5
	limiter.UpdateConfig(&Config{MaxConnPerIP: 5})

	// 现在应当允许了
	if !limiter.Check(meta2, now).Allow {
		t.Fatalf("更新限额后，第二个连接应当允许")
	}
}
