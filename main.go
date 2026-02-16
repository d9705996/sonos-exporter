package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

const (
	ssdpAddr                 = "239.255.255.250:1900"
	sonosDeviceType          = "urn:schemas-upnp-org:device:ZonePlayer:1"
	defaultDiscoveryInterval = 60 * time.Second
	defaultDiscoveryTimeout  = 3 * time.Second
	defaultSpeakerStaleAfter = 10 * time.Minute
	deviceReqTimeout         = 4 * time.Second
	soapTimeout              = 3 * time.Second
	maxResponseBytes         = 1 << 20 // 1 MiB safety limit for HTTP response bodies
	renderingControlService  = "urn:schemas-upnp-org:service:RenderingControl:1"
	avTransportService       = "urn:schemas-upnp-org:service:AVTransport:1"
)

type sonosExporter struct {
	client     *http.Client
	logger     *slog.Logger
	tracer     trace.Tracer
	staleAfter time.Duration
	nowPlaying sync.Map
	speakersMu sync.RWMutex
	speakers   map[string]*speaker
}

type nowPlayingInfo struct {
	Title  string
	Artist string
	Album  string
	URI    string
}

type positionSnapshot struct {
	Position float64
	Duration float64
	nowPlayingInfo
}

type speaker struct {
	UDN            string
	Name           string
	Model          string
	Version        string
	Host           string
	ModelNumber    string
	RenderingURL   string
	AVTransportURL string
	StatusURL      string
	FirstSeen      time.Time
	LastSeen       time.Time
}

type deviceDescription struct {
	Device struct {
		FriendlyName string `xml:"friendlyName"`
		ModelName    string `xml:"modelName"`
		ModelNumber  string `xml:"modelNumber"`
		Version      string `xml:"softwareVersion"`
		UDN          string `xml:"UDN"`
		ServiceList  struct {
			Services []struct {
				ServiceType string `xml:"serviceType"`
				ControlURL  string `xml:"controlURL"`
			} `xml:"service"`
		} `xml:"serviceList"`
	} `xml:"device"`
	URLBase string `xml:"URLBase"`
}

type zonePlayerStatus struct {
	Uptime string `xml:"uptime"`
}

var (
	speakerLabels       = []string{"uuid", "name", "model", "host"}
	upDesc              = prometheus.NewDesc("sonos_speaker_up", "Whether the Sonos speaker is reachable during scrape (1=up, 0=down).", speakerLabels, nil)
	volumeDesc          = prometheus.NewDesc("sonos_speaker_volume_percent", "Current Sonos volume percentage.", speakerLabels, nil)
	subLevelDesc        = prometheus.NewDesc("sonos_speaker_sub_level", "Current Sonos subwoofer level (SubGain) when available.", speakerLabels, nil)
	muteDesc            = prometheus.NewDesc("sonos_speaker_mute", "Whether Sonos speaker is muted (1=muted, 0=not).", speakerLabels, nil)
	bassDesc            = prometheus.NewDesc("sonos_speaker_bass", "Current Sonos bass EQ level.", speakerLabels, nil)
	trebleDesc          = prometheus.NewDesc("sonos_speaker_treble", "Current Sonos treble EQ level.", speakerLabels, nil)
	loudnessDesc        = prometheus.NewDesc("sonos_speaker_loudness", "Whether Sonos loudness is enabled (1=enabled, 0=disabled).", speakerLabels, nil)
	playingDesc         = prometheus.NewDesc("sonos_speaker_is_playing", "Whether Sonos reports currently playing (1=playing, 0=not).", speakerLabels, nil)
	playModeDesc        = prometheus.NewDesc("sonos_speaker_play_mode", "Current Sonos play mode as labeled state metric.", []string{"uuid", "name", "model", "host", "mode"}, nil)
	trackPositionDesc   = prometheus.NewDesc("sonos_speaker_track_position_seconds", "Current track playback position in seconds when available.", speakerLabels, nil)
	trackDurationDesc   = prometheus.NewDesc("sonos_speaker_track_duration_seconds", "Current track duration in seconds when available.", speakerLabels, nil)
	nowPlayingDesc      = prometheus.NewDesc("sonos_speaker_now_playing_info", "Current Sonos track metadata (value always 1).", []string{"uuid", "name", "model", "host", "title", "artist", "album", "uri"}, nil)
	lastSeenDesc        = prometheus.NewDesc("sonos_speaker_last_seen_timestamp_seconds", "Unix timestamp when speaker was last discovered.", speakerLabels, nil)
	discoveryAgeDesc    = prometheus.NewDesc("sonos_speaker_discovery_age_seconds", "Seconds since speaker was last discovered.", speakerLabels, nil)
	discoveredTotalDesc = prometheus.NewDesc("sonos_exporter_discovered_speakers", "Number of Sonos speakers in exporter cache.", nil, nil)
	uptimeDesc          = prometheus.NewDesc("sonos_speaker_uptime_seconds", "Speaker uptime in seconds if known; otherwise exporter-observed uptime.", []string{"uuid", "name", "model", "host", "source"}, nil)
	infoDesc            = prometheus.NewDesc("sonos_speaker_info", "Static Sonos speaker info metric with software/model labels.", []string{"uuid", "name", "model", "host", "version", "model_number"}, nil)
)

