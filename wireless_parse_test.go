package main

import (
	"fmt"
	"testing"
	"time"
)

func TestParseWirelessShow(t *testing.T) {
	tests := []struct {
		name             string
		in               string
		staSSID          string
		ssid0            string
		ssid1            string
	}{
		{
			name: "sta mode plus two APs",
			in: `wireless.radio0=wifi-device
wireless.cfg0a2b63=wifi-iface
wireless.cfg0a2b63.mode='sta'
wireless.cfg0a2b63.ssid='Upstream Hotspot_5G'
wireless.default_radio0=wifi-iface
wireless.default_radio0.ssid='photonicat2'
wireless.default_radio1=wifi-iface
wireless.default_radio1.ssid='photonicat2-5G'`,
			staSSID: "Upstream Hotspot_5G",
			// ssid0/ssid1 follow uci output order of ssid-bearing sections
			ssid0: "Upstream Hotspot_5G",
			ssid1: "photonicat2",
		},
		{
			name: "ap only, no sta",
			in: `wireless.default_radio0=wifi-iface
wireless.default_radio0.ssid='photonicat2'
wireless.default_radio1=wifi-iface
wireless.default_radio1.ssid='photonicat2-5G'`,
			staSSID: "",
			ssid0:   "photonicat2",
			ssid1:   "photonicat2-5G",
		},
		{
			name: "ssid with spaces and double quotes",
			in: `wireless.default_radio0.ssid="My Net 2.4"
`,
			staSSID: "",
			ssid0:   "My Net 2.4",
			ssid1:   "",
		},
		{
			name:    "empty",
			in:      "",
			staSSID: "",
			ssid0:   "",
			ssid1:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseWirelessShow(tt.in)
			if got.staSSID != tt.staSSID {
				t.Errorf("staSSID = %q, want %q", got.staSSID, tt.staSSID)
			}
			if got.ssid0 != tt.ssid0 {
				t.Errorf("ssid0 = %q, want %q", got.ssid0, tt.ssid0)
			}
			if got.ssid1 != tt.ssid1 {
				t.Errorf("ssid1 = %q, want %q", got.ssid1, tt.ssid1)
			}
		})
	}
}

func TestStringCache(t *testing.T) {
	c := newStringCache(time.Hour)
	calls := 0
	fetch := func() (string, error) { calls++; return "v1", nil }

	// First call fetches.
	if v, _ := c.get(fetch); v != "v1" || calls != 1 {
		t.Fatalf("first get: v=%q calls=%d", v, calls)
	}
	// Within TTL: cached, no new fetch.
	if v, _ := c.get(fetch); v != "v1" || calls != 1 {
		t.Fatalf("cached get: v=%q calls=%d (should not refetch)", v, calls)
	}

	// On error after expiry, the last good value is served.
	c2 := newStringCache(0) // 0 TTL -> always expired
	if v, _ := c2.get(func() (string, error) { return "good", nil }); v != "good" {
		t.Fatalf("seed: v=%q", v)
	}
	v, err := c2.get(func() (string, error) { return "", errTest })
	if err != nil || v != "good" {
		t.Fatalf("error path should serve stale: v=%q err=%v", v, err)
	}

	// With no prior value, an error propagates.
	c3 := newStringCache(time.Hour)
	if _, err := c3.get(func() (string, error) { return "", errTest }); err == nil {
		t.Fatal("expected error when no cached value exists")
	}
}

var errTest = fmt.Errorf("boom")
