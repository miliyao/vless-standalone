package main

import (
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

// ConnMeta 定义连接元数据，主要包含连接唯一 ID、源 IP 以及 UDP 活跃状态与清理控制
type ConnMeta struct {
	ConnID     string
	SourceIP   netip.Addr
	StartedAt  time.Time
	IsUDP      bool
	LastActive *int64       // 指向以纳秒为单位的 Unix 时间戳指针 (atomic)
	CloseFunc  func() error // 主动关闭底层的 UDP 报文连接函数
}

// LimitDecision 定义限制器的拦截决策结果
type LimitDecision struct {
	Allow  bool
	Reason string
}

// rateLimiter 使用高精度滑动窗口日志算法记录时间戳
type rateLimiter struct {
	times []time.Time
}

// allow 使用滑动窗口算法校验新建速率限制
func (rl *rateLimiter) allow(limit int, now time.Time, window time.Duration) bool {
	if limit <= 0 {
		return true
	}
	cutoff := now.Add(-window)
	// 清理超出时间窗口的历史记录
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

// limiterShard 分片锁结构体，减少高并发下的全局锁竞争
type limiterShard struct {
	mu             sync.RWMutex
	activeConnByIP map[string]map[string]ConnMeta
	recentConnByIP map[string]*rateLimiter
}

// Limiter 提供针对源 IP 维度的并发连接限制与新建连接频率限制，基于分片锁设计
type Limiter struct {
	shards [shardCount]*limiterShard

	window          time.Duration
	maxConnPerIP    int32 // 使用 atomic 读写
	maxNewConnPerIP int32 // 使用 atomic 读写
}

// fnv32 计算 IP 字符串的 FNV-1a 哈希，以确定分片索引
func fnv32(key string) uint32 {
	hash := uint32(2166136261)
	const prime = 16777619
	for i := 0; i < len(key); i++ {
		hash ^= uint32(key[i])
		hash *= prime
	}
	return hash
}

// getShard 获取对应 IP 的分片
func (l *Limiter) getShard(ipKey string) *limiterShard {
	idx := fnv32(ipKey) % shardCount
	return l.shards[idx]
}

// NewLimiter 构造并初始化 IP 限制器
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

	// 启动后台定时任务清理过期的 IP 速率限制器缓存，并主动回收空闲的 UDP 连接
	go l.runJanitor()

	return l
}

// runJanitor 运行后台清理和回收任务
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

// cleanupIdleUDP 自动扫描并关闭空闲 30 秒以上的 UDP 连接
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

		// 在锁外异步执行 Close，防死锁和长阻塞
		for _, closeFunc := range toClose {
			go func(cf func() error) {
				_ = cf()
			}(closeFunc)
		}
	}
}

// cleanupExpiredCache 定期清理最近 2 分钟无连接记录的 IP 缓存，防内存累积泄露
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

// UpdateConfig 动态刷新限制阈值
func (l *Limiter) UpdateConfig(cfg *Config) {
	atomic.StoreInt32(&l.maxConnPerIP, int32(cfg.MaxConnPerIP))
	atomic.StoreInt32(&l.maxNewConnPerIP, int32(cfg.MaxNewConnPerIPPerMin))
}

// Check 检查源 IP 并发连接数和新建速率限制
func (l *Limiter) Check(meta ConnMeta, now time.Time) LimitDecision {
	return l.checkLocked(meta, now, false)
}

// Acquire atomically checks and registers a connection under the same shard lock.
func (l *Limiter) Acquire(meta ConnMeta, now time.Time) LimitDecision {
	return l.checkLocked(meta, now, true)
}

func (l *Limiter) checkLocked(meta ConnMeta, now time.Time, register bool) LimitDecision {
	if !meta.SourceIP.IsValid() {
		// 无法获取源 IP 时默认放行，防误杀
		return LimitDecision{Allow: true}
	}

	ipKey := meta.SourceIP.String()
	shard := l.getShard(ipKey)

	maxNew := atomic.LoadInt32(&l.maxNewConnPerIP)
	maxConn := atomic.LoadInt32(&l.maxConnPerIP)

	shard.mu.Lock()
	defer shard.mu.Unlock()

	// 1. 校验单 IP 每分钟新建连接数 (滑动窗口)
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

	// 2. 校验单 IP 最大并发连接数
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

// Register 登记连接
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

// Unregister 注销连接
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

// Snapshot 导出节点当前负载统计
func (l *Limiter) Snapshot() map[string]interface{} {
	activeIPs := 0
	activeConnections := 0

	for i := 0; i < shardCount; i++ {
		shard := l.shards[i]
		shard.mu.RLock()
		activeIPs += len(shard.activeConnByIP)
		for _, conns := range shard.activeConnByIP {
			activeConnections += len(conns)
		}
		shard.mu.RUnlock()
	}

	return map[string]interface{}{
		"active_ips":         activeIPs,
		"active_connections": activeConnections,
	}
}
