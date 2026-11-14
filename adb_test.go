package main

import (
	"crypto/rsa"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/m3db/prometheus_remote_client_golang/promremote"
	"github.com/pgaskin/go-adb/adb/adbhost"
)

// fakeSysfs builds a minimal /sys with usb0 on USB device 1-2 (behind its
// interface 1-2:1.0) and eth0 on a PCI device.
func fakeSysfs(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mk := func(rel string) string {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	touch := func(dir string, names ...string) {
		for _, n := range names {
			if err := os.WriteFile(filepath.Join(dir, n), []byte("1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	link := func(target, name string) {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}

	touch(mk("devices/pci0000:00/0000:00:14.0/usb1"), "busnum", "devnum") // root hub
	touch(mk("devices/pci0000:00/0000:00:14.0/usb1/1-2"), "busnum", "devnum")
	usbNet := mk("devices/pci0000:00/0000:00:14.0/usb1/1-2/1-2:1.0/net/usb0")
	if err := os.Symlink("../../", filepath.Join(usbNet, "device")); err != nil {
		t.Fatal(err)
	}
	ethNet := mk("devices/pci0000:00/0000:00:1f.6/net/eth0")
	if err := os.Symlink("../../", filepath.Join(ethNet, "device")); err != nil {
		t.Fatal(err)
	}
	mk("class/net")
	link("../../devices/pci0000:00/0000:00:14.0/usb1/1-2/1-2:1.0/net/usb0", "class/net/usb0")
	link("../../devices/pci0000:00/0000:00:1f.6/net/eth0", "class/net/eth0")
	return root
}

func TestUSBDeviceID(t *testing.T) {
	root := fakeSysfs(t)
	if id, err := usbDeviceID(root, "usb0"); err != nil || id != "1-2" {
		t.Errorf("usb0: got %q, %v; want 1-2", id, err)
	}
	if id, err := usbDeviceID(root, "eth0"); err == nil {
		t.Errorf("eth0: got %q, want an error", id)
	}
	if id, err := usbDeviceID(root, "usb9"); err == nil {
		t.Errorf("usb9: got %q, want an error", id)
	}
}

func TestNewSeriesSortsLabels(t *testing.T) {
	ts := newSeries("m", 1, time.Unix(0, 0),
		[]promremote.Label{{Name: "interface", Value: "wan_usb"}, {Name: "device", Value: "phone"}},
		label("slot", "0"), label("rat", "lte"))
	var names []string
	for _, l := range ts.Labels {
		names = append(names, l.Name)
	}
	want := []string{"__name__", "device", "interface", "rat", "slot"}
	if len(names) != len(want) {
		t.Fatalf("labels %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("labels %v, want %v", names, want)
		}
	}
}

func TestAppendSeries(t *testing.T) {
	now := time.Now()
	c := &adbCollector{
		cfg: adbConfig{Interval: time.Minute, Timeout: 20 * time.Second},
		snaps: map[string]phoneSnapshot{
			"1-1": {Up: true, SampledAt: now.Add(-30 * time.Second), Samples: []sample{{Name: "tether_phone_battery_level_percent", Value: 80}}},
			"1-2": {Up: false, SampledAt: now.Add(-10 * time.Minute), Samples: []sample{{Name: "tether_phone_battery_level_percent", Value: 50}}},
			"1-3": {Up: false, Unauthorized: true},
		},
	}
	base := []promremote.Label{{Name: "device", Value: "phone"}, {Name: "interface", Value: "wan_usb"}}
	names := func(list []promremote.TimeSeries) map[string]float64 {
		m := make(map[string]float64)
		for _, ts := range list {
			m[ts.Labels[0].Value] = ts.Datapoint.Value
		}
		return m
	}

	if got := names(c.appendSeries(nil, "1-1", base, now)); len(got) != 2 || got["tether_phone_adb_up"] != 1 || got["tether_phone_battery_level_percent"] != 80 {
		t.Errorf("fresh phone: got %v", got)
	}
	if got := names(c.appendSeries(nil, "1-2", base, now)); len(got) != 1 || got["tether_phone_adb_up"] != 0 {
		t.Errorf("stale phone should only report adb_up=0, got %v", got)
	}
	if got := names(c.appendSeries(nil, "1-3", base, now)); len(got) != 1 || got["tether_phone_adb_up"] != 0 {
		t.Errorf("unauthorized phone should only report adb_up=0, got %v", got)
	}
	if got := c.appendSeries(nil, "1-9", base, now); len(got) != 0 {
		t.Errorf("phone without ADB should get nothing, got %v", got)
	}
}

func TestSetTargetsWakesOnChange(t *testing.T) {
	c := &adbCollector{wake: make(chan struct{}, 1), targets: map[string]bool{}}
	drained := func() bool {
		select {
		case <-c.wake:
			return true
		default:
			return false
		}
	}
	c.setTargets(map[string]bool{})
	if drained() {
		t.Error("unchanged (empty) targets should not wake the poller")
	}
	c.setTargets(map[string]bool{"1-2": true})
	if !drained() {
		t.Error("new target should wake the poller")
	}
	c.setTargets(map[string]bool{"1-2": true})
	if drained() {
		t.Error("same targets should not wake the poller")
	}
}

func TestLoadOrCreateADBKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "adbkey")
	k1, err := loadOrCreateADBKey(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("key mode %v, want 0600", st.Mode().Perm())
	}
	buf, _ := os.ReadFile(path)
	if blk, _ := pem.Decode(buf); blk == nil || blk.Type != "PRIVATE KEY" {
		t.Errorf("key is not a PKCS#8 PEM like adb's own adbkey")
	}
	if _, err := adbhost.ParseKey(buf); err != nil {
		t.Errorf("adbhost can't parse the generated key: %v", err)
	}
	k2, err := loadOrCreateADBKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !k1.Public().(*rsa.PublicKey).Equal(k2.Public()) {
		t.Error("second load generated a different key instead of reusing it")
	}
	if k1.Public().(*rsa.PublicKey).N.BitLen() != 2048 {
		t.Error("want an RSA-2048 key")
	}
}

func TestLoadADBConfig(t *testing.T) {
	for _, k := range []string{"ADB_ENABLED", "ADB_KEY_PATH", "ADB_INTERVAL_SECONDS", "ADB_TIMEOUT_SECONDS"} {
		t.Setenv(k, "")
	}
	cfg := loadADBConfig()
	if !cfg.Enabled || cfg.KeyPath != defaultADBKeyPath || cfg.Interval != time.Minute || cfg.Timeout != 20*time.Second {
		t.Errorf("defaults: %+v", cfg)
	}
	t.Setenv("ADB_ENABLED", "off")
	t.Setenv("ADB_KEY_PATH", "/tmp/k")
	t.Setenv("ADB_INTERVAL_SECONDS", "30")
	t.Setenv("ADB_TIMEOUT_SECONDS", "bogus")
	cfg = loadADBConfig()
	if cfg.Enabled || cfg.KeyPath != "/tmp/k" || cfg.Interval != 30*time.Second || cfg.Timeout != 20*time.Second {
		t.Errorf("overrides: %+v", cfg)
	}
}
