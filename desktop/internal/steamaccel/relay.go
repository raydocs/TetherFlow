package steamaccel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

type PathStatus struct {
	Requests, Active, Bytes, Errors uint64
	CooldownUntil                   time.Time
}

type Status struct{ Home, Phone PathStatus }

type path struct {
	weight    int
	transport http.RoundTripper
	status    PathStatus
}

// Relay is safe for concurrent use. Configuration and injected seams are immutable
// after New. Close cancels in-flight work and closes idle transport connections
// as well as all active phone sockets.
type Relay struct {
	cfg          Config
	mu           sync.Mutex
	paths        [2]path
	slots        chan struct{}
	dialSlots    chan struct{}
	phoneSockets phoneBudget
	ctx          context.Context
	cancel       context.CancelFunc
}

func New(config Config) (*Relay, error) {
	c, err := defaults(config)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Relay{cfg: c, slots: make(chan struct{}, c.MaxConcurrent), dialSlots: make(chan struct{}, c.MaxConcurrent), ctx: ctx, cancel: cancel}
	r.phoneSockets = phoneBudget{limit: c.MaxPhoneConnections, conns: make(map[*phoneConn]struct{})}
	homeDial := c.HomeDialContext
	if homeDial == nil {
		d := &net.Dialer{Timeout: c.DialTimeout, KeepAlive: 30 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(c.HomeIPv4)}}
		if err := configureHomeDialer(d, net.ParseIP(c.HomeIPv4)); err != nil {
			cancel()
			return nil, err
		}
		homeDial = d.DialContext
	}
	phoneDial := c.PhoneDialContext
	if phoneDial == nil {
		phoneDial = (&net.Dialer{Timeout: c.DialTimeout, KeepAlive: 30 * time.Second}).DialContext
	}
	home := c.HomeTransport
	if home == nil {
		home = r.transport(r.originDial(homeDial, false))
	}
	phone := c.PhoneTransport
	if phone == nil {
		t := r.transport(r.originDial(phoneDial, true))
		t.MaxIdleConns = c.MaxPhoneConnections
		t.MaxIdleConnsPerHost = c.MaxPhoneConnections
		phone = t
	}
	r.paths[0] = path{weight: c.HomeWeight, transport: home}
	r.paths[1] = path{weight: c.PhoneWeight, transport: phone}
	return r, nil
}

// Server supplies required listener-side resource bounds. A custom server must
// enforce equivalent bounds; ServeHTTP alone cannot bound HTTP parser allocation.
func (r *Relay) Server(addr string) *http.Server {
	return &http.Server{Addr: addr, Handler: r, ReadHeaderTimeout: r.cfg.HeaderTimeout,
		ReadTimeout: r.cfg.HeaderTimeout, WriteTimeout: r.cfg.RequestTimeout,
		IdleTimeout: r.cfg.IdleTimeout, MaxHeaderBytes: r.cfg.MaxHeaderBytes}
}

func (r *Relay) Snapshot() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return Status{Home: r.paths[0].status, Phone: r.paths[1].status}
}

func (r *Relay) Close() error {
	r.cancel()
	r.phoneSockets.close()
	for i := range r.paths {
		if c, ok := r.paths[i].transport.(interface{ CloseIdleConnections() }); ok {
			c.CloseIdleConnections()
		}
	}
	return nil
}

func (r *Relay) acquire(exclude int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	chosen := -1
	now := time.Now()
	for i := range r.paths {
		p := &r.paths[i]
		if i == exclude || now.Before(p.status.CooldownUntil) {
			continue
		}
		if chosen < 0 {
			chosen = i
			continue
		}
		q := &r.paths[chosen]
		a, b := p.status.Active*uint64(q.weight), q.status.Active*uint64(p.weight)
		if a < b || a == b && (p.status.Requests*uint64(q.weight) < q.status.Requests*uint64(p.weight) ||
			p.status.Requests*uint64(q.weight) == q.status.Requests*uint64(p.weight) && p.weight > q.weight) {
			chosen = i
		}
	}
	if chosen >= 0 {
		r.paths[chosen].status.Requests++
		r.paths[chosen].status.Active++
	}
	return chosen
}

func (r *Relay) finish(i int, failed bool, bytes uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := &r.paths[i]
	p.status.Active--
	p.status.Bytes += bytes
	if failed {
		p.status.Errors++
		p.status.CooldownUntil = time.Now().Add(r.cfg.FailureCooldown)
	}
	// A success already in flight must not erase a concurrent failure's cooldown.
}

