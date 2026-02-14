package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ssdpAddr          = "239.255.255.250:1900"
	sonosDeviceType   = "urn:schemas-upnp-org:device:ZonePlayer:1"
	discoveryInterval = 60 * time.Second
	discoveryTimeout  = 3 * time.Second
	deviceReqTimeout  = 4 * time.Second
	soapTimeout       = 3 * time.Second
)

type sonosExporter struct {
	client     *http.Client
	speakersMu sync.RWMutex
	speakers   map[string]*speaker
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
	FirstSeen      time.Time
	LastSeen       time.Time
}

type deviceDescription struct {
	Device struct {
		FriendlyName string `xml:"friendlyName"`
		ModelName    string `xml:"modelName"`
		ModelNumber  string `xml:"modelNumber"`
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

func newSonosExporter() *sonosExporter {
	return &sonosExporter{
		client:   &http.Client{Timeout: deviceReqTimeout},
		speakers: make(map[string]*speaker),
	}
}

func (e *sonosExporter) startDiscovery(ctx context.Context) {
	ticker := time.NewTicker(discoveryInterval)
	defer ticker.Stop()
	e.refreshSpeakers()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.refreshSpeakers()
		}
	}
}

func (e *sonosExporter) refreshSpeakers() {
	discovered, err := discoverSonos(discoveryTimeout)
	if err != nil {
		log.Printf("discovery error: %v", err)
		return
	}
	now := time.Now()
	e.speakersMu.Lock()
	defer e.speakersMu.Unlock()
	for _, sp := range discovered {
		if existing, ok := e.speakers[sp.UDN]; ok {
			existing.Name = sp.Name
			existing.Model = sp.Model
			existing.Version = sp.Version
			existing.ModelNumber = sp.ModelNumber
			existing.Host = sp.Host
			existing.RenderingURL = sp.RenderingURL
			existing.AVTransportURL = sp.AVTransportURL
			existing.LastSeen = now
			continue
		}
		sp.FirstSeen = now
		sp.LastSeen = now
		e.speakers[sp.UDN] = sp
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

func (e *sonosExporter) metricsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder
	b.WriteString("# HELP sonos_speaker_up Whether the Sonos speaker is reachable during scrape (1=up, 0=down).\n")
	b.WriteString("# TYPE sonos_speaker_up gauge\n")
	b.WriteString("# HELP sonos_speaker_volume_percent Current Sonos volume percentage.\n")
	b.WriteString("# TYPE sonos_speaker_volume_percent gauge\n")
	b.WriteString("# HELP sonos_speaker_is_playing Whether Sonos reports currently playing (1=playing, 0=not).\n")
	b.WriteString("# TYPE sonos_speaker_is_playing gauge\n")
	b.WriteString("# HELP sonos_speaker_uptime_seconds Speaker uptime in seconds if known; otherwise exporter-observed uptime.\n")
	b.WriteString("# TYPE sonos_speaker_uptime_seconds gauge\n")
	b.WriteString("# HELP sonos_speaker_info Static Sonos speaker info metric with software/model labels.\n")
	b.WriteString("# TYPE sonos_speaker_info gauge\n")

	for _, sp := range e.getSpeakers() {
		labels := map[string]string{"uuid": sp.UDN, "name": sp.Name, "model": sp.Model, "host": sp.Host}
		volume, errVol := e.getVolume(sp)
		playing, errPlay := e.getPlaying(sp)
		uptime, source := e.getUptime(sp)

		up := 1.0
		if errVol != nil && errPlay != nil {
			up = 0
		}
		b.WriteString(renderMetric("sonos_speaker_up", labels, up))
		infoLabels := cloneLabels(labels)
		infoLabels["version"] = sp.Version
		infoLabels["model_number"] = sp.ModelNumber
		b.WriteString(renderMetric("sonos_speaker_info", infoLabels, 1))

		if errVol == nil {
			b.WriteString(renderMetric("sonos_speaker_volume_percent", labels, volume))
		}
		if errPlay == nil {
			playVal := 0.0
			if playing {
				playVal = 1
			}
			b.WriteString(renderMetric("sonos_speaker_is_playing", labels, playVal))
		}
		uptimeLabels := cloneLabels(labels)
		uptimeLabels["source"] = source
		b.WriteString(renderMetric("sonos_speaker_uptime_seconds", uptimeLabels, uptime))
	}

	_, _ = io.WriteString(w, b.String())
}

func renderMetric(name string, labels map[string]string, value float64) string {
	var parts []string
	for k, v := range labels {
		parts = append(parts, fmt.Sprintf(`%s="%s"`, k, escapeLabel(v)))
	}
	return fmt.Sprintf("%s{%s} %g\n", name, strings.Join(parts, ","), value)
}

func cloneLabels(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+2)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func escapeLabel(v string) string {
	return strings.NewReplacer("\\", "\\\\", "\n", "\\n", `"`, `\"`).Replace(v)
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
	for {
		buf := make([]byte, 65535)
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
	body, err := io.ReadAll(resp.Body)
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
		case "urn:schemas-upnp-org:service:RenderingControl:1":
			renderingURL = resolveURL(baseURL, svc.ControlURL)
		case "urn:schemas-upnp-org:service:AVTransport:1":
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
	return &speaker{UDN: desc.Device.UDN, Name: desc.Device.FriendlyName, Model: desc.Device.ModelName, Version: desc.Device.ModelNumber, ModelNumber: desc.Device.ModelNumber, Host: host, RenderingURL: renderingURL, AVTransportURL: avTransportURL}, nil
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

func (e *sonosExporter) getVolume(sp *speaker) (float64, error) {
	resp, err := soapCall(sp.RenderingURL, "urn:schemas-upnp-org:service:RenderingControl:1", "GetVolume", map[string]string{"InstanceID": "0", "Channel": "Master"}, soapTimeout)
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(parseXMLTag(resp, "CurrentVolume"), 64)
}

func (e *sonosExporter) getPlaying(sp *speaker) (bool, error) {
	resp, err := soapCall(sp.AVTransportURL, "urn:schemas-upnp-org:service:AVTransport:1", "GetTransportInfo", map[string]string{"InstanceID": "0"}, soapTimeout)
	if err != nil {
		return false, err
	}
	state := strings.ToUpper(parseXMLTag(resp, "CurrentTransportState"))
	return state == "PLAYING" || state == "TRANSITIONING", nil
}

func (e *sonosExporter) getUptime(sp *speaker) (float64, string) {
	ctx, cancel := context.WithTimeout(context.Background(), soapTimeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:1400/status/zp", sp.Host), nil)
	resp, err := e.client.Do(req)
	if err == nil {
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode < 300 {
			if body, err := io.ReadAll(resp.Body); err == nil {
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

func soapCall(controlURL, serviceURN, action string, args map[string]string, timeout time.Duration) (string, error) {
	var body bytes.Buffer
	body.WriteString(`<?xml version="1.0" encoding="utf-8"?>`)
	body.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	body.WriteString(`<u:` + action + ` xmlns:u="` + serviceURN + `">`)
	for k, v := range args {
		body.WriteString(`<` + k + `>` + xmlEscape(v) + `</` + k + `>`)
	}
	body.WriteString(`</u:` + action + `></s:Body></s:Envelope>`)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, controlURL, bytes.NewReader(body.Bytes()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPACTION", fmt.Sprintf(`"%s#%s"`, serviceURN, action))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("soap %s status %s", action, resp.Status)
	}
	b, err := io.ReadAll(resp.Body)
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
	flag.Parse()

	exporter := newSonosExporter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go exporter.startDiscovery(ctx)

	http.HandleFunc(*metricsPath, exporter.metricsHandler)
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "<html><head><title>Sonos Exporter</title></head><body><h1>Sonos Exporter</h1><p><a href='%s'>Metrics</a></p></body></html>", *metricsPath)
	})
	log.Printf("starting sonos_exporter on %s", *listenAddr)
	log.Fatal(http.ListenAndServe(*listenAddr, nil))
}