func newSonosExporter(logger *slog.Logger) *sonosExporter {
	return newSonosExporterWithOptions(logger, defaultSpeakerStaleAfter)
}

func newSonosExporterWithOptions(logger *slog.Logger, staleAfter time.Duration) *sonosExporter {
	return &sonosExporter{
		client:     &http.Client{Timeout: deviceReqTimeout},
		logger:     logger,
		tracer:     otel.Tracer("sonos-exporter"),
		staleAfter: staleAfter,
		speakers:   make(map[string]*speaker),
	}
}

func (e *sonosExporter) startDiscovery(ctx context.Context, interval, timeout time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	e.refreshSpeakers(ctx, timeout)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.refreshSpeakers(ctx, timeout)
		}
	}
}

func (e *sonosExporter) refreshSpeakers(ctx context.Context, discoveryTimeout time.Duration) {
	_, span := e.tracer.Start(ctx, "discovery.refresh")
	defer span.End()
	discovered, err := discoverSonos(discoveryTimeout)
	if err != nil {
		e.logger.Error("discovery error", "error", err)
		return
	}
	now := time.Now()
	discoveredNow := make(map[string]struct{}, len(discovered))
	e.speakersMu.Lock()
	defer e.speakersMu.Unlock()
	for _, sp := range discovered {
		discoveredNow[sp.UDN] = struct{}{}
		if existing, ok := e.speakers[sp.UDN]; ok {
			existing.Name = sp.Name
			existing.Model = sp.Model
			existing.Version = sp.Version
			existing.ModelNumber = sp.ModelNumber
			existing.Host = sp.Host
			existing.RenderingURL = sp.RenderingURL
			existing.AVTransportURL = sp.AVTransportURL
			existing.StatusURL = sp.StatusURL
			existing.LastSeen = now
			continue
		}
		sp.FirstSeen = now
		sp.LastSeen = now
		e.speakers[sp.UDN] = sp
	}
	e.evictStaleLocked(now, discoveredNow)
}

func (e *sonosExporter) evictStaleLocked(now time.Time, discoveredNow map[string]struct{}) {
	if e.staleAfter <= 0 {
		return
	}
	for udn, sp := range e.speakers {
		if _, ok := discoveredNow[udn]; ok {
			continue
		}
		if now.Sub(sp.LastSeen) > e.staleAfter {
			delete(e.speakers, udn)
		}
	}
}

func (e *sonosExporter) getSpeakers() []*speaker {
	e.speakersMu.RLock()
	defer e.speakersMu.RUnlock()
	out := make([]*speaker, 0, len(e.speakers))
	for _, sp := range e.speakers {
		cp := *sp
		out = append(out, &cp)
	}
	return out
}