func (r *Relay) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if !r.peerAllowed(req.RemoteAddr) {
		http.Error(w, "local peer required", http.StatusForbidden)
		return
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "GET/HEAD only", http.StatusMethodNotAllowed)
		return
	}
	if req.ContentLength != 0 || len(req.TransferEncoding) != 0 || len(req.Trailer) != 0 {
		http.Error(w, "request body forbidden", http.StatusBadRequest)
		return
	}
	if req.URL == nil || req.URL.User != nil || req.URL.Opaque != "" || req.URL.Fragment != "" ||
		req.URL.Scheme != "" && req.URL.Scheme != "http" || req.URL.Host != "" && req.URL.Host != req.Host {
		http.Error(w, "invalid HTTP target", http.StatusBadRequest)
		return
	}
	if headerSize(req.Header)+len(req.Host)+len(req.RequestURI) > r.cfg.MaxHeaderBytes {
		http.Error(w, "headers too large", http.StatusRequestHeaderFieldsTooLarge)
		return
	}
	host, port, err := r.authority(req.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	select {
	case <-r.ctx.Done():
		http.Error(w, "relay closed", http.StatusServiceUnavailable)
		return
	default:
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	default:
		http.Error(w, "relay busy", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), r.cfg.RequestTimeout)
	stop := context.AfterFunc(r.ctx, cancel)
	defer func() { stop(); cancel() }()
	lookupCtx, lookupCancel := context.WithTimeout(ctx, r.cfg.DialTimeout)
	ips, err := r.cfg.LookupIP(lookupCtx, host)
	lookupCancel()
	if err != nil {
		http.Error(w, "origin DNS unavailable", http.StatusBadGateway)
		return
	}
	if len(ips) == 0 || len(ips) > 64 {
		http.Error(w, "invalid origin DNS result", http.StatusForbidden)
		return
	}
	pinned := make([]net.IP, len(ips))
	for i, ip := range ips {
		if !publicIP(ip) {
			http.Error(w, "non-public origin forbidden", http.StatusForbidden)
			return
		}
		pinned[i] = append(net.IP(nil), ip...)
	}
	ctx = context.WithValue(ctx, targetKey{}, dialTarget{host: host, port: port, ips: pinned})
	out := req.Clone(ctx)
	scheme := "http"
	if port == "443" {
		scheme = "https"
	}
	out.URL = &url.URL{Scheme: scheme, Host: net.JoinHostPort(host, port), Path: req.URL.Path, RawPath: req.URL.RawPath, RawQuery: req.URL.RawQuery, ForceQuery: req.URL.ForceQuery}
	out.RequestURI = ""
	out.Header = cleanHeaders(req.Header)
	out.Close = false
	out.Body = nil
	out.GetBody = nil
	out.ContentLength = 0
	out.TransferEncoding = nil
	out.Trailer = nil
	// Never send client proxy credentials, even when supplied outside Connection.
	out.Header.Del("Proxy-Authorization")
	if out.URL.Path == "" {
		out.URL.Path = "/"
	}
	previous := -1
	for attempt := 0; attempt < 2; attempt++ {
		if ctx.Err() != nil {
			r.contextError(w, req, ctx)
			return
		}
		i := r.acquire(previous)
		if i < 0 {
			http.Error(w, "uplinks cooling down", http.StatusServiceUnavailable)
			return
		}
		resp, roundErr := r.paths[i].transport.RoundTrip(out.Clone(ctx))
		failed := roundErr != nil || resp == nil
		if resp != nil && (resp.StatusCode == 502 || resp.StatusCode == 503 || resp.StatusCode == 504 ||
			resp.StatusCode < 200 || resp.StatusCode > 599 || headerSize(resp.Header) > r.cfg.MaxHeaderBytes) {
			failed = true
		}
		if failed {
			if resp != nil && resp.Body != nil {
				resp.Body.Close()
			}
			localCapacity := errors.Is(roundErr, errDialBusy) || errors.Is(roundErr, errPhoneCapacity)
			r.finish(i, req.Context().Err() == nil && r.ctx.Err() == nil && !localCapacity, 0)
			if errors.Is(roundErr, errDialBusy) {
				http.Error(w, "relay dial capacity exhausted", http.StatusServiceUnavailable)
				return
			}
			if ctx.Err() != nil {
				r.contextError(w, req, ctx)
				return
			}
			previous = i
			if attempt == 0 {
				continue
			}
			if localCapacity {
				http.Error(w, "relay socket capacity exhausted", http.StatusServiceUnavailable)
				return
			}
			http.Error(w, "uplinks unavailable", http.StatusBadGateway)
			return
		}
		r.writeResponse(w, req, resp, i)
		return
	}
}

