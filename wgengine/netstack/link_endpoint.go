// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package netstack

import (
	"bytes"
	"context"
	"sync"

	"github.com/tailscale/wireguard-go/tun"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/stack/gro"
	"tailscale.com/net/packet"
	"tailscale.com/types/ipproto"
)

type queue struct {
	c      chan *stack.PacketBuffer
	mu     sync.RWMutex // mu guards closed
	closed bool
}

func (q *queue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.closed {
		close(q.c)
	}
	q.closed = true
}

func (q *queue) Read() *stack.PacketBuffer {
	select {
	case p := <-q.c:
		return p
	default:
		return nil
	}
}

func (q *queue) ReadContext(ctx context.Context) *stack.PacketBuffer {
	select {
	case pkt := <-q.c:
		return pkt
	case <-ctx.Done():
		return nil
	}
}

func (q *queue) Write(pkt *stack.PacketBuffer) tcpip.Error {
	// q holds the PacketBuffer.
	q.mu.RLock()
	if q.closed {
		q.mu.RUnlock()
		return &tcpip.ErrClosedForSend{}
	}

	wrote := false
	select {
	case q.c <- pkt.IncRef():
		wrote = true
	default:
		// TODO(jwhited): reconsider/count
		pkt.DecRef()
	}
	q.mu.RUnlock()

	if wrote {
		return nil
	}
	return &tcpip.ErrNoBufferSpace{}
}

func (q *queue) Num() int {
	return len(q.c)
}

var _ stack.LinkEndpoint = (*linkEndpoint)(nil)
var _ stack.GSOEndpoint = (*linkEndpoint)(nil)

// linkEndpoint implements stack.LinkEndpoint and stack.GSOEndpoint. Outbound
// packets written by gVisor towards Tailscale are stored in a channel.
// Inbound is fed to gVisor via injectInbound or enqueueGRO. This is loosely
// modeled after gvisor.dev/pkg/tcpip/link/channel.Endpoint.
type linkEndpoint struct {
	SupportedGSOKind stack.SupportedGSO

	mu         sync.RWMutex // mu guards the following fields
	dispatcher stack.NetworkDispatcher
	linkAddr   tcpip.LinkAddress
	mtu        uint32

	q   *queue // outbound
	gro gro.GRO
}

func newLinkEndpoint(size int, mtu uint32, linkAddr tcpip.LinkAddress) *linkEndpoint {
	le := &linkEndpoint{
		q: &queue{
			c: make(chan *stack.PacketBuffer, size),
		},
		mtu:      mtu,
		linkAddr: linkAddr,
	}
	le.gro.Init(true)
	return le
}

// Close closes l. Further packet injections will return an error, and all
// pending packets are discarded. Close may be called concurrently with
// WritePackets.
func (l *linkEndpoint) Close() {
	l.q.Close()
	l.gro.Flush()
	l.Drain()
}

// Read does non-blocking read one packet from the outbound packet queue.
func (l *linkEndpoint) Read() *stack.PacketBuffer {
	return l.q.Read()
}

// ReadContext does blocking read for one packet from the outbound packet queue.
// It can be cancelled by ctx, and in this case, it returns nil.
func (l *linkEndpoint) ReadContext(ctx context.Context) *stack.PacketBuffer {
	return l.q.ReadContext(ctx)
}

// Drain removes all outbound packets from the channel and counts them.
func (l *linkEndpoint) Drain() int {
	c := 0
	for pkt := l.Read(); pkt != nil; pkt = l.Read() {
		pkt.DecRef()
		c++
	}
	return c
}

// NumQueued returns the number of packets queued for outbound.
func (l *linkEndpoint) NumQueued() int {
	return l.q.Num()
}

// rxChecksumOffload validates IPv4, TCP, and UDP header checksums in p,
// returning an equivalent *stack.PacketBuffer if they are valid, otherwise nil.
// The set of headers validated covers where gVisor would perform validation if
// !stack.PacketBuffer.RXChecksumValidated, i.e. it satisfies
// stack.CapabilityRXChecksumOffload.
func rxChecksumOffload(p *packet.Parsed) *stack.PacketBuffer {
	var (
		pn           tcpip.NetworkProtocolNumber
		csumStart    int
		csumRequired bool
	)
	buf := p.Buffer()

	switch p.IPVersion {
	case 4:
		if len(buf) < 20 {
			return nil
		}
		csumStart = int((buf[0] & 0x0F) * 4)
		if csumStart < 20 || csumStart > 60 || len(buf) < csumStart {
			return nil
		}
		if ^tun.Checksum(buf[:csumStart], 0) != 0 {
			return nil
		}
		pn = header.IPv4ProtocolNumber
	case 6:
		if len(buf) < 40 {
			return nil
		}
		csumStart = 40
		pn = header.IPv6ProtocolNumber
	}
	if p.IPProto == ipproto.TCP || p.IPProto == ipproto.UDP {
		csumRequired = true
	}

	if csumRequired {
		lenForPseudo := len(buf) - csumStart
		csum := tun.PseudoHeaderChecksum(
			uint8(p.IPProto),
			p.Src.Addr().AsSlice(),
			p.Dst.Addr().AsSlice(),
			uint16(lenForPseudo))
		csum = tun.Checksum(buf[csumStart:], csum)
		if ^csum != 0 {
			return nil
		}
	}

	packetBuf := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(bytes.Clone(buf)),
	})
	packetBuf.NetworkProtocolNumber = pn
	// Setting this is not technically required. gVisor overrides where
	// stack.CapabilityRXChecksumOffload is advertised from Capabilities().
	// https://github.com/google/gvisor/blob/64c016c92987cc04dfd4c7b091ddd21bdad875f8/pkg/tcpip/stack/nic.go#L763
	// This is also why we offload for all packets since we cannot signal this
	// per-packet.
	packetBuf.RXChecksumValidated = true
	return packetBuf
}

