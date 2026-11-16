package main

// Speed tests through each tethered phone. The monitor tests one phone at a
// time, each at its own fixed time within the test interval.
//
// mwan3 load-balances the router's own traffic too, so a test only measures the
// phone it means to if its sockets are pinned the way mwan3's own tracking
// pings are (`mwan3 use`): bound to the phone's network interface
// (SO_BINDTODEVICE) and given mwan3's "default" firewall mark, so mwan3's rules
// leave them alone and the main routing table sends them out that interface.
//
// Every byte is mobile data, so each direction stops at a byte cap or a time
// cap, whichever comes first.

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/m3db/prometheus_remote_client_golang/promremote"
)

const (
	// Cloudflare's speed test API (what speed.cloudflare.com uses): GET
	// /__down?bytes=N sends N bytes, POST /__up takes a body. It's anycast, so
	// each phone reaches the nearest Cloudflare data center through its own
	// carrier.
	defaultSpeedtestURL = "https://speed.cloudflare.com"

	// mwan3 keeps its firewall mask here. Packets marked with the whole mask
	// (mwan3's "default" mark) skip mwan3's rules.
	mwan3MaskPath    = "/var/run/mwan3/mmx_mask"
	defaultMwan3Mask = 0x3f00

	speedtestConns   = 4                      // parallel connections per direction
	latencyProbes    = 5                      // TCP handshakes timed before the transfers
	loadedProbeEvery = 200 * time.Millisecond // TCP handshakes timed during the transfers
	busyRetryAfter   = 5 * time.Minute
	maxBusyRetries   = 3 // then test anyway
	speedtestAgent   = "tether-router-monitor"
)

type speedtestConfig struct {
	Enabled       bool
	Interval      time.Duration // how often each phone is tested
	DownloadBytes int64         // download cap per test (0: no download test)
	UploadBytes   int64         // upload cap per test (0: no upload test)
	MaxDuration   time.Duration // time cap per direction
	BusyMbps      float64       // postpone a test while the phone carries more than this (0: never)
	URL           string        // speed test server
	FWMark        int           // firewall mark for the test sockets (0: none)
}

func isOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// envFloat returns the environment variable as a number, or def if it is unset,
// not a number, or negative.
func envFloat(name string, def float64) float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(os.Getenv(name)), 64); err == nil && v >= 0 {
		return v
	}
	return def
}

func loadSpeedtestConfig() speedtestConfig {
	cfg := speedtestConfig{
		Enabled:       isOn(os.Getenv("SPEEDTEST_ENABLED")),
		Interval:      time.Duration(envFloat("SPEEDTEST_INTERVAL_MINUTES", 60) * float64(time.Minute)),
		DownloadBytes: int64(envFloat("SPEEDTEST_DOWNLOAD_MB", 10) * 1e6),
		UploadBytes:   int64(envFloat("SPEEDTEST_UPLOAD_MB", 5) * 1e6),
		MaxDuration:   time.Duration(envFloat("SPEEDTEST_MAX_SECONDS", 5) * float64(time.Second)),
		BusyMbps:      envFloat("SPEEDTEST_BUSY_MBPS", 2),
		URL:           defaultSpeedtestURL,
	}
	if cfg.Interval < time.Minute {
		cfg.Interval = 60 * time.Minute
	}
	if cfg.MaxDuration <= 0 {
		cfg.MaxDuration = 5 * time.Second
	}
	if v := strings.TrimSpace(os.Getenv("SPEEDTEST_URL")); v != "" {
		cfg.URL = strings.TrimRight(v, "/")
	}
	cfg.FWMark = mwan3DefaultMark(mwan3MaskPath)
	if v := strings.TrimSpace(os.Getenv("SPEEDTEST_FWMARK")); v != "" {
		if n, err := strconv.ParseInt(v, 0, 32); err == nil && n >= 0 {
			cfg.FWMark = int(n)
		}
	}
	return cfg
}

// mwan3DefaultMark returns the firewall mark that makes mwan3 leave a packet
// alone: its whole mask, as `mwan3 use` sets it.
func mwan3DefaultMark(maskPath string) int {
	if buf, err := os.ReadFile(maskPath); err == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(string(buf)), 0, 32); err == nil && v > 0 {
			return int(v)
		}
	}
	return defaultMwan3Mask
}

