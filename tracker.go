package main

import (
	"context"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"go.uber.org/zap"
)

type limiterTracker struct {
	limiter      *Limiter
	logger       *zap.Logger
	connSequence uint64
}

// newLimiterTracker creates an IP-based connection tracker for sing-box.
func newLimiterTracker(limiter *Limiter, logger *zap.Logger) *limiterTracker {
	return &limiterTracker{limiter: limiter, logger: logger}
}

// RoutedConnection wraps TCP connections and applies concurrency/rate limits.
func (t *limiterTracker) RoutedConnection(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) net.Conn {
	meta := t.buildConnMeta(metadata)
	if decision := t.limiter.Acquire(meta, meta.StartedAt); !decision.Allow {
		if t.logger != nil {
			t.logger.Warn("TCP connection rejected by per-IP limiter",
				zap.String("ip", meta.SourceIP.String()),
				zap.String("reason", decision.Reason),
			)
		}
		_ = conn.Close()
		return conn
	}
	return &trackedConn{
		Conn:    conn,
		limiter: t.limiter,
		meta:    meta,
	}
}

// RoutedPacketConnection wraps UDP packet connections and tracks activity for idle cleanup.
func (t *limiterTracker) RoutedPacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, matchedRule adapter.Rule, matchOutbound adapter.Outbound) N.PacketConn {
	meta := t.buildConnMeta(metadata)
	meta.IsUDP = true

	lastActive := time.Now().UnixNano()
	meta.LastActive = &lastActive

	tpc := &trackedPacketConn{
		PacketConn: conn,
		limiter:    t.limiter,
		meta:       meta,
		lastActive: &lastActive,
	}
	tpc.meta.CloseFunc = tpc.Close

	if decision := t.limiter.Acquire(tpc.meta, meta.StartedAt); !decision.Allow {
		if t.logger != nil {
			t.logger.Warn("UDP packet connection rejected by per-IP limiter",
				zap.String("ip", meta.SourceIP.String()),
				zap.String("reason", decision.Reason),
			)
		}
		_ = conn.Close()
		return conn
	}

	return tpc
}

func (t *limiterTracker) buildConnMeta(metadata adapter.InboundContext) ConnMeta {
	seq := atomic.AddUint64(&t.connSequence, 1)
	meta := ConnMeta{
		ConnID:    strconv.FormatUint(seq, 36),
		StartedAt: time.Now(),
	}
	if metadata.Source.IsValid() && metadata.Source.Addr.IsValid() {
		meta.SourceIP = metadata.Source.Addr
	}
	return meta
}

type trackedConn struct {
	net.Conn
	limiter   *Limiter
	meta      ConnMeta
	closeOnce sync.Once
}

func (c *trackedConn) Upstream() any {
	return c.Conn
}

func (c *trackedConn) Unwrap() any {
	return c.Conn
}

func (c *trackedConn) Close() error {
	c.closeOnce.Do(func() {
		c.limiter.Unregister(c.meta)
	})
	return c.Conn.Close()
}

type trackedPacketConn struct {
	N.PacketConn
	limiter    *Limiter
	meta       ConnMeta
	closeOnce  sync.Once
	lastActive *int64
}

func (c *trackedPacketConn) Upstream() any {
	return c.PacketConn
}

func (c *trackedPacketConn) Unwrap() any {
	return c.PacketConn
}

// FrontHeadroom exposes the wrapped packet connection headroom to avoid mux buffer overflow panics.
func (c *trackedPacketConn) FrontHeadroom() int {
	if f, ok := c.PacketConn.(interface{ FrontHeadroom() int }); ok {
		return f.FrontHeadroom()
	}
	return 0
}

// RearHeadroom exposes the wrapped packet connection tailroom.
func (c *trackedPacketConn) RearHeadroom() int {
	if f, ok := c.PacketConn.(interface{ RearHeadroom() int }); ok {
		return f.RearHeadroom()
	}
	return 0
}

func (c *trackedPacketConn) ReadPacket(buffer *buf.Buffer) (metadata.Socksaddr, error) {
	addr, err := c.PacketConn.ReadPacket(buffer)
	if err == nil {
		atomic.StoreInt64(c.lastActive, time.Now().UnixNano())
	}
	return addr, err
}

func (c *trackedPacketConn) WritePacket(buffer *buf.Buffer, destination metadata.Socksaddr) error {
	err := c.PacketConn.WritePacket(buffer, destination)
	if err == nil {
		atomic.StoreInt64(c.lastActive, time.Now().UnixNano())
	}
	return err
}

func (c *trackedPacketConn) Close() error {
	c.closeOnce.Do(func() {
		c.limiter.Unregister(c.meta)
	})
	return c.PacketConn.Close()
}

var _ adapter.ConnectionTracker = (*limiterTracker)(nil)
