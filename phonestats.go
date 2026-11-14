package main

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/m3db/prometheus_remote_client_golang/promremote"
)

// phoneStatsCommand runs on the phone (as the adb shell user, no root needed)
// and prints every section we parse, each introduced by an "@@<name>" line, so
// one ADB round trip per poll is enough.
//
// mServiceState also carries the serving cell's identity (cell ID, TAC, PCI,
// MCC/MNC), which can locate the phone; sed cuts it down on the phone to the
// data registration state, the only part we use.
const phoneStatsCommand = `echo @@battery; dumpsys battery; ` +
	`echo @@thermal; dumpsys thermalservice; ` +
	`echo @@telephony; dumpsys telephony.registry | grep -E 'Phone Id=|mServiceState=|mSignalStrength=|mTelephonyDisplayInfo=|mDataConnectionState=' | ` +
	`sed 's/mServiceState=.*\(mDataRegState=[-0-9]*\).*/mServiceState={\1}/'; ` +
	`echo @@tethering; dumpsys tethering | grep -E 'mUsbTetheringFunction'; ` +
	`echo @@uptime; cat /proc/uptime; ` +
	`echo @@props; echo "model=$(getprop ro.product.model)"; echo "android=$(getprop ro.build.version.release)"`

// sample is one metric value parsed from a phone. Labels holds labels beyond
// the per-phone ones (device, interface) that are added when pushing.
type sample struct {
	Name   string
	Labels []promremote.Label
	Value  float64
}

func (s sample) String() string {
	return formatSeries(s.Name, s.Labels) + " " + strconv.FormatFloat(s.Value, 'g', -1, 64)
}

// formatSeries renders name{label="value",...} with labels sorted by name.
func formatSeries(name string, labels []promremote.Label) string {
	if len(labels) == 0 {
		return name
	}
	sorted := append([]promremote.Label(nil), labels...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	parts := make([]string, len(sorted))
	for i, l := range sorted {
		parts[i] = fmt.Sprintf("%s=%q", l.Name, l.Value)
	}
	return name + "{" + strings.Join(parts, ",") + "}"
}

func label(name, value string) promremote.Label {
	return promremote.Label{Name: name, Value: value}
}

// parsePhoneStats parses the output of phoneStatsCommand. Missing or
// unrecognised sections are skipped, so older or unusual phones simply report
// fewer metrics.
func parsePhoneStats(out string) []sample {
	sections := splitSections(out)
	var samples []sample
	samples = append(samples, parseBattery(sections["battery"])...)
	samples = append(samples, parseThermal(sections["thermal"])...)
	samples = append(samples, parseTelephony(sections["telephony"])...)
	samples = append(samples, parseUptime(sections["uptime"])...)
	samples = append(samples, parseProps(sections["props"])...)

	// A series repeated within one remote-write request can get the request
	// rejected, so keep only the first of any duplicates.
	seen := make(map[string]bool, len(samples))
	unique := samples[:0]
	for _, s := range samples {
		if key := formatSeries(s.Name, s.Labels); !seen[key] {
			seen[key] = true
			unique = append(unique, s)
		}
	}
	return unique
}

func splitSections(out string) map[string][]string {
	sections := make(map[string][]string)
	current := ""
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if name, ok := strings.CutPrefix(line, "@@"); ok {
			current = strings.TrimSpace(name)
			continue
		}
		if current != "" {
			sections[current] = append(sections[current], line)
		}
	}
	return sections
}

