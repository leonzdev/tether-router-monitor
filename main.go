package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/m3db/prometheus_remote_client_golang/promremote"
)

type Ifdev struct {
	Interface string `json:"interface"`
	Device    string `json:"device"`
}

type Mwan3ifstatus struct {
	Interface  string `json:"interface"`
	Status     string `json:"status"`
	OnlineTime string `json:"online_time"`
	Uptime     string `json:"uptime"`
	Tracking   string `json:"tracking"`
}

type CombinedData struct {
	Interface  string `json:"interface"`
	Device     string `json:"device"`
	Status     string `json:"status"`
	OnlineTime string `json:"online_time"`
	Uptime     string `json:"uptime"`
	Tracking   string `json:"tracking"`
	RX         int64  `json:"rx"` // Bytes received
	TX         int64  `json:"tx"` // Bytes sent
}

type NetworkTraffic struct {
	Interface string
	RX        int64 // Bytes received
	TX        int64 // Bytes sent
}

var (
	pushIntervalSeconds int
	pushURL             string
	username            string
	password            string

	sysRoot = "/sys" // overridden in tests
)

func init() {
	pushIntervalSeconds, _ = strconv.Atoi(os.Getenv("PUSH_INTERVAL_SECONDS"))
	pushURL = os.Getenv("PUSH_URL")
	username = os.Getenv("PUSH_USERNAME")
	password = os.Getenv("PUSH_PASSWORD")
}

func getBasicAuthHeader(username, password string) string {
	auth := username + ":" + password
	encodedAuth := base64.StdEncoding.EncodeToString([]byte(auth))
	return "Basic " + encodedAuth
}

func executeShellCommand(command string, args ...string) ([]byte, error) {
	cmd := exec.Command(command, args...)
	return cmd.Output()
}

func filterUSBInterfaces(ifdevData []Ifdev) []Ifdev {
	var usbInterfaces []Ifdev
	for _, item := range ifdevData {
		if len(item.Device) > 2 && item.Device[:3] == "usb" {
			usbInterfaces = append(usbInterfaces, item)
		}
	}
	return usbInterfaces
}

func getUSBDevice(interfaceName string) (string, error) {
	ifusbOutput, err := executeShellCommand("ifusb", interfaceName)
	if err != nil {
		return "", fmt.Errorf("Error executing ifusb for %s: %v", interfaceName, err)
	}

	var usbInfo struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(ifusbOutput, &usbInfo); err != nil {
		return "", fmt.Errorf("Error unmarshalling ifusb output: %v", err)
	}

	description := strings.TrimSpace(usbInfo.Description)
	if description == "" {
		// lsusb has no line for the device, usually because the phone is being
		// unplugged while mwan3 still reports the interface. Fall back to the
		// USB device strings in sysfs; if the device is gone there too, fail,
		// so no series without a device label gets pushed (dashboards would
		// show it as a nameless "Value").
		if id, err := usbDeviceID(sysRoot, interfaceName); err == nil {
			description = usbDeviceName(sysRoot, id)
		}
	}
	if description == "" {
		return "", fmt.Errorf("no USB device found for %s (unplugged?)", interfaceName)
	}
	return description, nil
}

func parseUptimeToSeconds(uptime string) float64 {
	// Split the uptime string by colons
	parts := strings.Split(uptime, ":")
	if len(parts) != 3 {
		return 0 // or handle the error appropriately
	}

	// Remove the 'h', 'm', and 's' characters and parse the numbers
	hours, err := strconv.ParseFloat(strings.TrimSuffix(parts[0], "h"), 64)
	if err != nil {
		return 0 // or handle the error appropriately
	}

	minutes, err := strconv.ParseFloat(strings.TrimSuffix(parts[1], "m"), 64)
	if err != nil {
		return 0 // or handle the error appropriately
	}

	seconds, err := strconv.ParseFloat(strings.TrimSuffix(parts[2], "s"), 64)
	if err != nil {
		return 0 // or handle the error appropriately
	}

	return hours*3600 + minutes*60 + seconds
}

