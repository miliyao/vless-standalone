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

// Engine manages the embedded sing-box lifecycle and config reloads.
type Engine struct {
	mu        sync.Mutex
	instance  *box.Box
	limiter   *Limiter
	logger    *zap.Logger
	activeCfg *Config
}

func NewEngine(cfg *Config, limiter *Limiter, logger *zap.Logger) *Engine {
	return &Engine{
		limiter: limiter,
		logger:  logger,
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
		return fmt.Errorf("start sing-box core: %w", err)
	}

	e.instance = instance
	e.activeCfg = cfg
	return nil
}

func (e *Engine) Reload(cfg *Config) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	e.logger.Info("starting config reload; listener rebinding may cause a brief interruption")
	newInstance, err := e.createBox(cfg)
	if err != nil {
		return fmt.Errorf("build new sing-box config: %w", err)
	}

	oldInstance := e.instance
	startTime := time.Now()

	if oldInstance != nil {
		_ = oldInstance.Close()
	}

	if err := newInstance.Start(); err != nil {
		e.logger.Error("new core instance failed to start; attempting rollback", zap.Error(err))
		_ = newInstance.Close()

		if e.activeCfg != nil {
			fallbackInstance, rollbackErr := e.createBox(e.activeCfg)
			if rollbackErr == nil {
				if rollbackStartErr := fallbackInstance.Start(); rollbackStartErr == nil {
					e.instance = fallbackInstance
					e.logger.Warn("service rolled back to previous config")
					return fmt.Errorf("start new config failed: %w; rolled back to previous config", err)
				}
				_ = fallbackInstance.Close()
			}
		}
		return fmt.Errorf("start new config failed and rollback failed: %w", err)
	}

	e.instance = newInstance
	e.activeCfg = cfg
	e.logger.Info("config reload completed", zap.Duration("interruption_duration", time.Since(startTime)))
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
		return nil, fmt.Errorf("initialize sing-box instance: %w", err)
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

	if strings.TrimSpace(cfg.ClashAPIListenAddr) != "" {
		opts.Experimental = &option.ExperimentalOptions{
			ClashAPI: &option.ClashAPIOptions{
				ExternalController: strings.TrimSpace(cfg.ClashAPIListenAddr),
			},
		}
	}

	if cfg.GoogleIPv6 {
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
	route.Rules = e.defaultSafetyRules()
	return route
}

func (e *Engine) defaultSafetyRules() []option.Rule {
	return []option.Rule{
		e.rejectRule(option.RawDefaultRule{
			Protocol: badoption.Listable[string]{C.ProtocolBitTorrent},
		}),
		e.routeRule(option.RawDefaultRule{
			IPIsPrivate: true,
		}),
		e.rejectRule(option.RawDefaultRule{
			Port: badoption.Listable[uint16]{25},
		}),
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
		return 0, fmt.Errorf("invalid tls_settings.server_port %q", raw)
	}
	if port < 1 || port > 65535 {
		return 0, fmt.Errorf("tls_settings.server_port out of range [1-65535]: %d", port)
	}
	return uint16(port), nil
}

func (e *Engine) resolveListenAddr(raw string) (netip.Addr, error) {
	if strings.TrimSpace(raw) == "" {
		return netip.IPv6Unspecified(), nil
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("invalid listen_ip %q: %w", raw, err)
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
