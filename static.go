package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limits apply only to the static site, across HTTP/1.1, HTTP/2 and HTTP/3.
// IPv6 addresses share a /64 bucket, while the log retains the actual address.
const (
	siteRequestsPerSecond = 5
	siteRequestBurst      = 30
	pageRequestsPerSecond = 1
	pageRequestBurst      = 5
	siteGlobalRate        = 200
	siteGlobalBurst       = 400
	maxVisitorBuckets     = 10000
	visitorBucketTTL      = 5 * time.Minute
)

type visitorBucket struct {
	requests *rate.Limiter
	pages    *rate.Limiter
	lastSeen time.Time
}

type visitorLimiter struct {
	mu        sync.Mutex
	visitors  map[netip.Prefix]*visitorBucket
	global    *rate.Limiter
	lastSweep time.Time
}

func newVisitorLimiter() *visitorLimiter {
	return &visitorLimiter{
		visitors: make(map[netip.Prefix]*visitorBucket),
		global:   rate.NewLimiter(siteGlobalRate, siteGlobalBurst),
	}
}

func (l *visitorLimiter) allow(ip netip.Addr, page bool, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastSweep) >= time.Minute {
		for key, v := range l.visitors {
			if now.Sub(v.lastSeen) > visitorBucketTTL {
				delete(l.visitors, key)
			}
		}
		l.lastSweep = now
	}
	bits := 32
	if ip.Is6() {
		bits = 64
	}
	key := netip.PrefixFrom(ip, bits).Masked()
	v := l.visitors[key]
	if v == nil {
		if len(l.visitors) >= maxVisitorBuckets {
			return false
		}
		v = &visitorBucket{
			requests: rate.NewLimiter(siteRequestsPerSecond, siteRequestBurst),
			pages:    rate.NewLimiter(pageRequestsPerSecond, pageRequestBurst),
		}
		l.visitors[key] = v
	}
	v.lastSeen = now
	// Rejected requests from one IP must not consume the global budget.
	if !v.requests.AllowN(now, 1) || (page && !v.pages.AllowN(now, 1)) {
		return false
	}
	return l.global.AllowN(now, 1)
}

func clientIP(remoteAddr string) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return netip.Addr{}, err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, err
	}
	return ip.Unmap().WithZone(""), nil
}

type visitLog struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *visitLog) record(ip netip.Addr, now time.Time) error {
	line := ip.String() + " " + now.UTC().Format(time.RFC3339Nano) + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	_, err := io.WriteString(l.w, line)
	return err
}

type staticAsset struct {
	data, compressed []byte
	etag, gzipETag   string
	contentType      string
	modified         time.Time
}

type staticSite struct {
	assets  map[string]*staticAsset
	visits  *visitLog
	limiter *visitorLimiter
}

func newStaticSite(root string, visits *visitLog) (*staticSite, error) {
	s := &staticSite{assets: make(map[string]*staticAsset), visits: visits, limiter: newVisitorLimiter()}
	load := func(name, urlPath string) error {
		full := filepath.Join(root, name)
		info, err := os.Lstat(full)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("static file is not a regular file: %s", name)
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		a := &staticAsset{data: data, modified: info.ModTime(), contentType: mime.TypeByExtension(filepath.Ext(name))}
		if a.contentType == "" {
			a.contentType = http.DetectContentType(data)
		}
		a.etag = fmt.Sprintf(`"%x"`, sha256.Sum256(data))
		var buf bytes.Buffer
		gz, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if err != nil {
			return err
		}
		if _, err = gz.Write(data); err != nil {
			return err
		}
		if err = gz.Close(); err != nil {
			return err
		}
		if buf.Len() < len(data) {
			a.compressed = buf.Bytes()
			a.gzipETag = fmt.Sprintf(`"%x"`, sha256.Sum256(a.compressed))
		}
		s.assets[urlPath] = a
		return nil
	}
	if err := load("index.html", "/"); err != nil {
		return nil, err
	}
	err := fs.WalkDir(os.DirFS(root), "assets", func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		return load(name, "/"+name)
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

func acceptsGzip(value string) bool {
	wildcard := false
	for _, part := range strings.Split(value, ",") {
		fields := strings.Split(part, ";")
		encoding := strings.TrimSpace(strings.ToLower(fields[0]))
		q := 1.0
		for _, param := range fields[1:] {
			key, val, ok := strings.Cut(strings.TrimSpace(param), "=")
			if ok && strings.EqualFold(strings.TrimSpace(key), "q") {
				parsed, err := strconv.ParseFloat(strings.TrimSpace(val), 64)
				if err != nil || !(parsed >= 0 && parsed <= 1) {
					q = 0
				} else {
					q = parsed
				}
			}
		}
		if encoding == "gzip" {
			return q > 0
		}
		if encoding == "*" {
			wildcard = q > 0
		}
	}
	return wildcard
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (s *staticSite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	ip, err := clientIP(r.RemoteAddr)
	if err != nil {
		http.Error(w, "invalid client address", http.StatusBadRequest)
		return
	}
	page := r.URL.Path == "/"
	now := time.Now()
	if !s.limiter.allow(ip, page, now) {
		w.Header().Set("Retry-After", "1")
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	// HTTP/3 may use -1 for an empty body without a Content-Length header.
	// Reject declared bodies and HTTP/1 chunked uploads; unread HTTP/3 request
	// streams are cancelled by the HTTP/3 server when this handler returns.
	if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		if r.ProtoMajor == 1 {
			w.Header().Set("Connection", "close")
			_ = http.NewResponseController(w).SetReadDeadline(now.Add(5 * time.Second))
		}
		http.Error(w, "request body not allowed", http.StatusRequestEntityTooLarge)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/index.html" {
		target := "/"
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently)
		return
	}
	a := s.assets[r.URL.Path]
	if a == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", a.contentType)
	w.Header().Set("Vary", "Accept-Encoding")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if page {
		// Revalidation keeps the page current and counts visits that return 304.
		w.Header().Set("Cache-Control", "no-cache")
	}
	data, etag := a.data, a.etag
	if len(a.compressed) > 0 && r.Header.Get("Range") == "" && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		data, etag = a.compressed, a.gzipETag
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	}
	w.Header().Set("ETag", etag)
	written := &statusWriter{ResponseWriter: w}
	http.ServeContent(written, r, r.URL.Path, a.modified, bytes.NewReader(data))
	if page && r.Method == http.MethodGet && (written.status == 200 || written.status == 206 || written.status == 304) {
		if err := s.visits.record(ip, now); err != nil {
			log.Printf("visitor log write failed: %v", err)
		}
	}
}
