package dump

import (
	"fmt"
	"net"
	"strconv"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
)

// Synthesized link/network parameters. Captures only contain application-layer
// payloads, so dbbat wraps every payload in a stable, entirely fabricated
// Ethernet/IPv4/TCP header stack. That is what makes the files readable by
// tcpdump and lets Wireshark reassemble the stream and run its protocol
// dissectors.
const (
	// snapLength is declared on the interface description block. It must be
	// non-zero: Apple's libpcap rejects captures whose interface advertises an
	// unlimited (0) snap length.
	snapLength = 262144

	// maxTCPPayload is the largest payload that fits in one synthesized
	// segment (65535 IPv4 total length - 20 IP header - 20 TCP header).
	maxTCPPayload = 65495

	// tcpWindow is the fixed advertised window on every synthesized segment.
	tcpWindow = 65535

	// ipTTL is the fixed TTL on every synthesized packet.
	ipTTL = 64
)

// Fake endpoints. The client side is always fabricated; the server side keeps
// the real upstream address when it is known, so that captures remain useful
// for correlating with server-side logs. `Anonymise` replaces it.
var (
	clientMAC = net.HardwareAddr{0x02, 0xdb, 0xba, 0x70, 0x00, 0x01}
	serverMAC = net.HardwareAddr{0x02, 0xdb, 0xba, 0x70, 0x00, 0x02}

	fakeClientIP = net.IPv4(10, 77, 0, 1)
	fakeServerIP = net.IPv4(10, 77, 0, 2)
)

// fakeClientPort is the fabricated ephemeral port of the client side.
const fakeClientPort = layers.TCPPort(54321)

// defaultPorts maps a protocol identifier to its well-known port. Used when the
// real upstream port is unknown, and by `Anonymise`. Wireshark's dissector
// heuristics key off these ports, so getting them right is what makes a capture
// dissect as PGSQL/TNS/MySQL/Mongo out of the box.
var defaultPorts = map[string]layers.TCPPort{
	ProtocolOracle:     1521,
	ProtocolPostgreSQL: 5432,
	ProtocolMySQL:      3306,
	ProtocolMongo:      27017,
}

// endpoints holds the synthesized addressing for one capture.
type endpoints struct {
	clientIP   net.IP
	serverIP   net.IP
	clientPort layers.TCPPort
	serverPort layers.TCPPort
}

// newEndpoints derives the synthesized addressing from the session metadata.
// When anonymous is true the real upstream address is ignored entirely and only
// fabricated values are used.
func newEndpoints(header Header, anonymous bool) endpoints {
	ep := endpoints{
		clientIP:   fakeClientIP,
		serverIP:   fakeServerIP,
		clientPort: fakeClientPort,
		serverPort: defaultPorts[header.Protocol],
	}

	if ep.serverPort == 0 {
		ep.serverPort = defaultPorts[ProtocolPostgreSQL]
	}

	if anonymous {
		return ep
	}

	addr, _ := header.Connection["upstream_addr"].(string)
	if addr == "" {
		return ep
	}

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ep
	}

	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			ep.serverIP = v4
		}
	}

	if p, err := strconv.ParseUint(port, 10, 16); err == nil && p > 0 {
		ep.serverPort = layers.TCPPort(p)
	}

	return ep
}

// framer serializes application payloads into synthesized Ethernet/IPv4/TCP
// frames, keeping per-direction TCP sequence numbers monotonically advancing so
// that Wireshark can reassemble each half of the conversation.
type framer struct {
	endpoints endpoints
	seq       [2]uint32 // indexed by direction (0 = c2s, 1 = s2c)
}

func newFramer(ep endpoints) *framer {
	return &framer{endpoints: ep, seq: [2]uint32{1, 1}}
}

// dirIndex maps a dump direction to the framer's sequence-number slot.
func dirIndex(direction byte) int {
	if direction == DirServerToClient {
		return 1
	}

	return 0
}

// frames turns one application payload into one or more wire frames. Payloads
// larger than maxTCPPayload are split into consecutive segments.
func (f *framer) frames(direction byte, payload []byte) ([][]byte, error) {
	idx := dirIndex(direction)
	peer := 1 - idx

	out := make([][]byte, 0, 1)

	for offset := 0; ; {
		end := offset + maxTCPPayload
		if end > len(payload) {
			end = len(payload)
		}

		chunk := payload[offset:end]

		frame, err := f.frame(direction, f.seq[idx], f.seq[peer], chunk)
		if err != nil {
			return nil, err
		}

		out = append(out, frame)
		f.seq[idx] += uint32(len(chunk))

		offset = end
		if offset >= len(payload) {
			break
		}
	}

	return out, nil
}

// frame serializes a single Ethernet/IPv4/TCP frame around chunk.
func (f *framer) frame(direction byte, seq, ack uint32, chunk []byte) ([]byte, error) {
	srcMAC, dstMAC := clientMAC, serverMAC
	srcIP, dstIP := f.endpoints.clientIP, f.endpoints.serverIP
	srcPort, dstPort := f.endpoints.clientPort, f.endpoints.serverPort

	if direction == DirServerToClient {
		srcMAC, dstMAC = serverMAC, clientMAC
		srcIP, dstIP = f.endpoints.serverIP, f.endpoints.clientIP
		srcPort, dstPort = f.endpoints.serverPort, f.endpoints.clientPort
	}

	eth := &layers.Ethernet{
		SrcMAC:       srcMAC,
		DstMAC:       dstMAC,
		EthernetType: layers.EthernetTypeIPv4,
	}

	ip := &layers.IPv4{
		Version:  4,
		IHL:      5,
		TTL:      ipTTL,
		Protocol: layers.IPProtocolTCP,
		SrcIP:    srcIP,
		DstIP:    dstIP,
	}

	tcp := &layers.TCP{
		SrcPort: srcPort,
		DstPort: dstPort,
		Seq:     seq,
		Ack:     ack,
		PSH:     true,
		ACK:     true,
		Window:  tcpWindow,
	}

	if err := tcp.SetNetworkLayerForChecksum(ip); err != nil {
		return nil, fmt.Errorf("set network layer for checksum: %w", err)
	}

	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}

	if err := gopacket.SerializeLayers(buf, opts, eth, ip, tcp, gopacket.Payload(chunk)); err != nil {
		return nil, fmt.Errorf("serialize frame: %w", err)
	}

	// SerializeBuffer reuses its backing array between calls, so hand back a copy.
	frame := make([]byte, len(buf.Bytes()))
	copy(frame, buf.Bytes())

	return frame, nil
}
