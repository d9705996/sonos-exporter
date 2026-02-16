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
			_, _ = fmt.Fprint(w, `<CurrentVolume>22</CurrentVolume>`)
		case `"urn:schemas-upnp-org:service:RenderingControl:1#GetEQ"`:
			_, _ = fmt.Fprint(w, `<CurrentValue>5</CurrentValue>`)
		case `"urn:schemas-upnp-org:service:AVTransport:1#GetTransportInfo"`:
			_, _ = fmt.Fprint(w, `<CurrentTransportState>PLAYING</CurrentTransportState>`)
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

func TestCollectorExportsNowPlayingMetric(t *testing.T) {
	t.Parallel()

	soapServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		switch r.Header.Get("SOAPACTION") {
		case `"urn:schemas-upnp-org:service:RenderingControl:1#GetVolume"`:
			_, _ = fmt.Fprint(w, `<CurrentVolume>22</CurrentVolume>`)
		case `"urn:schemas-upnp-org:service:RenderingControl:1#GetEQ"`:
			_, _ = fmt.Fprint(w, `<CurrentValue>5</CurrentValue>`)
		case `"urn:schemas-upnp-org:service:AVTransport:1#GetTransportInfo"`:
			_, _ = fmt.Fprint(w, `<CurrentTransportState>PLAYING</CurrentTransportState>`)
		case `"urn:schemas-upnp-org:service:AVTransport:1#GetPositionInfo"`:
			_, _ = fmt.Fprint(w, `<TrackDuration>00:04:00</TrackDuration><RelTime>00:01:30</RelTime><TrackURI>x-sonos-http:track.mp3</TrackURI><TrackMetaData>&lt;dc:title&gt;Song A&lt;/dc:title&gt;&lt;dc:creator&gt;Artist B&lt;/dc:creator&gt;&lt;upnp:album&gt;Album C&lt;/upnp:album&gt;</TrackMetaData>`)
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
# HELP sonos_speaker_now_playing_info Current Sonos track metadata (value always 1).
# TYPE sonos_speaker_now_playing_info gauge
sonos_speaker_now_playing_info{album="Album C",artist="Artist B",host="127.0.0.1",model="Arc",name="Living Room",title="Song A",uri="x-sonos-http:track.mp3",uuid="uuid:test"} 1
# HELP sonos_speaker_track_duration_seconds Current track duration in seconds when available.
# TYPE sonos_speaker_track_duration_seconds gauge
sonos_speaker_track_duration_seconds{host="127.0.0.1",model="Arc",name="Living Room",uuid="uuid:test"} 240
# HELP sonos_speaker_track_position_seconds Current track playback position in seconds when available.
# TYPE sonos_speaker_track_position_seconds gauge
sonos_speaker_track_position_seconds{host="127.0.0.1",model="Arc",name="Living Room",uuid="uuid:test"} 90
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "sonos_speaker_now_playing_info", "sonos_speaker_track_duration_seconds", "sonos_speaker_track_position_seconds"); err != nil {
		t.Fatalf("metrics mismatch: %v", err)
	}
}

func TestCollectorSkipsSubLevelMetricWhenUnavailable(t *testing.T) {
	t.Parallel()

	soapServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		switch r.Header.Get("SOAPACTION") {
		case `"urn:schemas-upnp-org:service:RenderingControl:1#GetVolume"`:
			_, _ = fmt.Fprint(w, `<CurrentVolume>22</CurrentVolume>`)
		case `"urn:schemas-upnp-org:service:RenderingControl:1#GetEQ"`:
			_, _ = fmt.Fprint(w, `<NoSubValue>true</NoSubValue>`)
		case `"urn:schemas-upnp-org:service:AVTransport:1#GetTransportInfo"`:
			_, _ = fmt.Fprint(w, `<CurrentTransportState>STOPPED</CurrentTransportState>`)
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

func TestRefreshSpeakersEvictsStaleSpeakers(t *testing.T) {
	t.Parallel()

	exporter := newSonosExporterWithOptions(slog.Default(), time.Minute)
	now := time.Now()
	exporter.speakers = map[string]*speaker{
		"uuid:gone": {
			UDN:      "uuid:gone",
			Name:     "Kitchen",
			LastSeen: now.Add(-2 * time.Minute),
		},
		"uuid:fresh": {
			UDN:      "uuid:fresh",
			Name:     "Office",
			LastSeen: now.Add(-30 * time.Second),
		},
	}

	exporter.speakersMu.Lock()
	exporter.evictStaleLocked(now, map[string]struct{}{"uuid:fresh": {}})
	exporter.speakersMu.Unlock()

	speakers := exporter.getSpeakers()
	if len(speakers) != 1 {
		t.Fatalf("expected one speaker after stale eviction, got %d", len(speakers))
	}
	if speakers[0].UDN != "uuid:fresh" {
		t.Fatalf("expected fresh speaker to remain, got %q", speakers[0].UDN)
	}
}

func TestSpeakerFromDescriptionUsesSoftwareVersion(t *testing.T) {
	t.Parallel()

	descServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `<?xml version="1.0"?>
<root>
  <URLBase>http://`+r.Host+`</URLBase>
  <device>
    <friendlyName>Bedroom</friendlyName>
    <modelName>Era 100</modelName>
    <modelNumber>E10</modelNumber>
    <softwareVersion>17.2-12345</softwareVersion>
    <UDN>uuid:bedroom</UDN>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:RenderingControl:1</serviceType>
        <controlURL>/MediaRenderer/RenderingControl/Control</controlURL>
      </service>
      <service>
        <serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>
        <controlURL>/MediaRenderer/AVTransport/Control</controlURL>
      </service>
    </serviceList>
  </device>
</root>`)
	}))
	defer descServer.Close()

	sp, err := speakerFromDescription(descServer.Client(), descServer.URL)
	if err != nil {
		t.Fatalf("speakerFromDescription failed: %v", err)
	}
	if sp.Version != "17.2-12345" {
		t.Fatalf("expected software version, got %q", sp.Version)
	}
}
