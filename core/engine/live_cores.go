package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/transport/internet"
)

// Xray-core resolves sockopt.domainStrategy, freedom's domainStrategy and sockopt.dialerProxy
// through ONE dns client and outbound manager per process (internet.InitSystemDialer), and
// every core.New points them at its own core. With two cores alive, both would resolve and
// chain through the newer one, and through a closed one after it stops. So every core this
// package runs starts and closes here, one at a time, and with more than one alive those
// globals point at routers that hand each lookup to the core it belongs to.

// liveCore is one running core; hosts are the server domains a ping core dials, nil for the
// tunnel's core, which owns every other lookup.
type liveCore struct {
	server *core.Instance
	dns    dns.Client
	obm    outbound.Manager
	hosts  map[string]bool
}

var liveCores struct {
	sync.Mutex
	cores []*liveCore
}

// startCore creates and starts a core and routes the dialer globals; hosts as in liveCore.
func startCore(config *core.Config, hosts map[string]bool) (*core.Instance, error) {
	liveCores.Lock()
	defer liveCores.Unlock()
	server, err := core.New(config)
	if err != nil {
		routeDialerGlobals()
		return nil, err
	}
	dnsClient, _ := server.GetFeature(dns.ClientType()).(dns.Client)
	manager, _ := server.GetFeature(outbound.ManagerType()).(outbound.Manager)
	liveCores.cores = append(liveCores.cores, &liveCore{server: server, dns: dnsClient, obm: manager, hosts: hosts})
	routeDialerGlobals()
	if err := server.Start(); err != nil {
		_ = closeLocked(server)
		return nil, err
	}
	return server, nil
}

func closeCore(server *core.Instance) error {
	liveCores.Lock()
	defer liveCores.Unlock()
	return closeLocked(server)
}

func closeLocked(server *core.Instance) error {
	liveCores.cores = slices.DeleteFunc(liveCores.cores, func(c *liveCore) bool { return c.server == server })
	err := server.Close()
	routeDialerGlobals()
	return err
}

// withoutLiveCores runs call, which builds a core this package doesn't own (libXray's testXray
// and pingBatch), only while no core runs, and routes the globals back afterwards.
func withoutLiveCores(method string, call func() string) string {
	liveCores.Lock()
	defer liveCores.Unlock()
	if len(liveCores.cores) > 0 {
		return encode(nil, fmt.Errorf("%s requires a process without a running core", method))
	}
	defer routeDialerGlobals()
	return call()
}

func routeDialerGlobals() {
	cores := slices.Clone(liveCores.cores)
	switch len(cores) {
	case 0:
		internet.InitSystemDialer(nil, nil)
	case 1:
		internet.InitSystemDialer(cores[0].dns, cores[0].obm)
	default:
		internet.InitSystemDialer(dnsRouter(cores), outboundRouter(cores))
	}
}

// owner is the ping core that dials domain, else the tunnel's core, else the newest core.
func owner(cores []*liveCore, domain string) *liveCore {
	domain = strings.ToLower(domain)
	var tunnelCore *liveCore
	for _, c := range cores {
		if c.hosts == nil {
			tunnelCore = c
		} else if c.hosts[domain] {
			return c
		}
	}
	if tunnelCore != nil {
		return tunnelCore
	}
	return cores[len(cores)-1]
}

// dnsRouter resolves each domain with the DNS of the core it belongs to.
type dnsRouter []*liveCore

func (dnsRouter) Type() interface{} { return dns.ClientType() }
func (dnsRouter) Start() error      { return nil }
func (dnsRouter) Close() error      { return nil }

func (r dnsRouter) LookupIP(domain string, option dns.IPOption) ([]net.IP, uint32, error) {
	resolver := owner(r, domain).dns
	if resolver == nil {
		return nil, 0, errors.New("no running core resolves " + domain)
	}
	return resolver.LookupIP(domain, option)
}

// outboundRouter finds a dialerProxy's outbound: the ping cores own the bodo-ping- tags, the
// tunnel's core every other.
type outboundRouter []*liveCore

var errRouterOnly = errors.New("the dialer's outbound router only finds handlers")

func (outboundRouter) Type() interface{} { return outbound.ManagerType() }
func (outboundRouter) Start() error      { return nil }
func (outboundRouter) Close() error      { return nil }

func (r outboundRouter) GetHandler(tag string) outbound.Handler {
	ping := strings.HasPrefix(tag, pingTagPrefix)
	for _, c := range r {
		if (c.hosts != nil) != ping || c.obm == nil {
			continue
		}
		if handler := c.obm.GetHandler(tag); handler != nil {
			return handler
		}
	}
	return nil
}

func (r outboundRouter) GetDefaultHandler() outbound.Handler {
	for _, c := range r {
		if c.hosts == nil && c.obm != nil {
			return c.obm.GetDefaultHandler()
		}
	}
	return nil
}

func (outboundRouter) AddHandler(context.Context, outbound.Handler) error { return errRouterOnly }
func (outboundRouter) RemoveHandler(context.Context, string) error        { return errRouterOnly }
func (outboundRouter) ListHandlers(context.Context) []outbound.Handler    { return nil }