func (e *sonosExporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- upDesc
	ch <- volumeDesc
	ch <- subLevelDesc
	ch <- muteDesc
	ch <- bassDesc
	ch <- trebleDesc
	ch <- loudnessDesc
	ch <- playingDesc
	ch <- playModeDesc
	ch <- trackPositionDesc
	ch <- trackDurationDesc
	ch <- nowPlayingDesc
	ch <- lastSeenDesc
	ch <- discoveryAgeDesc
	ch <- discoveredTotalDesc
	ch <- uptimeDesc
	ch <- infoDesc
}

func (e *sonosExporter) Collect(ch chan<- prometheus.Metric) {
	ctx := context.Background()
	speakers := e.getSpeakers()
	ch <- prometheus.MustNewConstMetric(discoveredTotalDesc, prometheus.GaugeValue, float64(len(speakers)))
	now := time.Now()

	type speakerMetrics struct {
		sp          *speaker
		volume      float64
		subLevel    float64
		mute        bool
		bass        float64
		treble      float64
		loudness    bool
		playing     bool
		playMode    string
		posSnap     positionSnapshot
		uptime      float64
		source      string
		errVol      error
		errSub      error
		errMute     error
		errBass     error
		errTreble   error
		errLoudness error
		errPlay     error
		errPlayMode error
		errPosition error
	}

	results := make([]speakerMetrics, len(speakers))
	var wg sync.WaitGroup
	for i, sp := range speakers {
		wg.Add(1)
		go func(idx int, sp *speaker) {
			defer wg.Done()
			ctxSpeaker, span := e.tracer.Start(ctx, "speaker.collect")
			defer span.End()
			m := &results[idx]
			m.sp = sp
			m.volume, m.errVol = e.getVolume(ctxSpeaker, sp)
			m.subLevel, m.errSub = e.getSubLevel(ctxSpeaker, sp)
			m.mute, m.errMute = e.getMute(ctxSpeaker, sp)
			m.bass, m.errBass = e.getBass(ctxSpeaker, sp)
			m.treble, m.errTreble = e.getTreble(ctxSpeaker, sp)
			m.loudness, m.errLoudness = e.getLoudness(ctxSpeaker, sp)
			m.playing, m.errPlay = e.getPlaying(ctxSpeaker, sp)
			m.playMode, m.errPlayMode = e.getPlayMode(ctxSpeaker, sp)
			m.posSnap, m.errPosition = e.getPositionSnapshot(ctxSpeaker, sp)
			m.uptime, m.source = e.getUptime(ctxSpeaker, sp)
		}(i, sp)
	}
	wg.Wait()

	for _, m := range results {
		sp := m.sp
		labelValues := []string{sp.UDN, sp.Name, sp.Model, sp.Host}

		up := 1.0
		if m.errVol != nil && m.errPlay != nil {
			up = 0
		}
		ch <- prometheus.MustNewConstMetric(upDesc, prometheus.GaugeValue, up, labelValues...)
		ch <- prometheus.MustNewConstMetric(infoDesc, prometheus.GaugeValue, 1, sp.UDN, sp.Name, sp.Model, sp.Host, sp.Version, sp.ModelNumber)
		ch <- prometheus.MustNewConstMetric(uptimeDesc, prometheus.GaugeValue, m.uptime, sp.UDN, sp.Name, sp.Model, sp.Host, m.source)
		ch <- prometheus.MustNewConstMetric(lastSeenDesc, prometheus.GaugeValue, float64(sp.LastSeen.Unix()), labelValues...)
		ch <- prometheus.MustNewConstMetric(discoveryAgeDesc, prometheus.GaugeValue, now.Sub(sp.LastSeen).Seconds(), labelValues...)

		if m.errVol == nil {
			ch <- prometheus.MustNewConstMetric(volumeDesc, prometheus.GaugeValue, m.volume, labelValues...)
		}
		if m.errSub == nil {
			ch <- prometheus.MustNewConstMetric(subLevelDesc, prometheus.GaugeValue, m.subLevel, labelValues...)
		}
		if m.errMute == nil {
			ch <- prometheus.MustNewConstMetric(muteDesc, prometheus.GaugeValue, boolToFloat(m.mute), labelValues...)
		}
		if m.errBass == nil {
			ch <- prometheus.MustNewConstMetric(bassDesc, prometheus.GaugeValue, m.bass, labelValues...)
		}
		if m.errTreble == nil {
			ch <- prometheus.MustNewConstMetric(trebleDesc, prometheus.GaugeValue, m.treble, labelValues...)
		}
		if m.errLoudness == nil {
			ch <- prometheus.MustNewConstMetric(loudnessDesc, prometheus.GaugeValue, boolToFloat(m.loudness), labelValues...)
		}
		if m.errPlay == nil {
			ch <- prometheus.MustNewConstMetric(playingDesc, prometheus.GaugeValue, boolToFloat(m.playing), labelValues...)
		}
		if m.errPlayMode == nil {
			ch <- prometheus.MustNewConstMetric(playModeDesc, prometheus.GaugeValue, 1, sp.UDN, sp.Name, sp.Model, sp.Host, m.playMode)
		}
		if m.errPosition == nil {
			if m.posSnap.Position >= 0 && m.posSnap.Duration >= 0 {
				ch <- prometheus.MustNewConstMetric(trackPositionDesc, prometheus.GaugeValue, m.posSnap.Position, labelValues...)
				ch <- prometheus.MustNewConstMetric(trackDurationDesc, prometheus.GaugeValue, m.posSnap.Duration, labelValues...)
			}
			if m.posSnap.Title != "" || m.posSnap.Artist != "" || m.posSnap.Album != "" || m.posSnap.URI != "" {
				ch <- prometheus.MustNewConstMetric(nowPlayingDesc, prometheus.GaugeValue, 1, sp.UDN, sp.Name, sp.Model, sp.Host, m.posSnap.Title, m.posSnap.Artist, m.posSnap.Album, m.posSnap.URI)
				e.logNowPlayingChange(sp, m.posSnap.nowPlayingInfo)
			}
		}
	}
}

