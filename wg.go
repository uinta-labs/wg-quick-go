package wgctl

import (
	"bytes"
	"encoding/base64"
	"github.com/mdlayher/wireguardctrl"
	"github.com/mdlayher/wireguardctrl/wgtypes"
	log "github.com/sirupsen/logrus"
	"github.com/vishvananda/netlink"
	"net"
	"os/exec"
	"syscall"
	"text/template"
)

type Config struct {
	wgtypes.Config

	// Address — a comma-separated list of IP (v4 or v6) addresses (optionally with CIDR masks) to be assigned to the interface. May be specified multiple times.
	Address []*net.IPNet

	// — a comma-separated list of IP (v4 or v6) addresses to be set as the interface’s DNS servers. May be specified multiple times. Upon bringing the interface up, this runs ‘resolvconf -a tun.INTERFACE -m 0 -x‘ and upon bringing it down, this runs ‘resolvconf -d tun.INTERFACE‘. If these particular invocations of resolvconf(8) are undesirable, the PostUp and PostDown keys below may be used instead.
	DNS []net.IP
	// — if not specified, the MTU is automatically determined from the endpoint addresses or the system default route, which is usually a sane choice. However, to manually specify an MTU to override this automatic discovery, this value may be specified explicitly.
	MTU int

	// Table — Controls the routing table to which routes are added. There are two special values: ‘off’ disables the creation of routes altogether, and ‘auto’ (the default) adds routes to the default table and enables special handling of default routes.
	Table int

	// PreUp, PostUp, PreDown, PostDown — script snippets which will be executed by bash(1) before/after setting up/tearing down the interface, most commonly used to configure custom DNS options or firewall rules. The special string ‘%i’ is expanded to INTERFACE. Each one may be specified multiple times, in which case the commands are executed in order.

	PreUp    string
	PostUp   string
	PreDown  string
	PostDown string

	// SaveConfig — if set to ‘true’, the configuration is saved from the current state of the interface upon shutdown.
	SaveConfig bool
}

func (cfg *Config) String() string {
	b, err := cfg.MarshalText()
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (cfg *Config) MarshalText() (text []byte, err error) {
	buff := &bytes.Buffer{}
	if err := cfgTemplate.Execute(buff, cfg); err != nil {
		return nil, err
	}
	return buff.Bytes(), nil
}

const wgtypeTemplateSpec = `[Interface]
{{- range := .Address }}
Address = {{ . }}
{{ end }}
{{- range := .DNS }}
DNS = {{ . }}
{{ end }}
PrivateKey = {{ .PrivateKey | wgKey }}
{{- if .ListenPort }}{{ "\n" }}ListenPort = {{ .ListenPort }}{{ end }}
{{- if .MTU }}{{ "\n" }}MTU = {{ .MTU }}{{ end }}
{{- if .Table }}{{ "\n" }}Table = {{ .Table }}{{ end }}
{{- if .PreUp }}{{ "\n" }}PreUp = {{ .PreUp }}{{ end }}
{{- if .PostUp }}{{ "\n" }}Table = {{ .Table }}{{ end }}
{{- if .PreDown }}{{ "\n" }}PreDown = {{ .PreDown }}{{ end }}
{{- if .PostDown }}{{ "\n" }}PostDown = {{ .PostDown }}{{ end }}
{{- if .SaveConfig }}{{ "\n" }}SaveConfig = {{ .SaveConfig }}{{ end }}

{{- range .Peers }}
[Peer]
PublicKey = {{ .PublicKey | wgKey }}
AllowedIps = {{ range $i, $el := .AllowedIPs }}{{if $i}}, {{ end }}{{ $el }}{{ end }}
{{- if .Endpoint }}{{ "\n" }}Endpoint = {{ .Endpoint }}{{ end }}
{{- end }}
`

func serializeKey(key *wgtypes.Key) string {
	return base64.StdEncoding.EncodeToString(key[:])
}

func ParseKey(key string) (wgtypes.Key, error) {
	var pkey wgtypes.Key
	pkeySlice, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return pkey, err
	}
	copy(pkey[:], pkeySlice[:])
	return pkey, nil
}

var cfgTemplate = template.Must(
	template.
		New("wg-cfg").
		Funcs(template.FuncMap(map[string]interface{}{"wgKey": serializeKey})).
		Parse(wgtypeTemplateSpec))

