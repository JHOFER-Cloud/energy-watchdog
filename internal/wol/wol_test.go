package wol

import (
	"bytes"
	"net"
	"testing"
)

// TestSend exercises the full send path (including the SO_BROADCAST setsockopt) against
// a local unicast listener, since sending real broadcast traffic isn't testable here.
func TestSend(t *testing.T) {
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := Send("01:02:03:04:05:06", conn.LocalAddr().String()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	buf := make([]byte, 200)
	n, _, err := conn.ReadFrom(buf)
	if err != nil {
		t.Fatal(err)
	}
	if n != 102 {
		t.Errorf("received %d bytes, want 102", n)
	}
}

func TestBuildPacket(t *testing.T) {
	for _, mac := range []string{"01:02:03:04:05:06", "01-02-03-04-05-06", "010203040506"} {
		p, err := buildPacket(mac)
		if err != nil {
			t.Fatalf("buildPacket(%q): %v", mac, err)
		}
		if len(p) != 102 {
			t.Fatalf("len = %d, want 102", len(p))
		}
		if !bytes.Equal(p[:6], []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}) {
			t.Errorf("header = %x, want 6x FF", p[:6])
		}
		want := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06}
		if !bytes.Equal(p[6:12], want) {
			t.Errorf("first MAC repeat = %x, want %x", p[6:12], want)
		}
		if !bytes.Equal(p[96:102], want) {
			t.Errorf("last MAC repeat = %x, want %x", p[96:102], want)
		}
	}
}

func TestBuildPacketInvalid(t *testing.T) {
	for _, mac := range []string{"", "zz:zz:zz:zz:zz:zz", "01:02:03"} {
		if _, err := buildPacket(mac); err == nil {
			t.Errorf("buildPacket(%q) = nil error, want error", mac)
		}
	}
}
