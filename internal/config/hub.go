package config

import (
	"errors"
	"fmt"
	"net/netip"
	"os"
	"slices"
	"strings"
)

// HubConfig is the [hub] section (#2710): the hub API that gitmoot clients on
// other machines call. Every request needs a client certificate from the
// hub's own certificate authority (`gitmoot hub machine add`).
type HubConfig struct {
	// Listen is the bind address, an address on the Tailscale network with a
	// non-zero port. Empty (the default) runs no hub.
	Listen string
	// Advertise are extra host names or addresses for the server certificate,
	// such as the machine's MagicDNS name. The Listen address is always in it.
	Advertise []string
	// Allow are the endpoint groups (HubGroups) remote machines may use.
	// Absent, it is DefaultHubAllow; `allow = []` leaves only /v1/whoami.
	Allow []string
}

// HubGroups are the hub's endpoint groups, in display order:
//
//	messages       /v1/messages...
//	notifications  /v1/notifications/... (claiming the caller's own mail)
//	org            /v1/org/...
//	tasks          /v1/tasks/...
//	reviews        /v1/reviews...
var HubGroups = []string{"messages", "notifications", "org", "tasks", "reviews"}

// DefaultHubAllow is messaging: sending and reading messages, and the
// notification claims a remote seat needs to receive its own mail.
var DefaultHubAllow = []string{"messages", "notifications"}

// Tailscale assigns node addresses from these prefixes only.
var TailscalePrefixes = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

// ValidateHubListen parses a [hub] listen address and refuses anything that is
// not a single Tailscale address with a port: no wildcard, no host name, no
// public or loopback address. The hub can therefore never listen on a public
// interface by mistake.
func ValidateHubListen(raw string) (netip.AddrPort, error) {
	address, err := netip.ParseAddrPort(strings.TrimSpace(raw))
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("[hub].listen %q: want <tailscale-ip>:<port>: %w", raw, err)
	}
	if address.Port() == 0 {
		return netip.AddrPort{}, fmt.Errorf("[hub].listen %q: the port must not be 0", raw)
	}
	ip := address.Addr().Unmap()
	if ip.Zone() != "" {
		return netip.AddrPort{}, fmt.Errorf("[hub].listen %q: a zoned address is not a Tailscale address", raw)
	}
	for _, prefix := range TailscalePrefixes {
		if prefix.Contains(ip) {
			return netip.AddrPortFrom(ip, address.Port()), nil
		}
	}
	return netip.AddrPort{}, fmt.Errorf("[hub].listen %q: %s is not a Tailscale address (100.64.0.0/10 or fd7a:115c:a1e0::/48)", raw, ip)
}

// LoadHubConfig reads [hub]. A missing file or section runs no hub. Unknown
// keys, an invalid listen address and a malformed [hub header are refused.
func LoadHubConfig(paths Paths) (HubConfig, error) {
	var cfg HubConfig
	content, err := os.ReadFile(paths.ConfigFile)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return HubConfig{}, err
	}
	current := false
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(stripConfigComment(raw))
		if line == "" {
			continue
		}
		if section, ok := sectionHeader(line); ok {
			if section == "" && malformedHeaderTargets(line, "hub") {
				return HubConfig{}, fmt.Errorf("malformed section header %q", line)
			}
			current = section == "hub"
			continue
		}
		if !current {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return HubConfig{}, fmt.Errorf("parse [hub]: expected key = value, got %q", line)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "listen":
			if cfg.Listen, err = parseConfigString(value); err != nil {
				return HubConfig{}, fmt.Errorf("parse [hub].listen: %w", err)
			}
		case "advertise":
			if cfg.Advertise, err = parseConfigStringArray(value); err != nil {
				return HubConfig{}, fmt.Errorf("parse [hub].advertise: %w", err)
			}
			for _, host := range cfg.Advertise {
				if strings.TrimSpace(host) == "" || strings.ContainsAny(host, " /:") && !isIPAddress(host) {
					return HubConfig{}, fmt.Errorf("parse [hub].advertise: %q is not a host name or address", host)
				}
			}
		case "allow":
			allow, err := parseConfigStringArray(value)
			if err != nil {
				return HubConfig{}, fmt.Errorf("parse [hub].allow: %w", err)
			}
			cfg.Allow = make([]string, 0, len(allow))
			for _, group := range allow {
				group = strings.ToLower(strings.TrimSpace(group))
				if !slices.Contains(HubGroups, group) {
					return HubConfig{}, fmt.Errorf("parse [hub].allow: unknown group %q (want any of %s)", group, strings.Join(HubGroups, ", "))
				}
				if !slices.Contains(cfg.Allow, group) {
					cfg.Allow = append(cfg.Allow, group)
				}
			}
		default:
			return HubConfig{}, fmt.Errorf("parse [hub]: unknown key %q", key)
		}
	}
	if cfg.Allow == nil {
		cfg.Allow = slices.Clone(DefaultHubAllow)
	}
	if cfg.Listen != "" {
		if _, err := ValidateHubListen(cfg.Listen); err != nil {
			return HubConfig{}, err
		}
	} else if len(cfg.Advertise) > 0 {
		return HubConfig{}, errors.New("[hub].advertise needs [hub].listen")
	}
	return cfg, nil
}

func isIPAddress(host string) bool {
	_, err := netip.ParseAddr(host)
	return err == nil
}