// speedTarget is a tethered interface the main loop saw on its last pass.
type speedTarget struct {
	Iface  string // mwan3 interface, e.g. tether2
	Netdev string // network device, e.g. usb2
	Device string // the phone's USB name, as in the device label
	Online bool   // mwan3 reports the interface online
}

func (t speedTarget) label() string {
	var extra []string
	if t.Netdev != "" && t.Netdev != t.Iface {
		extra = append(extra, t.Netdev)
	}
	if t.Device != "" {
		extra = append(extra, t.Device)
	}
	if len(extra) == 0 {
		return t.Iface
	}
	return t.Iface + " (" + strings.Join(extra, ", ") + ")"
}

type speedResult struct {
	At           time.Time
	OK           bool // every direction that was tested moved data
	Err          string
	DownloadMbps float64       // 0: not measured
	UploadMbps   float64       // 0: not measured
	Latency      time.Duration // idle: median TCP handshake before the transfers
	Jitter       time.Duration // mean change between consecutive idle samples
	DownLatency  time.Duration // median TCP handshake during the download (0: none)
	UpLatency    time.Duration // median TCP handshake during the upload (0: none)
	DownBytes    int64         // payload bytes downloaded
	UpBytes      int64         // payload bytes uploaded
	Colo         string        // Cloudflare data center that answered, e.g. SEA
	Took         time.Duration
}

func fmtMs(d time.Duration) string {
	return fmt.Sprintf("%.0f ms", float64(d)/float64(time.Millisecond))
}

// summary is the one-line result for the log and -speedtest-now.
func (r speedResult) summary() string {
	var parts []string
	if r.DownloadMbps > 0 {
		parts = append(parts, fmt.Sprintf("download %.1f Mb/s", r.DownloadMbps))
	}
	if r.UploadMbps > 0 {
		parts = append(parts, fmt.Sprintf("upload %.1f Mb/s", r.UploadMbps))
	}
	if r.Latency > 0 {
		l := "latency " + fmtMs(r.Latency) + " idle"
		if r.DownLatency > 0 {
			l += ", " + fmtMs(r.DownLatency) + " while downloading"
		}
		if r.UpLatency > 0 {
			l += ", " + fmtMs(r.UpLatency) + " while uploading"
		}
		parts = append(parts, l, "jitter "+fmtMs(r.Jitter))
	}
	if r.Colo != "" {
		parts = append(parts, "server "+r.Colo)
	}
	parts = append(parts, fmt.Sprintf("used %.1f MB in %.1f s", float64(r.DownBytes+r.UpBytes)/1e6, r.Took.Seconds()))
	s := strings.Join(parts, ", ")
	if r.Err != "" {
		s += "; error: " + r.Err
	}
	return s
}

// dueTest is a test whose slot has come.
type dueTest struct {
	speedTarget
	slot time.Time
}

type speedCollector struct {
	cfg speedtestConfig

	// Overridden in tests.
	sysRoot    string
	tlsConfig  *tls.Config
	busyWindow time.Duration
	pinned     func(netdev string) *net.Dialer
	runTest    func(ctx context.Context, t speedTarget) speedResult
	traffic    func(ctx context.Context, netdev string) (float64, error)

	mu      sync.Mutex
	ifaces  []string               // every USB interface mwan3 knows, sorted: fixes each one's slot
	targets map[string]speedTarget // by interface
	results map[string]speedResult // latest, by resultKey
	used    map[string][2]float64  // payload bytes down and up since start, by resultKey
	done    map[string]time.Time   // the slot last handled, by interface
	lastRun map[string]time.Time   // by interface
	retries map[string]int         // busy postponements in the current slot, by interface
	retryAt map[string]time.Time   // by interface
}

func newSpeedCollector(cfg speedtestConfig) *speedCollector {
	c := &speedCollector{
		cfg:        cfg,
		sysRoot:    sysRoot,
		busyWindow: 3 * time.Second,
		targets:    make(map[string]speedTarget),
		results:    make(map[string]speedResult),
		used:       make(map[string][2]float64),
		done:       make(map[string]time.Time),
		lastRun:    make(map[string]time.Time),
		retries:    make(map[string]int),
		retryAt:    make(map[string]time.Time),
	}
	c.pinned = func(netdev string) *net.Dialer { return pinnedDialer(netdev, cfg.FWMark) }
	c.runTest = c.test
	c.traffic = c.trafficMbps
	return c
}

