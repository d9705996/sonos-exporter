package main

import (
	"context"
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

// newTestSpeaker creates a speaker pre-wired to the given SOAP server URL.
func newTestSpeaker(soapURL string) *speaker {
	return &speaker{
		UDN:            "uuid:test",
		Name:           "Living Room",
		Model:          "Arc",
		Version:        "16.0",
		ModelNumber:    "S27",
		Host:           "127.0.0.1",
		RenderingURL:   soapURL + "/RenderingControl/Control",
		AVTransportURL: soapURL + "/AVTransport/Control",
		StatusURL:      soapURL + "/status/zp",
		FirstSeen:      time.Now().Add(-10 * time.Minute),
		LastSeen:       time.Now(),
	}
}

// newTestExporterWithSpeaker creates an exporter with a single test speaker.
func newTestExporterWithSpeaker(soapURL string) *sonosExporter {
	e := newSonosExporter(slog.Default())
	sp := newTestSpeaker(soapURL)
	e.speakers = map[string]*speaker{sp.UDN: sp}
	return e
}

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

	exporter := newTestExporterWithSpeaker(soapServer.URL)

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

	exporter := newTestExporterWithSpeaker(soapServer.URL)

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

	exporter := newTestExporterWithSpeaker(soapServer.URL)

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

// --- Unit tests for utility functions ---

func TestParseXMLTag(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, raw, tag, want string
	}{
		{"basic", "<Foo>bar</Foo>", "Foo", "bar"},
		{"with whitespace", "<Foo> bar </Foo>", "Foo", "bar"},
		{"namespaced", "<dc:title>Hello</dc:title>", "dc:title", "Hello"},
		{"missing tag", "<Foo>bar</Foo>", "Baz", ""},
		{"empty value", "<Foo></Foo>", "Foo", ""},
		{"no closing tag", "<Foo>bar", "Foo", ""},
		{"nested same-start", "<FooBar>x</FooBar><Foo>y</Foo>", "Foo", "y"},
		{"empty input", "", "Foo", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseXMLTag(tt.raw, tt.tag)
			if got != tt.want {
				t.Errorf("parseXMLTag(%q, %q) = %q, want %q", tt.raw, tt.tag, got, tt.want)
			}
		})
	}
}

func TestParseDurationString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want float64
	}{
		{"zero", "0:00:00", 0},
		{"one second", "0:00:01", 1},
		{"one minute", "0:01:00", 60},
		{"one hour", "1:00:00", 3600},
		{"complex", "2:30:45", 9045},
		{"invalid too few parts", "1:00", -1},
		{"invalid non-numeric", "a:b:c", -1},
		{"empty", "", -1},
		{"with whitespace", " 0:05:30 ", 330},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := parseDurationString(tt.in)
			if got != tt.want {
				t.Errorf("parseDurationString(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestXmlEscape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in, want string
	}{
		{"hello", "hello"},
		{"a&b", "a&amp;b"},
		{"<b>", "&lt;b&gt;"},
		{`"x"`, "&quot;x&quot;"},
		{"it's", "it&apos;s"},
		{"a&<>\"'b", "a&amp;&lt;&gt;&quot;&apos;b"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			t.Parallel()
			got := xmlEscape(tt.in)
			if got != tt.want {
				t.Errorf("xmlEscape(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestExtractLocation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, input, want string
	}{
		{"standard", "HTTP/1.1 200 OK\r\nLOCATION: http://192.168.1.5:1400/xml/device_description.xml\r\nST: upnp:rootdevice\r\n\r\n", "http://192.168.1.5:1400/xml/device_description.xml"},
		{"lowercase", "HTTP/1.1 200 OK\r\nlocation: http://host:1400/xml\r\n\r\n", "http://host:1400/xml"},
		{"mixed case", "HTTP/1.1 200 OK\r\nLocation: http://host:1400/xml\r\n\r\n", "http://host:1400/xml"},
		{"no location", "HTTP/1.1 200 OK\r\nST: upnp:rootdevice\r\n\r\n", ""},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := extractLocation(tt.input)
			if got != tt.want {
				t.Errorf("extractLocation() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, base, path, want string
	}{
		{"absolute path", "http://192.168.1.5:1400", "/MediaRenderer/RenderingControl/Control", "http://192.168.1.5:1400/MediaRenderer/RenderingControl/Control"},
		{"relative path", "http://192.168.1.5:1400", "control", "http://192.168.1.5:1400/control"},
		{"already absolute URL", "http://192.168.1.5:1400", "http://other:1400/x", "http://other:1400/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := resolveURL(tt.base, tt.path)
			if got != tt.want {
				t.Errorf("resolveURL(%q, %q) = %q, want %q", tt.base, tt.path, got, tt.want)
			}
		})
	}
}

func TestBoolToFloat(t *testing.T) {
	t.Parallel()
	if boolToFloat(true) != 1 {
		t.Error("expected 1 for true")
	}
	if boolToFloat(false) != 0 {
		t.Error("expected 0 for false")
	}
}

// --- SOAP call and method tests ---

func TestSoapCallReturnsErrorOnHTTPFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal", http.StatusInternalServerError)
	}))
	defer srv.Close()

	e := newSonosExporter(slog.Default())
	_, err := e.soapCall(context.Background(), srv.URL, "urn:test", "TestAction", nil, 2*time.Second)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("expected error to mention status code, got: %v", err)
	}
}

