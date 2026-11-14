package main

import (
	"math"
	"os"
	"strings"
	"testing"
)

const batteryFixture = `Current Battery Service state:
  AC powered: false
  USB powered: true
  Wireless powered: false
  Dock powered: false
  Max charging current: 500000
  Max charging voltage: 5000000
  Charge counter: 3954000
  status: 2
  health: 2
  present: true
  level: 85
  scale: 100
  voltage: 4223
  temperature: 312
  technology: Li-ion
  Charging state: 1
  Charging policy: 1
`

const thermalFixture = "IsStatusOverride: false\n" +
	"ThermalEventListeners:\n" +
	"\tcallbacks: 1\n" +
	"\tkilled: false\n" +
	"ThermalStatusListeners:\n" +
	"\tcallbacks: 2\n" +
	"Thermal Status: 1\n" +
	"Cached temperatures:\n" +
	"\tTemperature{mValue=40.1, mType=3, mName=skin, mStatus=1}\n" +
	"HAL Ready: true\n" +
	"HAL connection:\n" +
	"\tThermalHAL AIDL 1  connected: yes\n" +
	"Current temperatures from HAL:\n" +
	"\tTemperature{mValue=41.5, mType=0, mName=cpu-0-0, mStatus=0}\n" +
	"\tTemperature{mValue=36.2, mType=2, mName=battery, mStatus=0}\n" +
	"\tTemperature{mValue=39.8, mType=3, mName=skin, mStatus=1}\n" +
	"\tTemperature{mValue=3.9, mType=6, mName=vbat, mStatus=0}\n" +
	"\tTemperature{mValue=NaN, mType=12, mName=modem, mStatus=0}\n" +
	"\tTemperature{mValue=36.4, mType=2, mName=battery, mStatus=0}\n" +
	"\tTemperature{mValue=33.0, mType=42, mName=future-sensor, mStatus=0}\n" +
	"Current cooling devices from HAL:\n" +
	"\tCoolingDevice{mValue=0, mType=0, mName=cpu-cluster0}\n" +
	"Temperature static thresholds from HAL:\n" +
	"\tTemperatureThreshold{mType=3, mName=skin, mHotThrottlingThresholds=[39.0, 41.0, 43.0, 45.0, 47.0, 50.0, 55.0], mColdThrottlingThresholds=[NaN, NaN, NaN, NaN, NaN, NaN, NaN]}\n"

const invalidSignal = `SignalStrength:{mCdma=CellSignalStrengthCdma: cdmaDbm=2147483647 cdmaEcio=2147483647 evdoDbm=2147483647 evdoEcio=2147483647 evdoSnr=2147483647 level=0 oplevel=0,mGsm=CellSignalStrengthGsm: rssi=2147483647 ber=2147483647 mTa=2147483647 mLevel=0,mWcdma=CellSignalStrengthWcdma: ss=2147483647 ber=2147483647 rscp=2147483647 ecno=2147483647 level=0,mTdscdma=CellSignalStrengthTdscdma: rssi=2147483647 ber=2147483647 rscp=2147483647 level=0,mLte=CellSignalStrengthLte: rssi=2147483647 rsrp=2147483647 rsrq=2147483647 rssnr=2147483647 cqiTableIndex=2147483647 cqi=2147483647 ta=2147483647 level=0 parametersUseForLevel=0,mNr=CellSignalStrengthNr:{ csiRsrp = 2147483647 csiRsrq = 2147483647 csiCqiTableIndex = 2147483647 csiCqiReport = [] ssRsrp = 2147483647 ssRsrq = 2147483647 ssSinr = 2147483647 level = 0 parametersUseForLevel = 0 timingAdvance = 2147483647 },primary=CellSignalStrengthCdma,mLteAsPrimaryInNrNsa=true}`