// resultKey ties a result to the phone it measured, so a different phone that
// ends up on the same interface doesn't inherit it.
func resultKey(iface, device string) string { return iface + "\x00" + device }

// setTargets is called by the main loop on every pass. ifaces lists every USB
// interface mwan3 knows, tethered right now or not.
func (c *speedCollector) setTargets(ifaces []string, targets []speedTarget) {
	all := slices.Clone(ifaces)
	m := make(map[string]speedTarget, len(targets))
	for _, t := range targets {
		m[t.Iface] = t
		all = append(all, t.Iface)
	}
	slices.Sort(all)
	all = slices.Compact(all)
	c.mu.Lock()
	c.ifaces, c.targets = all, m
	c.mu.Unlock()
}

func (c *speedCollector) run(ctx context.Context) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.tick(ctx, time.Now())
		}
	}
}

// slotStart returns the start of the interface's latest test slot at or before
// now. The slots are spread evenly over the interval in interface order (with
// 5 interfaces and hourly tests: at :06, :18, :30, :42 and :54), so the phones
// are tested one at a time, and a restart doesn't move them. Call with c.mu held.
func (c *speedCollector) slotStart(iface string, now time.Time) time.Time {
	i, n := slices.Index(c.ifaces, iface), len(c.ifaces)
	if i < 0 {
		i, n = 0, 1
	}
	offset := time.Duration((float64(i) + 0.5) / float64(n) * float64(c.cfg.Interval))
	return now.Add(-offset).Truncate(c.cfg.Interval).Add(offset)
}

// due returns the targets whose slot has come and that haven't been tested in
// it yet.
func (c *speedCollector) due(now time.Time) []dueTest {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []dueTest
	for _, name := range slices.Sorted(maps.Keys(c.targets)) {
		t := c.targets[name]
		slot := c.slotStart(name, now)
		handled, seen := c.done[name]
		switch {
		case !seen:
			// First sight (the monitor just started, or the phone is new):
			// wait for its next slot, so a restart never sets off tests.
			c.done[name] = slot
		case !handled.Before(slot):
			// Already tested in this slot.
		case !t.Online, now.Before(c.retryAt[name]):
			// Wait until mwan3 has it online again, or for the busy retry.
		case !c.lastRun[name].IsZero() && now.Sub(c.lastRun[name]) < c.cfg.Interval/2:
			// Tested late in its previous slot (it was offline or busy then).
		default:
			out = append(out, dueTest{t, slot})
		}
	}
	return out
}

// tick runs the tests that are due, one after another.
func (c *speedCollector) tick(ctx context.Context, now time.Time) {
	for _, d := range c.due(now) {
		if ctx.Err() != nil {
			return
		}
		if c.cfg.BusyMbps > 0 {
			if mbps, err := c.traffic(ctx, d.Netdev); err == nil && mbps > c.cfg.BusyMbps && c.postpone(d, now, mbps) {
				continue
			}
		}
		c.record(d, c.runTest(ctx, d.speedTarget))
	}
}

// postpone holds off a test while the phone is busy with other traffic, so the
// test neither slows that traffic down nor gets skewed by it. After a few tries
// it tests anyway.
func (c *speedCollector) postpone(d dueTest, now time.Time, mbps float64) bool {
	c.mu.Lock()
	c.retries[d.Iface]++
	n := c.retries[d.Iface]
	if n <= maxBusyRetries {
		c.retryAt[d.Iface] = now.Add(busyRetryAfter)
	}
	c.mu.Unlock()
	if n <= maxBusyRetries {
		log.Printf("speed test on %s: busy with %.1f Mb/s of other traffic, trying again in %s", d.label(), mbps, busyRetryAfter)
		return true
	}
	log.Printf("speed test on %s: still busy (%.1f Mb/s), testing anyway", d.label(), mbps)
	return false
}

