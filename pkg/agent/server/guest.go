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

package server

import (
	"context"
	"fmt"
	"time"

	"github.com/digitalocean/go-openvswitch/ovs"

	"yunion.io/x/jsonutils"
	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"
	"yunion.io/x/sdnagent/pkg/agent/utils"

	computeapi "yunion.io/x/onecloud/pkg/apis/compute"
	fwdpb "yunion.io/x/onecloud/pkg/hostman/guestman/forwarder/api"
	"yunion.io/x/onecloud/pkg/mcclient/auth"
	mcclient_modules "yunion.io/x/onecloud/pkg/mcclient/modules/compute"
)

var (
	errNotRunning   = fmt.Errorf("not running")
	errPortNotReady = fmt.Errorf("port not ready") // no port is ready
	errVolatileHost = fmt.Errorf("volatile host")
)

type Guest struct {
	*utils.Guest
	watcher         *serversWatcher
	lastSeenPending *time.Time

	// vpcPortMapFwds tracks persistent ovnMd forwards opened for VPC nic port mappings.
	// key: proto/bindPort/remoteAddr/remotePort
	vpcPortMapFwds map[string]*fwdpb.OpenResponse
}

func NewGuest(guest *utils.Guest, watcher *serversWatcher) *Guest {
	return &Guest{
		Guest:   guest,
		watcher: watcher,
	}
}

// refreshNicPortNo updates openflow port number for guest's each nic.  Returns
// true if all nics' port numbers are correctly updated, false otherwise, which
// usually caused by nic port is not yet in the bridge
func (g *Guest) refreshNicPortNo(ctx context.Context, nics []*utils.GuestNIC) bool {
	someOk := false
	for _, nic := range nics {
		bridge := nic.Bridge
		ifname := nic.IfnameHost
		portStats, err := utils.DumpPort(bridge, ifname)
		if err == nil {
			someOk = true
			nic.PortNo = portStats.PortID
		}
	}
	return someOk
}

func (g *Guest) reloadDesc(ctx context.Context) error {
	oldM := map[string]uint16{}
	oldNICs := g.NICs
	for _, nic := range oldNICs {
		if nic.CtZoneIdSet {
			oldM[nic.MAC] = nic.CtZoneId
		}
	}

	err := g.LoadDesc()
	if err != nil {
		return err
	}
	if g.NeedsSync() {
		go func() {
			// desc change will be picked up by watcher
			log.Debugf("guest sync %s", g.Id)
			hc := g.watcher.hostConfig
			s := auth.GetAdminSession(ctx, hc.Region)
			_, err := mcclient_modules.Servers.PerformAction(s, g.Id, "sync", nil)
			if err != nil {
				log.Errorf("guest sync %s: %v", g.Id, err)
			}
		}()
	}

	for _, nic := range g.NICs {
		if i, ok := oldM[nic.MAC]; ok {
			delete(oldM, nic.MAC)
			nic.CtZoneId = i
			continue
		}
		zoneId, err := g.watcher.zoneMan.AllocateZoneId(nic.MAC)
		if err != nil {
			return fmt.Errorf("ct zone id allocation failed: %s", err)
		}
		nic.CtZoneId = zoneId
		nic.CtZoneIdSet = true
	}
	for mac, _ := range oldM {
		g.watcher.zoneMan.FreeZoneId(mac)
	}
	return nil
}

func (g *Guest) setPending() {
	if g.lastSeenPending == nil {
		now := time.Now()
		g.lastSeenPending = &now
		g.watcher.schedulePendingRetry()
	}
}

func (g *Guest) clearPending() {
	g.lastSeenPending = nil
}

func (g *Guest) IsPending() bool {
	if g.lastSeenPending == nil {
		return false
	}
	if time.Since(*g.lastSeenPending) < WatcherRecentPendingTime {
		return true
	}
	return false
}

func (g *Guest) refresh(ctx context.Context) (err error) {
	setPending := true
	defer func() {
		if err != nil {
			log.Warningf("update guest flows %s: %s", g.Id, err)
			if setPending {
				g.setPending()
			}
		} else {
			g.clearPending()
		}
	}()

	err = g.reloadDesc(ctx)
	if err != nil {
		return
	}
	if g.IsVolatileHost() {
		err = errVolatileHost
		setPending = false
		return
	}
	// serve if any nics are ready
	someOk0 := g.refreshNicPortNo(ctx, g.NICs)
	someOk1 := g.refreshNicPortNo(ctx, g.VpcNICs)
	if !someOk0 && !someOk1 {
		if g.IsVM() && !g.Running() {
			err = errNotRunning
		} else {
			// NOTE crashed container can make pending watcher busy
			err = errPortNotReady
		}
		// next we will clean flow rules for them
	}
	return
}