func (e *sonosExporter) getSubLevel(ctx context.Context, sp *speaker) (float64, error) {
	_, span := e.tracer.Start(ctx, "speaker.get_sub_level")
	defer span.End()
	resp, err := e.soapCall(ctx, sp.RenderingURL, renderingControlService, "GetEQ", map[string]string{"InstanceID": "0", "EQType": "SubGain"}, soapTimeout)
	if err != nil {
		return 0, err
	}
	v := parseXMLTag(resp, "CurrentValue")
	if v == "" {
		return 0, errors.New("sub level not present")
	}
	return strconv.ParseFloat(v, 64)
}

func (e *sonosExporter) getMute(ctx context.Context, sp *speaker) (bool, error) {
	_, span := e.tracer.Start(ctx, "speaker.get_mute")
	defer span.End()
	resp, err := e.soapCall(ctx, sp.RenderingURL, renderingControlService, "GetMute", map[string]string{"InstanceID": "0", "Channel": "Master"}, soapTimeout)
	if err != nil {
		return false, err
	}
	return parseXMLTag(resp, "CurrentMute") == "1", nil
}

func (e *sonosExporter) getBass(ctx context.Context, sp *speaker) (float64, error) {
	_, span := e.tracer.Start(ctx, "speaker.get_bass")
	defer span.End()
	resp, err := e.soapCall(ctx, sp.RenderingURL, renderingControlService, "GetBass", map[string]string{"InstanceID": "0"}, soapTimeout)
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(parseXMLTag(resp, "CurrentBass"), 64)
}

func (e *sonosExporter) getTreble(ctx context.Context, sp *speaker) (float64, error) {
	_, span := e.tracer.Start(ctx, "speaker.get_treble")
	defer span.End()
	resp, err := e.soapCall(ctx, sp.RenderingURL, renderingControlService, "GetTreble", map[string]string{"InstanceID": "0"}, soapTimeout)
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(parseXMLTag(resp, "CurrentTreble"), 64)
}