func getNetworkTraffic() (map[string]NetworkTraffic, error) {
	cmd := exec.Command("ifconfig") // or use 'ip -s link'
	output, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	return parseNetworkTraffic(string(output)), nil
}

func parseNetworkTraffic(output string) map[string]NetworkTraffic {
	trafficData := make(map[string]NetworkTraffic)
	blocks := strings.Split(output, "\n\n") // Split output into blocks

	rxTxRegex := regexp.MustCompile(`RX bytes:(\d+) .* TX bytes:(\d+)`)
	for _, block := range blocks {
		lines := strings.Split(block, "\n")
		if len(lines) > 0 {
			// The first line should contain the interface name
			interfaceLine := lines[0]
			parts := strings.Fields(interfaceLine)
			if len(parts) > 0 {
				currentInterface := parts[0]

				// Search for RX and TX bytes in the remaining lines
				for _, line := range lines {
					if strings.Contains(line, "RX bytes") {
						matches := rxTxRegex.FindStringSubmatch(line)
						if len(matches) == 3 {
							rx, _ := strconv.ParseInt(matches[1], 10, 64)
							tx, _ := strconv.ParseInt(matches[2], 10, 64)
							trafficData[currentInterface] = NetworkTraffic{
								Interface: currentInterface,
								RX:        rx,
								TX:        tx,
							}
							break // Exit the loop once RX and TX are found
						}
					}
				}
			}
		}
	}

	return trafficData
}

func mergeData(ifdevData []Ifdev, mwan3Data []Mwan3ifstatus, networkTrafficData map[string]NetworkTraffic) []CombinedData {
	var combined []CombinedData

	// Create a map with Interface as the key and the Ifdev struct as the value
	ifdevMap := make(map[string]Ifdev)
	for _, ifdev := range ifdevData {
		ifdevMap[ifdev.Interface] = ifdev
	}

	// Iterate over mwan3Data and merge using the map
	for _, mwan3 := range mwan3Data {
		if ifdev, exists := ifdevMap[mwan3.Interface]; exists {
			traffic := networkTrafficData[ifdev.Device]
			combined = append(combined, CombinedData{
				Interface:  ifdev.Interface,
				Device:     ifdev.Device,
				Status:     mwan3.Status,
				OnlineTime: mwan3.OnlineTime,
				Uptime:     mwan3.Uptime,
				Tracking:   mwan3.Tracking,
				RX:         traffic.RX,
				TX:         traffic.TX,
			})
		}
	}

	return combined
}

// collectInterfaces returns mwan3's USB (tethered phone) interfaces with their
// mwan3 status and traffic counters, from the ifdev and mwan3ifstatus helpers
// and ifconfig.
func collectInterfaces() ([]CombinedData, error) {
	ifdevOutput, err := executeShellCommand("ifdev")
	if err != nil {
		return nil, fmt.Errorf("Error executing ifdev: %v", err)
	}
	mwan3ifstatusOutput, err := executeShellCommand("mwan3ifstatus")
	if err != nil {
		return nil, fmt.Errorf("Error executing mwan3ifstatus: %v", err)
	}
	networkTraffic, err := getNetworkTraffic()
	if err != nil {
		log.Println("Error getting network traffic:", err)
	}

	var ifdevData []Ifdev
	var mwan3ifstatusData []Mwan3ifstatus
	json.Unmarshal(ifdevOutput, &ifdevData)
	json.Unmarshal(mwan3ifstatusOutput, &mwan3ifstatusData)
	return mergeData(filterUSBInterfaces(ifdevData), mwan3ifstatusData, networkTraffic), nil
}

func pushMetrics(timeSeriesList []promremote.TimeSeries) {
	cfg := promremote.NewConfig(
		promremote.WriteURLOption(pushURL),
		promremote.HTTPClientTimeoutOption(60*time.Second),
	)

	client, err := promremote.NewClient(cfg)
	if err != nil {
		log.Println("Error creating remote client:", err)
		return
	}

	ctx := context.Background()
	opts := promremote.WriteOptions{
		Headers: map[string]string{
			"Authorization": getBasicAuthHeader(username, password),
		},
	}

	if _, err := client.WriteTimeSeries(ctx, timeSeriesList, opts); err != nil {
		log.Println("Error writing metrics:", err)
	}
}

