package netguard

import (
	"net"
	"net/netip"
	"testing"
)

func TestDefaultPolicy(t *testing.T) {
	p := Default()
	denied := []string{"127.0.0.1", "10.1.2.3", "172.31.255.255", "192.168.0.1", "100.64.0.1", "100.100.100.200",
		"169.254.169.254", "0.0.0.0", "224.0.0.251", "255.255.255.255", "198.18.0.1", "::", "::1", "fc00::1",
		"fdff::1", "fe80::1", "ff02::1", "64:ff9b::a00:1", "2002:a00:1::1", "::ffff:127.0.0.1", "::ffff:100.100.100.200"}
	for _, s := range denied {
		if err := p.CheckAddr(netip.MustParseAddr(s)); err != ErrDenied {
			t.Errorf("%s should be denied, got %v", s, err)
		}
	}
	allowed := []string{"8.8.8.8", "1.1.1.1", "100.128.0.1", "47.236.165.75", "2400:3200::1"}
	for _, s := range allowed {
		if err := p.CheckAddr(netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s should be allowed, got %v", s, err)
		}
	}
	if err := p.CheckIP(net.ParseIP("100.100.100.200")); err != ErrDenied {
		t.Errorf("CheckIP: %v", err)
	}
	if err := p.Control("tcp", "[fe80::1%en0]:22", nil); err != ErrDenied {
		t.Errorf("zoned link-local: %v", err)
	}
	if err := p.Control("tcp", "garbage", nil); err != ErrDenied {
		t.Errorf("unparsable address must be denied: %v", err)
	}
}

func TestExtraDeny(t *testing.T) {
	p, err := New([]string{"47.236.0.0/16", "8.211.182.3", "2408:4000::/32"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"47.236.165.75", "8.211.182.3", "::ffff:8.211.182.3", "2408:4000::1"} {
		if err := p.CheckAddr(netip.MustParseAddr(s)); err != ErrDenied {
			t.Errorf("%s should be denied", s)
		}
	}
	if err := p.CheckAddr(netip.MustParseAddr("8.211.182.4")); err != nil {
		t.Errorf("single IP entry must not widen: %v", err)
	}
	for _, bad := range []string{"", "x", "10.0.0.0/33", "1.2.3"} {
		if _, err := New([]string{bad}); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