func (e *sonosExporter) getLoudness(ctx context.Context, sp *speaker) (bool, error) {
	_, span := e.tracer.Start(ctx, "speaker.get_loudness")
	defer span.End()
	resp, err := e.soapCall(ctx, sp.RenderingURL, renderingControlService, "GetLoudness", map[string]string{"InstanceID": "0", "Channel": "Master"}, soapTimeout)
	if err != nil {
		return false, err
	}
	return parseXMLTag(resp, "CurrentLoudness") == "1", nil
}

func (e *sonosExporter) getPlayMode(ctx context.Context, sp *speaker) (string, error) {
	_, span := e.tracer.Start(ctx, "speaker.get_play_mode")
	defer span.End()
	resp, err := e.soapCall(ctx, sp.AVTransportURL, avTransportService, "GetTransportSettings", map[string]string{"InstanceID": "0"}, soapTimeout)
	if err != nil {
		return "", err
	}
	mode := strings.TrimSpace(parseXMLTag(resp, "PlayMode"))
	if mode == "" {
		return "", errors.New("play mode not present")
	}
	return mode, nil
}

func (e *sonosExporter) getPositionSnapshot(ctx context.Context, sp *speaker) (positionSnapshot, error) {
	_, span := e.tracer.Start(ctx, "speaker.get_position_info")
	defer span.End()
	resp, err := e.soapCall(ctx, sp.AVTransportURL, avTransportService, "GetPositionInfo", map[string]string{"InstanceID": "0"}, soapTimeout)
	if err != nil {
		return positionSnapshot{}, err
	}
	metadata := html.UnescapeString(parseXMLTag(resp, "TrackMetaData"))
	return positionSnapshot{
		Position: parseDurationString(parseXMLTag(resp, "RelTime")),
		Duration: parseDurationString(parseXMLTag(resp, "TrackDuration")),
		nowPlayingInfo: nowPlayingInfo{
			Title:  parseXMLTag(metadata, "dc:title"),
			Artist: parseXMLTag(metadata, "dc:creator"),
			Album:  parseXMLTag(metadata, "upnp:album"),
			URI:    strings.TrimSpace(parseXMLTag(resp, "TrackURI")),
		},
	}, nil
}

func (e *sonosExporter) logNowPlayingChange(sp *speaker, nowPlaying nowPlayingInfo) {
	if prevRaw, ok := e.nowPlaying.Load(sp.UDN); ok {
		if prev, ok := prevRaw.(nowPlayingInfo); ok && prev == nowPlaying {
			return
		}
	}
	e.nowPlaying.Store(sp.UDN, nowPlaying)
	e.logger.Info("sonos now playing", "uuid", sp.UDN, "name", sp.Name, "title", nowPlaying.Title, "artist", nowPlaying.Artist, "album", nowPlaying.Album, "uri", nowPlaying.URI)
}

func boolToFloat(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

func discoverSonos(timeout time.Duration) ([]*speaker, error) {
	conn, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	msg := strings.Join([]string{"M-SEARCH * HTTP/1.1", "HOST: 239.255.255.250:1900", "MAN: \"ssdp:discover\"", "MX: 1", "ST: " + sonosDeviceType, "", ""}, "\r\n")
	dst, err := net.ResolveUDPAddr("udp4", ssdpAddr)
	if err != nil {
		return nil, err
	}
	if _, err := conn.WriteTo([]byte(msg), dst); err != nil {
		return nil, err
	}
	_ = conn.SetReadDeadline(time.Now().Add(timeout))

	client := &http.Client{Timeout: deviceReqTimeout}
	seen := map[string]struct{}{}
	var speakers []*speaker
	buf := make([]byte, 65535)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			if nErr, ok := err.(net.Error); ok && nErr.Timeout() {
				break
			}
			return nil, err
		}
		location := extractLocation(string(buf[:n]))
		if location == "" {
			continue
		}
		if _, ok := seen[location]; ok {
			continue
		}
		seen[location] = struct{}{}
		sp, err := speakerFromDescription(client, location)
		if err != nil {
			continue
		}
		speakers = append(speakers, sp)
	}
	return speakers, nil
}

