//go:build !android

package dns

import (
	"context"
	"encoding/binary"
	"fmt"
	"net/netip"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/hashicorp/go-version"
	"github.com/miekg/dns"
	nbversion "github.com/netbirdio/netbird/version"
	log "github.com/sirupsen/logrus"
)

const (
	networkManagerDest                                                              = "org.freedesktop.NetworkManager"
	networkManagerDbusObjectNode                                                    = "/org/freedesktop/NetworkManager"
	networkManagerDbusDNSManagerInterface                                           = "org.freedesktop.NetworkManager.DnsManager"
	networkManagerDbusDNSManagerObjectNode                                          = networkManagerDbusObjectNode + "/DnsManager"
	networkManagerDbusDNSManagerModeProperty                                        = networkManagerDbusDNSManagerInterface + ".Mode"
	networkManagerDbusDNSManagerRcManagerProperty                                   = networkManagerDbusDNSManagerInterface + ".RcManager"
	networkManagerDbusVersionProperty                                               = "org.freedesktop.NetworkManager.Version"
	networkManagerDbusGetDeviceByIPIfaceMethod                                      = networkManagerDest + ".GetDeviceByIpIface"
	networkManagerDbusDeviceInterface                                               = "org.freedesktop.NetworkManager.Device"
	networkManagerDbusDeviceGetAppliedConnectionMethod                              = networkManagerDbusDeviceInterface + ".GetAppliedConnection"
	networkManagerDbusDeviceReapplyMethod                                           = networkManagerDbusDeviceInterface + ".Reapply"
	networkManagerDbusDeviceDeleteMethod                                            = networkManagerDbusDeviceInterface + ".Delete"
	networkManagerDbusDefaultBehaviorFlag              networkManagerConfigBehavior = 0
	networkManagerDbusIPv4Key                                                       = "ipv4"
	networkManagerDbusIPv6Key                                                       = "ipv6"
	networkManagerDbusDNSKey                                                        = "dns"
	networkManagerDbusDNSSearchKey                                                  = "dns-search"
	networkManagerDbusDNSPriorityKey                                                = "dns-priority"

	// dns priority doc https://wiki.gnome.org/Projects/NetworkManager/DNS
	networkManagerDbusPrimaryDNSPriority       int32 = -500
	networkManagerDbusWithMatchDomainPriority  int32 = 0
	networkManagerDbusSearchDomainOnlyPriority int32 = 50
	supportedNetworkManagerVersionConstraint         = ">= 1.16, < 1.28"
)

type networkManagerDbusConfigurator struct {
	dbusLinkObject dbus.ObjectPath
	routingAll     bool
}

// the types below are based on dbus specification, each field is mapped to a dbus type
// see https://dbus.freedesktop.org/doc/dbus-specification.html#basic-types for more details on dbus types
// see https://networkmanager.dev/docs/api/latest/gdbus-org.freedesktop.NetworkManager.Device.html on Network Manager input types

// networkManagerConnSettings maps to a (a{sa{sv}}) dbus output from GetAppliedConnection and input for Reapply methods
type networkManagerConnSettings map[string]map[string]dbus.Variant

// networkManagerConfigVersion maps to a (t) dbus output from GetAppliedConnection and input for Reapply methods
type networkManagerConfigVersion uint64

// networkManagerConfigBehavior maps to a (u) dbus input for GetAppliedConnection and Reapply methods
type networkManagerConfigBehavior uint32

// cleanDeprecatedSettings cleans deprecated settings that still returned by
// the GetAppliedConnection methods but can't be reApplied
func (s networkManagerConnSettings) cleanDeprecatedSettings() {
	for _, key := range []string{"addresses", "routes"} {
		delete(s[networkManagerDbusIPv4Key], key)
		delete(s[networkManagerDbusIPv6Key], key)
	}
}

