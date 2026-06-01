package main

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// ConnMeta describes one tracked connection.
type ConnMeta struct {
	ConnID     string
	SourceIP   netip.Addr
	StartedAt  time.Time
	IsUDP      bool
	LastActive *int64       // Unix timestamp in nanoseconds, updated atomically.
	CloseFunc  func() error // Actively closes the underlying UDP packet connection.
}

// LimitDecision is the result returned by the limiter.
type LimitDecision struct {
	Allow  bool
	Reason string
}

// rateLimiter records timestamps for a sliding-window rate limit.
type rateLimiter struct {
	times []time.Time
}

// allow validates a new-connection rate limit with a sliding window.
func (rl *rateLimiter) allow(limit int, now time.Time, window time.Duration) bool {
	if limit <= 0 {
		return true
	}
	cutoff := now.Add(-window)
	i := 0
	for i < len(rl.times) && rl.times[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		rl.times = rl.times[i:]
	}
	if len(rl.times) >= limit {
		return false
	}
	rl.times = append(rl.times, now)
	return true
}

const shardCount = 64

// limiterShard reduces global lock contention under high concurrency.
type limiterShard struct {
	mu             sync.RWMutex
	activeConnByIP map[string]map[string]ConnMeta
	recentConnByIP map[string]*rateLimiter
}

// Limiter enforces per-source-IP concurrent connection and new-connection limits.
type Limiter struct {
	shards [shardCount]*limiterShard

	window          time.Duration
	maxConnPerIP    int32
	maxNewConnPerIP int32
}

// fnv32 calculates an FNV-1a hash for selecting a shard.
func fnv32(key string) uint32 {
	hash := uint32(2166136261)
	const prime = 16777619
	for i := 0; i < len(key); i++ {
		hash ^= uint32(key[i])
		hash *= prime
	}
	return hash
}

func (l *Limiter) getShard(ipKey string) *limiterShard {
	idx := fnv32(ipKey) % shardCount
	return l.shards[idx]
}

// NewLimiter creates a sharded limiter and starts the background cleanup task.
func NewLimiter(cfg *Config) *Limiter {
	l := &Limiter{
		window:          time.Minute,
		maxConnPerIP:    int32(cfg.MaxConnPerIP),
		maxNewConnPerIP: int32(cfg.MaxNewConnPerIPPerMin),
	}

	for i := 0; i < shardCount; i++ {
		l.shards[i] = &limiterShard{
			activeConnByIP: make(map[string]map[string]ConnMeta),
			recentConnByIP: make(map[string]*rateLimiter),
		}
	}

	go l.runJanitor()
	return l
}

func (l *Limiter) runJanitor() {
	udpTicker := time.NewTicker(10 * time.Second)
	cacheTicker := time.NewTicker(5 * time.Minute)
	defer udpTicker.Stop()
	defer cacheTicker.Stop()

	for {
		select {
		case <-udpTicker.C:
			l.cleanupIdleUDP()
		case <-cacheTicker.C:
			l.cleanupExpiredCache()
		}
	}
}

// cleanupIdleUDP closes UDP connections idle for more than 30 seconds.
func (l *Limiter) cleanupIdleUDP() {
	now := time.Now().UnixNano()
	idleTimeout := int64(30 * time.Second)

	for i := 0; i < shardCount; i++ {
		shard := l.shards[i]
		var toClose []func() error

		shard.mu.Lock()
		for _, activeConns := range shard.activeConnByIP {
			for _, meta := range activeConns {
				if meta.IsUDP && meta.LastActive != nil && meta.CloseFunc != nil {
					lastActive := atomic.LoadInt64(meta.LastActive)
					if now-lastActive > idleTimeout {
						toClose = append(toClose, meta.CloseFunc)
					}
				}
			}
		}
		shard.mu.Unlock()

		// Run close callbacks outside the shard lock to avoid lock contention.
		for _, closeFunc := range toClose {
			go func(cf func() error) {
				_ = cf()
			}(closeFunc)
		}
	}
}

// cleanupExpiredCache removes stale IP rate-limit buckets to avoid unbounded growth.
func (l *Limiter) cleanupExpiredCache() {
	now := time.Now()
	for i := 0; i < shardCount; i++ {
		shard := l.shards[i]
		shard.mu.Lock()
		for ip, rl := range shard.recentConnByIP {
			if len(rl.times) == 0 || now.Sub(rl.times[len(rl.times)-1]) > l.window*2 {
				delete(shard.recentConnByIP, ip)
			}
		}
		shard.mu.Unlock()
	}
}

