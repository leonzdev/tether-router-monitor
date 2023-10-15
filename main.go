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
	"strconv"
	"syscall"
	"time"

	"github.com/m3db/prometheus_remote_client_golang/promremote"
)

type Ifdev struct {
	Interface string `json:"interface"`
	Device    string `json:"device"`
}

type Mwan3ifstatus struct {
	Interface   string `json:"interface"`
	Status      string `json:"status"`
	OnlineTime  string `json:"online_time"`
	Uptime      string `json:"uptime"`
	Tracking    string `json:"tracking"`
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
		Device string `json:"device"`
	}
	if err := json.Unmarshal(ifusbOutput, &usbInfo); err != nil {
		return "", fmt.Errorf("Error unmarshalling ifusb output: %v", err)
	}

	return usbInfo.Device, nil
}

func parseUptimeToSeconds(uptime string) float64 {
	var hours, minutes, seconds float64
	fmt.Sscanf(uptime, "%2fh:%2fm:%2fs", &hours, &minutes, &seconds)
	return hours*3600 + minutes*60 + seconds
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

			var ifdevData []Ifdev
			var mwan3ifstatusData []Mwan3ifstatus

			json.Unmarshal(ifdevOutput, &ifdevData)
			json.Unmarshal(mwan3ifstatusOutput, &mwan3ifstatusData)

			ifdevData = filterUSBInterfaces(ifdevData)

			var timeSeriesList []promremote.TimeSeries
			for _, data := range mwan3ifstatusData {
				device, err := getUSBDevice(data.Interface)
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

			}

			// Push metrics
			pushMetrics(timeSeriesList)

		case sig := <-sigChan:
			log.Printf("Received signal: %s. Exiting...\n", sig)
			break loop
		}
	}
}
