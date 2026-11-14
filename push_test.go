package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/m3db/prometheus_remote_client_golang/promremote"
	"github.com/prometheus/prometheus/prompb"
)

// TestPushMetrics checks the remote-write payload end to end (snappy +
// protobuf), guarding against breakage from protobuf dependency upgrades.
func TestPushMetrics(t *testing.T) {
	var (
		req  prompb.WriteRequest
		auth string
		got  bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		raw, err := snappy.Decode(nil, body)
		if err != nil {
			t.Errorf("snappy: %v", err)
		}
		if err := req.Unmarshal(raw); err != nil {
			t.Errorf("protobuf: %v", err)
		}
		got = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	oldURL, oldUser, oldPass := pushURL, username, password
	pushURL, username, password = srv.URL, "user", "pass"
	defer func() { pushURL, username, password = oldURL, oldUser, oldPass }()

	now := time.Now()
	pushMetrics([]promremote.TimeSeries{
		newSeries("tether_phone_signal_rsrp_dbm", -89, now,
			[]promremote.Label{{Name: "device", Value: "phone"}, {Name: "interface", Value: "wan_usb"}},
			label("slot", "0"), label("rat", "lte")),
	})

	if !got {
		t.Fatal("no request received")
	}
	if auth != getBasicAuthHeader("user", "pass") {
		t.Errorf("auth header %q", auth)
	}
	if len(req.Timeseries) != 1 || len(req.Timeseries[0].Samples) != 1 {
		t.Fatalf("unexpected payload: %+v", req)
	}
	ts := req.Timeseries[0]
	want := []string{"__name__=tether_phone_signal_rsrp_dbm", "device=phone", "interface=wan_usb", "rat=lte", "slot=0"}
	if len(ts.Labels) != len(want) {
		t.Fatalf("labels %v, want %v", ts.Labels, want)
	}
	for i, l := range ts.Labels {
		if l.Name+"="+l.Value != want[i] {
			t.Errorf("label %d = %s=%s, want %s", i, l.Name, l.Value, want[i])
		}
	}
	if s := ts.Samples[0]; s.Value != -89 || s.Timestamp != now.UnixMilli() {
		t.Errorf("sample %+v, want -89 at %d", s, now.UnixMilli())
	}
}