func (g *Guest) updateClassicFlows(ctx context.Context) (err error) {
	bfs, err := g.FlowsMap()
	for bridge, flows := range bfs {
		flowman := g.watcher.agent.GetFlowMan(bridge)
		if flowman != nil {
			flowman.updateFlows(ctx, g.Who(), flows)
		}
	}
	if err2 := utils.SyncGuestPortMappingDNAT(g.Id, g.NICs); err2 != nil {
		log.Errorf("guest %s sync port mapping DNAT: %v", g.Id, err2)
		if err == nil {
			err = err2
		}
	}
	return
}

func (g *Guest) clearClassicFlows(ctx context.Context) {
	bridges := map[string]bool{}
	for _, nic := range g.NICs {
		bridges[nic.Bridge] = true
		g.watcher.zoneMan.FreeZoneId(nic.MAC)
	}
	for bridge, _ := range bridges {
		flowman := g.watcher.agent.GetFlowMan(bridge)
		if flowman != nil {
			flowman.updateFlows(ctx, g.Who(), []*ovs.Flow{})
		}
	}
	if err := utils.ClearGuestPortMappingDNAT(g.Id); err != nil {
		log.Errorf("guest %s clear port mapping DNAT: %v", g.Id, err)
	}
	g.clearPending()
}

func (g *Guest) updateTc(ctx context.Context, sync bool) {
	if g.watcher.tcMan == nil {
		return
	}
	data := []*utils.TcData{}
	for _, nic := range g.NICs {
		d := nic.TcData()
		data = append(data, d)
	}
	g.watcher.tcMan.AddIfaces(ctx, g.Who(), data, sync)
}

func (g *Guest) clearTc(ctx context.Context) {
	if g.watcher.tcMan == nil {
		return
	}
	g.watcher.tcMan.ClearIfaces(ctx, g.Who())
}

func (g *Guest) updateOvn(ctx context.Context) {
	if g.HostConfig.DisableLocalVpc {
		return
	}
	if g.watcher.ovnMan == nil || g.watcher.ovnMdMan == nil {
		return
	}

	if len(g.VpcNICs) == 0 {
		g.clearVpcPortMappingForwards(ctx)
		g.watcher.ovnMan.SetGuestNICs(ctx, g.Id, nil)
		g.watcher.ovnMdMan.SetGuestNICs(ctx, g.Id, nil)
		return
	}

	if len(g.VpcNICs) > 0 && g.HostId != "" {
		ovnMan := g.watcher.ovnMan
		ovnMan.SetHostId(ctx, g.HostId)
		ovnMan.SetGuestNICs(ctx, g.Id, g.VpcNICs)

		ovnMdMan := g.watcher.ovnMdMan
		ovnMdMan.SetGuestNICs(ctx, g.Id, g.VpcNICs)

		if err := g.syncVpcPortMappingForwards(ctx); err != nil {
			log.Errorf("guest %s sync vpc port mapping forwards: %v", g.Id, err)
			g.setPending()
		}
	}
}

func (g *Guest) clearOvn(ctx context.Context) {
	if g.HostConfig.DisableLocalVpc {
		return
	}

	g.clearVpcPortMappingForwards(ctx)

	ovnMan := g.watcher.ovnMan
	ovnMan.SetGuestNICs(ctx, g.Id, nil)

	ovnMdMan := g.watcher.ovnMdMan
	ovnMdMan.SetGuestNICs(ctx, g.Id, nil)
}

func (g *Guest) syncVpcPortMappingForwards(ctx context.Context) error {
	if g.watcher.ovnMdMan == nil {
		return nil
	}
	if g.vpcPortMapFwds == nil {
		g.vpcPortMapFwds = map[string]*fwdpb.OpenResponse{}
	}
	var errs []error
	aliveIPs := map[string]bool{}
	for _, nic := range g.VpcNICs {
		if nic.IP != "" {
			aliveIPs[nic.IP] = true
		}
		if err := g.syncNicPortMappingForwards(ctx, nic); err != nil {
			log.Errorf("guest %s nic %s sync vpc port mapping forwards: %v", g.Id, nic.NetId, err)
			errs = append(errs, err)
		}
	}
	// close forwards for VPC nics that are gone
	for key, fwd := range g.vpcPortMapFwds {
		if aliveIPs[fwd.RemoteAddr] {
			continue
		}
		if err := g.closePortMappingForward(ctx, g.vpcPortMapFwdNetId(fwd), fwd); err != nil {
			log.Errorf("guest %s close stale port mapping forward %s:%d -> %s:%d: %v",
				g.Id, fwd.BindAddr, fwd.BindPort, fwd.RemoteAddr, fwd.RemotePort, err)
		}
		delete(g.vpcPortMapFwds, key)
	}
	if len(errs) > 0 {
		return errors.NewAggregate(errs)
	}
	return nil
}

