package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testSite(t *testing.T) (*staticSite, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "assets"), 0755); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{
		"index.html":       strings.Repeat("<p>hello</p>", 100),
		"assets/style.css": strings.Repeat("body{color:green}", 100),
		"poeple.log":       "private data",
	} {
		if err := os.WriteFile(filepath.Join(root, path), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	var log bytes.Buffer
	s, err := newStaticSite(root, &visitLog{w: &log})
	if err != nil {
		t.Fatal(err)
	}
	return s, &log
}

func request(s http.Handler, method, path, remote string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://shadowblade.space"+path, nil)
	r.RemoteAddr = remote
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestVisitLogUsesConnectionIPAndOnlyPageGETs(t *testing.T) {
	s, log := testSite(t)
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/", 200}, {"GET", "/assets/style.css", 200},
		{"HEAD", "/", 200}, {"GET", "/index.html", 301},
		{"GET", "/missing", 404}, {"GET", "/poeple.log", 404},
		{"GET", "/assets/", 404}, {"POST", "/", 405},
	} {
		w := request(s, tc.method, tc.path, "192.0.2.5:1234", map[string]string{
			"X-Forwarded-For": "203.0.113.8", "X-Real-IP": "203.0.113.9",
		})
		if w.Code != tc.status {
			t.Fatalf("%s %s: status %d, want %d", tc.method, tc.path, w.Code, tc.status)
		}
	}
	fields := strings.Fields(log.String())
	if len(fields) != 2 || fields[0] != "192.0.2.5" || !strings.HasSuffix(fields[1], "Z") {
		t.Fatalf("unexpected visitor log: %q", log.String())
	}
	if _, err := time.Parse(time.RFC3339Nano, fields[1]); err != nil {
		t.Fatal(err)
	}
}

func TestGzipAndConditionalVisits(t *testing.T) {
	s, log := testSite(t)
	w := request(s, "GET", "/", "[2001:db8::1234]:1234", map[string]string{"Accept-Encoding": "gzip"})
	if w.Code != 200 || w.Header().Get("Content-Encoding") != "gzip" || w.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("unexpected compressed response: %d %v", w.Code, w.Header())
	}
	gz, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(gz)
	if err != nil || !bytes.Equal(data, s.assets["/"].data) {
		t.Fatalf("gzip body mismatch: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	w = request(s, "GET", "/", "[2001:db8::1234]:1234", map[string]string{
		"Accept-Encoding": "gzip", "If-None-Match": w.Header().Get("ETag"),
	})
	if w.Code != 304 || w.Body.Len() != 0 {
		t.Fatalf("conditional GET returned %d", w.Code)
	}
	lines := strings.Split(strings.TrimSpace(log.String()), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "2001:db8::1234 ") {
		t.Fatalf("expected two IPv6 visits, got %q", log.String())
	}
	for _, encoding := range []string{"gzip;q=0", "gzip;q=0, *;q=1", "gzip;q=garbage", "br"} {
		w = request(s, "GET", "/assets/style.css", "192.0.2.6:1234", map[string]string{"Accept-Encoding": encoding})
		if w.Header().Get("Content-Encoding") != "" || !bytes.Equal(w.Body.Bytes(), s.assets["/assets/style.css"].data) {
			t.Fatalf("unwanted compression for %q", encoding)
		}
	}
}

func TestHTTP3UnknownBodyLengthAndDeclaredBodies(t *testing.T) {
	for _, method := range []string{"GET", "HEAD"} {
		s, log := testSite(t)
		r := httptest.NewRequest(method, "https://shadowblade.space/", nil)
		r.ProtoMajor, r.ProtoMinor, r.Proto = 3, 0, "HTTP/3.0"
		r.ContentLength = -1
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("empty HTTP/3 %s rejected: %d", method, w.Code)
		}
		if (log.Len() > 0) != (method == "GET") {
			t.Fatalf("incorrect HTTP/3 visit count for %s", method)
		}
	}
	for _, chunked := range []bool{false, true} {
		s, log := testSite(t)
		r := httptest.NewRequest("GET", "https://shadowblade.space/", strings.NewReader("unwanted"))
		if chunked {
			r.ContentLength = -1
			r.TransferEncoding = []string{"chunked"}
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 413 || w.Header().Get("Connection") != "close" || log.Len() != 0 {
			t.Fatalf("request body was accepted or logged: %d", w.Code)
		}
	}
}