const nsaSignal = `SignalStrength:{mCdma=CellSignalStrengthCdma: cdmaDbm=2147483647 cdmaEcio=2147483647 evdoDbm=2147483647 evdoEcio=2147483647 evdoSnr=2147483647 level=0 oplevel=0,mGsm=CellSignalStrengthGsm: rssi=2147483647 ber=2147483647 mTa=2147483647 mLevel=0,mWcdma=CellSignalStrengthWcdma: ss=2147483647 ber=2147483647 rscp=2147483647 ecno=2147483647 level=0,mTdscdma=CellSignalStrengthTdscdma: rssi=2147483647 ber=2147483647 rscp=2147483647 level=0,mLte=CellSignalStrengthLte: rssi=-61 rsrp=-89 rsrq=-10 rssnr=10 cqiTableIndex=2147483647 cqi=2147483647 ta=2147483647 level=4 parametersUseForLevel=0,mNr=CellSignalStrengthNr:{ csiRsrp = 2147483647 csiRsrq = 2147483647 csiCqiTableIndex = 2147483647 csiCqiReport = [] ssRsrp = -98 ssRsrq = -11 ssSinr = 8 level = 3 parametersUseForLevel = 0 timingAdvance = 2147483647 },primary=CellSignalStrengthLte,mLteAsPrimaryInNrNsa=true}`

const (
	inService    = `mServiceState={mVoiceRegState=0(IN_SERVICE), mDataRegState=0(IN_SERVICE), mChannelNumber=66986, duplexMode()=1, mCellBandwidths=[20000], mOperatorAlphaLong=T-Mobile}`
	outOfService = `mServiceState={mVoiceRegState=1(OUT_OF_SERVICE), mDataRegState=1(OUT_OF_SERVICE), mChannelNumber=0, duplexMode()=0, mCellBandwidths=[], mOperatorAlphaLong=null}`
)

// Slot 1 is an empty SIM slot: it still has a default display network type.
const telephonyFixture = `  Phone Id=0
    ` + inService + `
    mSignalStrength=` + nsaSignal + `
    mDataConnectionState=2
    mTelephonyDisplayInfo=TelephonyDisplayInfo {network=LTE, overrideNetwork=NR_NSA, isRoaming=false}
  Phone Id=1
    ` + outOfService + `
    mSignalStrength=` + invalidSignal + `
    mDataConnectionState=-1
    mTelephonyDisplayInfo=TelephonyDisplayInfo {network=LTE, overrideNetwork=NONE, isRoaming=false}
`

func lines(s string) []string { return strings.Split(s, "\n") }

// find returns the value of the sample whose String() starts with series
// (e.g. `tether_phone_signal_rsrp_dbm{rat="lte",slot="0"}`).
func find(t *testing.T, samples []sample, series string) float64 {
	t.Helper()
	for _, s := range samples {
		if formatSeries(s.Name, s.Labels) == series {
			return s.Value
		}
	}
	t.Fatalf("no sample %s in:\n%s", series, dump(samples))
	return 0
}

func dump(samples []sample) string {
	var b strings.Builder
	for _, s := range samples {
		b.WriteString(s.String())
		b.WriteByte('\n')
	}
	return b.String()
}

func approx(t *testing.T, got, want float64, what string) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestParseBattery(t *testing.T) {
	s := parseBattery(lines(batteryFixture))
	for series, want := range map[string]float64{
		"tether_phone_battery_level_percent":              85,
		"tether_phone_battery_temperature_celsius":        31.2,
		"tether_phone_battery_voltage_volts":              4.223,
		"tether_phone_battery_status":                     2,
		"tether_phone_battery_health":                     2,
		"tether_phone_battery_plugged":                    1,
		"tether_phone_battery_max_charging_current_amps":  0.5,
		"tether_phone_battery_max_charging_voltage_volts": 5,
		"tether_phone_battery_charge_counter_mah":         3954,
	} {
		approx(t, find(t, s, series), want, series)
	}
	if len(s) != 9 {
		t.Errorf("got %d samples, want 9:\n%s", len(s), dump(s))
	}
}

func TestParseBatteryScaleAndUnplugged(t *testing.T) {
	s := parseBattery(lines("  AC powered: false\n  USB powered: false\n  level: 40\n  scale: 50\n  voltage: 3850000\n"))
	approx(t, find(t, s, "tether_phone_battery_level_percent"), 80, "level")
	approx(t, find(t, s, "tether_phone_battery_voltage_volts"), 3.85, "microvolt voltage")
	approx(t, find(t, s, "tether_phone_battery_plugged"), 0, "plugged")
}

