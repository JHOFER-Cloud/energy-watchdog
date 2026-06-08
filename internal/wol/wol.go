// Package wol sends Wake-on-LAN magic packets.
package wol

import (
	"encoding/hex"
	"fmt"
	"net"
	"strings"
	"syscall"
)

// Send broadcasts a magic packet for mac to broadcastAddr (e.g. "255.255.255.255:9",
// or a subnet-directed address like "10.1.1.255:9"). The pod must share the target's
// broadcast domain (hostNetwork on an always-on node).
func Send(mac, broadcastAddr string) error {
	packet, err := buildPacket(mac)
	if err != nil {
		return err
	}
	addr, err := net.ResolveUDPAddr("udp", broadcastAddr)
	if err != nil {
		return fmt.Errorf("resolve %q: %w", broadcastAddr, err)
	}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return fmt.Errorf("dial %s: %w", broadcastAddr, err)
	}
	defer conn.Close()
	// Linux rejects sends to a broadcast address unless SO_BROADCAST is set.
	if err := enableBroadcast(conn); err != nil {
		return fmt.Errorf("enable broadcast: %w", err)
	}
	if _, err := conn.Write(packet); err != nil {
		return fmt.Errorf("send magic packet: %w", err)
	}
	return nil
}

func enableBroadcast(conn *net.UDPConn) error {
	rc, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := rc.Control(func(fd uintptr) {
		setErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
	}); err != nil {
		return err
	}
	return setErr
}

// buildPacket assembles the 102-byte magic packet: 6 bytes of 0xFF then the MAC ×16.
func buildPacket(mac string) ([]byte, error) {
	clean := strings.NewReplacer(":", "", "-", "", ".", "").Replace(mac)
	hw, err := hex.DecodeString(clean)
	if err != nil || len(hw) != 6 {
		return nil, fmt.Errorf("invalid MAC %q", mac)
	}
	packet := make([]byte, 0, 6+16*6)
	for range 6 {
		packet = append(packet, 0xFF)
	}
	for range 16 {
		packet = append(packet, hw...)
	}
	return packet, nil
}
