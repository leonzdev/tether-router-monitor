package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/m3dbx/prometheus_remote_client_golang/promremote"
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

func parseUptimeToSeconds(uptime string) float64 {
	var hours, minutes, seconds float64
	fmt.Sscanf(uptime, "%2fh:%2fm:%2fs", &hours, &minutes, &seconds)
	return hours*3600 + minutes*60 + seconds
}

func pushMetrics(req promremote.WriteRequest) {
	cfg := promremote.NewConfig(
		pushURL,
		promremote.WithHTTPClient(
			&http.Client{
				Transport: &http.Transport{
					MaxIdleConns:        10,
					IdleConnTimeout:     30 * time.Second,
					DisableCompression:  true,
					MaxIdleConnsPerHost: 10,
				},
			},
		),
		promremote.WithBasicAuth(username, password),
	)

	client, err := promremote.NewClient(cfg)
	if err != nil {
		fmt.Println("Error creating remote client:", err)
		return
	}

	err = client.Write(req)
	if err != nil {
		fmt.Println("Error writing metrics:", err)
	}
}

func main() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(time.Duration(pushIntervalSeconds) * time.Second)
	defer ticker.Stop()

loop:
	for {
		select {
		case <-ticker.C:
			ifdevOutput, err := executeShellCommand("./ifdev")
			if err != nil {
				fmt.Println("Error executing ifdev:", err)
				break
			}

			mwan3ifstatusOutput, err := executeShellCommand("./mwan3ifstatus")
			if err != nil {
				fmt.Println("Error executing mwan3ifstatus:", err)
				break
			}

			var ifdevData []Ifdev
			var mwan3ifstatusData []Mwan3ifstatus

			json.Unmarshal(ifdevOutput, &ifdevData)
			json.Unmarshal(mwan3ifstatusOutput, &mwan3ifstatusData)

			ifdevData = filterUSBInterfaces(ifdevData)

			req := promremote.WriteRequest{}
			for _, data := range ifdevData {
				device := data.Device
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

				// Add metrics to the write request
				req.Add(promremote.Timeseries{
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

				req.Add(promremote.Timeseries{
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

				req.Add(promremote.Timeseries{
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

				req.Add(promremote.Timeseries{
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

				req.Add(promremote.Timeseries{
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
			pushMetrics(req)

		case sig := <-sigChan:
			fmt.Printf("Received signal: %s. Exiting...\n", sig)
			break loop
		}
	}
}
