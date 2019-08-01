package net

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"

	bosharp "github.com/cloudfoundry/bosh-agent/platform/net/arp"
	boship "github.com/cloudfoundry/bosh-agent/platform/net/ip"
	boshsettings "github.com/cloudfoundry/bosh-agent/settings"
	bosherr "github.com/cloudfoundry/bosh-utils/errors"
	boshlog "github.com/cloudfoundry/bosh-utils/logger"
	boshsys "github.com/cloudfoundry/bosh-utils/system"
)

const UbuntuNetManagerLogTag = "UbuntuNetManager"

type UbuntuNetManager struct {
	cmdRunner                     boshsys.CmdRunner
	fs                            boshsys.FileSystem
	ipResolver                    boship.Resolver
	macAddressDetector            MACAddressDetector
	interfaceConfigurationCreator InterfaceConfigurationCreator
	interfaceAddressesValidator   boship.InterfaceAddressesValidator
	dnsValidator                  DNSValidator
	addressBroadcaster            bosharp.AddressBroadcaster
	kernelIPv6                    KernelIPv6
	logger                        boshlog.Logger
}

func NewUbuntuNetManager(
	fs boshsys.FileSystem,
	cmdRunner boshsys.CmdRunner,
	ipResolver boship.Resolver,
	macAddressDetector MACAddressDetector,
	interfaceConfigurationCreator InterfaceConfigurationCreator,
	interfaceAddressesValidator boship.InterfaceAddressesValidator,
	dnsValidator DNSValidator,
	addressBroadcaster bosharp.AddressBroadcaster,
	kernelIPv6 KernelIPv6,
	logger boshlog.Logger,
) Manager {
	return UbuntuNetManager{
		cmdRunner:                     cmdRunner,
		fs:                            fs,
		ipResolver:                    ipResolver,
		macAddressDetector:            macAddressDetector,
		interfaceConfigurationCreator: interfaceConfigurationCreator,
		interfaceAddressesValidator:   interfaceAddressesValidator,
		dnsValidator:                  dnsValidator,
		addressBroadcaster:            addressBroadcaster,
		kernelIPv6:                    kernelIPv6,
		logger:                        logger,
	}
}

// DHCP Config file - /etc/dhcp/dhclient.conf
// Ubuntu 14.04 accepts several DNS as a list in a single prepend directive
const ubuntuDHCPConfigTemplate = `# Generated by bosh-agent

option rfc3442-classless-static-routes code 121 = array of unsigned integer 8;

send host-name = gethostname();

request subnet-mask, broadcast-address, time-offset, routers,
	domain-name, domain-name-servers, domain-search, host-name,
	netbios-name-servers, netbios-scope, interface-mtu,
	rfc3442-classless-static-routes, ntp-servers;
{{ if . }}
prepend domain-name-servers {{ . }};{{ end }}
`

func (net UbuntuNetManager) ComputeNetworkConfig(networks boshsettings.Networks) ([]StaticInterfaceConfiguration, []DHCPInterfaceConfiguration, []string, error) {
	nonVipNetworks := boshsettings.Networks{}
	for networkName, networkSettings := range networks {
		if networkSettings.IsVIP() {
			continue
		}
		nonVipNetworks[networkName] = networkSettings
	}

	staticConfigs, dhcpConfigs, err := net.buildInterfaces(nonVipNetworks)
	if err != nil {
		return nil, nil, nil, err
	}

	dnsNetwork, _ := nonVipNetworks.DefaultNetworkFor("dns")
	dnsServers := dnsNetwork.DNS
	return staticConfigs, dhcpConfigs, dnsServers, nil
}

func (net UbuntuNetManager) SetupIPv6(config boshsettings.IPv6, stopCh <-chan struct{}) error {
	if config.Enable {
		return net.kernelIPv6.Enable(stopCh)
	}
	return nil
}

