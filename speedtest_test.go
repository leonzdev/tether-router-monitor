package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/m3db/prometheus_remote_client_golang/promremote"
)

func TestSteadyMbps(t *testing.T) {
	var pts []meterPoint
	var total int64
	add := func(at time.Duration, n int64) {
		total += n
		pts = append(pts, meterPoint{at, total})
	}
	// TCP ramp-up: 2 MB in the first second.
	for i := 1; i <= 20; i++ {
		add(time.Duration(i)*50*time.Millisecond, 100_000)
	}
	// Then 8 MB at 10 MB/s, i.e. 80 Mb/s.
	for i := 1; i <= 80; i++ {
		add(time.Second+time.Duration(i)*10*time.Millisecond, 100_000)
	}
	if got := steadyMbps(pts); math.Abs(got-80) > 0.5 {
		t.Errorf("steadyMbps = %.2f, want 80 (the average over the whole transfer would be 44)", got)
	}

	if got := steadyMbps([]meterPoint{{0, 1000}, {time.Second, 2000}}); got <= 0 {
		t.Errorf("two points: %v, want > 0", got)
	}
	if got := steadyMbps([]meterPoint{{time.Second, 1000}}); got != 0 {
		t.Errorf("one point: %v, want 0", got)
	}
	if got := steadyMbps(nil); got != 0 {
		t.Errorf("no points: %v, want 0", got)
	}
}