func (c *speedCollector) record(d dueTest, res speedResult) {
	key := resultKey(d.Iface, d.Device)
	c.mu.Lock()
	c.results[key] = res
	used := c.used[key]
	c.used[key] = [2]float64{used[0] + float64(res.DownBytes), used[1] + float64(res.UpBytes)}
	c.done[d.Iface] = d.slot
	c.lastRun[d.Iface] = res.At
	delete(c.retries, d.Iface)
	delete(c.retryAt, d.Iface)
	c.mu.Unlock()
	log.Printf("speed test on %s: %s", d.label(), res.summary())
}

// trafficMbps measures the traffic an interface carries right now, over
// busyWindow, from its kernel counters.
func (c *speedCollector) trafficMbps(ctx context.Context, netdev string) (float64, error) {
	a, err := ifaceBytes(c.sysRoot, netdev)
	if err != nil {
		return 0, err
	}
	if !sleepCtx(ctx, c.busyWindow) {
		return 0, ctx.Err()
	}
	b, err := ifaceBytes(c.sysRoot, netdev)
	if err != nil {
		return 0, err
	}
	return float64(b-a) * 8 / c.busyWindow.Seconds() / 1e6, nil
}

// ifaceBytes returns the bytes an interface has received plus sent.
func ifaceBytes(sysRoot, netdev string) (int64, error) {
	var total int64
	for _, name := range []string{"rx_bytes", "tx_bytes"} {
		buf, err := os.ReadFile(filepath.Join(sysRoot, "class", "net", netdev, "statistics", name))
		if err != nil {
			return 0, err
		}
		n, err := strconv.ParseInt(strings.TrimSpace(string(buf)), 10, 64)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// pinnedDialer returns a dialer whose sockets go out through netdev (if set) and
// carry the firewall mark (if not 0), like mwan3's own `mwan3 use`. The mark
// needs root (CAP_NET_ADMIN).
func pinnedDialer(netdev string, mark int) *net.Dialer {
	return &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, rc syscall.RawConn) error {
			var opErr error
			err := rc.Control(func(fd uintptr) {
				if netdev != "" {
					if err := syscall.BindToDevice(int(fd), netdev); err != nil {
						opErr = fmt.Errorf("bind to %s: %w", netdev, err)
						return
					}
				}
				if mark != 0 {
					if err := syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_MARK, mark); err != nil {
						opErr = fmt.Errorf("set firewall mark %#x: %w", mark, err)
					}
				}
			})
			if err != nil {
				return err
			}
			return opErr
		},
	}
}

type dialFunc func(ctx context.Context) (net.Conn, error)

// test runs one speed test through the target's interface: idle latency, then
// the download, then the upload.
func (c *speedCollector) test(ctx context.Context, t speedTarget) (res speedResult) {
	start := time.Now()
	res.At = start
	defer func() { res.Took = time.Since(start) }()

	base, err := url.Parse(c.cfg.URL)
	if err != nil || base.Hostname() == "" {
		res.Err = fmt.Sprintf("bad speed test URL %q", c.cfg.URL)
		return res
	}
	host, port := base.Hostname(), base.Port()
	if port == "" {
		port = "443"
		if base.Scheme == "http" {
			port = "80"
		}
	}
	// The name is resolved the usual way (not pinned to the phone). All test
	// connections then go to the same address, so the latency probes measure
	// the server that serves the transfers.
	ip := net.ParseIP(host)
	if ip == nil {
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host)
		if err != nil || len(ips) == 0 {
			res.Err = fmt.Sprintf("resolve %s: %v", host, err)
			return res
		}
		ip = ips[0]
	}
	addr := net.JoinHostPort(ip.String(), port)
	dialer := c.pinned(t.Netdev)
	dial := func(ctx context.Context) (net.Conn, error) { return dialer.DialContext(ctx, "tcp4", addr) }

	idle, err := probeLatency(ctx, dial, latencyProbes, 100*time.Millisecond)
	if len(idle) == 0 {
		res.Err = fmt.Sprintf("can't reach %s through %s: %v", host, t.Netdev, err)
		return res
	}
	res.Latency, res.Jitter = median(idle), jitter(idle)

	// HTTP/1.1 only, so each request gets its own TCP connection.
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	tr := &http.Transport{
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		TLSClientConfig:     c.tlsConfig.Clone(),
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConnsPerHost: speedtestConns,
		DisableCompression:  true,
		Protocols:           &protocols,
	}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}

	var errs []string
	if c.cfg.DownloadBytes > 0 {
		x := c.transfer(ctx, client, base, dial, false, c.cfg.DownloadBytes)
		res.DownloadMbps, res.DownBytes, res.DownLatency, res.Colo = x.mbps, x.bytes, x.latency, x.colo
		if x.err != nil {
			errs = append(errs, "download: "+x.err.Error())
		}
	}
	if c.cfg.UploadBytes > 0 {
		x := c.transfer(ctx, client, base, dial, true, c.cfg.UploadBytes)
		res.UploadMbps, res.UpBytes, res.UpLatency = x.mbps, x.bytes, x.latency
		if res.Colo == "" {
			res.Colo = x.colo
		}
		if x.err != nil {
			errs = append(errs, "upload: "+x.err.Error())
		}
	}
	res.OK = (c.cfg.DownloadBytes == 0 || res.DownloadMbps > 0) && (c.cfg.UploadBytes == 0 || res.UploadMbps > 0)
	res.Err = strings.Join(errs, "; ")
	return res
}