func (net UbuntuNetManager) SetupNetworking(networks boshsettings.Networks, errCh chan error) error {
	if networks.IsPreconfigured() {
		// Note in this case IPs are not broadcast
		return net.writeResolvConf(networks)
	}

	staticConfigs, dhcpConfigs, dnsServers, err := net.ComputeNetworkConfig(networks)
	if err != nil {
		return bosherr.WrapError(err, "Computing network configuration")
	}

	if StaticInterfaceConfigurations(staticConfigs).HasVersion6() {
		err := net.kernelIPv6.Enable(make(chan struct{}))
		if err != nil {
			return bosherr.WrapError(err, "Enabling IPv6 in kernel")
		}
	}

	changed, err := net.writeNetConfigs(dhcpConfigs, staticConfigs, dnsServers, boshsys.ConvergeFileContentsOpts{})
	if err != nil {
		return bosherr.WrapError(err, "Updating network configs")
	}

	if changed {
		err = net.removeDhcpDNSConfiguration()
		if err != nil {
			return err
		}

		err := net.restartNetworking()
		if err != nil {
			return bosherr.WrapError(err, "Failure restarting networking")
		}
	}

	staticAddresses, dynamicAddresses := net.ifaceAddresses(staticConfigs, dhcpConfigs)

	var staticAddressesWithoutVirtual []boship.InterfaceAddress
	r, err := regexp.Compile(`:\d+`)
	if err != nil {
		return bosherr.WrapError(err, "There is a problem with your regexp: ':\\d+'. That is used to skip validation of virtual interfaces(e.g., eth0:0, eth0:1)")
	}
	for _, addr := range staticAddresses {
		if r.MatchString(addr.GetInterfaceName()) == true {
			continue
		} else {
			staticAddressesWithoutVirtual = append(staticAddressesWithoutVirtual, addr)
		}
	}
	err = net.interfaceAddressesValidator.Validate(staticAddressesWithoutVirtual)
	if err != nil {
		return bosherr.WrapError(err, "Validating static network configuration")
	}

	err = net.dnsValidator.Validate(dnsServers)
	if err != nil {
		return bosherr.WrapError(err, "Validating dns configuration")
	}

	go net.addressBroadcaster.BroadcastMACAddresses(append(staticAddressesWithoutVirtual, dynamicAddresses...))

	return nil
}

func (net UbuntuNetManager) writeNetConfigs(
	dhcpConfigs DHCPInterfaceConfigurations,
	staticConfigs StaticInterfaceConfigurations,
	dnsServers []string,
	opts boshsys.ConvergeFileContentsOpts) (bool, error) {

	interfacesChanged, err := net.writeNetworkInterfaces(dhcpConfigs, staticConfigs, dnsServers, opts)
	if err != nil {
		return false, bosherr.WrapError(err, "Writing network configuration")
	}

	dhcpChanged := false

	if len(dhcpConfigs) > 0 {
		dhcpChanged, err = net.writeDHCPConfiguration(dnsServers, opts)
		if err != nil {
			return false, err
		}
	}

	return (interfacesChanged || dhcpChanged), nil
}

func (net UbuntuNetManager) GetConfiguredNetworkInterfaces() ([]string, error) {
	interfaces := []string{}

	interfacesByMacAddress, err := net.macAddressDetector.DetectMacAddresses()
	if err != nil {
		return interfaces, bosherr.WrapError(err, "Getting network interfaces")
	}

	for _, iface := range interfacesByMacAddress {
		_, stderr, _, err := net.cmdRunner.RunCommand("ip", "link", "show", iface)
		if err != nil {
			net.logger.Error(UbuntuNetManagerLogTag, "Ignoring failures to get network interface: %s", err)
		}

		re := regexp.MustCompile(fmt.Sprintf(`Device "%s" does not exist`, iface))

		if !re.MatchString(stderr) {
			interfaces = append(interfaces, iface)
		}
	}

	return interfaces, nil
}

func (net UbuntuNetManager) removeDhcpDNSConfiguration() error {
	// Removing dhcp configuration from /etc/network/interfaces
	// and restarting network does not stop dhclient if dhcp
	// is no longer needed. See https://bugs.launchpad.net/ubuntu/+source/dhcp3/+bug/38140
	_, _, _, err := net.cmdRunner.RunCommand("pkill", "dhclient")
	if err != nil {
		net.logger.Error(UbuntuNetManagerLogTag, "Ignoring failure calling 'pkill dhclient': %s", err)
	}

	interfacesByMacAddress, err := net.macAddressDetector.DetectMacAddresses()
	if err != nil {
		return err
	}

	for _, ifaceName := range interfacesByMacAddress {
		// Explicitly delete the resolvconf record about given iface
		// It seems to hold on to old dhclient records after dhcp configuration
		// is removed from /etc/network/interfaces.
		_, _, _, err = net.cmdRunner.RunCommand("resolvconf", "-d", ifaceName+".dhclient")
		if err != nil {
			net.logger.Error(UbuntuNetManagerLogTag, "Ignoring failure calling 'resolvconf -d %s.dhclient': %s", ifaceName, err)
		}
	}

	return nil
}

func (net UbuntuNetManager) buildInterfaces(networks boshsettings.Networks) ([]StaticInterfaceConfiguration, []DHCPInterfaceConfiguration, error) {
	interfacesByMacAddress, err := net.macAddressDetector.DetectMacAddresses()
	if err != nil {
		return nil, nil, bosherr.WrapError(err, "Getting network interfaces")
	}

	// if len(interfacesByMacAddress) == 0 {
	// 	return nil, nil, bosherr.Error("No network interfaces found")
	// }

	staticConfigs, dhcpConfigs, err := net.interfaceConfigurationCreator.CreateInterfaceConfigurations(networks, interfacesByMacAddress)
	if err != nil {
		return nil, nil, bosherr.WrapError(err, "Creating interface configurations")
	}

	return staticConfigs, dhcpConfigs, nil
}