func extractLocation(resp string) string {
	for _, line := range strings.Split(resp, "\r\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 && strings.EqualFold(strings.TrimSpace(parts[0]), "location") {
			return strings.TrimSpace(parts[1])
		}
	}
	return ""
}

func speakerFromDescription(client *http.Client, location string) (*speaker, error) {
	resp, err := client.Get(location)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("http status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}
	var desc deviceDescription
	if err := xml.Unmarshal(body, &desc); err != nil {
		return nil, err
	}
	baseURL := strings.TrimSpace(desc.URLBase)
	if baseURL == "" {
		u, err := url.Parse(location)
		if err != nil {
			return nil, err
		}
		baseURL = fmt.Sprintf("%s://%s", u.Scheme, u.Host)
	}

	var renderingURL, avTransportURL string
	for _, svc := range desc.Device.ServiceList.Services {
		switch svc.ServiceType {
		case renderingControlService:
			renderingURL = resolveURL(baseURL, svc.ControlURL)
		case avTransportService:
			avTransportURL = resolveURL(baseURL, svc.ControlURL)
		}
	}
	if renderingURL == "" || avTransportURL == "" {
		return nil, errors.New("required services missing")
	}
	host := ""
	if u, err := url.Parse(baseURL); err == nil {
		host = u.Hostname()
	}
	return &speaker{
		UDN:            desc.Device.UDN,
		Name:           desc.Device.FriendlyName,
		Model:          desc.Device.ModelName,
		Version:        desc.Device.Version,
		ModelNumber:    desc.Device.ModelNumber,
		Host:           host,
		RenderingURL:   renderingURL,
		AVTransportURL: avTransportURL,
		StatusURL:      fmt.Sprintf("%s/status/zp", strings.TrimRight(baseURL, "/")),
	}, nil
}

func resolveURL(baseURL, p string) string {
	base, err := url.Parse(baseURL)
	if err != nil {
		return p
	}
	rel, err := url.Parse(p)
	if err != nil {
		return p
	}
	return base.ResolveReference(rel).String()
}

func (e *sonosExporter) getVolume(ctx context.Context, sp *speaker) (float64, error) {
	_, span := e.tracer.Start(ctx, "speaker.get_volume")
	defer span.End()
	resp, err := e.soapCall(ctx, sp.RenderingURL, renderingControlService, "GetVolume", map[string]string{"InstanceID": "0", "Channel": "Master"}, soapTimeout)
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(parseXMLTag(resp, "CurrentVolume"), 64)
}

func (e *sonosExporter) getPlaying(ctx context.Context, sp *speaker) (bool, error) {
	_, span := e.tracer.Start(ctx, "speaker.get_playing")
	defer span.End()
	resp, err := e.soapCall(ctx, sp.AVTransportURL, avTransportService, "GetTransportInfo", map[string]string{"InstanceID": "0"}, soapTimeout)
	if err != nil {
		return false, err
	}
	state := strings.ToUpper(parseXMLTag(resp, "CurrentTransportState"))
	return state == "PLAYING" || state == "TRANSITIONING", nil
}

func (e *sonosExporter) getUptime(ctx context.Context, sp *speaker) (float64, string) {
	_, span := e.tracer.Start(ctx, "speaker.get_uptime")
	defer span.End()
	ctx, cancel := context.WithTimeout(ctx, soapTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, sp.StatusURL, nil)
	resp, err := e.client.Do(req)
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode < 300 {
			if body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes)); err == nil {
				var s zonePlayerStatus
				if xml.Unmarshal(body, &s) == nil {
					if d := parseDurationString(s.Uptime); d > 0 {
						return d, "device"
					}
				}
			}
		}
	}
	return time.Since(sp.FirstSeen).Seconds(), "observed"
}