// timeHandshake times a TCP handshake with the server: one round trip, with no
// server processing in it.
func timeHandshake(ctx context.Context, dial dialFunc) (time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	start := time.Now()
	conn, err := dial(ctx)
	if err != nil {
		return 0, err
	}
	d := time.Since(start)
	conn.Close()
	return d, nil
}

func probeLatency(ctx context.Context, dial dialFunc, n int, gap time.Duration) ([]time.Duration, error) {
	var out []time.Duration
	var lastErr error
	for i := 0; i < n; i++ {
		if i > 0 && !sleepCtx(ctx, gap) {
			break
		}
		d, err := timeHandshake(ctx, dial)
		if err != nil {
			lastErr = err
			continue
		}
		out = append(out, d)
	}
	return out, lastErr
}

func median(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// jitter is the mean change between consecutive latency samples.
func jitter(ds []time.Duration) time.Duration {
	if len(ds) < 2 {
		return 0
	}
	var sum time.Duration
	for i := 1; i < len(ds); i++ {
		d := ds[i] - ds[i-1]
		if d < 0 {
			d = -d
		}
		sum += d
	}
	return sum / time.Duration(len(ds)-1)
}

type transferResult struct {
	mbps    float64
	bytes   int64
	latency time.Duration // median TCP handshake during the transfer (0: none)
	colo    string
	err     error
}

// transfer moves up to budget payload bytes in one direction over
// speedtestConns connections, stopping at the time cap, and times TCP
// handshakes alongside to see how much the load delays everything else.
func (c *speedCollector) transfer(ctx context.Context, client *http.Client, base *url.URL, dial dialFunc, upload bool, budget int64) transferResult {
	ctx, cancel := context.WithTimeout(ctx, c.cfg.MaxDuration)
	defer cancel()

	var probes []time.Duration
	probed := make(chan struct{})
	go func() {
		defer close(probed)
		ticker := time.NewTicker(loadedProbeEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if d, err := timeHandshake(ctx, dial); err == nil {
					probes = append(probes, d)
				}
			}
		}
	}()

	m := newMeter()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		colo     string
		firstErr error
	)
	per := budget / speedtestConns
	for i := 0; i < speedtestConns; i++ {
		n := per
		if i == speedtestConns-1 {
			n = budget - per*(speedtestConns-1)
		}
		if n <= 0 {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cf, err := request(ctx, client, base, upload, n, m)
			mu.Lock()
			defer mu.Unlock()
			if cf != "" {
				colo = cf
			}
			// Errors after the time cap are just the cap.
			if err != nil && firstErr == nil && ctx.Err() == nil {
				firstErr = err
			}
		}()
	}
	wg.Wait()
	cancel()
	<-probed

	r := transferResult{mbps: steadyMbps(m.snapshot()), bytes: m.total(), colo: colo, err: firstErr}
	if len(probes) > 0 {
		r.latency = median(probes)
	}
	if r.mbps == 0 && r.err == nil {
		r.err = errors.New("no data moved")
	}
	return r
}