func (r *Relay) contextError(w http.ResponseWriter, req *http.Request, ctx context.Context) {
	if req.Context().Err() != nil {
		return
	}
	if r.ctx.Err() != nil {
		http.Error(w, "relay closed", http.StatusServiceUnavailable)
		return
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		http.Error(w, "relay request timeout", http.StatusGatewayTimeout)
	}
}

// Tracking distinguishes a downstream write failure from an upstream read failure.
type downstream struct {
	w   io.Writer
	err error
}

func (d *downstream) Write(p []byte) (int, error) {
	n, err := d.w.Write(p)
	if err != nil {
		d.err = err
	} else if n != len(p) {
		d.err = io.ErrShortWrite
	}
	return n, err
}

func (r *Relay) writeResponse(w http.ResponseWriter, req *http.Request, resp *http.Response, i int) {
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	for k, vs := range cleanHeaders(resp.Header) {
		w.Header()[k] = vs
	}
	w.WriteHeader(resp.StatusCode)
	var n int64
	var err error
	d := &downstream{w: w}
	if req.Method != http.MethodHead && resp.Body != nil {
		// Exactly one fixed-size buffer per admitted stream; no download buffering.
		n, err = io.CopyBuffer(d, struct{ io.Reader }{resp.Body}, make([]byte, 32<<10))
	}
	failed := err != nil && d.err == nil && req.Context().Err() == nil && r.ctx.Err() == nil
	r.finish(i, failed, uint64(n))
	if err != nil {
		// Do not let net/http terminate a truncated chunked body as if it were complete.
		// The response is already committed; this intentionally prevents failover/replay.
		panic(http.ErrAbortHandler)
	}
}

func (r *Relay) transport(dial DialContextFunc) *http.Transport {
	return &http.Transport{Proxy: nil, DialContext: dial, ForceAttemptHTTP2: false,
		MaxIdleConns: r.cfg.MaxConcurrent * 2, MaxIdleConnsPerHost: r.cfg.MaxConcurrent,
		MaxConnsPerHost: r.cfg.MaxConcurrent, IdleConnTimeout: r.cfg.IdleTimeout,
		ResponseHeaderTimeout: r.cfg.HeaderTimeout, TLSHandshakeTimeout: r.cfg.HeaderTimeout,
		ExpectContinueTimeout: time.Second, MaxResponseHeaderBytes: int64(r.cfg.MaxHeaderBytes), DisableCompression: true}
}

var errDialBusy = errors.New("relay dial capacity exhausted")

type targetKey struct{}
type dialTarget struct {
	host, port string
	ips        []net.IP
}

func (r *Relay) originDial(dial DialContextFunc, phone bool) DialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		target, ok := ctx.Value(targetKey{}).(dialTarget)
		if !ok || address != net.JoinHostPort(target.host, target.port) || network != "tcp" {
			return nil, errors.New("unvalidated origin dial")
		}
		// net/http can detach dial cancellation to reuse sockets for another
		// request. Bound those background dials too, including cancellation storms.
		select {
		case r.dialSlots <- struct{}{}:
			defer func() { <-r.dialSlots }()
		default:
			return nil, errDialBusy
		}
		ctx, cancel := context.WithTimeout(ctx, r.cfg.DialTimeout)
		stop := context.AfterFunc(r.ctx, cancel)
		defer func() { stop(); cancel() }()
		if r.ctx.Err() != nil {
			return nil, context.Canceled
		}
		var last error
		for _, ip := range target.ips {
			if !phone && ip.To4() == nil {
				last = errors.New("home uplink requires an IPv4 origin")
				continue
			}
			pinned := net.JoinHostPort(ip.String(), target.port)
			var conn net.Conn
			var err error
			if phone {
				conn, err = r.connectPhone(ctx, dial, pinned)
			} else {
				conn, err = dial(ctx, "tcp4", pinned)
			}
			if err == nil {
				return conn, nil
			}
			last = err
			if ctx.Err() != nil || errors.Is(err, errPhoneCapacity) {
				break
			}
		}
		return nil, fmt.Errorf("origin connection failed: %w", last)
	}
}
