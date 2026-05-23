package main

import (
	"net/netip"
	"sync"
	"time"
)

// ConnMeta 定义连接元数据，主要包含连接唯一 ID 和源 IP
type ConnMeta struct {
	ConnID    string
	SourceIP  netip.Addr
	StartedAt time.Time
}

// LimitDecision 定义限制器的拦截决策结果
type LimitDecision struct {
	Allow  bool
	Reason string
}

type rateLimiter struct {
	lastReset time.Time
	count     int
}

// Limiter 提供针对源 IP 维度的并发连接限制与新建连接频率限制
type Limiter struct {
	stateMu sync.RWMutex

	// 活跃连接状态及新建速率监控，受 stateMu 保护
	activeConnByIP map[string]map[string]ConnMeta
	recentConnByIP map[string]*rateLimiter

	window          time.Duration
	maxConnPerIP    int
	maxNewConnPerIP int
}

// NewLimiter 构造并初始化 IP 限制器
func NewLimiter(cfg *Config) *Limiter {
	l := &Limiter{
		activeConnByIP:  make(map[string]map[string]ConnMeta),
		recentConnByIP:  make(map[string]*rateLimiter),
		window:          time.Minute,
		maxConnPerIP:    cfg.MaxConnPerIP,
		maxNewConnPerIP: cfg.MaxNewConnPerIPPerMin,
	}

	// 启动后台定时任务清理过期的 IP 速率限制器缓存，避免内存随时间积累泄露
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		for range ticker.C {
			l.stateMu.Lock()
			now := time.Now()
			for ip, rl := range l.recentConnByIP {
				if now.Sub(rl.lastReset) > l.window*2 {
					delete(l.recentConnByIP, ip)
				}
			}
			l.stateMu.Unlock()
		}
	}()

	return l
}

// UpdateConfig 动态刷新限制阈值
func (l *Limiter) UpdateConfig(cfg *Config) {
	l.stateMu.Lock()
	defer l.stateMu.Unlock()
	l.maxConnPerIP = cfg.MaxConnPerIP
	l.maxNewConnPerIP = cfg.MaxNewConnPerIPPerMin
}

func (l *Limiter) checkCPS(index map[string]*rateLimiter, key string, limit int, now time.Time) bool {
	rl, ok := index[key]
	if !ok {
		index[key] = &rateLimiter{
			lastReset: now,
			count:     1,
		}
		return true
	}
	if now.Sub(rl.lastReset) >= l.window {
		rl.lastReset = now
		rl.count = 1
		return true
	}
	if rl.count >= limit {
		return false
	}
	rl.count++
	return true
}

// Check 检查源 IP 并发连接数和新建速率限制
func (l *Limiter) Check(meta ConnMeta, now time.Time) LimitDecision {
	if !meta.SourceIP.IsValid() {
		// 无法获取源 IP 时默认放行，防误杀
		return LimitDecision{Allow: true}
	}

	ipKey := meta.SourceIP.String()

	if meta.StartedAt.IsZero() {
		meta.StartedAt = now
	}

	l.stateMu.Lock()
	defer l.stateMu.Unlock()

	// 1. 校验单 IP 每分钟新建连接数 (CPS)
	if l.maxNewConnPerIP > 0 {
		if !l.checkCPS(l.recentConnByIP, ipKey, l.maxNewConnPerIP, now) {
			return LimitDecision{Allow: false, Reason: "new connections per ip limit exceeded"}
		}
	}

	// 2. 校验单 IP 最大并发连接数
	if l.maxConnPerIP > 0 {
		activeConns := l.activeConnByIP[ipKey]
		if len(activeConns) >= l.maxConnPerIP {
			return LimitDecision{Allow: false, Reason: "active connections per ip limit exceeded"}
		}
	}

	return LimitDecision{Allow: true}
}

// Register 登记连接
func (l *Limiter) Register(meta ConnMeta) {
	if !meta.SourceIP.IsValid() {
		return
	}

	l.stateMu.Lock()
	defer l.stateMu.Unlock()

	ipKey := meta.SourceIP.String()
	conns := l.activeConnByIP[ipKey]
	if conns == nil {
		conns = make(map[string]ConnMeta)
		l.activeConnByIP[ipKey] = conns
	}
	conns[meta.ConnID] = meta
}

// Unregister 注销连接
func (l *Limiter) Unregister(meta ConnMeta) {
	if !meta.SourceIP.IsValid() {
		return
	}

	l.stateMu.Lock()
	defer l.stateMu.Unlock()

	ipKey := meta.SourceIP.String()
	conns, ok := l.activeConnByIP[ipKey]
	if !ok {
		return
	}

	delete(conns, meta.ConnID)
	if len(conns) == 0 {
		delete(l.activeConnByIP, ipKey)
	}
}

// Snapshot 导出节点当前负载统计
func (l *Limiter) Snapshot() map[string]interface{} {
	l.stateMu.RLock()
	defer l.stateMu.RUnlock()

	activeConnections := 0
	for _, conns := range l.activeConnByIP {
		activeConnections += len(conns)
	}

	return map[string]interface{}{
		"active_ips":         len(l.activeConnByIP),
		"active_connections": activeConnections,
	}
}