// request moves n payload bytes over one connection: a GET of n bytes from
// __down, or a POST of n bytes to __up. It returns the Cloudflare data center
// that answered.
func request(ctx context.Context, client *http.Client, base *url.URL, upload bool, n int64, m *meter) (string, error) {
	var req *http.Request
	var err error
	if upload {
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, base.JoinPath("__up").String(), &payload{n: n, m: m})
		if err != nil {
			return "", err
		}
		req.ContentLength = n
		req.Header.Set("Content-Type", "application/octet-stream")
	} else {
		u := base.JoinPath("__down")
		u.RawQuery = url.Values{"bytes": {strconv.FormatInt(n, 10)}}.Encode()
		if req, err = http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil); err != nil {
			return "", err
		}
	}
	req.Header.Set("User-Agent", speedtestAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	colo := coloFromRay(resp.Header.Get("Cf-Ray"))
	if resp.StatusCode != http.StatusOK {
		return colo, fmt.Errorf("HTTP %s", resp.Status)
	}
	if upload {
		_, err = io.Copy(io.Discard, resp.Body)
		return colo, err
	}
	buf := make([]byte, 64<<10)
	for {
		k, err := resp.Body.Read(buf)
		m.add(k)
		if err == io.EOF {
			return colo, nil
		}
		if err != nil {
			return colo, err
		}
	}
}

// coloFromRay returns the Cloudflare data center code at the end of a CF-RAY
// header ("8f1c2a3b4c5d6e7f-SEA" gives "SEA").
func coloFromRay(ray string) string {
	i := strings.LastIndexByte(ray, '-')
	if i < 0 {
		return ""
	}
	colo := ray[i+1:]
	if len(colo) < 3 || len(colo) > 4 {
		return ""
	}
	for _, r := range colo {
		if r < 'A' || r > 'Z' {
			return ""
		}
	}
	return colo
}

// uploadChunk is what upload bodies are made of: random, so nothing along the
// way can compress it.
var uploadChunk = func() []byte {
	b := make([]byte, 64<<10)
	rand.Read(b)
	return b
}()

// payload is an upload body of n bytes that records its progress in m. Bytes
// count when the connection takes them.
type payload struct {
	n int64
	m *meter
}

func (p *payload) Read(b []byte) (int, error) {
	if p.n <= 0 {
		return 0, io.EOF
	}
	if int64(len(b)) > p.n {
		b = b[:p.n]
	}
	k := copy(b, uploadChunk)
	p.n -= int64(k)
	p.m.add(k)
	return k, nil
}

type meterPoint struct {
	at    time.Duration // since the meter started
	bytes int64         // moved so far
}

// meter records when payload bytes moved.
type meter struct {
	mu     sync.Mutex
	start  time.Time
	n      int64
	points []meterPoint
}

func newMeter() *meter { return &meter{start: time.Now()} }

func (m *meter) add(k int) {
	if k <= 0 {
		return
	}
	m.mu.Lock()
	m.n += int64(k)
	m.points = append(m.points, meterPoint{time.Since(m.start), m.n})
	m.mu.Unlock()
}

func (m *meter) total() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.n
}

func (m *meter) snapshot() []meterPoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.points)
}

// steadyMbps estimates a transfer's steady rate: the rate between the moments
// 20% and 90% of its bytes had moved. That leaves out TCP's ramp-up at the
// start and the tail where connections finish one by one, both of which make a
// short, capped test read low.
func steadyMbps(points []meterPoint) float64 {
	if len(points) < 2 {
		return 0
	}
	first, last := points[0], points[len(points)-1]
	lo := timeAtBytes(points, 0.2*float64(last.bytes))
	hi := timeAtBytes(points, 0.9*float64(last.bytes))
	if hi > lo {
		return 0.7 * float64(last.bytes) * 8 / (hi - lo).Seconds() / 1e6
	}
	// Too few points to tell the phases apart: the average after the first read.
	if last.at > first.at {
		return float64(last.bytes-first.bytes) * 8 / (last.at - first.at).Seconds() / 1e6
	}
	return 0
}

// timeAtBytes returns when the byte count reached b, interpolating between the
// recorded points.
func timeAtBytes(points []meterPoint, b float64) time.Duration {
	prev := points[0]
	for _, p := range points {
		if float64(p.bytes) >= b {
			if p.bytes == prev.bytes {
				return p.at
			}
			frac := (b - float64(prev.bytes)) / float64(p.bytes-prev.bytes)
			return prev.at + time.Duration(frac*float64(p.at-prev.at))
		}
		prev = p
	}
	return points[len(points)-1].at
}