func validateParameters() error {
	if pushURL == "" {
		return fmt.Errorf("PUSH_URL environment variable is not set")
	}

	if pushIntervalSeconds <= 0 {
		return fmt.Errorf("PUSH_INTERVAL_SECONDS environment variable is not set or has an invalid value")
	}

	// Additional validations can be added here if needed

	return nil
}

func main() {
	adbDump := flag.Bool("adb-dump", false, "connect to every USB ADB device on this machine, print its phone metrics, and exit")
	adbRaw := flag.Bool("adb-raw", false, "with -adb-dump, also print the raw output read from each phone")
	speedNow := flag.Bool("speedtest-now", false, "run one speed test through each online tethered interface, print the results, and exit")
	speedDevs := flag.String("speedtest-dev", "", "with -speedtest-now, test these network interfaces instead (comma-separated, e.g. usb2)")
	flag.Parse()

	adbCfg := loadADBConfig()
	if *adbDump {
		os.Exit(runADBDump(adbCfg, *adbRaw))
	}
	speedCfg := loadSpeedtestConfig()
	if *speedNow {
		os.Exit(runSpeedtestNow(speedCfg, *speedDevs))
	}

	if err := validateParameters(); err != nil {
		log.Fatalf("Parameter validation failed: %s", err)
	}
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var phones *adbCollector
	if adbCfg.Enabled {
		var err error
		if phones, err = newADBCollector(adbCfg); err != nil {
			log.Printf("ADB phone metrics disabled: %v", err)
		} else {
			go phones.run(ctx)
		}
	}

	var speed *speedCollector
	if speedCfg.Enabled {
		speed = newSpeedCollector(speedCfg)
		go speed.run(ctx)
		log.Printf("Speed tests on: each phone every %s, up to %.1f MB down and %.1f MB up, %s per direction (server %s, firewall mark %#x)",
			speedCfg.Interval, float64(speedCfg.DownloadBytes)/1e6, float64(speedCfg.UploadBytes)/1e6, speedCfg.MaxDuration, speedCfg.URL, speedCfg.FWMark)
	}

	ticker := time.NewTicker(time.Duration(pushIntervalSeconds) * time.Second)
	defer ticker.Stop()

loop:
	for {
		select {
		case <-ticker.C:
			combinedData, err := collectInterfaces()
			if err != nil {
				log.Println(err)
				break
			}

			var timeSeriesList []promremote.TimeSeries

			// USB device behind each tethered interface: for its USB link speed,
			// and so the ADB collector only talks to tethered phones and its
			// metrics get the same labels.
			usbIDs := make(map[string]string)
			matchedPhones := make(map[string]bool)
			targets := make(map[string]bool)
			var speedIfaces []string
			var speedTargets []speedTarget
			for _, data := range combinedData {
				speedIfaces = append(speedIfaces, data.Interface)
				if id, err := usbDeviceID(sysRoot, data.Device); err == nil {
					usbIDs[data.Device] = id
					targets[id] = true
				}
			}
			if phones != nil {
				phones.setTargets(targets)
			}

			for _, data := range combinedData {
				device, err := getUSBDevice(data.Device)
				if err != nil {
					log.Printf("Error getting USB device for interface %s: %v", data.Interface, err)
					continue
				}
				iface := data.Interface

				uptimeInSeconds := parseUptimeToSeconds(data.Uptime)
				onlineTimeInSeconds := parseUptimeToSeconds(data.OnlineTime)

				status := data.Status
				tracking := data.Tracking

				statusOnline := 0.0
				if status == "online" {
					statusOnline = 1.0
				}

				statusEnabled := 0.0
				if status != "disabled" {
					statusEnabled = 1.0
				}

				statusTracking := 0.0
				if tracking == "active" {
					statusTracking = 1.0
				}

				// Add metrics to the time series list
				timeSeriesList = append(timeSeriesList, promremote.TimeSeries{
					Labels: []promremote.Label{
						{Name: "__name__", Value: "tether_iface_up_time"},
						{Name: "device", Value: device},
						{Name: "interface", Value: iface},
					},
					Datapoint: promremote.Datapoint{
						Timestamp: time.Now(),
						Value:     uptimeInSeconds,
					},
				})

				timeSeriesList = append(timeSeriesList, promremote.TimeSeries{
					Labels: []promremote.Label{
						{Name: "__name__", Value: "tether_iface_online_time"},
						{Name: "device", Value: device},
						{Name: "interface", Value: iface},
					},
					Datapoint: promremote.Datapoint{
						Timestamp: time.Now(),
						Value:     onlineTimeInSeconds,
					},
				})

				timeSeriesList = append(timeSeriesList, promremote.TimeSeries{
					Labels: []promremote.Label{
						{Name: "__name__", Value: "tether_iface_status_online"},
						{Name: "device", Value: device},
						{Name: "interface", Value: iface},
					},
					Datapoint: promremote.Datapoint{
						Timestamp: time.Now(),
						Value:     statusOnline,
					},
				})

				timeSeriesList = append(timeSeriesList, promremote.TimeSeries{
					Labels: []promremote.Label{
						{Name: "__name__", Value: "tether_iface_status_enabled"},
						{Name: "device", Value: device},
						{Name: "interface", Value: iface},
					},
					Datapoint: promremote.Datapoint{
						Timestamp: time.Now(),
						Value:     statusEnabled,
					},
				})

				timeSeriesList = append(timeSeriesList, promremote.TimeSeries{
					Labels: []promremote.Label{
						{Name: "__name__", Value: "tether_iface_status_tracking"},
						{Name: "device", Value: device},
						{Name: "interface", Value: iface},
					},
					Datapoint: promremote.Datapoint{
						Timestamp: time.Now(),
						Value:     statusTracking,
					},
				})

				timeSeriesList = append(timeSeriesList, promremote.TimeSeries{
					Labels: []promremote.Label{
						{Name: "__name__", Value: "tether_iface_tx"},
						{Name: "device", Value: device},
						{Name: "interface", Value: iface},
					},
					Datapoint: promremote.Datapoint{
						Timestamp: time.Now(),
						Value:     float64(data.TX),
					},
				})

				timeSeriesList = append(timeSeriesList, promremote.TimeSeries{
					Labels: []promremote.Label{
						{Name: "__name__", Value: "tether_iface_rx"},
						{Name: "device", Value: device},
						{Name: "interface", Value: iface},
					},
					Datapoint: promremote.Datapoint{
						Timestamp: time.Now(),
						Value:     float64(data.RX),
					},
				})

				if id, ok := usbIDs[data.Device]; ok {
					base := []promremote.Label{
						{Name: "device", Value: device},
						{Name: "interface", Value: iface},
					}
					// USB link speed: 480 means the phone is running in USB 2.0
					// mode, which also caps a standard port at 500 mA.
					if mbps, ok := usbSpeedMbps(sysRoot, id); ok {
						timeSeriesList = append(timeSeriesList, newSeries("tether_iface_usb_speed_mbps", mbps, time.Now(), base, promremote.Label{Name: "usb_port", Value: id}))
					}
					if phones != nil {
						matchedPhones[id] = true
						timeSeriesList = phones.appendSeries(timeSeriesList, id, base, time.Now())
					}
				}

				if speed != nil {
					speedTargets = append(speedTargets, speedTarget{Iface: iface, Netdev: data.Device, Device: device, Online: status == "online"})
					timeSeriesList = speed.appendSeries(timeSeriesList, iface, device, []promremote.Label{
						{Name: "device", Value: device},
						{Name: "interface", Value: iface},
					}, time.Now())
				}
			}

			// Known phones plugged in without USB tethering (no interface label).
			if phones != nil {
				timeSeriesList = phones.appendUntethered(timeSeriesList, matchedPhones, time.Now())
			}
			if speed != nil {
				speed.setTargets(speedIfaces, speedTargets)
			}

			// Push metrics
			pushMetrics(timeSeriesList)

		case sig := <-sigChan:
			log.Printf("Received signal: %s. Exiting...\n", sig)
			break loop
		}
	}
}