func TestMedianJitter(t *testing.T) {
	ms := func(vs ...float64) []time.Duration {
		var out []time.Duration
		for _, v := range vs {
			out = append(out, time.Duration(v*float64(time.Millisecond)))
		}
		return out
	}
	for _, tc := range []struct {
		in   []time.Duration
		want time.Duration
	}{
		{ms(30, 10, 20), 20 * time.Millisecond},
		{ms(10, 20, 30, 40), 25 * time.Millisecond},
		{nil, 0},
	} {
		if got := median(tc.in); got != tc.want {
			t.Errorf("median(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
	if got := jitter(ms(10, 20, 15)); got != 7500*time.Microsecond {
		t.Errorf("jitter = %v, want 7.5ms", got)
	}
	if got := jitter(ms(10)); got != 0 {
		t.Errorf("jitter of one sample = %v, want 0", got)
	}
}

func TestColoFromRay(t *testing.T) {
	for ray, want := range map[string]string{
		"8f1c2a3b4c5d6e7f-SEA": "SEA",
		"8f1c2a3b4c5d6e7f-LAX": "LAX",
		"8f1c2a3b4c5d6e7f":     "",
		"8f1c2a3b4c5d6e7f-sea": "",
		"8f1c2a3b4c5d6e7f-X1Y": "",
		"":                     "",
	} {
		if got := coloFromRay(ray); got != want {
			t.Errorf("coloFromRay(%q) = %q, want %q", ray, got, want)
		}
	}
}

func TestLoadSpeedtestConfig(t *testing.T) {
	vars := []string{"SPEEDTEST_ENABLED", "SPEEDTEST_INTERVAL_MINUTES", "SPEEDTEST_DOWNLOAD_MB", "SPEEDTEST_UPLOAD_MB",
		"SPEEDTEST_MAX_SECONDS", "SPEEDTEST_BUSY_MBPS", "SPEEDTEST_URL", "SPEEDTEST_FWMARK"}
	for _, v := range vars {
		t.Setenv(v, "")
	}
	want := speedtestConfig{
		Interval:      time.Hour,
		DownloadBytes: 10_000_000,
		UploadBytes:   5_000_000,
		MaxDuration:   5 * time.Second,
		BusyMbps:      2,
		URL:           defaultSpeedtestURL,
		FWMark:        mwan3DefaultMark(mwan3MaskPath),
	}
	if got := loadSpeedtestConfig(); got != want {
		t.Errorf("defaults:\n got %+v\nwant %+v", got, want)
	}

	for v, val := range map[string]string{
		"SPEEDTEST_ENABLED":          "1",
		"SPEEDTEST_INTERVAL_MINUTES": "30",
		"SPEEDTEST_DOWNLOAD_MB":      "25",
		"SPEEDTEST_UPLOAD_MB":        "0",
		"SPEEDTEST_MAX_SECONDS":      "3.5",
		"SPEEDTEST_BUSY_MBPS":        "0",
		"SPEEDTEST_URL":              "https://speed.example.test/",
		"SPEEDTEST_FWMARK":           "0x100",
	} {
		t.Setenv(v, val)
	}
	want = speedtestConfig{
		Enabled:       true,
		Interval:      30 * time.Minute,
		DownloadBytes: 25_000_000,
		UploadBytes:   0,
		MaxDuration:   3500 * time.Millisecond,
		BusyMbps:      0,
		URL:           "https://speed.example.test",
		FWMark:        0x100,
	}
	if got := loadSpeedtestConfig(); got != want {
		t.Errorf("set:\n got %+v\nwant %+v", got, want)
	}

	t.Setenv("SPEEDTEST_INTERVAL_MINUTES", "0") // too short: default
	t.Setenv("SPEEDTEST_FWMARK", "0")           // no mark
	if got := loadSpeedtestConfig(); got.Interval != time.Hour || got.FWMark != 0 {
		t.Errorf("interval 0 and mark 0: got %s and %#x, want 1h and 0", got.Interval, got.FWMark)
	}
}

func TestMwan3DefaultMark(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mmx_mask")
	if got := mwan3DefaultMark(path); got != 0x3f00 {
		t.Errorf("no mwan3: %#x, want 0x3f00", got)
	}
	os.WriteFile(path, []byte("0xff00\n"), 0o644)
	if got := mwan3DefaultMark(path); got != 0xff00 {
		t.Errorf("custom mask: %#x, want 0xff00", got)
	}
	os.WriteFile(path, []byte("junk\n"), 0o644)
	if got := mwan3DefaultMark(path); got != 0x3f00 {
		t.Errorf("unreadable mask: %#x, want 0x3f00", got)
	}
}

func clockAt(t *testing.T, hms string) time.Time {
	t.Helper()
	ts, err := time.Parse("2006-01-02 15:04:05", "2026-11-14 "+hms)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

var allTethers = []string{"tether0", "tether1", "tether2", "tether3", "tether4"}

func TestSpeedtestSlots(t *testing.T) {
	c := newSpeedCollector(speedtestConfig{Interval: time.Hour})
	c.setTargets([]string{"tether3", "tether0", "tether1", "tether2", "tether4"}, nil)
	for i, want := range []string{"10:06", "10:18", "10:30", "10:42", "10:54"} {
		iface := fmt.Sprintf("tether%d", i)
		if got := c.slotStart(iface, clockAt(t, "10:59:59")); !got.Equal(clockAt(t, want+":00")) {
			t.Errorf("%s: slot %s, want %s", iface, got.Format("15:04:05"), want)
		}
	}
	if got := c.slotStart("tether2", clockAt(t, "10:29:59")); !got.Equal(clockAt(t, "09:30:00")) {
		t.Errorf("just before its slot: %s, want 09:30", got.Format("15:04:05"))
	}
}

func TestSpeedtestSchedule(t *testing.T) {
	c := newSpeedCollector(speedtestConfig{Interval: time.Hour, BusyMbps: 2})
	var clock time.Time
	var ran []string
	busy := 0.0
	c.runTest = func(_ context.Context, tg speedTarget) speedResult {
		ran = append(ran, tg.Iface)
		return speedResult{At: clock, OK: true, DownloadMbps: 50}
	}
	c.traffic = func(context.Context, string) (float64, error) { return busy, nil }
	targets := func(online ...string) []speedTarget {
		var out []speedTarget
		for _, name := range []string{"tether0", "tether2"} {
			out = append(out, speedTarget{Iface: name, Netdev: "usb" + name[6:], Online: slices.Contains(online, name)})
		}
		return out
	}
	step := func(hms string, want ...string) {
		t.Helper()
		clock, ran = clockAt(t, hms), nil
		c.tick(context.Background(), clock)
		if !slices.Equal(ran, want) {
			t.Errorf("at %s: tested %v, want %v", hms, ran, want)
		}
	}

	c.setTargets(allTethers, targets("tether0", "tether2")) // slots: tether0 at :06, tether2 at :30
	step("10:20:00")                                        // just started: nothing, though tether0's slot has passed
	step("10:29:59")
	step("10:30:05", "tether2")
	step("10:31:00")
	step("11:06:10", "tether0")

	busy = 10 // Mb/s of other traffic
	step("11:30:05")
	step("11:33:00")
	step("11:35:10")
	step("11:40:15")
	step("11:45:20", "tether2") // still busy after 3 tries: test anyway
	busy = 0

	c.setTargets(allTethers, targets("tether2")) // tether0 offline in mwan3
	step("12:06:10")
	step("12:30:05", "tether2")
	c.setTargets(allTethers, targets("tether0", "tether2"))
	step("12:50:00", "tether0") // back online: tested late in its slot
	step("13:06:10")            // too soon after that
	step("13:20:05", "tether0")

	// A restart (a new collector) waits for the next slot, too.
	c2 := newSpeedCollector(c.cfg)
	c2.runTest, c2.traffic = c.runTest, c.traffic
	c2.setTargets(allTethers, targets("tether0", "tether2"))
	ran = nil
	c2.tick(context.Background(), clockAt(t, "13:31:00"))
	if len(ran) != 0 {
		t.Errorf("after a restart: tested %v, want nothing until the next slot", ran)
	}
}

// fakeSpeedServer serves the speed test API like Cloudflare does; down
// replaces the /__down handler if set. The returned counter has the upload
// bytes received.
func fakeSpeedServer(t *testing.T, down http.HandlerFunc) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var upBytes atomic.Int64
	if down == nil {
		down = func(w http.ResponseWriter, r *http.Request) {
			n, err := strconv.Atoi(r.URL.Query().Get("bytes"))
			if err != nil || n < 0 {
				http.Error(w, "bad bytes", http.StatusBadRequest)
				return
			}
			w.Header().Set("Cf-Ray", "8f1c2a3b4c5d6e7f-SEA")
			w.Header().Set("Content-Length", strconv.Itoa(n))
			buf := make([]byte, 32<<10)
			for n > 0 {
				k := min(n, len(buf))
				if _, err := w.Write(buf[:k]); err != nil {
					return
				}
				n -= k
			}
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/__down", down)
	mux.HandleFunc("/__up", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		n, _ := io.Copy(io.Discard, r.Body)
		upBytes.Add(n)
		w.Header().Set("Cf-Ray", "8f1c2a3b4c5d6e7f-SEA")
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv, &upBytes
}

// testCollector talks to srv without binding to an interface or marking.
func testCollector(srv *httptest.Server, cfg speedtestConfig) *speedCollector {
	cfg.URL = srv.URL
	c := newSpeedCollector(cfg)
	c.tlsConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
	c.pinned = func(string) *net.Dialer { return &net.Dialer{Timeout: 5 * time.Second} }
	return c
}

func TestSpeedtestAgainstServer(t *testing.T) {
	srv, upBytes := fakeSpeedServer(t, nil)
	c := testCollector(srv, speedtestConfig{DownloadBytes: 2_000_000, UploadBytes: 1_000_003, MaxDuration: 10 * time.Second})
	res := c.test(context.Background(), speedTarget{Iface: "tether2", Netdev: "usb2"})
	if !res.OK || res.Err != "" {
		t.Fatalf("test failed: %+v", res)
	}
	if res.DownBytes != 2_000_000 || res.UpBytes != 1_000_003 || upBytes.Load() != 1_000_003 {
		t.Errorf("moved %d down and %d up (server got %d up), want exactly the caps", res.DownBytes, res.UpBytes, upBytes.Load())
	}
	if res.DownloadMbps <= 0 || res.UploadMbps <= 0 || res.Latency <= 0 {
		t.Errorf("no rates or latency: %+v", res)
	}
	if res.Colo != "SEA" {
		t.Errorf("colo = %q, want SEA", res.Colo)
	}
	if !strings.Contains(res.summary(), "server SEA, used 3.0 MB in") {
		t.Errorf("summary: %s", res.summary())
	}
}

func TestSpeedtestTimeCap(t *testing.T) {
	// A slow link: each connection trickles 1 kB every 10 ms.
	srv, _ := fakeSpeedServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cf-Ray", "8f1c2a3b4c5d6e7f-SEA")
		for range 1000 {
			if _, err := w.Write(make([]byte, 1000)); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	})
	c := testCollector(srv, speedtestConfig{DownloadBytes: 50_000_000, MaxDuration: 400 * time.Millisecond})
	res := c.test(context.Background(), speedTarget{Iface: "tether2", Netdev: "usb2"})
	if res.Took > 3*time.Second {
		t.Errorf("took %s, want it stopped by the 400ms cap", res.Took)
	}
	if !res.OK || res.DownBytes == 0 || res.DownBytes >= 1_000_000 {
		t.Errorf("want a partial download: %+v", res)
	}
	// 4 connections x 100 kB/s = 3.2 Mb/s
	if res.DownloadMbps <= 0.5 || res.DownloadMbps > 20 {
		t.Errorf("rate %.2f Mb/s, want about 3", res.DownloadMbps)
	}
}

func TestSpeedtestErrors(t *testing.T) {
	c := newSpeedCollector(speedtestConfig{URL: "http://127.0.0.1:1", DownloadBytes: 1000, MaxDuration: time.Second})
	c.pinned = func(string) *net.Dialer { return &net.Dialer{Timeout: time.Second} }
	if res := c.test(context.Background(), speedTarget{Iface: "tether2", Netdev: "usb2"}); res.OK || !strings.Contains(res.Err, "can't reach 127.0.0.1 through usb2") {
		t.Errorf("unreachable server: %+v", res)
	}

	srv, _ := fakeSpeedServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "slow down", http.StatusTooManyRequests)
	})
	c = testCollector(srv, speedtestConfig{DownloadBytes: 1000, MaxDuration: 2 * time.Second})
	if res := c.test(context.Background(), speedTarget{Iface: "tether2", Netdev: "usb2"}); res.OK || !strings.Contains(res.Err, "download: HTTP 429") {
		t.Errorf("rate limited: %+v", res)
	}
}

func TestPinnedDialer(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	addr := ln.Addr().String()

	if _, err := pinnedDialer("nosuchdev0", 0).Dial("tcp4", addr); err == nil || !strings.Contains(err.Error(), "bind to nosuchdev0") {
		t.Errorf("unknown interface: %v, want a bind error", err)
	}
	if conn, err := pinnedDialer("lo", 0).Dial("tcp4", addr); errors.Is(err, syscall.EPERM) {
		t.Log("binding to an interface needs CAP_NET_RAW on this kernel")
	} else if err != nil {
		t.Errorf("bound to lo: %v", err)
	} else {
		conn.Close()
	}
	conn, err := pinnedDialer("", 0x3f00).Dial("tcp4", addr)
	switch {
	case os.Geteuid() == 0 && err != nil:
		t.Errorf("marked, as root: %v", err)
	case os.Geteuid() != 0 && (err == nil || !strings.Contains(err.Error(), "set firewall mark 0x3f00")):
		t.Errorf("marked, not root: %v, want a permission error", err)
	}
	if conn != nil {
		conn.Close()
	}
}

func TestTrafficMbps(t *testing.T) {
	root := t.TempDir()
	stats := filepath.Join(root, "class", "net", "usb2", "statistics")
	if err := os.MkdirAll(stats, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rx, tx int) {
		os.WriteFile(filepath.Join(stats, "rx_bytes"), []byte(strconv.Itoa(rx)+"\n"), 0o644)
		os.WriteFile(filepath.Join(stats, "tx_bytes"), []byte(strconv.Itoa(tx)+"\n"), 0o644)
	}
	write(1000, 500)
	c := newSpeedCollector(speedtestConfig{})
	c.sysRoot, c.busyWindow = root, 100*time.Millisecond
	go func() {
		time.Sleep(20 * time.Millisecond)
		write(1000+100_000, 500+25_000) // 125 kB in 100 ms = 10 Mb/s
	}()
	if got, err := c.trafficMbps(context.Background(), "usb2"); err != nil || math.Abs(got-10) > 1e-9 {
		t.Errorf("trafficMbps = %v, %v; want 10", got, err)
	}
	if _, err := c.trafficMbps(context.Background(), "usb9"); err == nil {
		t.Error("missing interface: want an error")
	}
}

func TestSpeedtestSeries(t *testing.T) {
	c := newSpeedCollector(speedtestConfig{Interval: time.Hour})
	at := clockAt(t, "10:30:05")
	d := dueTest{speedTarget{Iface: "tether2", Netdev: "usb2", Device: "Google Pixel 8"}, clockAt(t, "10:30:00")}
	res := speedResult{
		At: at, OK: true, DownloadMbps: 85.3, UploadMbps: 12.1,
		Latency: 31 * time.Millisecond, Jitter: 4 * time.Millisecond,
		DownLatency: 120 * time.Millisecond, UpLatency: 380 * time.Millisecond,
		DownBytes: 10_000_000, UpBytes: 5_000_000, Colo: "SEA",
	}
	c.record(d, res)
	c.record(d, res) // a second test adds to the data used

	base := []promremote.Label{{Name: "device", Value: "Google Pixel 8"}, {Name: "interface", Value: "tether2"}}
	collect := func(list []promremote.TimeSeries) map[string]float64 {
		got := make(map[string]float64)
		for _, s := range list {
			if !sort.SliceIsSorted(s.Labels, func(i, j int) bool { return s.Labels[i].Name < s.Labels[j].Name }) {
				t.Errorf("labels not sorted: %v", s.Labels)
			}
			var key []string
			for _, l := range s.Labels {
				if l.Name != "device" && l.Name != "interface" {
					key = append(key, l.Name+"="+l.Value)
				}
			}
			got[strings.Join(key, ",")] = s.Datapoint.Value
		}
		return got
	}
	got := collect(c.appendSeries(nil, "tether2", "Google Pixel 8", base, at.Add(time.Minute)))
	want := map[string]float64{
		"__name__=tether_iface_speedtest_download_mbps":                  85.3,
		"__name__=tether_iface_speedtest_upload_mbps":                    12.1,
		"__name__=tether_iface_speedtest_latency_seconds,phase=idle":     0.031,
		"__name__=tether_iface_speedtest_latency_seconds,phase=download": 0.12,
		"__name__=tether_iface_speedtest_latency_seconds,phase=upload":   0.38,
		"__name__=tether_iface_speedtest_jitter_seconds":                 0.004,
		"__name__=tether_iface_speedtest_bytes_total,direction=download": 20_000_000,
		"__name__=tether_iface_speedtest_bytes_total,direction=upload":   10_000_000,
		"__name__=tether_iface_speedtest_success":                        1,
		"__name__=tether_iface_speedtest_last_run_timestamp_seconds":     float64(at.Unix()),
		"__name__=tether_iface_speedtest_server_info,colo=SEA":           1,
	}
	if len(got) != len(want) {
		t.Errorf("got %d series, want %d: %v", len(got), len(want), got)
	}
	for k, v := range want {
		if g, ok := got[k]; !ok || math.Abs(g-v) > 1e-9 {
			t.Errorf("%s = %v (present %t), want %v", k, g, ok, v)
		}
	}

	// Another phone now on the same interface doesn't inherit the result.
	if list := c.appendSeries(nil, "tether2", "Google Pixel 10", base, at.Add(time.Minute)); len(list) != 0 {
		t.Errorf("other phone: got %d series, want none", len(list))
	}
	// Long after the test only the bookkeeping series remain.
	if got := collect(c.appendSeries(nil, "tether2", "Google Pixel 8", base, at.Add(91*time.Minute))); len(got) != 4 {
		t.Errorf("stale: got %v, want only bytes_total, last_run and success", got)
	}
}

// fakeCommands puts scripts that print the given output first on PATH.
func fakeCommands(t *testing.T, outputs map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, out := range outputs {
		script := "#!/bin/sh\ncat <<'EOF'\n" + out + "\nEOF\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestCollectInterfaces(t *testing.T) {
	fakeCommands(t, map[string]string{
		"ifdev": `[
  { "interface": "lan", "device": "br-lan" },
  { "interface": "tether0", "device": "usb0" },
  { "interface": "tether2", "device": "usb2" }
]`,
		"mwan3ifstatus": `[ {"interface":"tether0","status":"online","online_time":"00h:11m:11s","uptime":"19h:03m:23s","tracking":"active"}, ` +
			`{"interface":"tether2","status":"offline","online_time":"","uptime":"","tracking":"paused"}, ` +
			`{"interface":"wan","status":"online","online_time":"01h:00m:00s","uptime":"01h:00m:00s","tracking":"active"} ]`,
		"ifconfig": `usb0      Link encap:Ethernet  HWaddr 02:00:00:00:00:01
          RX packets:10 errors:0 dropped:0 overruns:0 frame:0
          RX bytes:1234 (1.2 KiB)  TX bytes:5678 (5.5 KiB)

br-lan    Link encap:Ethernet  HWaddr 02:00:00:00:00:02
          RX bytes:1 (1.0 B)  TX bytes:2 (2.0 B)`,
	})
	got, err := collectInterfaces()
	if err != nil {
		t.Fatal(err)
	}
	want := []CombinedData{
		{Interface: "tether0", Device: "usb0", Status: "online", OnlineTime: "00h:11m:11s", Uptime: "19h:03m:23s", Tracking: "active", RX: 1234, TX: 5678},
		{Interface: "tether2", Device: "usb2", Status: "offline", Tracking: "paused"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("collectInterfaces:\n got %+v\nwant %+v", got, want)
	}
}