// staleAfter is how long a result is pushed after its test.
func (c *speedCollector) staleAfter() time.Duration {
	return c.cfg.Interval + c.cfg.Interval/2
}

// appendSeries appends the speed test metrics of the phone on an interface: the
// latest results for a while after each test, plus when it ran, whether it
// worked, and the data the tests have used since the monitor started.
func (c *speedCollector) appendSeries(list []promremote.TimeSeries, iface, device string, base []promremote.Label, now time.Time) []promremote.TimeSeries {
	key := resultKey(iface, device)
	c.mu.Lock()
	res, ok := c.results[key]
	used := c.used[key]
	c.mu.Unlock()
	if !ok {
		return list
	}
	phase := func(p string) promremote.Label { return promremote.Label{Name: "phase", Value: p} }
	list = append(list,
		newSeries("tether_iface_speedtest_bytes_total", used[0], now, base, promremote.Label{Name: "direction", Value: "download"}),
		newSeries("tether_iface_speedtest_bytes_total", used[1], now, base, promremote.Label{Name: "direction", Value: "upload"}),
		newSeries("tether_iface_speedtest_last_run_timestamp_seconds", float64(res.At.Unix()), now, base),
		newSeries("tether_iface_speedtest_success", boolValue(res.OK), now, base),
	)
	if now.Sub(res.At) > c.staleAfter() {
		return list
	}
	if res.DownloadMbps > 0 {
		list = append(list, newSeries("tether_iface_speedtest_download_mbps", res.DownloadMbps, now, base))
	}
	if res.UploadMbps > 0 {
		list = append(list, newSeries("tether_iface_speedtest_upload_mbps", res.UploadMbps, now, base))
	}
	if res.Latency > 0 {
		list = append(list,
			newSeries("tether_iface_speedtest_latency_seconds", res.Latency.Seconds(), now, base, phase("idle")),
			newSeries("tether_iface_speedtest_jitter_seconds", res.Jitter.Seconds(), now, base),
		)
	}
	if res.DownLatency > 0 {
		list = append(list, newSeries("tether_iface_speedtest_latency_seconds", res.DownLatency.Seconds(), now, base, phase("download")))
	}
	if res.UpLatency > 0 {
		list = append(list, newSeries("tether_iface_speedtest_latency_seconds", res.UpLatency.Seconds(), now, base, phase("upload")))
	}
	if res.Colo != "" {
		list = append(list, newSeries("tether_iface_speedtest_server_info", 1, now, base, promremote.Label{Name: "colo", Value: res.Colo}))
	}
	return list
}

// runSpeedtestNow runs one speed test through each online tethered interface,
// or through the given network interfaces, prints the results and returns the
// process exit code.
func runSpeedtestNow(cfg speedtestConfig, devs string) int {
	var targets []speedTarget
	if devs != "" {
		for _, d := range strings.Split(devs, ",") {
			if d = strings.TrimSpace(d); d != "" {
				targets = append(targets, speedTarget{Iface: d, Netdev: d, Online: true})
			}
		}
	} else {
		data, err := collectInterfaces()
		if err != nil {
			log.Print(err)
			return 1
		}
		for _, d := range data {
			if d.Status != "online" {
				fmt.Fprintf(os.Stderr, "%s (%s): skipped, mwan3 reports it %s\n", d.Interface, d.Device, d.Status)
				continue
			}
			name, _ := getUSBDevice(d.Device)
			targets = append(targets, speedTarget{Iface: d.Interface, Netdev: d.Device, Device: name, Online: true})
		}
	}
	if len(targets) == 0 {
		fmt.Fprintln(os.Stderr, "no online tethered interfaces to test")
		return 1
	}
	fmt.Fprintf(os.Stderr, "testing %d interface(s) one at a time: up to %.1f MB down and %.1f MB up each, %s per direction, firewall mark %#x\n",
		len(targets), float64(cfg.DownloadBytes)/1e6, float64(cfg.UploadBytes)/1e6, cfg.MaxDuration, cfg.FWMark)
	c := newSpeedCollector(cfg)
	code := 0
	for _, t := range targets {
		res := c.runTest(context.Background(), t)
		fmt.Printf("%s: %s\n", t.label(), res.summary())
		if !res.OK {
			code = 1
		}
	}
	return code
}
