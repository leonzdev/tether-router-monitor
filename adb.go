package main

// Phone metrics over ADB. For tethered phones with USB debugging enabled, the
// router talks ADB over the same USB cable that carries the tethering link,
// directly through Linux usbfs (github.com/pgaskin/go-adb): no adb binary or
// adb server is needed, so the monitor stays a single static binary.

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/m3db/prometheus_remote_client_golang/promremote"
	"github.com/pgaskin/go-adb/adb/adbhost"
	"github.com/pgaskin/go-adb/adb/adbproto/atransport"
	"github.com/pgaskin/go-adb/adb/adbusb"
	"github.com/pgaskin/go-adb/adblib/adbexec"
)

const defaultADBKeyPath = "/etc/tether-router-monitor/adbkey"

type adbConfig struct {
	Enabled  bool
	KeyPath  string
	Interval time.Duration // how often to query the phones
	Timeout  time.Duration // per-phone limit for one query
}

func loadADBConfig() adbConfig {
	cfg := adbConfig{
		Enabled:  true,
		KeyPath:  defaultADBKeyPath,
		Interval: 60 * time.Second,
		Timeout:  20 * time.Second,
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("ADB_ENABLED"))) {
	case "0", "false", "no", "off":
		cfg.Enabled = false
	}
	if v := os.Getenv("ADB_KEY_PATH"); v != "" {
		cfg.KeyPath = v
	}
	if n, err := strconv.Atoi(os.Getenv("ADB_INTERVAL_SECONDS")); err == nil && n > 0 {
		cfg.Interval = time.Duration(n) * time.Second
	}
	if n, err := strconv.Atoi(os.Getenv("ADB_TIMEOUT_SECONDS")); err == nil && n > 0 {
		cfg.Timeout = time.Duration(n) * time.Second
	}
	return cfg
}

// loadOrCreateADBKey loads the ADB private key at path, generating a new
// RSA-2048 key in adb's own format (PKCS#8 PEM, like ~/.android/adbkey) if it
// doesn't exist yet.
func loadOrCreateADBKey(path string) (crypto.Signer, error) {
	key, err := adbhost.LoadKey(path)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	log.Printf("ADB: generated a new key at %s; approve the USB debugging prompt on each phone", path)
	return priv, nil
}

// phoneSnapshot is the latest state of one ADB-capable phone.
type phoneSnapshot struct {
	Up           bool      // the last query succeeded
	Unauthorized bool      // waiting for the USB debugging prompt to be approved
	Samples      []sample  // from the last successful query
	SampledAt    time.Time // when Samples was collected
	Raw          string    // raw command output (only kept for -adb-dump)
	Serial       string
}

type phoneConn struct {
	dev *adbusb.DeviceInfo
	t   *atransport.Transport
}

type adbCollector struct {
	cfg     adbConfig
	tconf   *atransport.Config
	keepRaw bool
	wake    chan struct{}

	conns map[string]*phoneConn // keyed by USB device id; only used by the polling goroutine

	mu      sync.Mutex
	targets map[string]bool // USB device ids of tethered phones; nil means every ADB device (-adb-dump)
	snaps   map[string]phoneSnapshot
	lastLog map[string]string // last message logged per device, to avoid repeating it every poll
}

func newADBCollector(cfg adbConfig) (*adbCollector, error) {
	if !adbusb.Supported() {
		return nil, errors.New("USB ADB is not supported on this platform")
	}
	key, err := loadOrCreateADBKey(cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("ADB key %s: %w", cfg.KeyPath, err)
	}
	host, _ := os.Hostname()
	return &adbCollector{
		cfg: cfg,
		tconf: &atransport.Config{
			Keys:          []crypto.Signer{key},
			PublicKeyName: "tether-router-monitor@" + host,
		},
		wake:    make(chan struct{}, 1),
		conns:   make(map[string]*phoneConn),
		targets: make(map[string]bool), // nothing until the main loop reports the tethered phones
		snaps:   make(map[string]phoneSnapshot),
		lastLog: make(map[string]string),
	}, nil
}

// setTargets sets the USB device ids (see usbDeviceID) of the tethered phones.
// Only those are queried, so other ADB devices on the router (modems, phones
// that are just charging) are left alone. A change triggers an immediate poll.
func (c *adbCollector) setTargets(ids map[string]bool) {
	c.mu.Lock()
	changed := !maps.Equal(c.targets, ids)
	c.targets = ids
	c.mu.Unlock()
	if changed {
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
}

func (c *adbCollector) run(ctx context.Context) {
	ticker := time.NewTicker(c.cfg.Interval)
	defer ticker.Stop()
	defer c.closeAll()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-c.wake:
		}
		c.poll(ctx)
	}
}