func newNetworkManagerDbusConfigurator(wgInterface WGIface) (hostManager, error) {
	obj, closeConn, err := getDbusObject(networkManagerDest, networkManagerDbusObjectNode)
	if err != nil {
		return nil, err
	}
	defer closeConn()
	var s string
	err = obj.Call(networkManagerDbusGetDeviceByIPIfaceMethod, dbusDefaultFlag, wgInterface.Name()).Store(&s)
	if err != nil {
		return nil, err
	}

	log.Debugf("got network manager dbus Link Object: %s from net interface %s", s, wgInterface.Name())

	return &networkManagerDbusConfigurator{
		dbusLinkObject: dbus.ObjectPath(s),
	}, nil
}

func (n *networkManagerDbusConfigurator) supportCustomPort() bool {
	return false
}

func (n *networkManagerDbusConfigurator) applyDNSConfig(config hostDNSConfig) error {
	connSettings, configVersion, err := n.getAppliedConnectionSettings()
	if err != nil {
		return fmt.Errorf("got an error while retrieving the applied connection settings, error: %s", err)
	}

	connSettings.cleanDeprecatedSettings()

	dnsIP, err := netip.ParseAddr(config.serverIP)
	if err != nil {
		return fmt.Errorf("unable to parse ip address, error: %s", err)
	}
	convDNSIP := binary.LittleEndian.Uint32(dnsIP.AsSlice())
	connSettings[networkManagerDbusIPv4Key][networkManagerDbusDNSKey] = dbus.MakeVariant([]uint32{convDNSIP})
	var (
		searchDomains []string
		matchDomains  []string
	)
	for _, dConf := range config.domains {
		if dConf.disabled {
			continue
		}
		if dConf.matchOnly {
			matchDomains = append(matchDomains, "~."+dns.Fqdn(dConf.domain))
			continue
		}
		searchDomains = append(searchDomains, dns.Fqdn(dConf.domain))
	}

	newDomainList := append(searchDomains, matchDomains...) //nolint:gocritic

	priority := networkManagerDbusSearchDomainOnlyPriority
	switch {
	case config.routeAll:
		priority = networkManagerDbusPrimaryDNSPriority
		newDomainList = append(newDomainList, "~.")
		if !n.routingAll {
			log.Infof("configured %s:%d as main DNS forwarder for this peer", config.serverIP, config.serverPort)
		}
	case len(matchDomains) > 0:
		priority = networkManagerDbusWithMatchDomainPriority
	}

	if priority != networkManagerDbusPrimaryDNSPriority && n.routingAll {
		log.Infof("removing %s:%d as main DNS forwarder for this peer", config.serverIP, config.serverPort)
		n.routingAll = false
	}

	connSettings[networkManagerDbusIPv4Key][networkManagerDbusDNSPriorityKey] = dbus.MakeVariant(priority)
	connSettings[networkManagerDbusIPv4Key][networkManagerDbusDNSSearchKey] = dbus.MakeVariant(newDomainList)

	log.Infof("adding %d search domains and %d match domains. Search list: %s , Match list: %s", len(searchDomains), len(matchDomains), searchDomains, matchDomains)
	err = n.reApplyConnectionSettings(connSettings, configVersion)
	if err != nil {
		return fmt.Errorf("got an error while reapplying the connection with new settings, error: %s", err)
	}
	return nil
}

func (n *networkManagerDbusConfigurator) restoreHostDNS() error {
	// once the interface is gone network manager cleans all config associated with it
	return n.deleteConnectionSettings()
}

func (n *networkManagerDbusConfigurator) getAppliedConnectionSettings() (networkManagerConnSettings, networkManagerConfigVersion, error) {
	obj, closeConn, err := getDbusObject(networkManagerDest, n.dbusLinkObject)
	if err != nil {
		return nil, 0, fmt.Errorf("got error while attempting to retrieve the applied connection settings, err: %s", err)
	}
	defer closeConn()

	ctx, cancel := context.WithTimeout(context.TODO(), 5*time.Second)
	defer cancel()

	var (
		connSettings  networkManagerConnSettings
		configVersion networkManagerConfigVersion
	)

	err = obj.CallWithContext(ctx, networkManagerDbusDeviceGetAppliedConnectionMethod, dbusDefaultFlag,
		networkManagerDbusDefaultBehaviorFlag).Store(&connSettings, &configVersion)
	if err != nil {
		return nil, 0, fmt.Errorf("got error while calling GetAppliedConnection method with context, err: %s", err)
	}

	return connSettings, configVersion, nil
}