func (l *linkEndpoint) injectInbound(p *packet.Parsed) {
	l.mu.RLock()
	d := l.dispatcher
	l.mu.RUnlock()
	if d != nil {
		pkt := rxChecksumOffload(p)
		if pkt != nil {
			d.DeliverNetworkPacket(pkt.NetworkProtocolNumber, pkt)
			pkt.DecRef()
		}
	}
}

// enqueueGRO enqueues the provided pkt for GRO. This does not flush packets;
// that must be called explicitly via flushGRO(). enqueueGRO is NOT thread-safe.
func (l *linkEndpoint) enqueueGRO(p *packet.Parsed) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.gro.Dispatcher != nil {
		pkt := rxChecksumOffload(p)
		if pkt != nil {
			// TODO(jwhited): gro.Enqueue() duplicates a lot of p.Decode().
			//  We may want to push stack.PacketBuffer further up, or inversely
			//  push packet.Parsed down into refactored GRO logic.
			l.gro.Enqueue(pkt)
			pkt.DecRef()
		}
	}
}

// flushGRO flushes previously enqueueGRO'd packets. flushGRO is NOT
// thread-safe.
func (l *linkEndpoint) flushGRO() {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.gro.Dispatcher != nil {
		l.gro.Flush()
	}
}

// Attach saves the stack network-layer dispatcher for use later when packets
// are injected.
func (l *linkEndpoint) Attach(dispatcher stack.NetworkDispatcher) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dispatcher = dispatcher
	l.gro.Dispatcher = dispatcher
}

// IsAttached implements stack.LinkEndpoint.IsAttached.
func (l *linkEndpoint) IsAttached() bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.dispatcher != nil
}

// MTU implements stack.LinkEndpoint.MTU.
func (l *linkEndpoint) MTU() uint32 {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.mtu
}

// SetMTU implements stack.LinkEndpoint.SetMTU.
func (l *linkEndpoint) SetMTU(mtu uint32) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.mtu = mtu
}

// Capabilities implements stack.LinkEndpoint.Capabilities.
func (l *linkEndpoint) Capabilities() stack.LinkEndpointCapabilities {
	// We are required to offload RX checksum validation for the purposes of
	// GRO.
	return stack.CapabilityRXChecksumOffload
}

// GSOMaxSize implements stack.GSOEndpoint.
func (*linkEndpoint) GSOMaxSize() uint32 {
	// This an increase from 32k returned by channel.Endpoint.GSOMaxSize() to
	// 64k, which improves throughput.
	return (1 << 16) - 1
}

// SupportedGSO implements stack.GSOEndpoint.
func (l *linkEndpoint) SupportedGSO() stack.SupportedGSO {
	return l.SupportedGSOKind
}

// MaxHeaderLength returns the maximum size of the link layer header. Given it
// doesn't have a header, it just returns 0.
func (*linkEndpoint) MaxHeaderLength() uint16 {
	return 0
}

// LinkAddress returns the link address of this endpoint.
func (l *linkEndpoint) LinkAddress() tcpip.LinkAddress {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.linkAddr
}

// SetLinkAddress implements stack.LinkEndpoint.SetLinkAddress.
func (l *linkEndpoint) SetLinkAddress(addr tcpip.LinkAddress) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.linkAddr = addr
}

// WritePackets stores outbound packets into the channel.
// Multiple concurrent calls are permitted.
func (l *linkEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	n := 0
	// TODO(jwhited): evaluate writing a stack.PacketBufferList instead of a
	//  single packet. We can split 2 x 64K GSO across
	//  wireguard-go/conn.IdealBatchSize (128 slots) @ 1280 MTU, and non-GSO we
	//  could do more. Read API would need to change to take advantage. Verify
	//  gVisor limits around max number of segments packed together. Since we
	//  control MTU (and by effect TCP MSS in gVisor) we *shouldn't* expect to
	//  ever overflow 128 slots (see wireguard-go/tun.ErrTooManySegments usage).
	for _, pkt := range pkts.AsSlice() {
		if err := l.q.Write(pkt); err != nil {
			if _, ok := err.(*tcpip.ErrNoBufferSpace); !ok && n == 0 {
				return 0, err
			}
			break
		}
		n++
	}

	return n, nil
}

// Wait implements stack.LinkEndpoint.Wait.
func (*linkEndpoint) Wait() {}

// ARPHardwareType implements stack.LinkEndpoint.ARPHardwareType.
func (*linkEndpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareNone
}

// AddHeader implements stack.LinkEndpoint.AddHeader.
func (*linkEndpoint) AddHeader(*stack.PacketBuffer) {}

// ParseHeader implements stack.LinkEndpoint.ParseHeader.
func (*linkEndpoint) ParseHeader(*stack.PacketBuffer) bool { return true }

// SetOnCloseAction implements stack.LinkEndpoint.
func (*linkEndpoint) SetOnCloseAction(func()) {}