func TestParseThermal(t *testing.T) {
	s := parseThermal(lines(thermalFixture))
	approx(t, find(t, s, "tether_phone_thermal_status"), 1, "status")
	approx(t, find(t, s, `tether_phone_thermal_sensor_celsius{sensor="cpu-0-0",type="cpu"}`), 41.5, "cpu")
	approx(t, find(t, s, `tether_phone_thermal_sensor_celsius{sensor="battery",type="battery"}`), 36.2, "battery (first wins)")
	approx(t, find(t, s, `tether_phone_thermal_sensor_celsius{sensor="skin",type="skin"}`), 39.8, "skin from HAL, not cache")
	approx(t, find(t, s, `tether_phone_thermal_sensor_celsius{sensor="future-sensor",type="type_42"}`), 33, "unknown type")
	// vbat (BCL voltage, not a temperature), NaN modem, and the duplicate battery are dropped.
	if len(s) != 5 {
		t.Errorf("got %d samples, want 5:\n%s", len(s), dump(s))
	}
}

func TestParseThermalFallsBackToCache(t *testing.T) {
	s := parseThermal(lines("Thermal Status: 0\nCached temperatures:\n\tTemperature{mValue=40.1, mType=3, mName=skin, mStatus=1}\nHAL Ready: false\n"))
	approx(t, find(t, s, `tether_phone_thermal_sensor_celsius{sensor="skin",type="skin"}`), 40.1, "cached skin")
}

func TestParseTelephony(t *testing.T) {
	s := parseTelephony(lines(telephonyFixture))
	for series, want := range map[string]float64{
		`tether_phone_signal_rsrp_dbm{rat="lte",slot="0"}`:                    -89,
		`tether_phone_signal_rsrq_db{rat="lte",slot="0"}`:                     -10,
		`tether_phone_signal_snr_db{rat="lte",slot="0"}`:                      10,
		`tether_phone_signal_level{rat="lte",slot="0"}`:                       4,
		`tether_phone_signal_rsrp_dbm{rat="nr",slot="0"}`:                     -98,
		`tether_phone_signal_rsrq_db{rat="nr",slot="0"}`:                      -11,
		`tether_phone_signal_snr_db{rat="nr",slot="0"}`:                       8,
		`tether_phone_signal_level{rat="nr",slot="0"}`:                        3,
		`tether_phone_network_info{network="LTE",override="NR_NSA",slot="0"}`: 1,
		`tether_phone_network_5g{slot="0"}`:                                   1,
		`tether_phone_mobile_data_connected{slot="0"}`:                        1,
		`tether_phone_mobile_data_in_service{slot="0"}`:                       1,
	} {
		approx(t, find(t, s, series), want, series)
	}
	for _, x := range s {
		for _, l := range x.Labels {
			if l.Name == "slot" && l.Value == "1" {
				t.Errorf("empty SIM slot should be skipped, got %s", x)
			}
		}
	}
	if len(s) != 12 {
		t.Errorf("got %d samples, want 12:\n%s", len(s), dump(s))
	}
}

func TestParseTelephonyNetworkTypes(t *testing.T) {
	for _, tc := range []struct {
		display, network, override string
		fiveG                      float64
	}{
		{"TelephonyDisplayInfo {network=NR, overrideNetwork=NONE, isRoaming=false, isNtn=false}", "NR", "NONE", 1},
		{"TelephonyDisplayInfo {network=LTE, overrideNetwork=NR_ADVANCED, isRoaming=false}", "LTE", "NR_ADVANCED", 1},
		{"TelephonyDisplayInfo {network=LTE, overrideNetwork=LTE_CA, isRoaming=false}", "LTE", "LTE_CA", 0},
		{"TelephonyDisplayInfo {network=LTE, overrideNetwork=NONE, isRoaming=false}", "LTE", "NONE", 0},
		{"TelephonyDisplayInfo {network=LTE, override=NR_NSA}", "LTE", "NR_NSA", 1}, // Android 13 and older
	} {
		s := parseTelephony([]string{"Phone Id=0", inService, "mTelephonyDisplayInfo=" + tc.display})
		approx(t, find(t, s, `tether_phone_network_5g{slot="0"}`), tc.fiveG, tc.display)
		find(t, s, `tether_phone_network_info{network="`+tc.network+`",override="`+tc.override+`",slot="0"}`)
	}
}