func TestSoapCallReturnsBody(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<Result>OK</Result>`)
	}))
	defer srv.Close()

	e := newSonosExporter(slog.Default())
	body, err := e.soapCall(context.Background(), srv.URL, "urn:test", "TestAction", map[string]string{"Key": "Val"}, 2*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(body, "OK") {
		t.Errorf("expected body to contain OK, got: %s", body)
	}
}

func TestGetVolumeAndMuteAndBassAndTrebleAndLoudness(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		switch r.Header.Get("SOAPACTION") {
		case `"urn:schemas-upnp-org:service:RenderingControl:1#GetVolume"`:
			_, _ = fmt.Fprint(w, `<CurrentVolume>55</CurrentVolume>`)
		case `"urn:schemas-upnp-org:service:RenderingControl:1#GetMute"`:
			_, _ = fmt.Fprint(w, `<CurrentMute>1</CurrentMute>`)
		case `"urn:schemas-upnp-org:service:RenderingControl:1#GetBass"`:
			_, _ = fmt.Fprint(w, `<CurrentBass>-3</CurrentBass>`)
		case `"urn:schemas-upnp-org:service:RenderingControl:1#GetTreble"`:
			_, _ = fmt.Fprint(w, `<CurrentTreble>7</CurrentTreble>`)
		case `"urn:schemas-upnp-org:service:RenderingControl:1#GetLoudness"`:
			_, _ = fmt.Fprint(w, `<CurrentLoudness>0</CurrentLoudness>`)
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	e := newSonosExporter(slog.Default())
	sp := newTestSpeaker(srv.URL)
	ctx := context.Background()

	vol, err := e.getVolume(ctx, sp)
	if err != nil || vol != 55 {
		t.Errorf("getVolume: got %v, %v", vol, err)
	}

	mute, err := e.getMute(ctx, sp)
	if err != nil || !mute {
		t.Errorf("getMute: got %v, %v", mute, err)
	}

	bass, err := e.getBass(ctx, sp)
	if err != nil || bass != -3 {
		t.Errorf("getBass: got %v, %v", bass, err)
	}

	treble, err := e.getTreble(ctx, sp)
	if err != nil || treble != 7 {
		t.Errorf("getTreble: got %v, %v", treble, err)
	}

	loudness, err := e.getLoudness(ctx, sp)
	if err != nil || loudness {
		t.Errorf("getLoudness: got %v, %v", loudness, err)
	}
}

func TestGetPlayingStates(t *testing.T) {
	t.Parallel()
	tests := []struct {
		state string
		want  bool
	}{
		{"PLAYING", true},
		{"TRANSITIONING", true},
		{"STOPPED", false},
		{"PAUSED_PLAYBACK", false},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprintf(w, `<CurrentTransportState>%s</CurrentTransportState>`, tt.state)
			}))
			defer srv.Close()

			e := newSonosExporter(slog.Default())
			sp := newTestSpeaker(srv.URL)
			got, err := e.getPlaying(context.Background(), sp)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("getPlaying with state %q = %v, want %v", tt.state, got, tt.want)
			}
		})
	}
}

func TestGetPlayModeReturnsMode(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<PlayMode>SHUFFLE</PlayMode>`)
	}))
	defer srv.Close()

	e := newSonosExporter(slog.Default())
	sp := newTestSpeaker(srv.URL)
	mode, err := e.getPlayMode(context.Background(), sp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mode != "SHUFFLE" {
		t.Errorf("getPlayMode = %q, want SHUFFLE", mode)
	}
}

