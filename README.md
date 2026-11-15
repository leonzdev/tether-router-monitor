# tether-router-monitor

Runs on an OpenWrt router (with mwan3) and pushes metrics about USB-tethered
phones to a Prometheus remote-write endpoint: interface status, uptime and
traffic, plus battery, thermal and signal stats for phones with USB debugging
enabled, and optionally periodic speed tests through each phone.

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
| `SPEEDTEST_ENABLED` | `0` | Set to `1` to run speed tests through each phone (uses mobile data, see [Speed tests](#speed-tests)) |
| `SPEEDTEST_INTERVAL_MINUTES` | `60` | How often each phone is tested |
| `SPEEDTEST_DOWNLOAD_MB`, `SPEEDTEST_UPLOAD_MB` | `10`, `5` | Data cap per test for each direction (MB = 10⁶ bytes); `0` skips that direction |
| `SPEEDTEST_MAX_SECONDS` | `5` | Time cap for each direction |
| `SPEEDTEST_BUSY_MBPS` | `2` | Postpone a test while the phone carries more other traffic than this; `0` never postpones |
| `SPEEDTEST_URL` | `https://speed.cloudflare.com` | Speed test server |
| `SPEEDTEST_FWMARK` | mwan3's mask, normally `0x3f00` | Firewall mark for test traffic; `0` for none |

## Interface metrics

One series per tethered interface, labelled with `device` (the phone's USB
name) and `interface`.

| Metric | |
|---|---|
| `tether_iface_status_online` | 1 if mwan3 reports the interface online |
| `tether_iface_status_enabled` | 1 unless mwan3 reports it disabled |
| `tether_iface_status_tracking` | 1 if mwan3 tracking is active |
| `tether_iface_up_time` | Seconds since the interface came up |
| `tether_iface_online_time` | Seconds since mwan3 last marked it online |
| `tether_iface_rx`, `tether_iface_tx` | Bytes received and sent |
| `tether_iface_usb_speed_mbps{usb_port}` | USB link speed: 480 means the phone runs in USB 2.0 mode (a USB 2.0 phone, or a USB 2.0-only cable), 5000 or 10000 means USB 3. `usb_port` is the kernel's bus-port path (e.g. `2-3`); a port's USB 2.0 and USB 3 sides are on different buses |

An interface whose USB device can't be found (for a moment while a phone is
being unplugged) is skipped, so no series without a `device` label is pushed.

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
| `tether_phone_battery_max_charging_current_amps` | What the USB port offers, as the phone sees it. A standard port gives 0.5 A in USB 2.0 mode and 0.9 A in USB 3 mode; a charging port (BC 1.2) gives up to 1.5 A |
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

## Speed tests

With `SPEEDTEST_ENABLED=1`, the monitor measures download and upload speed and
latency through each tethered phone, hourly by default. It's off by default
because every test uses mobile data.

- **One phone at a time.** Each interface gets its own fixed time within the
  interval, spread evenly over the interfaces mwan3 knows. With 5 interfaces
  and hourly tests, they run at :06, :18, :30, :42 and :54. After a restart,
  each phone waits for its next slot, so restarts don't set off extra tests.
- **Through that phone only.** mwan3 load-balances the router's own traffic
  too, so test connections are pinned the way `mwan3 use` pins a command: bound
  to the phone's interface and given mwan3's default firewall mark, which
  mwan3's rules leave alone. Setting the mark needs root.
- **Skips and waits.** A phone that is unplugged or that mwan3 reports offline
  at its slot is tested once it's back, at most twice per interval. While a
  phone carries more than `SPEEDTEST_BUSY_MBPS` of other traffic, its test
  waits 5 minutes, up to 3 times, then runs anyway.
- **Each test:** 5 TCP handshakes with the server time the idle latency. Then
  it downloads over 4 connections until the data cap or the time cap, and
  uploads the same way. Throughput is the rate between the moments 20% and 90%
  of the bytes had moved, which leaves out TCP's ramp-up and the tail. TCP
  handshakes timed during the transfers show how much a busy link delays
  everything else, which is usually the number that makes a cellular link feel
  slow.

The server is Cloudflare's speed test API (the one speed.cloudflare.com uses).
It's anycast, so each phone reaches the nearest Cloudflare data center through
its own carrier. It isn't an official, documented API; `SPEEDTEST_URL` can
point at any server with the same `/__down?bytes=N` and `/__up` endpoints.

### Data use

A test uses at most its caps, 15 MB by default; slower links use less. Run
hourly, that is up to about 11 GB per phone per month. It counts against the
phone's plan like any tethered traffic, and shows up in `tether_iface_rx` and
`tether_iface_tx`. Small caps read fast links low, because TCP is still
speeding up when the cap is reached:

| Caps per test | Per phone per month, hourly | Reads well up to about |
|---|---|---|
| 10 MB down + 5 MB up (default) | 11 GB | 100 Mb/s |
| 25 MB down + 10 MB up | 25 GB | 200 Mb/s |

A test keeps the phone's link busy for up to `2 × SPEEDTEST_MAX_SECONDS`, which
can delay mwan3's tracking pings. mwan3's defaults (a ping every 10 s, offline
after 5 misses in a row) ride that out easily. Keep the time cap well below
`interval × down` of your mwan3 tracking settings.

To try it, run `tether-router-monitor -speedtest-now`. It tests each online
tethered interface once and prints the results, whether or not
`SPEEDTEST_ENABLED` is set, and it uses the same caps. It needs the helpers
on `PATH`, like the service (for example `PATH=/root/.bin:$PATH`).
`-speedtest-dev usb2` tests just that network interface, without the helpers.

### Metrics

Labelled with `device` and `interface`, like the `tether_iface_*` series. The
results of a test are pushed until 1.5 intervals after it ran.

| Metric | |
|---|---|
| `tether_iface_speedtest_download_mbps` | Download speed |
| `tether_iface_speedtest_upload_mbps` | Upload speed |
| `tether_iface_speedtest_latency_seconds{phase}` | Median TCP handshake time with the server: `idle` before the transfers, `download` and `upload` during them |
| `tether_iface_speedtest_jitter_seconds` | Mean change between consecutive idle samples |
| `tether_iface_speedtest_server_info{colo}` | The Cloudflare data center that answered (e.g. `SEA`); always 1 |
| `tether_iface_speedtest_success` | 1 if the latest test worked |
| `tether_iface_speedtest_last_run_timestamp_seconds` | When the latest test ran (Unix time) |
| `tether_iface_speedtest_bytes_total{direction}` | Data the tests have used since the monitor started |

## Build

```sh
./build_x64.sh
```

Needs Go 1.27+. The build script disables cgo so the binary is static and runs
on OpenWrt's musl libc.