func (cfg *Config) Up(iface string) error {

	link, err := netlink.LinkByName(iface)
	if err != nil {
		if _, ok := err.(netlink.LinkNotFoundError); !ok {
			log.Error(err, "cannot read link, probably doesn't exist")
			return err
		}
		log.Info("link not found, creating")
		wgLink := &netlink.GenericLink{
			LinkAttrs: netlink.LinkAttrs{
				Name: iface,
			},
			LinkType: "wireguard",
		}
		if err := netlink.LinkAdd(wgLink); err != nil {
			log.Error(err, "cannot create link", "iface", iface)
			return err
		}
		if err := exec.Command("ip", "link", "add", "dev", iface, "type", "wireguard").Run(); err != nil {
		}

		link, err = netlink.LinkByName(iface)
		if err != nil {
			log.Error(err, "cannot read link")
			return err
		}
	}
	log.Info("link", "type", link.Type(), "attrs", link.Attrs())
	if err := netlink.LinkSetUp(link); err != nil {
		log.Error(err, "cannot set link up", "type", link.Type(), "attrs", link.Attrs())
		return err
	}
	log.Info("set device up", "iface", iface)

	cl, err := wireguardctrl.New()
	if err != nil {
		log.Error(err, "cannot setup wireguard device")
		return err
	}

	if err := cl.ConfigureDevice(iface, cfg.Config); err != nil {
		log.Error(err, "cannot configure device", "iface", iface)
		return err
	}

	if err := syncAddress(link, cfg); err != nil {
		log.Error(err, "cannot sync addresses")
		return err
	}

	if err := syncRoutes(link, cfg); err != nil {
		log.Error(err, "cannot sync routes")
		return err
	}

	log.Info("Successfully setup device", "iface", iface)
	return nil

}

func syncAddress(link netlink.Link, cfg *Config) error {
	addrs, err := netlink.AddrList(link, syscall.AF_INET)
	if err != nil {
		log.Error(err, "cannot read link address")
		return err
	}

	presentAddresses := make(map[string]int, 0)
	for _, addr := range addrs {
		presentAddresses[addr.IPNet.String()] = 1
	}

	for _, addr := range cfg.Address {
		_, present := presentAddresses[addr.String()]
		presentAddresses[addr.String()] = 2
		if present {
			log.Info("address present", "addr", addr, "iface", link.Attrs().Name)
			continue
		}

		if err := netlink.AddrAdd(link, &netlink.Addr{
			IPNet: addr,
		}); err != nil {
			log.Error(err, "cannot add addr", "iface", link.Attrs().Name)
			return err
		}
		log.Info("address added", "addr", addr, "iface", link.Attrs().Name)
	}

	for addr, p := range presentAddresses {
		if p < 2 {
			nlAddr, err := netlink.ParseAddr(addr)
			if err != nil {
				log.Error(err, "cannot parse del addr", "iface", link.Attrs().Name, "addr", addr)
				return err
			}
			if err := netlink.AddrAdd(link, nlAddr); err != nil {
				log.Error(err, "cannot delete addr", "iface", link.Attrs().Name, "addr", addr)
				return err
			}
			log.Info("address deleted", "addr", addr, "iface", link.Attrs().Name)
		}
	}
	return nil
}

func syncRoutes(link netlink.Link, cfg *Config) error {
	routes, err := netlink.RouteList(link, syscall.AF_INET)
	if err != nil {
		log.Error(err, "cannot read existing routes")
		return err
	}

	presentRoutes := make(map[string]int, 0)
	for _, r := range routes {
		presentRoutes[r.Dst.String()] = 1
	}

	for _, peer := range cfg.Peers {
		for _, rt := range peer.AllowedIPs {
			_, present := presentRoutes[rt.String()]
			presentRoutes[rt.String()] = 2
			if present {
				log.Info("route present", "iface", link.Attrs().Name, "route", rt.String())
				continue
			}
			if err := netlink.RouteAdd(&netlink.Route{
				LinkIndex: link.Attrs().Index,
				Dst:       &rt,
			}); err != nil {
				log.Error(err, "cannot setup route", "iface", link.Attrs().Name, "route", rt.String())
				return err
			}
			log.Info("route added", "iface", link.Attrs().Name, "route", rt.String())
		}
	}

	// Clean extra routes
	for rtStr, p := range presentRoutes {
		_, rt, err := net.ParseCIDR(rtStr)
		if err != nil {
			log.Info("cannot parse route", "iface", link.Attrs().Name, "route", rtStr)
			return err
		}
		if p < 2 {
			log.Info("extra manual route found", "iface", link.Attrs().Name, "route", rt.String())
			if err := netlink.RouteDel(&netlink.Route{
				LinkIndex: link.Attrs().Index,
				Dst:       rt,
			}); err != nil {
				log.Error(err, "cannot setup route", "iface", link.Attrs().Name, "route", rt.String())
				return err
			}
			log.Info("route deleted", "iface", link.Attrs().Name, "route", rt)
		}
	}
	return nil
}