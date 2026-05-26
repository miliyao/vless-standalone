package main

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/endpoint"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/adapter/outbound"
	boxService "github.com/sagernet/sing-box/adapter/service"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/dns"
	dnsTransport "github.com/sagernet/sing-box/dns/transport"
	"github.com/sagernet/sing-box/dns/transport/local"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/direct"
	"github.com/sagernet/sing-box/protocol/vless"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/sagernet/sing/service"
	"go.uber.org/zap"
)

const (
	defaultRealityDestPort      uint16 = 443
	defaultVLESSFlow                   = "xtls-rprx-vision"
	inboundTag                         = "vless-in"
	directOutboundTag                  = "direct"
	directIPv6OutboundTag              = "direct-v6"
	localDNSOutboundTag                = "local-dns"
	defaultSniffTimeout                = time.Second
	defaultTCPKeepAlive                = 5 * time.Minute
	defaultTCPKeepAliveInterval        = 75 * time.Second
)

// Engine 负责管理底层嵌入式 sing-box 的生命周期与配置重载
type Engine struct {
	mu           sync.Mutex
	instance     *box.Box
	limiter      *Limiter
	logger       *zap.Logger
	googleIPv6   bool
	clashAPIAddr string
	activeCfg    *Config // 保存当前成功运行的配置，用于回滚
}

func NewEngine(cfg *Config, limiter *Limiter, logger *zap.Logger) *Engine {
	return &Engine{
		limiter:      limiter,
		logger:       logger,
		googleIPv6:   cfg.GoogleIPv6,
		clashAPIAddr: cfg.ClashAPIListenAddr,
	}
}

func (e *Engine) Start(cfg *Config) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	instance, err := e.createBox(cfg)
	if err != nil {
		return err
	}

	if err := instance.Start(); err != nil {
		_ = instance.Close()
		return fmt.Errorf("启动 sing-box 内核失败: %w", err)
	}

	e.instance = instance
	e.activeCfg = cfg
	return nil
}

func (e *Engine) Reload(cfg *Config) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.logger.Info("开始重载配置文件 (注：由于端口独占绑定限制，热重载过程中会有毫秒级的短暂连接中断)...")
	newInstance, err := e.createBox(cfg)
	if err != nil {
		return fmt.Errorf("解析新配置文件失败: %w", err)
	}

	oldInstance := e.instance
	startTime := time.Now()

	// 1. 先行关闭旧实例释放独占绑定的监听端口
	if oldInstance != nil {
		_ = oldInstance.Close()
	}

	// 2. 启动新实例重新接管监听端口
	if err := newInstance.Start(); err != nil {
		e.logger.Error("启动新内核实例失败，尝试拉起旧配置防灾回滚...", zap.Error(err))
		_ = newInstance.Close()

		// 回滚逻辑：使用备份的 activeCfg 重新初始化并启动
		if e.activeCfg != nil {
			fallbackInstance, rollbackErr := e.createBox(e.activeCfg)
			if rollbackErr == nil {
				if rollbackStartErr := fallbackInstance.Start(); rollbackStartErr == nil {
					e.instance = fallbackInstance
					e.logger.Warn("已成功将服务实例回滚至老配置运行，避免了节点瘫痪")
					return fmt.Errorf("启动新配置失败: %w；已成功回滚至老配置运行", err)
				} else {
					_ = fallbackInstance.Close()
				}
			}
		}
		return fmt.Errorf("启动新配置失败且回滚失败: %w", err)
	}

	e.instance = newInstance
	e.activeCfg = cfg
	e.logger.Info("热重载配置文件完成", zap.Duration("interruption_duration", time.Since(startTime)))
	return nil
}

func (e *Engine) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.instance != nil {
		err := e.instance.Close()
		e.instance = nil
		return err
	}
	return nil
}

func (e *Engine) createBox(cfg *Config) (*box.Box, error) {
	opts, err := e.buildSingboxOptions(cfg)
	if err != nil {
		return nil, err
	}

	ctx := e.createSingboxContext()
	instance, err := box.New(box.Options{
		Context: ctx,
		Options: opts,
	})
	if err != nil {
		return nil, fmt.Errorf("初始化 sing-box 实例失败: %w", err)
	}

	router := service.FromContext[adapter.Router](ctx)
	if router != nil && e.limiter != nil {
		router.AppendTracker(newLimiterTracker(e.limiter, e.logger))
	}

	return instance, nil
}