func (c *adbCollector) closeAll() {
	for id, pc := range c.conns {
		pc.t.Close()
		delete(c.conns, id)
	}
}

// poll queries every targeted ADB device in parallel and waits for all of them
// (each is bounded by the configured timeout).
func (c *adbCollector) poll(ctx context.Context) {
	devs, err := adbusb.Devices()
	if err != nil {
		c.logf("", "ADB: list USB devices: %v", err)
		return
	}
	c.mu.Lock()
	targets := c.targets
	c.mu.Unlock()

	present := make(map[string]bool)
	var wg sync.WaitGroup
	for _, d := range devs {
		if d.ID == "" || present[d.ID] || (targets != nil && !targets[d.ID]) {
			continue
		}
		present[d.ID] = true
		pc, err := c.conn(d)
		if err != nil {
			c.markDown(d, false)
			c.logf(d.ID, "ADB %s: %v", describe(d), err)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.collect(ctx, pc)
		}()
	}
	wg.Wait()

	for id, pc := range c.conns {
		if !present[id] {
			pc.t.Close()
			delete(c.conns, id)
		}
	}
	c.mu.Lock()
	for id := range c.snaps {
		if !present[id] {
			delete(c.snaps, id)
			delete(c.lastLog, id)
		}
	}
	c.mu.Unlock()
}

// conn returns the open transport for d, reconnecting if the previous one died
// or the device re-enumerated (e.g. the phone rebooted or tethering was toggled).
func (c *adbCollector) conn(d *adbusb.DeviceInfo) (*phoneConn, error) {
	if pc := c.conns[d.ID]; pc != nil {
		select {
		case <-pc.t.Kicked():
		default:
			if pc.dev.Path == d.Path {
				return pc, nil
			}
		}
		pc.t.Close()
		delete(c.conns, d.ID)
	}
	conn, err := adbusb.Open(d)
	if err != nil {
		return nil, fmt.Errorf("open (is an adb server running on this machine?): %w", err)
	}
	t, err := atransport.Connect(conn, c.tconf)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("connect: %w", err)
	}
	pc := &phoneConn{dev: d, t: t}
	c.conns[d.ID] = pc
	return pc, nil
}

var errUnauthorized = errors.New("not authorized yet")

// waitConnected waits for the ADB handshake to finish. It returns
// errUnauthorized as soon as the phone has been asked to approve our key; the
// transport is kept open so the prompt stays up until someone answers it.
func waitConnected(ctx context.Context, t *atransport.Transport) error {
	select {
	case <-t.Connected():
		return nil
	default:
	}
	select {
	case <-t.Connected():
		return nil
	case <-t.Unauthorized():
		select {
		case <-t.Connected():
			return nil
		default:
			return errUnauthorized
		}
	case <-t.Kicked():
		if err := t.Error(); err != nil {
			return fmt.Errorf("connection closed: %w", err)
		}
		return errors.New("connection closed")
	case <-ctx.Done():
		return fmt.Errorf("handshake: %w", ctx.Err())
	}
}

func (c *adbCollector) collect(ctx context.Context, pc *phoneConn) {
	d := pc.dev
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()

	if err := waitConnected(ctx, pc.t); err != nil {
		c.markDown(d, errors.Is(err, errUnauthorized))
		if errors.Is(err, errUnauthorized) {
			c.logf(d.ID, "ADB %s: waiting for the \"Allow USB debugging?\" prompt to be approved on the phone (tick \"Always allow\")", describe(d))
			return
		}
		c.logf(d.ID, "ADB %s: %v", describe(d), err)
		pc.t.Close() // reconnect on the next poll
		return
	}

	out, err := adbexec.Output(ctx, pc.t, phoneStatsCommand, nil)
	if err != nil {
		c.markDown(d, false)
		c.logf(d.ID, "ADB %s: query failed: %v", describe(d), err)
		pc.t.Close()
		return
	}
	samples := parsePhoneStats(string(out))

	c.mu.Lock()
	snap := phoneSnapshot{Up: true, Samples: samples, SampledAt: time.Now(), Serial: d.Serial}
	if c.keepRaw {
		snap.Raw = string(out)
	}
	c.snaps[d.ID] = snap
	recovered := c.lastLog[d.ID] != ""
	delete(c.lastLog, d.ID)
	c.mu.Unlock()
	if recovered {
		log.Printf("ADB %s: connected, collecting phone metrics", describe(d))
	}
}

func (c *adbCollector) markDown(d *adbusb.DeviceInfo, unauthorized bool) {
	c.mu.Lock()
	snap := c.snaps[d.ID]
	snap.Up = false
	snap.Unauthorized = unauthorized
	snap.Serial = d.Serial
	c.snaps[d.ID] = snap
	c.mu.Unlock()
}

