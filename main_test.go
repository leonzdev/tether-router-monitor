package main

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeIfusb puts an ifusb helper that prints out first on PATH.
func fakeIfusb(t *testing.T, out string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncat <<'EOF'\n" + out + "\nEOF\n"
	if err := os.WriteFile(filepath.Join(dir, "ifusb"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestGetUSBDevice(t *testing.T) {
	old := sysRoot
	sysRoot = fakeSysfs(t) // usb0 is on USB device 1-2, a "motorola" "motorola edge 2024"
	defer func() { sysRoot = old }()

	fakeIfusb(t, `{ "bus": "001", "device": "005", "description": "Google Pixel 8" }`)
	if got, err := getUSBDevice("usb0"); err != nil || got != "Google Pixel 8" {
		t.Errorf("from lsusb: %q, %v", got, err)
	}

	// lsusb lost the device but sysfs still has it: use its USB strings.
	fakeIfusb(t, `{ "bus": "001", "device": "005", "description": "" }`)
	if got, err := getUSBDevice("usb0"); err != nil || got != "motorola motorola edge 2024" {
		t.Errorf("sysfs fallback: %q, %v", got, err)
	}

	// Unplugged: gone from lsusb and sysfs alike (ifusb prints null). This used
	// to yield an empty device label, i.e. a nameless series on dashboards.
	fakeIfusb(t, `null`)
	if got, err := getUSBDevice("usb9"); err == nil {
		t.Errorf("unplugged phone: got %q, want an error", got)
	}
}