// parseBattery parses `dumpsys battery`:
//
//	Current Battery Service state:
//	  AC powered: false
//	  USB powered: true
//	  Max charging current: 500000
//	  status: 2
//	  level: 85
//	  temperature: 312
//	  ...
func parseBattery(lines []string) []sample {
	kv := make(map[string]string)
	for _, line := range lines {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	num := func(key string) (float64, bool) {
		v, ok := kv[key]
		if !ok {
			return 0, false
		}
		f, err := strconv.ParseFloat(v, 64)
		return f, err == nil
	}

	var out []sample
	add := func(name string, value float64) {
		out = append(out, sample{Name: name, Value: value})
	}
	if level, ok := num("level"); ok {
		scale, ok := num("scale")
		if !ok || scale <= 0 {
			scale = 100
		}
		add("tether_phone_battery_level_percent", level*100/scale)
	}
	if t, ok := num("temperature"); ok {
		add("tether_phone_battery_temperature_celsius", t/10) // tenths of a degree
	}
	if v, ok := num("voltage"); ok && v > 0 {
		if v > 100000 {
			v /= 1e6 // a few HALs report microvolts
		} else {
			v /= 1000 // millivolts
		}
		add("tether_phone_battery_voltage_volts", v)
	}
	if v, ok := num("status"); ok {
		add("tether_phone_battery_status", v)
	}
	if v, ok := num("health"); ok {
		add("tether_phone_battery_health", v)
	}
	plugged, sawPower := 0.0, false
	for _, k := range []string{"AC powered", "USB powered", "Wireless powered", "Dock powered"} {
		if v, ok := kv[k]; ok {
			sawPower = true
			if v == "true" {
				plugged = 1
			}
		}
	}
	if sawPower {
		add("tether_phone_battery_plugged", plugged)
	}
	if v, ok := num("Max charging current"); ok {
		add("tether_phone_battery_max_charging_current_amps", v/1e6) // microamps
	}
	if v, ok := num("Max charging voltage"); ok {
		add("tether_phone_battery_max_charging_voltage_volts", v/1e6) // microvolts
	}
	if v, ok := num("Charge counter"); ok && v > 0 {
		add("tether_phone_battery_charge_counter_mah", v/1000) // microamp-hours
	}

	// Extra fields some OEMs print (seen on Motorola, Android 15).
	if v, ok := num("cycle count"); ok && v >= 0 {
		add("tether_phone_battery_cycle_count", v)
	}
	full, okFull := num("Full capacity")            // learned full-charge capacity, µAh
	design, okDesign := num("Full design capacity") // µAh
	if okFull && okDesign && full > 0 && design > 0 {
		add("tether_phone_battery_state_of_health_percent", full*100/design)
	}
	if v, ok := num("current now property"); ok && math.Abs(v) < 20e6 { // µA; out-of-range = unsupported
		add("tether_phone_battery_current_amps", v/1e6)
	}
	return out
}

var (
	thermalStatusRe = regexp.MustCompile(`^Thermal Status:\s*(-?\d+)`)
	temperatureRe   = regexp.MustCompile(`Temperature\{mValue=([^,]+), mType=(-?\d+), mName=([^,]*), mStatus=(-?\d+)\}`)
)

// Temperature.TYPE_* from android.os.Temperature. The BCL types are battery
// voltage/current/percentage limits, not temperatures, so they are skipped.
var thermalTypeNames = map[int]string{
	-1: "unknown", 0: "cpu", 1: "gpu", 2: "battery", 3: "skin", 4: "usb_port",
	5: "power_amplifier", 9: "npu", 10: "tpu", 11: "display", 12: "modem",
	13: "soc", 14: "wifi", 15: "camera", 16: "flashlight", 17: "speaker",
	18: "ambient", 19: "pogo",
}

var thermalTypesSkipped = map[int]bool{6: true, 7: true, 8: true}

// parseThermal parses `dumpsys thermalservice` (Android 10+): the overall
// "Thermal Status: N" line and the sensors listed under "Current temperatures
// from HAL:" (falling back to "Cached temperatures:"), one per line:
//
//	Temperature{mValue=31.8, mType=3, mName=skin, mStatus=0}
func parseThermal(lines []string) []sample {
	type sensorKey struct{ name, typ string }
	type sensorList struct {
		samples []sample
		seen    map[sensorKey]bool // duplicate series would be rejected by the remote-write receiver
	}
	current := &sensorList{seen: make(map[sensorKey]bool)}
	cached := &sensorList{seen: make(map[sensorKey]bool)}
	var out []sample
	section := ""

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if m := thermalStatusRe.FindStringSubmatch(trimmed); m != nil {
			v, _ := strconv.Atoi(m[1])
			out = append(out, sample{Name: "tether_phone_thermal_status", Value: float64(v)})
			section = ""
			continue
		}
		if line[0] != ' ' && line[0] != '\t' { // unindented line: a new section header
			section = ""
			if strings.HasSuffix(trimmed, ":") {
				section = trimmed
			}
			continue
		}

		var dst *sensorList
		switch section {
		case "Current temperatures from HAL:":
			dst = current
		case "Cached temperatures:":
			dst = cached
		default:
			continue
		}
		m := temperatureRe.FindStringSubmatch(trimmed)
		if m == nil {
			continue
		}
		value, err := strconv.ParseFloat(m[1], 64)
		if err != nil || math.IsNaN(value) || value < -100 || value > 300 {
			continue
		}
		typ, _ := strconv.Atoi(m[2])
		if thermalTypesSkipped[typ] {
			continue
		}
		typeName, ok := thermalTypeNames[typ]
		if !ok {
			typeName = "type_" + m[2]
		}
		key := sensorKey{m[3], typeName}
		if dst.seen[key] {
			continue
		}
		dst.seen[key] = true
		dst.samples = append(dst.samples, sample{
			Name:   "tether_phone_thermal_sensor_celsius",
			Labels: []promremote.Label{label("sensor", m[3]), label("type", typeName)},
			Value:  value,
		})
	}
	if len(current.samples) > 0 {
		return append(out, current.samples...)
	}
	return append(out, cached.samples...)
}