func (net UbuntuNetManager) ifaceAddresses(staticConfigs []StaticInterfaceConfiguration, dhcpConfigs []DHCPInterfaceConfiguration) ([]boship.InterfaceAddress, []boship.InterfaceAddress) {
	staticAddresses := []boship.InterfaceAddress{}
	for _, iface := range staticConfigs {
		staticAddresses = append(staticAddresses, boship.NewSimpleInterfaceAddress(iface.Name, iface.Address))
	}
	dynamicAddresses := []boship.InterfaceAddress{}
	for _, iface := range dhcpConfigs {
		dynamicAddresses = append(dynamicAddresses, boship.NewResolvingInterfaceAddress(iface.Name, net.ipResolver))
	}

	return staticAddresses, dynamicAddresses
}

func (net UbuntuNetManager) restartNetworking() error {
	_, _, _, err := net.cmdRunner.RunCommand("/var/vcap/bosh/bin/restart_networking")
	if err != nil {
		return err
	}
	return nil
}

func (net UbuntuNetManager) writeDHCPConfiguration(dnsServers []string, opts boshsys.ConvergeFileContentsOpts) (bool, error) {
	buffer := bytes.NewBuffer([]byte{})
	t := template.Must(template.New("dhcp-config").Parse(ubuntuDHCPConfigTemplate))

	// Keep DNS servers in the order specified by the network
	// because they are added by a *single* DHCP's prepend command
	dnsServersList := strings.Join(dnsServers, ", ")
	err := t.Execute(buffer, dnsServersList)
	if err != nil {
		return false, bosherr.WrapError(err, "Generating config from template")
	}

	dhclientConfigFile := "/etc/dhcp/dhclient.conf"

	changed, err := net.fs.ConvergeFileContents(dhclientConfigFile, buffer.Bytes(), opts)
	if err != nil {
		return changed, bosherr.WrapErrorf(err, "Writing to %s", dhclientConfigFile)
	}

	return changed, nil
}

type networkInterfaceConfig struct {
	DNSServers        []string
	InterfaceConfig   interface{}
	HasDNSNameServers bool
}

func (net UbuntuNetManager) updateConfiguration(name, templateDefinition string, templateConfiguration interface{}, opts boshsys.ConvergeFileContentsOpts) (bool, error) {
	interfaceBasename := fmt.Sprintf("10_%s.network", name)
	interfaceFile := filepath.Join("/etc/systemd/network", interfaceBasename)
	buffer := bytes.NewBuffer([]byte{})
	templateFuncs := template.FuncMap{
		"NetmaskToCIDR": boshsettings.NetmaskToCIDR,
	}

	t := template.Must(template.New(interfaceBasename).Funcs(templateFuncs).Parse(templateDefinition))

	err := t.Execute(buffer, templateConfiguration)
	if err != nil {
		return false, bosherr.WrapError(err, fmt.Sprintf("Generating config from template %s", interfaceBasename))
	}

	net.logger.Error(UbuntuNetManagerLogTag, "Updating %s configuration with contents: %s", interfaceBasename, buffer.Bytes())
	return net.fs.ConvergeFileContents(
		interfaceFile,
		buffer.Bytes(),
		opts,
	)
}

func (net UbuntuNetManager) writeNetworkInterfaces(
	dhcpConfigs DHCPInterfaceConfigurations,
	staticConfigs StaticInterfaceConfigurations,
	dnsServers []string,
	opts boshsys.ConvergeFileContentsOpts) (bool, error) {

	sort.Stable(dhcpConfigs)
	sort.Stable(staticConfigs)

	anyChanged := false
	for _, dynamicAddressConfiguration := range dhcpConfigs {
		networkInterfaceConfiguration := networkInterfaceConfig{
			InterfaceConfig:   dynamicAddressConfiguration,
			HasDNSNameServers: true,
			DNSServers:        dnsServers,
		}
		changed, err := net.updateConfiguration(dynamicAddressConfiguration.Name, dynamicNetworkInterfaceTemplate, networkInterfaceConfiguration, opts)
		if err != nil {
			return false, bosherr.WrapError(err, fmt.Sprintf("Updating network configuration for %s", dynamicAddressConfiguration.Name))
		}

		anyChanged = anyChanged || changed
	}

	for _, staticAddressConfiguration := range staticConfigs {
		networkInterfaceConfiguration := networkInterfaceConfig{
			InterfaceConfig:   staticAddressConfiguration,
			HasDNSNameServers: true,
			DNSServers:        dnsServers,
		}
		changed, err := net.updateConfiguration(staticAddressConfiguration.Name, staticNetworkInterfaceTemplate, networkInterfaceConfiguration, opts)
		if err != nil {
			return false, bosherr.WrapError(err, fmt.Sprintf("Updating network configuration for %s", staticAddressConfiguration.Name))
		}

		anyChanged = anyChanged || changed
	}
	return anyChanged, nil
}