// logf logs a per-device message, skipping it if it repeats the previous one.
func (c *adbCollector) logf(id, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	c.mu.Lock()
	repeat := c.lastLog[id] == msg
	c.lastLog[id] = msg
	c.mu.Unlock()
	if !repeat {
		log.Print(msg)
	}
}

func describe(d *adbusb.DeviceInfo) string {
	if d.Serial == "" {
		return "usb " + d.ID
	}
	return fmt.Sprintf("usb %s (serial %s)", d.ID, d.Serial)
}

// staleAfter is how long samples are pushed after the last successful query.
func (c *adbCollector) staleAfter() time.Duration {
	return 3*c.cfg.Interval + c.cfg.Timeout
}

// appendSeries appends the phone metrics for the ADB device with the given USB
// device id. Phones without USB debugging have no snapshot and get nothing.
func (c *adbCollector) appendSeries(list []promremote.TimeSeries, usbID string, base []promremote.Label, now time.Time) []promremote.TimeSeries {
	c.mu.Lock()
	snap, ok := c.snaps[usbID]
	c.mu.Unlock()
	if !ok {
		return list
	}
	up := 0.0
	if snap.Up {
		up = 1
	}
	list = append(list, newSeries("tether_phone_adb_up", up, now, base))
	if snap.SampledAt.IsZero() || now.Sub(snap.SampledAt) > c.staleAfter() {
		return list
	}
	for _, s := range snap.Samples {
		list = append(list, newSeries(s.Name, s.Value, now, base, s.Labels...))
	}
	return list
}

// newSeries builds a time series with its labels sorted by name, as the
// remote-write protocol requires.
func newSeries(name string, value float64, ts time.Time, base []promremote.Label, extra ...promremote.Label) promremote.TimeSeries {
	labels := make([]promremote.Label, 0, 1+len(base)+len(extra))
	labels = append(labels, promremote.Label{Name: "__name__", Value: name})
	labels = append(labels, base...)
	labels = append(labels, extra...)
	sort.Slice(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })
	return promremote.TimeSeries{
		Labels:    labels,
		Datapoint: promremote.Datapoint{Timestamp: ts, Value: value},
	}
}

// usbDeviceID returns the sysfs name (e.g. "1-2" or "1-1.3") of the USB device
// behind a network interface, the same device the ifusb helper resolves. It
// matches adbusb.DeviceInfo.ID for the phone's ADB interface.
func usbDeviceID(sysRoot, netdev string) (string, error) {
	p, err := filepath.EvalSymlinks(filepath.Join(sysRoot, "class", "net", netdev, "device"))
	if err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(sysRoot)
	if err != nil {
		return "", err
	}
	for ; p != root && strings.HasPrefix(p, root); p = filepath.Dir(p) {
		if fileExists(filepath.Join(p, "busnum")) && fileExists(filepath.Join(p, "devnum")) {
			return filepath.Base(p), nil
		}
	}
	return "", fmt.Errorf("%s is not a USB network device", netdev)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// runADBDump connects to every USB ADB device on this machine, prints what the
// monitor would push for it, and returns the process exit code. It waits a
// while for phones that still need their USB debugging prompt approved.
func runADBDump(cfg adbConfig, raw bool) int {
	c, err := newADBCollector(cfg)
	if err != nil {
		log.Print(err)
		return 1
	}
	c.keepRaw = raw
	c.targets = nil // every ADB device
	defer c.closeAll()

	ctx := context.Background()
	deadline := time.Now().Add(90 * time.Second)
	for announced := false; ; {
		c.poll(ctx)
		c.mu.Lock()
		total, pending := len(c.snaps), 0
		for _, s := range c.snaps {
			if s.Unauthorized {
				pending++
			}
		}
		c.mu.Unlock()
		if total == 0 {
			fmt.Fprintln(os.Stderr, "no USB devices with an ADB interface found (is USB debugging on?)")
			return 1
		}
		if pending == 0 || time.Now().After(deadline) {
			break
		}
		if !announced {
			fmt.Fprintf(os.Stderr, "approve the \"Allow USB debugging?\" prompt on %d phone(s); waiting up to %s\n", pending, time.Until(deadline).Round(time.Second))
			announced = true
		}
		time.Sleep(2 * time.Second)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	code := 0
	for _, id := range slices.Sorted(maps.Keys(c.snaps)) {
		snap := c.snaps[id]
		fmt.Printf("# usb %s serial=%q up=%t\n", id, snap.Serial, snap.Up)
		if !snap.Up {
			code = 1
		}
		if raw && snap.Raw != "" {
			fmt.Println("## raw output")
			fmt.Println(strings.TrimRight(snap.Raw, "\n"))
			fmt.Println("## parsed")
		}
		for _, s := range snap.Samples {
			fmt.Println(s)
		}
	}
	return code
}
