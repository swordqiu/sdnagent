// Copyright 2019 Yunion
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package utils

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"
	"time"

	"yunion.io/x/jsonutils"
	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"

	apis "yunion.io/x/onecloud/pkg/apis/compute"
	"yunion.io/x/onecloud/pkg/apis/identity"
	"yunion.io/x/onecloud/pkg/hostman/options"
	"yunion.io/x/onecloud/pkg/mcclient/auth"
	"yunion.io/x/onecloud/pkg/util/fileutils2"
)

type HostConfigNetwork struct {
	Bridge string
	Ifname string
	ip     net.IP
	mac    net.HardwareAddr

	gw    net.IP
	gwMac net.HardwareAddr

	HostLocalNets []apis.NetworkDetails
}

func NewHostConfigNetwork(network string) (*HostConfigNetwork, error) {
	chunks := strings.Split(network, "/")
	if len(chunks) >= 3 {
		// the 3rd field can be an ip address or platform network name.
		// net.ParseIP will return nil when it fails
		return &HostConfigNetwork{
			Ifname: chunks[0],
			Bridge: chunks[1],
			ip:     net.ParseIP(chunks[2]),
		}, nil
	}
	return nil, fmt.Errorf("invalid host.conf networks config: %q", network)
}

func (hcn *HostConfigNetwork) IPMAC() (net.IP, net.HardwareAddr, error) {
	if hcn.mac == nil {
		iface, err := net.InterfaceByName(hcn.Bridge)
		if err != nil {
			return nil, nil, err
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, nil, err
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok {
				ip := ipnet.IP.To4()
				if ip != nil {
					hcn.ip = ip
					break
				}
			}
		}
		hcn.mac = iface.HardwareAddr
	}
	if hcn.ip != nil && hcn.mac != nil {
		return hcn.ip, hcn.mac, nil
	}
	return nil, nil, fmt.Errorf("cannot find proper ip/mac")
}

func findRandomPortRange() (int, int, error) {
	const path = "/proc/sys/net/ipv4/ip_local_port_range"
	content, err := os.ReadFile(path)
	if err != nil {
		return -1, -1, errors.Wrap(err, "read ip_local_port_range")
	}
	fields := strings.Fields(strings.TrimSpace(string(content)))
	if len(fields) != 2 {
		return -1, -1, errors.Wrapf(errors.ErrInvalidFormat, "invalid ip_local_port_range: %s", string(content))
	}
	start, err := strconv.Atoi(fields[0])
	if err != nil {
		return -1, -1, errors.Wrapf(err, "invalid ip_local_port_range: %s", string(content))
	}
	end, err := strconv.Atoi(fields[1])
	if err != nil {
		return -1, -1, errors.Wrapf(err, "invalid ip_local_port_range: %s", string(content))
	}
	return start, end, nil
}

func findNeighborMac(ip string, dev string) (string, error) {
	cmds := []string{"neigh", "show", ip, "dev", dev}
	cmd := exec.Command("ip", cmds...)
	output, err := cmd.Output()
	if err != nil {
		return "", errors.Wrap(err, "ip neigh show")
	}
	log.Debugf("command %s show output: %s", strings.Join(cmds, " "), string(output))
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		// 172.22.121.254 lladdr e4:84:29:bc:c8:01 REACHABLE
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 4 {
			return "", errors.Wrapf(errors.ErrInvalidFormat, "cannot find proper neighbor mac: %s", line)
		}
		if fields[0] == ip && strings.ToUpper(fields[3]) == "REACHABLE" {
			return fields[2], nil
		}
	}
	return "", errors.Wrapf(errors.ErrInvalidFormat, "cannot find proper neighbor mac: %s", ip)
}