func TestGetPlayModeReturnsErrorWhenEmpty(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<OtherTag>x</OtherTag>`)
	}))
	defer srv.Close()

	e := newSonosExporter(slog.Default())
	sp := newTestSpeaker(srv.URL)
	_, err := e.getPlayMode(context.Background(), sp)
	if err == nil {
		t.Fatal("expected error for missing play mode")
	}
}

func TestGetSubLevelReturnsErrorWhenMissing(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<OtherTag>x</OtherTag>`)
	}))
	defer srv.Close()

	e := newSonosExporter(slog.Default())
	sp := newTestSpeaker(srv.URL)
	_, err := e.getSubLevel(context.Background(), sp)
	if err == nil {
		t.Fatal("expected error when sub level not present")
	}
}

func TestGetUptimeFallsBackToObserved(t *testing.T) {
	t.Parallel()
	// Use a server that returns a non-200 to trigger fallback
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	e := newSonosExporter(slog.Default())
	sp := newTestSpeaker(srv.URL)
	sp.FirstSeen = time.Now().Add(-5 * time.Minute)

	uptime, source := e.getUptime(context.Background(), sp)
	if source != "observed" {
		t.Errorf("expected source='observed', got %q", source)
	}
	if uptime < 290 || uptime > 310 {
		t.Errorf("expected ~300s uptime, got %v", uptime)
	}
}

func TestCollectorSpeakerDownWhenAllCallsFail(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fail", http.StatusInternalServerError)
	}))
	defer srv.Close()

	exporter := newTestExporterWithSpeaker(srv.URL)
	reg := prometheus.NewRegistry()
	reg.MustRegister(exporter)

	expected := `
# HELP sonos_speaker_up Whether the Sonos speaker is reachable during scrape (1=up, 0=down).
# TYPE sonos_speaker_up gauge
sonos_speaker_up{host="127.0.0.1",model="Arc",name="Living Room",uuid="uuid:test"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "sonos_speaker_up"); err != nil {
		t.Fatalf("metrics mismatch: %v", err)
	}
}

func TestCollectorEmptyWhenNoSpeakers(t *testing.T) {
	t.Parallel()
	exporter := newSonosExporter(slog.Default())
	reg := prometheus.NewRegistry()
	reg.MustRegister(exporter)

	expected := `
# HELP sonos_exporter_discovered_speakers Number of Sonos speakers in exporter cache.
# TYPE sonos_exporter_discovered_speakers gauge
sonos_exporter_discovered_speakers 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected), "sonos_exporter_discovered_speakers"); err != nil {
		t.Fatalf("metrics mismatch: %v", err)
	}
}

func TestSpeakerFromDescriptionMissingServices(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `<?xml version="1.0"?>
<root>
  <URLBase>http://`+r.Host+`</URLBase>
  <device>
    <friendlyName>Test</friendlyName>
    <modelName>One</modelName>
    <modelNumber>S1</modelNumber>
    <UDN>uuid:x</UDN>
    <serviceList></serviceList>
  </device>
</root>`)
	}))
	defer srv.Close()

	_, err := speakerFromDescription(srv.Client(), srv.URL)
	if err == nil {
		t.Fatal("expected error for missing required services")
	}
}

func TestSpeakerFromDescriptionBadXML(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `not xml at all`)
	}))
	defer srv.Close()

	_, err := speakerFromDescription(srv.Client(), srv.URL)
	if err == nil {
		t.Fatal("expected error for invalid XML")
	}
}

func TestSpeakerFromDescriptionHTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "fail", http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := speakerFromDescription(srv.Client(), srv.URL)
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
}

