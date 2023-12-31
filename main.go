package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
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

	return usbInfo.Description, nil
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
	if err := validateParameters(); err != nil {
		log.Fatalf("Parameter validation failed: %s", err)
	}
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(time.Duration(pushIntervalSeconds) * time.Second)
	defer ticker.Stop()

loop:
	for {
		select {
		case <-ticker.C:
			ifdevOutput, err := executeShellCommand("ifdev")
			if err != nil {
				log.Println("Error executing ifdev:", err)
				break
			}

			mwan3ifstatusOutput, err := executeShellCommand("mwan3ifstatus")
			if err != nil {
				log.Println("Error executing mwan3ifstatus:", err)
				break
			}
			networkTraffic, err := getNetworkTraffic()
			if err != nil {
				log.Println("Error getting network traffic:", err)
			}
			var ifdevData []Ifdev
			var mwan3ifstatusData []Mwan3ifstatus

			json.Unmarshal(ifdevOutput, &ifdevData)
			json.Unmarshal(mwan3ifstatusOutput, &mwan3ifstatusData)

			ifdevData = filterUSBInterfaces(ifdevData)

			var timeSeriesList []promremote.TimeSeries
			combinedData := mergeData(ifdevData, mwan3ifstatusData, networkTraffic)
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
			}

			// Push metrics
			pushMetrics(timeSeriesList)

		case sig := <-sigChan:
			log.Printf("Received signal: %s. Exiting...\n", sig)
			break loop
		}
	}
}