func (hcn *HostConfigNetwork) GatewayIPMac() (net.IP, net.HardwareAddr, error) {
	if hcn.gw == nil {
		cmd := exec.Command("ip", "route", "show", "default", "dev", hcn.Bridge)
		output, err := cmd.Output()
		if err != nil {
			return nil, nil, errors.Wrap(err, "ip route show default dev")
		}
		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			fields := strings.Fields(strings.TrimSpace(line))
			if len(fields) < 3 {
				return nil, nil, errors.Wrapf(errors.ErrInvalidFormat, "cannot find proper gateway ip/mac: %s", line)
			}
			if fields[0] == "default" && fields[1] == "via" {
				hcn.gw = net.ParseIP(fields[2])
				gwMacStr, err := findNeighborMac(fields[2], hcn.Bridge)
				if err != nil {
					return nil, nil, errors.Wrap(err, "find neighbor mac")
				}
				hcn.gwMac, err = net.ParseMAC(gwMacStr)
				if err != nil {
					return nil, nil, errors.Wrapf(err, "invalid mac address: %s", gwMacStr)
				}
				break
			}
		}
	}
	if hcn.gw != nil && hcn.gwMac != nil {
		return hcn.gw, hcn.gwMac, nil
	}
	return nil, nil, errors.Wrapf(errors.ErrInvalidFormat, "cannot find proper gateway ip/mac")
}

func (hcn *HostConfigNetwork) loadHostLocalNetconfs(hc *HostConfig) {
	log.Infof("HostConfigNetwork loadHostLocalNetconfs!!!")
	if hcn.ip == nil {
		return
	}
	fn := hc.HostLocalNetconfPath(hcn.Bridge)
	confStr, err := fileutils2.FileGetContents(fn)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warningf("fail to load host local netconfs %s: %s", fn, err)
		}
		return
	}
	confJson, err := jsonutils.ParseString(confStr)
	if err != nil {
		log.Warningf("fail to parse host local netconfs %s: %s", fn, err)
		return
	}
	hcn.HostLocalNets = make([]apis.NetworkDetails, 0)
	err = confJson.Unmarshal(&hcn.HostLocalNets)
	if err != nil {
		log.Warningf("fail to unmarshal host local netconfs %s: %s", fn, err)
		return
	}
}

type HostConfig struct {
	options.SHostOptions

	networks []*HostConfigNetwork
}

func (hc *HostConfig) MetadataPort() int {
	return hc.Port + 1000
}

func NewHostConfig() (*HostConfig, error) {
	hostOpts := options.Parse()
	hc := &HostConfig{
		SHostOptions: hostOpts,
	}

	if hc.AllowSwitchVMs && !hc.AllowRouterVMs {
		hc.AllowRouterVMs = true
	}

	for _, network := range hc.Networks {
		hcn, err := NewHostConfigNetwork(network)
		if err != nil {
			// NOTE error ignored
			continue
		}
		hcn.loadHostLocalNetconfs(hc)
		hc.networks = append(hc.networks, hcn)
	}

	return hc, nil
}

func (hc *HostConfig) GetOverlayMTU() int {
	mtu := hc.OvnUnderlayMtu
	if mtu < 576 {
		mtu = 576
	}
	mtu -= apis.VPC_OVN_ENCAP_COST
	return mtu
}

func (hc *HostConfig) HostNetworkConfigs() []*HostConfigNetwork {
	return hc.networks
}

func (hc *HostConfig) HostNetworkConfig(bridge string) *HostConfigNetwork {
	for _, hcn := range hc.networks {
		if hcn.Bridge == bridge {
			return hcn
		}
	}
	return nil
}

func (hc *HostConfig) Equals(hc1 *HostConfig) bool {
	return reflect.DeepEqual(hc.SHostOptions, hc1.SHostOptions)
}

func (hc *HostConfig) WatchChange(ctx context.Context, cb func()) {
	tick := time.NewTicker(13 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			hc1, err := NewHostConfig()
			if err != nil {
				log.Errorf("watch host config: NewHostConfig: %v", err)
				cb()
				return
			}
			if !hc.Equals(hc1) {
				cb()
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

func (hc *HostConfig) Auth(ctx context.Context) error {
	a := auth.NewAuthInfo(
		hc.AuthURL,
		hc.AdminDomain,
		hc.AdminUser,
		hc.AdminPassword,
		hc.AdminProject,
		hc.AdminProjectDomain,
	)

	if t := hc.SessionEndpointType; t != "" {
		if t != identity.EndpointInterfacePublic && t != identity.EndpointInterfaceInternal {
			return fmt.Errorf("Invalid session endpoint type %q", t)
		}
		auth.SetEndpointType(t)
	}

	var (
		debugClient = false
		insecure    = true
		certfile    = hc.SslCertfile
		keyfile     = hc.SslKeyfile
	)
	if !hc.EnableSsl {
		certfile = ""
		keyfile = ""
	}
	auth.Init(a, debugClient, insecure, certfile, keyfile)
	return nil
}
