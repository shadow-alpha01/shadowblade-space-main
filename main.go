package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/crypto/acme/autocert"
	"golang.org/x/time/rate"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	addr := env("ADDR", ":443")
	webroot := env("WEBROOT", "/srv/www")
	// ACME_DOMAIN takes one hostname or a comma-separated list. Only this server
	// owns :80/:443, so only it can answer an ACME challenge — every hostname
	// served from this machine has to be listed here. Hosts that are not routed
	// by PROXY_MAP simply get the static site.
	domains := splitHosts(env("ACME_DOMAIN", ""))
	if len(domains) == 0 {
		log.Fatal("ACME_DOMAIN is required")
	}

	// PROXY_MAP routes whole hostnames to another backend instead of the static
	// site: "host=upstream,host=upstream". Used to put the n8n stack on standard
	// :443, so it works from networks that only allow 80/443.
	proxies, err := buildProxies(env("PROXY_MAP", ""))
	if err != nil {
		log.Fatalf("PROXY_MAP: %v", err)
	}
	for host := range proxies {
		log.Printf("routing %s to its own backend", host)
	}

	visitFile, err := os.OpenFile(env("VISIT_LOG", "/var/log/site/poeple.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		log.Fatalf("visitor log: %v", err)
	}
	defer visitFile.Close()
	static, err := newStaticSite(webroot, &visitLog{w: visitFile})
	if err != nil {
		log.Fatalf("static site: %v", err)
	}
	staticHosts := make(map[string]bool)
	for _, domain := range domains {
		if _, proxied := proxies[hostOf(domain)]; !proxied {
			staticHosts[hostOf(domain)] = true
		}
	}
	router := &hostRouter{static: static, proxies: proxies, staticHosts: staticHosts}

	manager := &autocert.Manager{
		Prompt:     autocert.AcceptTOS,
		HostPolicy: autocert.HostWhitelist(domains...),
		Cache:      autocert.DirCache(env("ACME_CACHE_DIR", "/acme")),
		Email:      env("ACME_EMAIL", ""),
	}

	httpServer := &http.Server{
		Addr:              env("ACME_HTTP_ADDR", ":80"),
		Handler:           manager.HTTPHandler(nil),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	tlsConfH3 := &tls.Config{
		MinVersion:     tls.VersionTLS13,
		NextProtos:     []string{"h3"},
		GetCertificate: manager.GetCertificate,
	}
	tlsConfTCP := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		NextProtos:     []string{"h2", "http/1.1", "acme-tls/1"},
		GetCertificate: manager.GetCertificate,
	}

	h3 := &http3.Server{
		Addr:           addr,
		Handler:        router,
		TLSConfig:      tlsConfH3,
		MaxHeaderBytes: 32 << 10,
		IdleTimeout:    60 * time.Second,
		QUICConfig: &quic.Config{
			Versions:              []quic.Version{quic.Version2, quic.Version1},
			HandshakeIdleTimeout:  5 * time.Second,
			MaxIdleTimeout:        60 * time.Second,
			MaxIncomingStreams:    64,
			MaxIncomingUniStreams: 16,
		},
	}

	altSvc := fmt.Sprintf(`h3=":%s"; ma=86400`, env("ALT_SVC_PORT", portOf(addr)))
	tcpHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, proxied := router.backend(r)
		if !proxied {
			// Only advertise HTTP/3 for the static site. A proxied backend may need
			// WebSocket upgrades, which this h3 server cannot carry — steering the
			// browser to QUIC would break them.
			w.Header().Set("Alt-Svc", altSvc)
		}
		w.Header().Set("X-Powered-By", "shadowblade/quic")
		h.ServeHTTP(w, r)
	})
	tcpServer := &http.Server{
		Addr:              addr,
		Handler:           tcpHandler,
		TLSConfig:         tlsConfTCP,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}

	udpConn, err := net.ListenPacket("udp", addr)
	if err != nil {
		log.Fatalf("quic socket: %v", err)
	}
	defer udpConn.Close()
	// Above this handshake rate, require a Retry token before allocating a
	// connection. This validates the source address of a potential UDP flood.
	handshakes := rate.NewLimiter(20, 40)
	transport := &quic.Transport{
		Conn:                udpConn,
		VerifySourceAddress: func(net.Addr) bool { return !handshakes.Allow() },
	}
	defer transport.Close()
	listener, err := transport.ListenEarly(http3.ConfigureTLSConfig(tlsConfH3), h3.QUICConfig)
	if err != nil {
		log.Fatalf("quic listener: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 3)
	go func() { serveErr <- httpServer.ListenAndServe() }()
	go func() { serveErr <- h3.ServeListener(listener) }()
	go func() { serveErr <- tcpServer.ListenAndServeTLS("", "") }()
	log.Printf("serving HTTP/3 with QUIC v2/v1; visitor log: %s", visitFile.Name())
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server: %v", err)
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var shutdown sync.WaitGroup
	for _, closeServer := range []func(context.Context) error{httpServer.Shutdown, tcpServer.Shutdown, h3.Shutdown} {
		shutdown.Add(1)
		go func(closeServer func(context.Context) error) {
			defer shutdown.Done()
			if err := closeServer(shutdownCtx); err != nil {
				log.Printf("shutdown: %v", err)
			}
		}(closeServer)
	}
	shutdown.Wait()
}

// hostRouter serves configured hostnames and rejects unknown hosts.
type hostRouter struct {
	static      http.Handler
	proxies     map[string]http.Handler
	staticHosts map[string]bool
}

// backend also reports whether the request is proxied, which the TCP handler
// needs in order to decide about the Alt-Svc header.
func (rt *hostRouter) backend(r *http.Request) (http.Handler, bool) {
	if p, ok := rt.proxies[hostOf(r.Host)]; ok {
		return p, true
	}
	if !rt.staticHosts[hostOf(r.Host)] {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "unknown host", http.StatusMisdirectedRequest)
		}), false
	}
	return rt.static, false
}

func (rt *hostRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h, _ := rt.backend(r)
	h.ServeHTTP(w, r)
}

func hostOf(h string) string {
	if name, _, err := net.SplitHostPort(h); err == nil {
		h = name
	}
	return strings.ToLower(h)
}

// buildProxies parses "host=upstream,host=upstream" into reverse proxies.
func buildProxies(spec string) (map[string]http.Handler, error) {
	out := make(map[string]http.Handler)
	for _, entry := range strings.Split(spec, ",") {
		if entry = strings.TrimSpace(entry); entry == "" {
			continue
		}
		host, upstream, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("bad entry %q, want host=upstream", entry)
		}
		host = hostOf(strings.TrimSpace(host))
		u, err := url.Parse(strings.TrimSpace(upstream))
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("bad upstream %q for %s", upstream, host)
		}

		rp := httputil.NewSingleHostReverseProxy(u)
		inner := rp.Director
		rp.Director = func(r *http.Request) {
			inner(r)
			// The hop to the upstream is plain HTTP on an internal network. Tell it
			// the client arrived over TLS so it builds https:// URLs and is willing
			// to set secure cookies. (Director leaves r.Host alone, so the upstream
			// still sees the original hostname.)
			r.Header.Set("X-Forwarded-Proto", "https")
		}
		rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("proxy %s -> %s: %v", host, u, err)
			http.Error(w, "backend unavailable", http.StatusBadGateway)
		}
		out[host] = rp
	}
	return out, nil
}

func splitHosts(v string) []string {
	var hosts []string
	for _, h := range strings.Split(v, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

func portOf(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i+1:]
		}
	}
	return addr
}