func TestRateLimitedVisitsAreNotLogged(t *testing.T) {
	s, log := testSite(t)
	for range pageRequestBurst {
		w := request(s, "GET", "/", "192.0.2.8:1234", nil)
		if w.Code != 200 {
			t.Fatalf("initial page request rejected: %d", w.Code)
		}
	}
	w := request(s, "GET", "/", "192.0.2.8:1234", map[string]string{"X-Forwarded-For": "198.51.100.8"})
	if w.Code != 429 || w.Header().Get("Retry-After") == "" {
		t.Fatalf("missing rate limit: %d", w.Code)
	}
	if strings.Count(log.String(), "\n") != pageRequestBurst {
		t.Fatal("rejected request was logged")
	}
}

func TestLimiterRefillIsolationAndIPv6Grouping(t *testing.T) {
	l := newVisitorLimiter()
	now := time.Now()
	ip := netip.MustParseAddr("2001:db8::1")
	for range pageRequestBurst {
		if !l.allow(ip, true, now) {
			t.Fatal("burst rejected")
		}
	}
	if l.allow(netip.MustParseAddr("2001:db8::2"), true, now) {
		t.Fatal("IPv6 address rotation bypassed /64 limit")
	}
	if !l.allow(netip.MustParseAddr("2001:db8:1::1"), true, now) {
		t.Fatal("independent IPv6 network was rejected")
	}
	if !l.allow(ip, true, now.Add(time.Second)) {
		t.Fatal("page token did not refill")
	}
	if !l.allow(ip, false, now.Add(time.Second)) {
		t.Fatal("page throttling prevented loading assets")
	}
}

func TestLimiterBoundsMemoryAndExpiresEntries(t *testing.T) {
	l := newVisitorLimiter()
	now := time.Now()
	for i := range maxVisitorBuckets {
		ip := netip.AddrFrom4([4]byte{10, byte(i >> 16), byte(i >> 8), byte(i)})
		l.allow(ip, false, now)
	}
	newIP := netip.MustParseAddr("192.0.2.55")
	if l.allow(newIP, false, now) || len(l.visitors) != maxVisitorBuckets {
		t.Fatal("visitor map exceeded its bound")
	}
	if !l.allow(newIP, false, now.Add(visitorBucketTTL+time.Minute)) || len(l.visitors) != 1 {
		t.Fatal("expired visitor entries were not reclaimed")
	}
}

func TestGlobalLimitAndRejectedIPIsolation(t *testing.T) {
	l := newVisitorLimiter()
	now := time.Now()
	noisy := netip.MustParseAddr("192.0.2.99")
	for range siteRequestBurst + 1000 {
		l.allow(noisy, false, now)
	}
	accepted := 0
	for i := range siteGlobalBurst {
		ip := netip.AddrFrom4([4]byte{10, 1, byte(i >> 8), byte(i)})
		if l.allow(ip, false, now) {
			accepted++
		}
	}
	if accepted != siteGlobalBurst-siteRequestBurst {
		t.Fatalf("global quota or isolation failed: accepted %d", accepted)
	}
}

func TestConcurrentLogLinesAndAppendPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "poeple.log")
	for run := range 2 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		l := &visitLog{w: f}
		var wg sync.WaitGroup
		for i := range 100 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				ip := netip.MustParseAddr(fmt.Sprintf("192.0.2.%d", i+1))
				if err := l.record(ip, time.Now()); err != nil {
					t.Error(err)
				}
			}(i)
		}
		wg.Wait()
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil || strings.Count(string(data), "\n") != (run+1)*100 {
			t.Fatalf("lost or overwritten log records: %v", err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				t.Fatalf("interleaved line: %q", line)
			}
			if _, err := netip.ParseAddr(fields[0]); err != nil {
				t.Fatal(err)
			}
			if _, err := time.Parse(time.RFC3339Nano, fields[1]); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestProxyRoutingBypassesStaticLoggingAndThrottling(t *testing.T) {
	s, log := testSite(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Forwarded-Proto") != "https" {
			t.Error("missing forwarded HTTPS scheme")
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer backend.Close()
	proxies, err := buildProxies("n8n.shadowblade.space=" + backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	rt := &hostRouter{static: s, proxies: proxies, staticHosts: map[string]bool{"shadowblade.space": true}}
	for range siteRequestBurst + 1 {
		r := httptest.NewRequest("POST", "https://n8n.shadowblade.space/webhook", strings.NewReader("payload"))
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, r)
		if w.Code != 202 {
			t.Fatalf("proxied request was affected by static limits: %d", w.Code)
		}
	}
	r := httptest.NewRequest("GET", "https://unknown.example/", nil)
	w := httptest.NewRecorder()
	rt.ServeHTTP(w, r)
	if w.Code != 421 || log.Len() != 0 {
		t.Fatalf("unknown host or proxy was served/logged as a static visit: %d %q", w.Code, log.String())
	}
}