func (e *Engine) createSingboxContext() context.Context {
	ctx := context.Background()

	inboundRegistry := inbound.NewRegistry()
	vless.RegisterInbound(inboundRegistry)

	outboundRegistry := outbound.NewRegistry()
	direct.RegisterOutbound(outboundRegistry)

	endpointRegistry := endpoint.NewRegistry()

	dnsTransportRegistry := dns.NewTransportRegistry()
	dnsTransport.RegisterUDP(dnsTransportRegistry)
	dnsTransport.RegisterTCP(dnsTransportRegistry)
	local.RegisterTransport(dnsTransportRegistry)

	serviceRegistry := boxService.NewRegistry()

	return box.Context(ctx, inboundRegistry, outboundRegistry, endpointRegistry, dnsTransportRegistry, serviceRegistry)
}

func (e *Engine) buildSingboxOptions(cfg *Config) (option.Options, error) {
	flow := strings.TrimSpace(cfg.Flow)
	if flow == "" {
		flow = defaultVLESSFlow
	}

	destPort, err := e.parseRealityDestPort(cfg.TLSSettings.ServerPort)
	if err != nil {
		return option.Options{}, err
	}

	listenAddr, err := e.resolveListenAddr(cfg.ListenIP)
	if err != nil {
		return option.Options{}, err
	}

	// 解析匿名用户 UUID 列表并生成用户标识
	sbUsers := make([]option.VLESSUser, 0, len(cfg.UUIDs))
	for i, uuid := range cfg.UUIDs {
		sbUsers = append(sbUsers, option.VLESSUser{
			Name: "user-" + strconv.Itoa(i+1),
			UUID: uuid,
			Flow: flow,
		})
	}

	routes := e.buildRouteOptions()

	opts := option.Options{
		Log: &option.LogOptions{
			Level:    cfg.LogLevel,
			Disabled: false,
		},
		Inbounds: []option.Inbound{
			{
				Type: "vless",
				Tag:  inboundTag,
				Options: &option.VLESSInboundOptions{
					ListenOptions: option.ListenOptions{
						Listen:               (*badoption.Addr)(&listenAddr),
						ListenPort:           uint16(cfg.ServerPort),
						ReuseAddr:            true,
						TCPFastOpen:          true,
						TCPKeepAlive:         badoption.Duration(defaultTCPKeepAlive),
						TCPKeepAliveInterval: badoption.Duration(defaultTCPKeepAliveInterval),
						InboundOptions: option.InboundOptions{
							SniffEnabled:             true,
							SniffOverrideDestination: false,
							SniffTimeout:             badoption.Duration(defaultSniffTimeout),
						},
					},
					Users: sbUsers,
					InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
						TLS: &option.InboundTLSOptions{
							Enabled:    true,
							ServerName: cfg.TLSSettings.ServerName,
							Reality: &option.InboundRealityOptions{
								Enabled: true,
								Handshake: option.InboundRealityHandshakeOptions{
									ServerOptions: option.ServerOptions{
										Server:     cfg.TLSSettings.ServerName,
										ServerPort: destPort,
									},
								},
								PrivateKey: cfg.TLSSettings.PrivateKey,
								ShortID:    toList(cfg.TLSSettings.ShortID),
							},
						},
					},
				},
			},
		},
		Outbounds: []option.Outbound{
			{
				Type: "direct",
				Tag:  directOutboundTag,
			},
		},
		Route: routes,
		DNS:   e.buildDefaultDNSOptions(),
	}

	if strings.TrimSpace(e.clashAPIAddr) != "" {
		opts.Experimental = &option.ExperimentalOptions{
			ClashAPI: &option.ClashAPIOptions{
				ExternalController: strings.TrimSpace(e.clashAPIAddr),
			},
		}
	}

	if e.googleIPv6 {
		opts.Outbounds = append(opts.Outbounds, e.googleIPv6Outbound())
		opts.Route.Rules = append(opts.Route.Rules, e.googleIPv6Rule())
	}

	return opts, nil
}

func (e *Engine) buildRouteOptions() *option.RouteOptions {
	route := &option.RouteOptions{
		AutoDetectInterface: true,
		Final:               directOutboundTag,
	}

	// 注入安全规则
	route.Rules = e.defaultSafetyRules()

	return route
}

func (e *Engine) defaultSafetyRules() []option.Rule {
	return []option.Rule{
		// 1. 拦截 BitTorrent (P2P)
		e.rejectRule(option.RawDefaultRule{
			Protocol: badoption.Listable[string]{C.ProtocolBitTorrent},
		}),
		// 2. 拦截本地和私有 IP (直接路由 direct)
		e.routeRule(option.RawDefaultRule{
			IPIsPrivate: true,
		}),
		// 3. 拦截 SMTP 发信端口 (防止垃圾邮件)
		e.rejectRule(option.RawDefaultRule{
			Port: badoption.Listable[uint16]{25},
		}),
		// 4. 拦截常见高危端口 (SMB, RDP, NetBIOS)
		e.rejectRule(option.RawDefaultRule{
			Port:      badoption.Listable[uint16]{445, 3389},
			PortRange: badoption.Listable[string]{"135:139"},
		}),
	}
}