// UpdateConfig refreshes limiter thresholds without recreating the limiter.
func (l *Limiter) UpdateConfig(cfg *Config) {
	atomic.StoreInt32(&l.maxConnPerIP, int32(cfg.MaxConnPerIP))
	atomic.StoreInt32(&l.maxNewConnPerIP, int32(cfg.MaxNewConnPerIPPerMin))
}

// Check validates per-source-IP concurrency and new-connection limits.
func (l *Limiter) Check(meta ConnMeta, now time.Time) LimitDecision {
	return l.checkLocked(meta, now, false)
}

// Acquire atomically checks and registers a connection under the same shard lock.
func (l *Limiter) Acquire(meta ConnMeta, now time.Time) LimitDecision {
	return l.checkLocked(meta, now, true)
}

func (l *Limiter) checkLocked(meta ConnMeta, now time.Time, register bool) LimitDecision {
	if !meta.SourceIP.IsValid() {
		return LimitDecision{Allow: true}
	}

	ipKey := meta.SourceIP.String()
	shard := l.getShard(ipKey)

	maxNew := atomic.LoadInt32(&l.maxNewConnPerIP)
	maxConn := atomic.LoadInt32(&l.maxConnPerIP)

	shard.mu.Lock()
	defer shard.mu.Unlock()

	if maxNew > 0 {
		rl, ok := shard.recentConnByIP[ipKey]
		if !ok {
			rl = &rateLimiter{}
			shard.recentConnByIP[ipKey] = rl
		}
		if !rl.allow(int(maxNew), now, l.window) {
			return LimitDecision{Allow: false, Reason: "new connections per ip limit exceeded"}
		}
	}

	if maxConn > 0 {
		activeConns := shard.activeConnByIP[ipKey]
		if len(activeConns) >= int(maxConn) {
			return LimitDecision{Allow: false, Reason: "active connections per ip limit exceeded"}
		}
	}

	if register {
		conns := shard.activeConnByIP[ipKey]
		if conns == nil {
			conns = make(map[string]ConnMeta)
			shard.activeConnByIP[ipKey] = conns
		}
		conns[meta.ConnID] = meta
	}

	return LimitDecision{Allow: true}
}

// Register tracks a connection.
func (l *Limiter) Register(meta ConnMeta) {
	if !meta.SourceIP.IsValid() {
		return
	}

	ipKey := meta.SourceIP.String()
	shard := l.getShard(ipKey)

	shard.mu.Lock()
	defer shard.mu.Unlock()

	conns := shard.activeConnByIP[ipKey]
	if conns == nil {
		conns = make(map[string]ConnMeta)
		shard.activeConnByIP[ipKey] = conns
	}
	conns[meta.ConnID] = meta
}

// Unregister stops tracking a connection.
func (l *Limiter) Unregister(meta ConnMeta) {
	if !meta.SourceIP.IsValid() {
		return
	}

	ipKey := meta.SourceIP.String()
	shard := l.getShard(ipKey)

	shard.mu.Lock()
	defer shard.mu.Unlock()

	conns, ok := shard.activeConnByIP[ipKey]
	if !ok {
		return
	}

	delete(conns, meta.ConnID)
	if len(conns) == 0 {
		delete(shard.activeConnByIP, ipKey)
	}
}

// Snapshot returns current load counters for the status API.
func (l *Limiter) Snapshot() map[string]interface{} {
	activeIPs := 0
	activeConnections := 0
	activeUDPConnections := 0

	for i := 0; i < shardCount; i++ {
		shard := l.shards[i]
		shard.mu.RLock()
		activeIPs += len(shard.activeConnByIP)
		for _, conns := range shard.activeConnByIP {
			activeConnections += len(conns)
			for _, meta := range conns {
				if meta.IsUDP {
					activeUDPConnections++
				}
			}
		}
		shard.mu.RUnlock()
	}

	return map[string]interface{}{
		"active_ips":             activeIPs,
		"active_connections":     activeConnections,
		"active_udp_connections": activeUDPConnections,
	}
}

// Limits returns the active limiter thresholds for diagnostics.
func (l *Limiter) Limits() map[string]interface{} {
	return map[string]interface{}{
		"max_conn_per_ip":             int(atomic.LoadInt32(&l.maxConnPerIP)),
		"max_new_conn_per_ip_per_min": int(atomic.LoadInt32(&l.maxNewConnPerIP)),
		"window_seconds":              int(l.window.Seconds()),
	}
}