func (g *Guest) clearVpcPortMappingForwards(ctx context.Context) {
	if g.watcher.ovnMdMan == nil {
		return
	}
	for key, fwd := range g.vpcPortMapFwds {
		netId := g.vpcPortMapFwdNetId(fwd)
		if err := g.closePortMappingForward(ctx, netId, fwd); err != nil {
			log.Errorf("guest %s clear port mapping forward %s:%d -> %s:%d: %v",
				g.Id, fwd.BindAddr, fwd.BindPort, fwd.RemoteAddr, fwd.RemotePort, err)
		}
		delete(g.vpcPortMapFwds, key)
	}
}

func (g *Guest) vpcPortMapFwdNetId(fwd *fwdpb.OpenResponse) string {
	if fwd == nil {
		return ""
	}
	if fwd.NetId != "" {
		return fwd.NetId
	}
	for _, nic := range g.VpcNICs {
		if nic.IP == fwd.RemoteAddr {
			return nic.NetId
		}
	}
	return ""
}

func (g *Guest) syncNicPortMappingForwards(ctx context.Context, nic *utils.GuestNIC) error {
	if nic == nil || nic.NetId == "" || nic.IP == "" {
		return nil
	}

	desired := map[string]*fwdpb.OpenRequest{}
	for _, pm := range nic.PortMappings {
		req, key, ok := portMappingToOpenRequest(nic, pm)
		if !ok {
			continue
		}
		req.BindAddr = g.resolvePortMappingBindAddr(req.BindAddr)
		// key is independent of bind addr
		desired[key] = req
		log.Infof("guest %s port mapping forward %s", key, jsonutils.Marshal(req).String())
	}

	// Close forwards we previously opened for this nic but are no longer desired.
	// Do not close untracked forwards (e.g. ephemeral ssh) returned by ListByRemote.
	for key, fwd := range g.vpcPortMapFwds {
		if fwd.RemoteAddr != nic.IP {
			continue
		}
		if _, ok := desired[key]; ok {
			continue
		}
		if err := g.closePortMappingForward(ctx, nic.NetId, fwd); err != nil {
			log.Errorf("guest %s close port mapping forward %s:%d -> %s:%d: %v",
				g.Id, fwd.BindAddr, fwd.BindPort, fwd.RemoteAddr, fwd.RemotePort, err)
		}
		delete(g.vpcPortMapFwds, key)
	}

	existingByKey := map[string]*fwdpb.OpenResponse{}
	existing, err := g.listNicPortMappingForwards(ctx, nic)
	if err != nil {
		// md server may not be ready yet; still try Open below
		log.Warningf("guest %s nic %s list port mapping forwards: %v", g.Id, nic.NetId, err)
	} else {
		for _, fwd := range existing {
			key := portMappingFwdKey(fwd.Proto, fwd.BindPort, fwd.RemoteAddr, fwd.RemotePort)
			existingByKey[key] = fwd
		}
	}

	var openErrs []error
	for key, req := range desired {
		if fwd, ok := existingByKey[key]; ok {
			if fwd.NetId == "" {
				fwd.NetId = nic.NetId
			}
			g.vpcPortMapFwds[key] = fwd
			continue
		}
		// tracked or not, forward is not running — (re)open
		delete(g.vpcPortMapFwds, key)
		pbresp, err := g.watcher.ovnMdMan.ForwardRequest(ctx, ovnMdFwdReq{pbreq: req})
		if err != nil {
			log.Errorf("guest %s open port mapping forward %s:%d -> %s:%d: %v",
				g.Id, req.BindAddr, req.BindPort, req.RemoteAddr, req.RemotePort, err)
			openErrs = append(openErrs, err)
			continue
		}
		fwd, ok := pbresp.(*fwdpb.OpenResponse)
		if !ok || fwd == nil {
			continue
		}
		if fwd.NetId == "" {
			fwd.NetId = nic.NetId
		}
		g.vpcPortMapFwds[key] = fwd
		log.Infof("guest %s port mapping forward ready %s %s:%d -> %s:%d",
			g.Id, fwd.Proto, fwd.BindAddr, fwd.BindPort, fwd.RemoteAddr, fwd.RemotePort)
	}
	if len(openErrs) > 0 {
		return errors.NewAggregate(openErrs)
	}
	return nil
}

func (g *Guest) listNicPortMappingForwards(ctx context.Context, nic *utils.GuestNIC) ([]*fwdpb.OpenResponse, error) {
	if nic == nil || nic.NetId == "" || nic.IP == "" {
		return nil, nil
	}
	pbresp, err := g.watcher.ovnMdMan.ForwardRequest(ctx, ovnMdFwdReq{
		pbreq: &fwdpb.ListByRemoteRequest{
			NetId:      nic.NetId,
			RemoteAddr: nic.IP,
		},
	})
	if err != nil {
		return nil, err
	}
	listResp, ok := pbresp.(*fwdpb.ListByRemoteResponse)
	if !ok || listResp == nil {
		return nil, nil
	}
	return listResp.Forwards, nil
}

