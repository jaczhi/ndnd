/* YaNFD - Yet another NDN Forwarding Daemon
 *
 * Copyright (C) 2020-2021 Eric Newberry.
 *
 * This file is licensed under the terms of the MIT License, as found in LICENSE.md.
 */

package face

import (
	"fmt"
	"net"
	"strings"

	"github.com/named-data/ndnd/fw/core"
	defn "github.com/named-data/ndnd/fw/defn"
	"github.com/named-data/ndnd/fw/face/impl"
	spec_mgmt "github.com/named-data/ndnd/std/ndn/mgmt_2022"
	ndn_io "github.com/named-data/ndnd/std/utils/io"
)

// MulticastUDPTransport is a multicast UDP transport.
type MulticastUDPTransport struct {
	dialer    *net.Dialer
	sendConn  *net.UDPConn
	recvConn  *net.UDPConn
	groupAddr net.UDPAddr
	localAddr net.UDPAddr
	transportBase
}

// MakeMulticastUDPTransport creates a new multicast UDP transport.
func MakeMulticastUDPTransport(localURI *defn.URI) (*MulticastUDPTransport, error) {
	// Validate local URI
	localURI.Canonize()
	if !localURI.IsCanonical() || (localURI.Scheme() != "udp4" && localURI.Scheme() != "udp6") {
		return nil, defn.ErrNotCanonical
	}

	// Get remote Uri
	var remote string
	if localURI.Scheme() == "udp4" {
		remote = fmt.Sprintf("udp4://%s:%d", CfgUDP4MulticastAddress(), CfgUDPMulticastPort())
	} else if localURI.Scheme() == "udp6" {
		remote = fmt.Sprintf("udp6://[%s]:%d", CfgUDP6MulticastAddress(), CfgUDPMulticastPort())
	}

	// Create transport
	t := &MulticastUDPTransport{}
	t.makeTransportBase(
		defn.DecodeURIString(remote),
		localURI, spec_mgmt.PersistencyPermanent,
		defn.NonLocal, defn.MultiAccess,
		int(core.C.Faces.Udp.DefaultMtu))

	// Format group and local addresses
	t.groupAddr.IP = net.ParseIP(t.remoteURI.PathHost())
	t.groupAddr.Port = int(t.remoteURI.Port())
	t.groupAddr.Zone = t.remoteURI.PathZone()
	t.localAddr.IP = net.ParseIP(t.localURI.PathHost())
	t.localAddr.Port = 0 // int(t.localURI.Port())
	t.localAddr.Zone = t.localURI.PathZone()

	// Configure dialer so we can allow address reuse
	t.dialer = &net.Dialer{LocalAddr: &t.localAddr, Control: impl.SyscallReuseAddr}
	t.running.Store(true)

	// Create send connection
	err := t.connectSend()
	if err != nil {
		t.Close()
		return nil, err
	}

	// Create receive connection
	err = t.connectRecv()
	if err != nil {
		t.Close()
		return nil, err
	}

	return t, nil
}

func (t *MulticastUDPTransport) connectSend() error {
	sendConn, err := t.dialer.Dial(t.remoteURI.Scheme(), t.groupAddr.String())
	if err != nil {
		return fmt.Errorf("unable to create send connection to group address: %w", err)
	}
	t.sendConn = sendConn.(*net.UDPConn)
	return nil
}

func (t *MulticastUDPTransport) connectRecv() error {
	localIf, err := InterfaceByIP(net.ParseIP(t.localURI.PathHost()))
	if err != nil || localIf == nil {
		return fmt.Errorf("unable to get interface for local URI %s: %s", t.localURI, err.Error())
	}

	t.recvConn, err = net.ListenMulticastUDP(t.remoteURI.Scheme(), localIf, &t.groupAddr)
	if err != nil {
		return fmt.Errorf("unable to create receive conn for group %s: %s", localIf.Name, err.Error())
	}
	return nil
}

func (t *MulticastUDPTransport) String() string {
	return fmt.Sprintf("multicast-udp-transport (faceid=%d remote=%s local=%s)", t.faceID, t.remoteURI, t.localURI)
}

func (t *MulticastUDPTransport) SetPersistency(persistency spec_mgmt.Persistency) bool {
	if persistency == t.persistency {
		return true
	}

	if persistency == spec_mgmt.PersistencyPermanent {
		t.persistency = persistency
		return true
	}

	return false
}

func (t *MulticastUDPTransport) GetSendQueueSize() uint64 {
	rawConn, err := t.recvConn.SyscallConn()
	if err != nil {
		core.Log.Warn(t, "Unable to get raw connection to get socket length", "err", err)
	}
	return impl.SyscallGetSocketSendQueueSize(rawConn)
}

func (t *MulticastUDPTransport) sendFrame(frame []byte) {
	if !t.running.Load() {
		return
	}

	if len(frame) > t.MTU() {
		core.Log.Warn(t, "Attempted to send frame larger than MTU")
		return
	}

	_, err := t.sendConn.Write(frame)
	if err != nil {
		core.Log.Warn(t, "Unable to send on socket")

		// Re-create the socket if connection is still running
		if t.running.Load() {
			err = t.connectSend()
			if err != nil {
				core.Log.Error(t, "Unable to re-create send connection", "err", err)
				return
			}
		}
	}

	t.nOutBytes += uint64(len(frame))
}

func (t *MulticastUDPTransport) runReceive() {
	defer t.Close()

	for t.running.Load() {
		err := ndn_io.ReadTlvStream(t.recvConn, func(b []byte) bool {
			t.nInBytes += uint64(len(b))
			t.linkService.handleIncomingFrame(b)
			return true
		}, func(err error) bool {
			// Same as unicast UDP transport
			return strings.Contains(err.Error(), "connection refused")
		})
		if err != nil && t.running.Load() {
			// Re-create the socket if connection is still running
			core.Log.Warn(t, "Unable to read from socket - Face DOWN", "err", err)
			err = t.connectRecv()
			if err != nil {
				core.Log.Error(t, "Unable to re-create receive connection", "err", err)
				return
			}
		}
	}
}

func (t *MulticastUDPTransport) Close() {
	if t.running.Swap(false) {
		if t.sendConn != nil {
			t.sendConn.Close()
		}
		if t.recvConn != nil {
			t.recvConn.Close()
		}
	}
}