const staticNetworkInterfaceTemplate = `# Generated by bosh-agent
[Match]
Name={{ .InterfaceConfig.Name }}

[Address]
Address={{ .InterfaceConfig.Address }}/{{ .InterfaceConfig.CIDR }}
{{ if .InterfaceConfig.IsDefaultForGateway }}{{ if not .InterfaceConfig.IsVersion6 }}Broadcast={{ end }}{{ .InterfaceConfig.Broadcast }}{{ end }}

[Network]
{{ if .InterfaceConfig.IsDefaultForGateway }}Gateway={{ .InterfaceConfig.Gateway }}{{ end }}
{{ if .InterfaceConfig.IsVersion6 }}IPv6AcceptRA=true{{ end }}
{{ if .DNSServers }}{{ range .DNSServers }}DNS={{ . }}
{{ end }}{{ end }}

[Route]
{{ range .InterfaceConfig.PostUpRoutes }}
Destination={{ .Destination }}/{{ NetmaskToCIDR .Netmask $.InterfaceConfig.IsVersion6 }}
Gateway={{ .Gateway }}
{{ end }}`

const dynamicNetworkInterfaceTemplate = `# Generated by bosh-agent
[Match]
Name={{ .InterfaceConfig.Name }}

[Network]
DHCP=yes
{{ if .InterfaceConfig.IsVersion6 }}IPv6AcceptRA=true{{ end }}
{{ if .DNSServers }}{{ range .DNSServers }}DNS={{ . }}
{{ end }}{{ end }}

[Route]
{{ range .InterfaceConfig.PostUpRoutes }}
Destination={{ .Destination }}/{{ NetmaskToCIDR .Netmask $.InterfaceConfig.IsVersion6 }}
Gateway={{ .Gateway }}
{{ end }}`

func (net UbuntuNetManager) ifaceNames(dhcpConfigs DHCPInterfaceConfigurations, staticConfigs StaticInterfaceConfigurations) []string {
	ifaceNames := []string{}
	for _, config := range dhcpConfigs {
		ifaceNames = append(ifaceNames, config.Name)
	}
	for _, config := range staticConfigs {
		ifaceNames = append(ifaceNames, config.Name)
	}
	return ifaceNames
}

func (net UbuntuNetManager) writeResolvConf(networks boshsettings.Networks) error {
	buffer := bytes.NewBuffer([]byte{})

	const resolvConfTemplate = `# Generated by bosh-agent
{{ range .DNSServers }}nameserver {{ . }}
{{ end }}`

	t := template.Must(template.New("resolv-conf").Parse(resolvConfTemplate))

	// Keep DNS servers in the order specified by the network
	dnsNetwork, _ := networks.DefaultNetworkFor("dns")

	type dnsConfigArg struct {
		DNSServers []string
	}

	dnsServersArg := dnsConfigArg{dnsNetwork.DNS}

	err := t.Execute(buffer, dnsServersArg)
	if err != nil {
		return bosherr.WrapError(err, "Generating DNS config from template")
	}

	if len(dnsNetwork.DNS) > 0 {
		// Write out base so that releases may overwrite head
		err = net.fs.WriteFile("/etc/resolvconf/resolv.conf.d/base", buffer.Bytes())
		if err != nil {
			return bosherr.WrapError(err, "Writing to /etc/resolvconf/resolv.conf.d/base")
		}
	} else {
		// For the first time before resolv.conf is symlinked to /run/...
		// inherit possibly configured resolv.conf

		targetPath, err := net.fs.ReadAndFollowLink("/etc/resolv.conf")
		if err != nil {
			return bosherr.WrapError(err, "Reading /etc/resolv.conf symlink")
		}

		expectedPath, err := filepath.Abs("/etc/resolv.conf")
		if err != nil {
			return bosherr.WrapError(err, "Resolving path to native OS")
		}
		if targetPath == expectedPath {
			err := net.fs.CopyFile("/etc/resolv.conf", "/etc/resolvconf/resolv.conf.d/base")
			if err != nil {
				return bosherr.WrapError(err, "Copying /etc/resolv.conf for backwards compat")
			}
		}
	}

	err = net.fs.Symlink("/run/resolvconf/resolv.conf", "/etc/resolv.conf")
	if err != nil {
		return bosherr.WrapError(err, "Setting up /etc/resolv.conf symlink")
	}

	_, _, _, err = net.cmdRunner.RunCommand("resolvconf", "-u")
	if err != nil {
		return bosherr.WrapError(err, "Updating resolvconf")
	}

	return nil
}