func (g *Guest) closePortMappingForward(ctx context.Context, netId string, fwd *fwdpb.OpenResponse) error {
	if fwd == nil {
		return nil
	}
	if netId == "" {
		netId = fwd.NetId
	}
	if netId == "" {
		return fmt.Errorf("missing netId for forward %s:%d", fwd.BindAddr, fwd.BindPort)
	}
	_, err := g.watcher.ovnMdMan.ForwardRequest(ctx, ovnMdFwdReq{
		pbreq: &fwdpb.CloseRequest{
			NetId:    netId,
			Proto:    fwd.Proto,
			BindAddr: fwd.BindAddr,
			BindPort: fwd.BindPort,
		},
	})
	return err
}

func portMappingToOpenRequest(nic *utils.GuestNIC, pm *computeapi.GuestPortMapping) (*fwdpb.OpenRequest, string, bool) {
	if nic == nil || pm == nil || pm.HostPort == nil || nic.IP == "" || pm.Port <= 0 || nic.NetId == "" {
		return nil, "", false
	}
	proto := string(pm.Protocol)
	if proto == "" {
		proto = string(computeapi.GuestPortMappingProtocolTCP)
	}
	// ovnMdForward currently only supports tcp
	if proto != string(computeapi.GuestPortMappingProtocolTCP) {
		log.Warningf("skip vpc port mapping forward: unsupported proto %s nic=%s port=%d", proto, nic.IP, pm.Port)
		return nil, "", false
	}
	bindAddr := pm.HostIp
	if bindAddr == "" || bindAddr == "0.0.0.0" {
		// follow hostman OpenForward: bind master IP for reachable host-side proxy
		bindAddr = ""
	}
	req := &fwdpb.OpenRequest{
		NetId:      nic.NetId,
		Proto:      proto,
		BindAddr:   bindAddr,
		BindPort:   uint32(*pm.HostPort),
		RemoteAddr: nic.IP,
		RemotePort: uint32(pm.Port),
	}
	return req, portMappingFwdKey(req.Proto, req.BindPort, req.RemoteAddr, req.RemotePort), true
}

func (g *Guest) resolvePortMappingBindAddr(bindAddr string) string {
	if bindAddr != "" && bindAddr != "0.0.0.0" {
		return bindAddr
	}
	if g.HostConfig != nil {
		if nic := g.HostConfig.MasterNic(); nic != nil && nic.Addr != "" {
			return nic.Addr
		}
	}
	return "0.0.0.0"
}

func portMappingFwdKey(proto string, bindPort uint32, remoteAddr string, remotePort uint32) string {
	return fmt.Sprintf("%s/%d/%s/%d", proto, bindPort, remoteAddr, remotePort)
}

func (g *Guest) UpdateSettings(ctx context.Context, sync bool) {
	start := time.Now()
	err := g.refresh(ctx)
	log.Debugf("guest UpdateSettings refresh %f", time.Since(start).Seconds())
	switch err {
	case nil:
		g.updateClassicFlows(ctx)
		log.Debugf("guest UpdateSettings updateClassicFlows %f", time.Since(start).Seconds())
		g.updateTc(ctx, sync)
		log.Debugf("guest UpdateSettings updateTc %f", time.Since(start).Seconds())
		g.updateOvn(ctx)
		log.Debugf("guest UpdateSettings updateOvn %f", time.Since(start).Seconds())
		if g.HostId != "" {
			g.watcher.agent.HostId(g.HostId)
		}
	case errPortNotReady:
		// Classic/OVS port may be late, but VPC metadata forward does not need PortNo.
		// Keep OVN + port-mapping forwards for running VPC guests instead of clearing them.
		if g.Running() && len(g.VpcNICs) > 0 && !g.HostConfig.DisableLocalVpc {
			log.Debugf("guest %s(%s) classic port not ready, still update OVN/port mapping", g.Name, g.Id)
			g.clearClassicFlows(ctx)
			g.clearTc(ctx)
			g.updateOvn(ctx)
			g.setPending()
			break
		}
		log.Debugf("guest %s(%s) ClearSettings due to g.refresh %s", g.Name, g.Id, err)
		g.ClearSettings(ctx)
	case errNotRunning, errVolatileHost:
		log.Debugf("guest %s(%s) ClearSettings due to g.refresh %s", g.Name, g.Id, err)
		g.ClearSettings(ctx)
	default:
		log.Errorf("guest %s(%s) g.refresh error %s", g.Name, g.Id, err)
	}
}

func (g *Guest) ClearSettings(ctx context.Context) {
	g.clearClassicFlows(ctx)
	g.clearTc(ctx)
	g.clearOvn(ctx)
}
