# tether-router-monitor

Runs on an OpenWrt router (with mwan3) and pushes metrics about USB-tethered
phones to a Prometheus remote-write endpoint: interface status, uptime and
traffic, plus battery, thermal and signal stats for phones with USB debugging
enabled.

The helper scripts in `bin/` (`ifdev`, `ifusb`, `mwan3ifstatus`) must be on the
router's `PATH`.

## Configuration

| Variable | Default | |
|---|---|---|
| `PUSH_URL` | required | Prometheus remote-write URL |
| `PUSH_INTERVAL_SECONDS` | required | How often to collect and push |
| `PUSH_USERNAME`, `PUSH_PASSWORD` | | Basic auth for the push |
| `ADB_ENABLED` | `1` | Set to `0` to turn off phone metrics over ADB |
| `ADB_KEY_PATH` | `/etc/tether-router-monitor/adbkey` | ADB private key, created on first run |
| `ADB_INTERVAL_SECONDS` | `60` | How often to query the phones |
| `ADB_TIMEOUT_SECONDS` | `20` | Time limit for one phone's query |
| `ADB_RETETHER` | `1` | Set to `0` to stop switching USB tethering back on for known phones |

## Phone metrics over ADB

For tethered phones with USB debugging on, the monitor speaks ADB over the same
USB cable that carries the tethering link. It does this itself through Linux
usbfs ([go-adb](https://github.com/pgaskin/go-adb)), so the router needs no
`adb` binary or adb server. Phones without USB debugging are skipped. The
monitor only touches phones that back a tethered interface, or have done so
before (see below).

Setup:

1. On each phone, enable Developer options → USB debugging. Tethering keeps
   working alongside it.
2. Start the monitor. Each phone asks "Allow USB debugging?": tick "Always
   allow from this computer" and tap Allow. Until then the log says it is
   waiting and `tether_phone_adb_up` is 0. If the prompt was dismissed,
   restart the monitor or replug the phone to see it again.
3. Add `/etc/tether-router-monitor/` to `/etc/sysupgrade.conf` so the key
   survives firmware upgrades; otherwise every phone has to be approved again.

### Switching tethering back on

Android turns USB tethering off whenever the cable is unplugged, including
when the router reboots, and it stays off after the phone is plugged back in.
The monitor remembers every phone it has seen tethering through the router, by
USB serial, in `tether-phones` next to the ADB key. When one of those phones is
plugged in with tethering off, the monitor switches it back on over ADB.

- It uses `svc usb setFunctions rndis|ncm`, picking the function the phone
  itself reports for USB tethering. Newer Pixels only do NCM.
- If tethering stays off, it retries after 2 minutes, then waits twice as long
  each time, up to 30 minutes.
- Turn it off with `ADB_RETETHER=0`, or forget a phone by deleting its line from
  the file.
- `-adb-dump` never changes anything on the phones.

While a known phone isn't tethering, its metrics are still reported, labelled
with its `device` name only (no `interface`), and with
`tether_phone_usb_tethering` 0.

Notes:

- Android 11+ forgets approvals that go unused for 7 days. The monitor
  connects continuously, but if the router is off for longer, approve again
  (or turn on "Disable adb authorization timeout" in Developer options).
- While the monitor runs it holds the phones' ADB interfaces, so `adb` on the
  router can't use them at the same time.
- Approving the router gives anyone with root on it shell access to the phones.

To see what the monitor reads from the phones, stop the service and run
`tether-router-monitor -adb-dump` (add `-adb-raw` for the raw `dumpsys`
output). It queries every ADB device on the machine, so it also works on a PC
with a phone plugged in (point `ADB_KEY_PATH` at `~/.android/adbkey` to reuse
an approved key, and stop any running adb server first).

### Metrics

Labelled with `device` and `interface`, like the `tether_iface_*` series.

| Metric | |
|---|---|
| `tether_phone_adb_up` | 1 if the last query worked, 0 while awaiting approval or on errors |
| `tether_phone_usb_tethering` | 1 if the phone's USB tethering is on (it has a network interface on the router) |
| `tether_phone_info{model,android}` | Always 1 |
| `tether_phone_uptime_seconds` | |
| `tether_phone_battery_level_percent` | |
| `tether_phone_battery_temperature_celsius` | |
| `tether_phone_battery_voltage_volts` | |
| `tether_phone_battery_status` | 1 unknown, 2 charging, 3 discharging, 4 not charging, 5 full |
| `tether_phone_battery_health` | 1 unknown, 2 good, 3 overheat, 4 dead, 5 over voltage, 6 failure, 7 cold |
| `tether_phone_battery_plugged` | 1 when on external power |
| `tether_phone_battery_max_charging_current_amps` | What the USB port or charger offers |
| `tether_phone_battery_max_charging_voltage_volts` | |
| `tether_phone_battery_charge_counter_mah` | Remaining charge; its value at 100% tracks capacity fade |
| `tether_phone_battery_cycle_count` | Only on phones that report it (e.g. Motorola) |
| `tether_phone_battery_state_of_health_percent` | Full-charge vs design capacity; only on phones that report both (e.g. Motorola) |
| `tether_phone_battery_current_amps` | Current into (+) or out of (−) the battery right now; only on phones that report it (e.g. Motorola) |
| `tether_phone_thermal_status` | 0 none, 1 light, 2 moderate, 3 severe, 4 critical, 5 emergency, 6 shutdown |
| `tether_phone_thermal_sensor_celsius{sensor,type}` | Temperatures reported by the thermal HAL |
| `tether_phone_signal_rsrp_dbm{slot,rat}` | `rat` is `lte` or `nr` |
| `tether_phone_signal_rsrq_db{slot,rat}` | |
| `tether_phone_signal_snr_db{slot,rat}` | LTE RSSNR, NR SS-SINR |
| `tether_phone_signal_level{slot,rat}` | Signal bars, 0–4 |
| `tether_phone_network_info{slot,network,override}` | What the status bar shows, e.g. `LTE` + `NR_NSA` for 5G NSA; only while in service |
| `tether_phone_network_5g{slot}` | 1 when the phone shows 5G; only while in service |
| `tether_phone_mobile_data_in_service{slot}` | 1 when registered for data service |
| `tether_phone_mobile_data_connected{slot}` | 1 when the mobile data connection is up |

Empty SIM slots are skipped. Fields a phone doesn't report are omitted.

## Build

```sh
./build_x64.sh
```

Needs Go 1.27+. The build script disables cgo so the binary is static and runs
on OpenWrt's musl libc.
