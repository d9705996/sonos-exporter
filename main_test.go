package main

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestCollectorExportsSpeakerMetrics(t *testing.T) {
	t.Parallel()

	soapServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		switch r.Header.Get("SOAPACTION") {
		case `"urn:schemas-upnp-org:service:RenderingControl:1#GetVolume"`:
			fmt.Fprint(w, `<CurrentVolume>22</CurrentVolume>`)
		case `"urn:schemas-upnp-org:service:RenderingControl:1#GetEQ"`:
			fmt.Fprint(w, `<CurrentValue>5</CurrentValue>`)
		case `"urn:schemas-upnp-org:service:AVTransport:1#GetTransportInfo"`:
			fmt.Fprint(w, `<CurrentTransportState>PLAYING</CurrentTransportState>`)
		default:
			http.Error(w, "unexpected soap action", http.StatusBadRequest)
		}
	}))
	defer soapServer.Close()

	exporter := newSonosExporter(slog.Default())
	now := time.Now()
	exporter.speakers = map[string]*speaker{
		"uuid:test": {
			UDN:            "uuid:test",
			Name:           "Living Room",
			Model:          "Arc",
			Version:        "16.0",
			ModelNumber:    "S27",
			Host:           "127.0.0.1",
			RenderingURL:   soapServer.URL + "/RenderingControl/Control",
			AVTransportURL: soapServer.URL + "/AVTransport/Control",
			FirstSeen:      now.Add(-10 * time.Minute),
			LastSeen:       now,
		},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(exporter)

	expected := `
# HELP sonos_speaker_info Static Sonos speaker info metric with software/model labels.
# TYPE sonos_speaker_info gauge
sonos_speaker_info{host="127.0.0.1",model="Arc",model_number="S27",name="Living Room",uuid="uuid:test",version="16.0"} 1
# HELP sonos_speaker_is_playing Whether Sonos reports currently playing (1=playing, 0=not).
# TYPE sonos_speaker_is_playing gauge
sonos_speaker_is_playing{host="127.0.0.1",model="Arc",name="Living Room",uuid="uuid:test"} 1
# HELP sonos_speaker_sub_level Current Sonos subwoofer level (SubGain) when available.
# TYPE sonos_speaker_sub_level gauge
sonos_speaker_sub_level{host="127.0.0.1",model="Arc",name="Living Room",uuid="uuid:test"} 5
# HELP sonos_speaker_up Whether the Sonos speaker is reachable during scrape (1=up, 0=down).
# TYPE sonos_speaker_up gauge
sonos_speaker_up{host="127.0.0.1",model="Arc",name="Living Room",uuid="uuid:test"} 1
# HELP sonos_speaker_volume_percent Current Sonos volume percentage.
# TYPE sonos_speaker_volume_percent gauge
sonos_speaker_volume_percent{host="127.0.0.1",model="Arc",name="Living Room",uuid="uuid:test"} 22
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "sonos_speaker_info", "sonos_speaker_is_playing", "sonos_speaker_sub_level", "sonos_speaker_up", "sonos_speaker_volume_percent"); err != nil {
		t.Fatalf("metrics mismatch: %v", err)
	}

	gotUptime, err := testutil.GatherAndCount(reg, "sonos_speaker_uptime_seconds")
	if err != nil {
		t.Fatalf("count uptime metrics: %v", err)
	}
	if gotUptime != 1 {
		t.Fatalf("expected one uptime metric, got %d", gotUptime)
	}
}

func TestCollectorSkipsSubLevelMetricWhenUnavailable(t *testing.T) {
	t.Parallel()

	soapServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		switch r.Header.Get("SOAPACTION") {
		case `"urn:schemas-upnp-org:service:RenderingControl:1#GetVolume"`:
			fmt.Fprint(w, `<CurrentVolume>22</CurrentVolume>`)
		case `"urn:schemas-upnp-org:service:RenderingControl:1#GetEQ"`:
			fmt.Fprint(w, `<NoSubValue>true</NoSubValue>`)
		case `"urn:schemas-upnp-org:service:AVTransport:1#GetTransportInfo"`:
			fmt.Fprint(w, `<CurrentTransportState>STOPPED</CurrentTransportState>`)
		default:
			http.Error(w, "unexpected soap action", http.StatusBadRequest)
		}
	}))
	defer soapServer.Close()

	exporter := newSonosExporter(slog.Default())
	now := time.Now()
	exporter.speakers = map[string]*speaker{
		"uuid:test": {
			UDN:            "uuid:test",
			Name:           "Living Room",
			Model:          "Arc",
			Version:        "16.0",
			ModelNumber:    "S27",
			Host:           "127.0.0.1",
			RenderingURL:   soapServer.URL + "/RenderingControl/Control",
			AVTransportURL: soapServer.URL + "/AVTransport/Control",
			FirstSeen:      now.Add(-10 * time.Minute),
			LastSeen:       now,
		},
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(exporter)

	expected := `
# HELP sonos_speaker_is_playing Whether Sonos reports currently playing (1=playing, 0=not).
# TYPE sonos_speaker_is_playing gauge
sonos_speaker_is_playing{host="127.0.0.1",model="Arc",name="Living Room",uuid="uuid:test"} 0
# HELP sonos_speaker_volume_percent Current Sonos volume percentage.
# TYPE sonos_speaker_volume_percent gauge
sonos_speaker_volume_percent{host="127.0.0.1",model="Arc",name="Living Room",uuid="uuid:test"} 22
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "sonos_speaker_is_playing", "sonos_speaker_volume_percent"); err != nil {
		t.Fatalf("metrics mismatch: %v", err)
	}

	gotSub, err := testutil.GatherAndCount(reg, "sonos_speaker_sub_level")
	if err != nil {
		t.Fatalf("count sub-level metrics: %v", err)
	}
	if gotSub != 0 {
		t.Fatalf("expected no sub-level metrics, got %d", gotSub)
	}
}