const unavailable = math.MaxInt32 // CellInfo.UNAVAILABLE

var (
	signalFieldRe     = regexp.MustCompile(`(\w+)\s*=\s*(-?\d+)`)
	dataRegStateRe    = regexp.MustCompile(`\bmDataRegState=(-?\d+)`)
	displayNetworkRe  = regexp.MustCompile(`\bnetwork=([A-Za-z0-9_]+)`)
	displayOverrideRe = regexp.MustCompile(`\boverride(?:Network)?=([A-Za-z0-9_]+)`) // "override=" before Android 14
)

type radioSlot struct {
	signal    []sample
	signalOK  bool   // has a valid signal reading
	network   string // TelephonyDisplayInfo network type
	override  string // TelephonyDisplayInfo override (e.g. NR_NSA)
	dataState *int   // TelephonyManager.DATA_*: -1 unknown, 0 disconnected, 2 connected, ...
	dataReg   *int   // ServiceState data registration: 0 in service, 1 out of service, ...
}

// parseTelephony parses the per-SIM-slot lines of `dumpsys telephony.registry`
// (pre-filtered on the phone by grep):
//
//	Phone Id=0
//	  mServiceState={mVoiceRegState=0(IN_SERVICE), mDataRegState=0(IN_SERVICE), ...}
//	  mSignalStrength=SignalStrength:{...,mLte=CellSignalStrengthLte: rssi=-61 rsrp=-89 rsrq=-10 rssnr=10 ... level=4 ...,mNr=CellSignalStrengthNr:{ ... ssRsrp = -98 ssRsrq = -11 ssSinr = 8 level = 3 ... },primary=...}
//	  mDataConnectionState=2
//	  mTelephonyDisplayInfo=TelephonyDisplayInfo {network=LTE, overrideNetwork=NR_NSA, isRoaming=false}
//
// A slot is reported only if it has a valid signal reading, a known data
// connection state, or data service. That skips empty SIM slots, whose
// TelephonyDisplayInfo still shows a default network type (seen on a SIM-less
// Android 13 phone as "network=LTE"). For the same reason the network type is
// only reported while the slot is in service.
func parseTelephony(lines []string) []sample {
	slots := make(map[string]*radioSlot)
	var order []string
	slotName := "0"
	slot := func() *radioSlot {
		s, ok := slots[slotName]
		if !ok {
			s = &radioSlot{}
			slots[slotName] = s
			order = append(order, slotName)
		}
		return s
	}
	atoi := func(s string) *int {
		v, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			return nil
		}
		return &v
	}

	for _, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "Phone Id="):
			slotName = strings.TrimSpace(strings.TrimPrefix(line, "Phone Id="))
		case strings.HasPrefix(line, "mServiceState="):
			if m := dataRegStateRe.FindStringSubmatch(line); m != nil {
				slot().dataReg = atoi(m[1])
			}
		case strings.HasPrefix(line, "mSignalStrength="):
			s := slot()
			s.signal = append(s.signal, parseSignalStrength(slotName, line, &s.signalOK)...)
		case strings.HasPrefix(line, "mTelephonyDisplayInfo="):
			s := slot()
			if m := displayNetworkRe.FindStringSubmatch(line); m != nil {
				s.network = m[1]
			}
			if m := displayOverrideRe.FindStringSubmatch(line); m != nil {
				s.override = m[1]
			}
		case strings.HasPrefix(line, "mDataConnectionState="):
			slot().dataState = atoi(strings.TrimPrefix(line, "mDataConnectionState="))
		}
	}

	var out []sample
	for _, name := range order {
		s := slots[name]
		dataKnown := s.dataState != nil && *s.dataState != -1 // DATA_UNKNOWN
		inService := s.dataReg != nil && *s.dataReg == 0      // ServiceState.STATE_IN_SERVICE
		if !s.signalOK && !dataKnown && !inService {
			continue
		}
		slotLabel := []promremote.Label{label("slot", name)}
		out = append(out, s.signal...)
		if s.dataReg != nil {
			out = append(out, sample{Name: "tether_phone_mobile_data_in_service", Labels: slotLabel, Value: boolValue(inService)})
		}
		if dataKnown {
			out = append(out, sample{Name: "tether_phone_mobile_data_connected", Labels: slotLabel, Value: boolValue(*s.dataState == 2)}) // DATA_CONNECTED
		}
		if s.network != "" && s.network != "UNKNOWN" && (s.dataReg == nil || inService) {
			out = append(out,
				sample{
					Name:   "tether_phone_network_info",
					Labels: []promremote.Label{label("slot", name), label("network", s.network), label("override", s.override)},
					Value:  1,
				},
				sample{Name: "tether_phone_network_5g", Labels: slotLabel, Value: boolValue(s.network == "NR" || strings.HasPrefix(s.override, "NR"))},
			)
		}
	}
	return out
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// parseSignalStrength extracts the LTE and NR measurements from a
// SignalStrength dump. Unavailable values (Integer.MAX_VALUE) are dropped, and
// a radio technology is only reported when it has a valid RSRP.
func parseSignalStrength(slot, line string, active *bool) []sample {
	type rat struct {
		name, marker, end string
		rsrp, rsrq, snr   string
	}
	rats := []rat{
		{"lte", "CellSignalStrengthLte:", ",", "rsrp", "rsrq", "rssnr"},
		{"nr", "CellSignalStrengthNr:", "}", "ssRsrp", "ssRsrq", "ssSinr"},
	}
	var out []sample
	for _, r := range rats {
		start := strings.Index(line, r.marker)
		if start < 0 {
			continue
		}
		segment := line[start+len(r.marker):]
		if end := strings.Index(segment, r.end); end >= 0 {
			segment = segment[:end]
		}
		fields := make(map[string]int)
		for _, m := range signalFieldRe.FindAllStringSubmatch(segment, -1) {
			if v, err := strconv.Atoi(m[2]); err == nil && v != unavailable && v != math.MinInt32 {
				fields[m[1]] = v
			}
		}
		rsrp, ok := fields[r.rsrp]
		if !ok || rsrp >= 0 {
			continue
		}
		*active = true
		labels := []promremote.Label{label("slot", slot), label("rat", r.name)}
		out = append(out, sample{Name: "tether_phone_signal_rsrp_dbm", Labels: labels, Value: float64(rsrp)})
		if v, ok := fields[r.rsrq]; ok {
			out = append(out, sample{Name: "tether_phone_signal_rsrq_db", Labels: labels, Value: float64(v)})
		}
		if v, ok := fields[r.snr]; ok {
			out = append(out, sample{Name: "tether_phone_signal_snr_db", Labels: labels, Value: float64(v)})
		}
		if v, ok := fields["level"]; ok {
			out = append(out, sample{Name: "tether_phone_signal_level", Labels: labels, Value: float64(v)})
		}
	}
	return out
}

// parseTetherFunction returns the phone's USB tethering function ("RNDIS" or
// "NCM") from the "mUsbTetheringFunction: RNDIS" line of `dumpsys tethering`
// (Android 13+), or "" if the phone doesn't report it.
func parseTetherFunction(out string) string {
	for _, line := range splitSections(out)["tethering"] {
		if _, v, ok := strings.Cut(line, "mUsbTetheringFunction:"); ok {
			return strings.ToUpper(strings.TrimSpace(v))
		}
	}
	return ""
}

// parseUptime parses /proc/uptime ("12345.67 23456.78").
func parseUptime(lines []string) []sample {
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if v, err := strconv.ParseFloat(fields[0], 64); err == nil {
			return []sample{{Name: "tether_phone_uptime_seconds", Value: v}}
		}
		return nil
	}
	return nil
}

// parseProps parses the "model=..." and "android=..." lines into an info metric.
func parseProps(lines []string) []sample {
	props := make(map[string]string)
	for _, line := range lines {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = strings.TrimSpace(v)
		}
	}
	if props["model"] == "" {
		return nil
	}
	return []sample{{
		Name:   "tether_phone_info",
		Labels: []promremote.Label{label("model", props["model"]), label("android", props["android"])},
		Value:  1,
	}}
}