func (e *sonosExporter) soapCall(parent context.Context, controlURL, serviceURN, action string, args map[string]string, timeout time.Duration) (string, error) {
	var body bytes.Buffer
	body.WriteString(`<?xml version="1.0" encoding="utf-8"?>`)
	body.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	body.WriteString(`<u:` + action + ` xmlns:u="` + serviceURN + `">`)
	for k, v := range args {
		body.WriteString(`<` + k + `>` + xmlEscape(v) + `</` + k + `>`)
	}
	body.WriteString(`</u:` + action + `></s:Body></s:Envelope>`)

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, controlURL, bytes.NewReader(body.Bytes()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPACTION", fmt.Sprintf(`"%s#%s"`, serviceURN, action))
	resp, err := e.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("soap %s status %s", action, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func parseXMLTag(raw, tag string) string {
	open, close := "<"+tag+">", "</"+tag+">"
	start := strings.Index(raw, open)
	if start == -1 {
		return ""
	}
	start += len(open)
	end := strings.Index(raw[start:], close)
	if end == -1 {
		return ""
	}
	return strings.TrimSpace(raw[start : start+end])
}

func parseDurationString(v string) float64 {
	parts := strings.Split(strings.TrimSpace(v), ":")
	if len(parts) != 3 {
		return -1
	}
	h, e1 := strconv.Atoi(parts[0])
	m, e2 := strconv.Atoi(parts[1])
	s, e3 := strconv.Atoi(parts[2])
	if e1 != nil || e2 != nil || e3 != nil {
		return -1
	}
	return float64(h*3600 + m*60 + s)
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}

func main() {
	listenAddr := flag.String("web.listen-address", ":9798", "Address to listen on for HTTP requests")
	metricsPath := flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics")
	discoveryInterval := flag.Duration("sonos.discovery-interval", defaultDiscoveryInterval, "How often to rediscover speakers")
	discoveryTimeout := flag.Duration("sonos.discovery-timeout", defaultDiscoveryTimeout, "How long SSDP discovery waits for responses")
	speakerStaleAfter := flag.Duration("sonos.speaker-stale-after", defaultSpeakerStaleAfter, "How long to keep a speaker in cache without rediscovery (0 disables eviction)")
	otelEndpoint := flag.String("otel.exporter.otlp.endpoint", "", "OTLP endpoint for OpenTelemetry logs and traces (e.g. localhost:4317)")
	otelInsecure := flag.Bool("otel.exporter.otlp.insecure", true, "Use insecure OTLP gRPC connection")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	logger := fallbackLogger()
	if *otelEndpoint != "" {
		shutdownTelemetry, otelLogger, err := initTelemetry(ctx, "sonos-exporter", *otelEndpoint, *otelInsecure)
		if err != nil {
			logger.Error("failed to initialize OpenTelemetry, using fallback logger", "error", err)
		} else {
			logger = otelLogger
			defer func() {
				shutdownCtx, shutdownCancel := withTimeoutContext(context.Background())
				defer shutdownCancel()
				if err := shutdownTelemetry(shutdownCtx); err != nil {
					logger.Error("failed to shutdown telemetry", "error", err)
				}
			}()
		}
	}

	exporter := newSonosExporterWithOptions(logger, *speakerStaleAfter)
	go exporter.startDiscovery(ctx, *discoveryInterval, *discoveryTimeout)

	registry := prometheus.NewRegistry()
	registry.MustRegister(exporter)
	metricsHandler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})

	http.Handle(*metricsPath, metricsHandler)
	http.HandleFunc("/debug/speakers", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(exporter.getSpeakers()); err != nil {
			logger.Error("failed to encode speakers", "error", err)
		}
	})
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "<html><head><title>Sonos Exporter</title></head><body><h1>Sonos Exporter</h1><p><a href='%s'>Metrics</a></p></body></html>", *metricsPath)
	})

	srv := &http.Server{Addr: *listenAddr}
	go func() {
		<-ctx.Done()
		logger.Info("shutting down http server")
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("http server shutdown error", "error", err)
		}
	}()

	logger.Info("starting sonos_exporter", "listen_address", *listenAddr, "metrics_path", *metricsPath)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("http server exited", "error", err)
	}
}