func TestEvictStaleDisabledWhenZero(t *testing.T) {
	t.Parallel()
	exporter := newSonosExporterWithOptions(slog.Default(), 0)
	now := time.Now()
	exporter.speakers = map[string]*speaker{
		"uuid:old": {
			UDN:      "uuid:old",
			LastSeen: now.Add(-1 * time.Hour),
		},
	}

	exporter.speakersMu.Lock()
	exporter.evictStaleLocked(now, map[string]struct{}{})
	exporter.speakersMu.Unlock()

	if len(exporter.getSpeakers()) != 1 {
		t.Fatal("speaker should not be evicted when staleAfter=0")
	}
}

func TestLogNowPlayingChangeDeduplicates(t *testing.T) {
	t.Parallel()
	e := newSonosExporter(slog.Default())
	sp := &speaker{UDN: "uuid:x", Name: "Test"}
	info := nowPlayingInfo{Title: "Song", Artist: "Art"}

	// First call should store
	e.logNowPlayingChange(sp, info)
	raw, ok := e.nowPlaying.Load(sp.UDN)
	if !ok {
		t.Fatal("expected nowPlaying entry after first call")
	}
	if raw.(nowPlayingInfo) != info {
		t.Fatal("stored info mismatch")
	}

	// Second call with same info should be a no-op (no panic/error)
	e.logNowPlayingChange(sp, info)

	// Third call with different info should update
	info2 := nowPlayingInfo{Title: "Song2", Artist: "Art2"}
	e.logNowPlayingChange(sp, info2)
	raw2, _ := e.nowPlaying.Load(sp.UDN)
	if raw2.(nowPlayingInfo) != info2 {
		t.Fatal("expected updated info")
	}
}

func TestGetSpeakersReturnsCopies(t *testing.T) {
	t.Parallel()
	e := newSonosExporter(slog.Default())
	e.speakers = map[string]*speaker{
		"uuid:a": {UDN: "uuid:a", Name: "Original"},
	}

	copies := e.getSpeakers()
	copies[0].Name = "Modified"

	orig := e.getSpeakers()
	if orig[0].Name != "Original" {
		t.Fatal("getSpeakers should return copies, not references")
	}
}

func TestFallbackLogger(t *testing.T) {
	t.Parallel()
	logger := fallbackLogger()
	if logger == nil {
		t.Fatal("fallbackLogger returned nil")
	}
	// Smoke-test: should not panic
	logger.Info("test message")
}

func TestWithTimeoutContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := withTimeoutContext(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected context to have a deadline")
	}
	remaining := time.Until(deadline)
	if remaining < 4*time.Second || remaining > 6*time.Second {
		t.Errorf("expected ~5s deadline, got %v remaining", remaining)
	}
}

func TestResolveURLInvalidBase(t *testing.T) {
	t.Parallel()
	// When base URL is invalid, should return the path as-is
	got := resolveURL("://bad", "/some/path")
	if got != "/some/path" {
		t.Errorf("expected fallback to path, got %q", got)
	}
}

func TestGetUptimeDevicePath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<ZPSupportInfo><uptime>1:30:00</uptime></ZPSupportInfo>`)
	}))
	defer srv.Close()

	e := newSonosExporter(slog.Default())
	sp := newTestSpeaker(srv.URL)

	uptime, source := e.getUptime(context.Background(), sp)
	if source != "device" {
		t.Errorf("expected source='device', got %q", source)
	}
	if uptime != 5400 {
		t.Errorf("expected 5400s, got %v", uptime)
	}
}

func TestSpeakerFromDescriptionNoURLBase(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `<?xml version="1.0"?>
<root>
  <device>
    <friendlyName>Kitchen</friendlyName>
    <modelName>One</modelName>
    <modelNumber>S1</modelNumber>
    <softwareVersion>16.0</softwareVersion>
    <UDN>uuid:kitchen</UDN>
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
	defer srv.Close()

	sp, err := speakerFromDescription(srv.Client(), srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sp.UDN != "uuid:kitchen" {
		t.Errorf("unexpected UDN: %q", sp.UDN)
	}
	// Host should be derived from the location URL
	if sp.Host == "" {
		t.Error("expected non-empty host")
	}
}