// A real SIM-less Android 13 phone (Nokia G310 5G): everything invalid or
// unknown, but the display info still claims LTE. Nothing should be reported.
func TestParseTelephonyNoSIM(t *testing.T) {
	s := parseTelephony([]string{
		"  Phone Id=0",
		"    " + outOfService,
		"    mSignalStrength=" + invalidSignal,
		"    mDataConnectionState=-1",
		"    mTelephonyDisplayInfo=TelephonyDisplayInfo {network=LTE, override=NONE}",
	})
	if len(s) != 0 {
		t.Errorf("expected nothing for a phone without a SIM, got:\n%s", dump(s))
	}
}

// SIM present but no coverage: report the outage, not the stale network type.
func TestParseTelephonyOutOfService(t *testing.T) {
	s := parseTelephony([]string{
		"Phone Id=0",
		outOfService,
		"mSignalStrength=" + invalidSignal,
		"mDataConnectionState=0",
		"mTelephonyDisplayInfo=TelephonyDisplayInfo {network=LTE, overrideNetwork=NONE, isRoaming=false}",
	})
	approx(t, find(t, s, `tether_phone_mobile_data_in_service{slot="0"}`), 0, "in service")
	approx(t, find(t, s, `tether_phone_mobile_data_connected{slot="0"}`), 0, "data connected")
	if len(s) != 2 {
		t.Errorf("want only the two outage samples, got:\n%s", dump(s))
	}
}

func TestParsePhoneStats(t *testing.T) {
	out := "@@battery\n" + batteryFixture +
		"@@thermal\n" + thermalFixture +
		"@@telephony\n" + telephonyFixture +
		"@@uptime\n123456.78 234567.89\n" +
		"@@props\nmodel=motorola edge 2024\nandroid=16\n"
	out = strings.ReplaceAll(out, "\n", "\r\n") // adb shells may emit CRLF
	s := parsePhoneStats(out)
	approx(t, find(t, s, "tether_phone_battery_level_percent"), 85, "battery")
	approx(t, find(t, s, "tether_phone_thermal_status"), 1, "thermal")
	approx(t, find(t, s, `tether_phone_signal_rsrp_dbm{rat="lte",slot="0"}`), -89, "signal")
	approx(t, find(t, s, "tether_phone_uptime_seconds"), 123456.78, "uptime")
	approx(t, find(t, s, `tether_phone_info{android="16",model="motorola edge 2024"}`), 1, "info")
}

func TestParsePhoneStatsDropsDuplicates(t *testing.T) {
	s := parsePhoneStats("@@thermal\nThermal Status: 2\nThermal Status: 3\n@@telephony\nPhone Id=0\nmSignalStrength=" + nsaSignal + "\nPhone Id=0\nmSignalStrength=" + nsaSignal + "\n")
	approx(t, find(t, s, "tether_phone_thermal_status"), 2, "first status wins")
	counts := make(map[string]int)
	for _, x := range s {
		counts[formatSeries(x.Name, x.Labels)]++
	}
	for k, n := range counts {
		if n > 1 {
			t.Errorf("%s appears %d times", k, n)
		}
	}
}