func (n *networkManagerDbusConfigurator) reApplyConnectionSettings(connSettings networkManagerConnSettings, configVersion networkManagerConfigVersion) error {
	obj, closeConn, err := getDbusObject(networkManagerDest, n.dbusLinkObject)
	if err != nil {
		return fmt.Errorf("got error while attempting to retrieve the applied connection settings, err: %s", err)
	}
	defer closeConn()

	ctx, cancel := context.WithTimeout(context.TODO(), 5*time.Second)
	defer cancel()

	err = obj.CallWithContext(ctx, networkManagerDbusDeviceReapplyMethod, dbusDefaultFlag,
		connSettings, configVersion, networkManagerDbusDefaultBehaviorFlag).Store()
	if err != nil {
		return fmt.Errorf("got error while calling ReApply method with context, err: %s", err)
	}

	return nil
}

func (n *networkManagerDbusConfigurator) deleteConnectionSettings() error {
	obj, closeConn, err := getDbusObject(networkManagerDest, n.dbusLinkObject)
	if err != nil {
		return fmt.Errorf("got error while attempting to retrieve the applied connection settings, err: %s", err)
	}
	defer closeConn()

	ctx, cancel := context.WithTimeout(context.TODO(), 5*time.Second)
	defer cancel()

	err = obj.CallWithContext(ctx, networkManagerDbusDeviceDeleteMethod, dbusDefaultFlag).Store()
	if err != nil {
		return fmt.Errorf("got error while calling delete method with context, err: %s", err)
	}

	return nil
}

func isNetworkManagerSupported() bool {
	return isNetworkManagerSupportedVersion() && isNetworkManagerSupportedMode()
}

func isNetworkManagerSupportedMode() bool {
	var mode string
	err := getNetworkManagerDNSProperty(networkManagerDbusDNSManagerModeProperty, &mode)
	if err != nil {
		log.Error(err)
		return false
	}
	switch mode {
	case "dnsmasq", "unbound", "systemd-resolved":
		return true
	default:
		var rcManager string
		err = getNetworkManagerDNSProperty(networkManagerDbusDNSManagerRcManagerProperty, &rcManager)
		if err != nil {
			log.Error(err)
			return false
		}
		if rcManager == "unmanaged" {
			return false
		}
	}
	return true
}

func getNetworkManagerDNSProperty(property string, store any) error {
	obj, closeConn, err := getDbusObject(networkManagerDest, networkManagerDbusDNSManagerObjectNode)
	if err != nil {
		return fmt.Errorf("got error while attempting to retrieve the network manager dns manager object, error: %s", err)
	}
	defer closeConn()

	v, e := obj.GetProperty(property)
	if e != nil {
		return fmt.Errorf("got an error getting property %s: %v", property, e)
	}

	return v.Store(store)
}

func isNetworkManagerSupportedVersion() bool {
	obj, closeConn, err := getDbusObject(networkManagerDest, networkManagerDbusObjectNode)
	if err != nil {
		log.Errorf("got error while attempting to get the network manager object, err: %s", err)
		return false
	}

	defer closeConn()

	value, err := obj.GetProperty(networkManagerDbusVersionProperty)
	if err != nil {
		log.Errorf("unable to retrieve network manager mode, got error: %s", err)
		return false
	}
	versionValue, err := parseVersion(value.Value().(string))
	if err != nil {
		return false
	}

	constraints, err := version.NewConstraint(supportedNetworkManagerVersionConstraint)
	if err != nil {
		return false
	}

	return constraints.Check(versionValue)
}

func parseVersion(inputVersion string) (*version.Version, error) {
	if inputVersion == "" || !nbversion.SemverRegexp.MatchString(inputVersion) {
		return nil, fmt.Errorf("couldn't parse the provided version: Not SemVer")
	}

	verObj, err := version.NewVersion(inputVersion)
	if err != nil {
		return nil, err
	}

	return verObj, nil
}