func (e *Engine) routeRule(raw option.RawDefaultRule) option.Rule {
	return option.Rule{
		Type: C.RuleTypeDefault,
		DefaultOptions: option.DefaultRule{
			RawDefaultRule: raw,
			RuleAction: option.RuleAction{
				Action: C.RuleActionTypeRoute,
				RouteOptions: option.RouteActionOptions{
					Outbound: directOutboundTag,
				},
			},
		},
	}
}

func (e *Engine) rejectRule(raw option.RawDefaultRule) option.Rule {
	return option.Rule{
		Type: C.RuleTypeDefault,
		DefaultOptions: option.DefaultRule{
			RawDefaultRule: raw,
			RuleAction: option.RuleAction{
				Action: C.RuleActionTypeReject,
				RejectOptions: option.RejectActionOptions{
					Method: "default",
				},
			},
		},
	}
}

func (e *Engine) buildDefaultDNSOptions() *option.DNSOptions {
	return &option.DNSOptions{
		RawDNSOptions: option.RawDNSOptions{
			Servers: []option.DNSServerOptions{
				{
					Type: "local",
					Tag:  localDNSOutboundTag,
					Options: &option.LocalDNSServerOptions{
						PreferGo: true,
					},
				},
			},
			Rules: []option.DNSRule{
				e.dnsRuleRejectDomain([]string{"ads", "tracker"}),
			},
			Final: localDNSOutboundTag,
		},
	}
}

func (e *Engine) dnsRuleRejectDomain(keywords []string) option.DNSRule {
	return option.DNSRule{
		Type: C.RuleTypeDefault,
		DefaultOptions: option.DefaultDNSRule{
			RawDefaultDNSRule: option.RawDefaultDNSRule{
				DomainKeyword: badoption.Listable[string](keywords),
			},
			DNSRuleAction: option.DNSRuleAction{
				Action: C.RuleActionTypeReject,
				RejectOptions: option.RejectActionOptions{
					Method: "default",
				},
			},
		},
	}
}

func (e *Engine) googleIPv6Outbound() option.Outbound {
	var ds option.DomainStrategy
	_ = ds.UnmarshalJSON([]byte(`"prefer_ipv6"`))
	return option.Outbound{
		Type: "direct",
		Tag:  directIPv6OutboundTag,
		Options: &option.DirectOutboundOptions{
			DialerOptions: option.DialerOptions{
				DomainStrategy: ds,
			},
		},
	}
}

func (e *Engine) googleIPv6Rule() option.Rule {
	return option.Rule{
		Type: C.RuleTypeDefault,
		DefaultOptions: option.DefaultRule{
			RawDefaultRule: option.RawDefaultRule{
				DomainSuffix: badoption.Listable[string]{
					"google.com",
					"googleapis.com",
					"gstatic.com",
					"googleusercontent.com",
					"youtube.com",
					"ytimg.com",
					"ggpht.com",
					"googlevideo.com",
				},
			},
			RuleAction: option.RuleAction{
				Action: C.RuleActionTypeRoute,
				RouteOptions: option.RouteActionOptions{
					Outbound: directIPv6OutboundTag,
				},
			},
		},
	}
}

func (e *Engine) parseRealityDestPort(raw string) (uint16, error) {
	if strings.TrimSpace(raw) == "" {
		return defaultRealityDestPort, nil
	}
	var port int
	_, err := fmt.Sscanf(strings.TrimSpace(raw), "%d", &port)
	if err != nil {
		return 0, fmt.Errorf("无效的 tls_settings.server_port %q", raw)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("tls_settings.server_port 超出端口范围 [1-65535]: %d", port)
	}
	return uint16(port), nil
}

func (e *Engine) resolveListenAddr(raw string) (netip.Addr, error) {
	if strings.TrimSpace(raw) == "" {
		return netip.IPv6Unspecified(), nil
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("无效的监听 IP 格式 %q: %w", raw, err)
	}
	return addr, nil
}

func toList[T any](values []T) badoption.Listable[T] {
	if len(values) == 0 {
		return nil
	}
	out := make(badoption.Listable[T], len(values))
	copy(out, values)
	return out
}