// Real output captured with -adb-dump -adb-raw from a Nokia G310 5G
// (Android 13, no SIM, charging from a laptop USB port).
func TestParsePhoneStatsNokiaG310(t *testing.T) {
	raw, err := os.ReadFile("testdata/nokia-g310-android13.txt")
	if err != nil {
		t.Fatal(err)
	}
	s := parsePhoneStats(string(raw))
	for series, want := range map[string]float64{
		"tether_phone_battery_level_percent":                                   100,
		"tether_phone_battery_status":                                          5, // full
		"tether_phone_battery_temperature_celsius":                             21.2,
		"tether_phone_battery_voltage_volts":                                   4.377,
		"tether_phone_battery_max_charging_current_amps":                       0.5,
		"tether_phone_battery_charge_counter_mah":                              4220,
		"tether_phone_thermal_status":                                          0,
		`tether_phone_thermal_sensor_celsius{sensor="skin",type="skin"}`:       21.919,
		`tether_phone_thermal_sensor_celsius{sensor="battery",type="battery"}`: 21.2,
		"tether_phone_uptime_seconds":                                          852.06,
		`tether_phone_info{android="13",model="Nokia G310 5G"}`:                1,
	} {
		approx(t, find(t, s, series), want, series)
	}
	var sensors, telephony int
	for _, x := range s {
		switch {
		case x.Name == "tether_phone_thermal_sensor_celsius":
			sensors++
		case strings.HasPrefix(x.Name, "tether_phone_signal_"), strings.HasPrefix(x.Name, "tether_phone_network_"), strings.HasPrefix(x.Name, "tether_phone_mobile_"):
			telephony++
		}
	}
	if sensors != 12 { // 15 HAL entries minus the BCL ones (vbat, ibat, socd)
		t.Errorf("got %d thermal sensors, want 12", sensors)
	}
	if telephony != 0 {
		t.Errorf("phone has no SIM, but got %d telephony samples:\n%s", telephony, dump(s))
	}
}

// Real output from a motorola edge (2022) (Android 15, T-Mobile, on 5G SA,
// charging from a laptop USB port). mServiceState is already cut down on the
// phone by phoneStatsCommand's sed.
func TestParsePhoneStatsMotoEdge2022(t *testing.T) {
	raw, err := os.ReadFile("testdata/moto-edge-2022-android15.txt")
	if err != nil {
		t.Fatal(err)
	}
	s := parsePhoneStats(string(raw))
	for series, want := range map[string]float64{
		"tether_phone_battery_level_percent":                                   87,
		"tether_phone_battery_status":                                          2, // charging
		"tether_phone_battery_charge_counter_mah":                              4407.246,
		"tether_phone_battery_cycle_count":                                     83,
		"tether_phone_battery_state_of_health_percent":                         5065800.0 * 100 / 5082000,
		`tether_phone_signal_rsrp_dbm{rat="nr",slot="0"}`:                      -100,
		`tether_phone_signal_rsrq_db{rat="nr",slot="0"}`:                       -11,
		`tether_phone_signal_snr_db{rat="nr",slot="0"}`:                        13,
		`tether_phone_signal_level{rat="nr",slot="0"}`:                         4,
		`tether_phone_mobile_data_in_service{slot="0"}`:                        1,
		`tether_phone_mobile_data_connected{slot="0"}`:                         1,
		`tether_phone_network_info{network="NR",override="NONE",slot="0"}`:     1,
		`tether_phone_network_5g{slot="0"}`:                                    1,
		`tether_phone_info{android="15",model="motorola edge (2022)"}`:         1,
		`tether_phone_thermal_sensor_celsius{sensor="BATTERY",type="battery"}`: 24,
	} {
		approx(t, find(t, s, series), want, series)
	}
	if v := find(t, s, "tether_phone_battery_current_amps"); v <= 0 || v > 1 {
		t.Errorf("charging current %v A, want a small positive value", v)
	}
	find(t, s, `tether_phone_thermal_sensor_celsius{sensor="POWER_AMPLIFIER",type="power_amplifier"}`)
	for _, x := range s {
		for _, l := range x.Labels {
			if l.Name == "rat" && l.Value == "lte" {
				t.Errorf("LTE is unavailable on 5G SA, got %s", x)
			}
		}
	}
}

func TestParsePhoneStatsEmpty(t *testing.T) {
	if s := parsePhoneStats("/system/bin/sh: dumpsys: not found\n"); len(s) != 0 {
		t.Errorf("expected no samples, got:\n%s", dump(s))
	}
}
