package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type allowEntry struct {
	host string
	wild bool
	port string // empty: the scheme's default
}

type allowlist []allowEntry

func parseAllow(items []string) (allowlist, error) {
	var l allowlist

	for _, item := range items {
		s := strings.TrimSpace(item)
		host, port := s, ""

		if h, p, err := net.SplitHostPort(s); err == nil {
			n, perr := strconv.Atoi(p)
			if perr != nil || n < 1 || n > 65535 {
				return nil, fmt.Errorf("proxy: allow entry %q has an invalid port", item)
			}

			host, port = h, strconv.Itoa(n)
		}

		e := allowEntry{port: port}

		if rest, ok := strings.CutPrefix(host, "*."); ok {
			e.wild, host = true, rest
		}

		e.host = normHost(host)
		if e.host == "" || strings.ContainsAny(e.host, "*/ ") {
			return nil, fmt.Errorf("proxy: allow entry %q is not a host", item)
		}

		l = append(l, e)
	}

	return l, nil
}

// normHost lowercases, drops brackets and a trailing dot, and canonicalises IP literals.
func normHost(h string) string {
	h = strings.TrimSuffix(strings.ToLower(strings.Trim(h, "[]")), ".")
	if a, err := netip.ParseAddr(h); err == nil {
		return a.String()
	}

	return h
}

// splitAuthority returns the normalised host and port of host[:port].
func splitAuthority(authority, defPort string) (host, port string, err error) {
	host, port = authority, defPort

	if h, p, e := net.SplitHostPort(authority); e == nil {
		host, port = h, p
	}

	n, perr := strconv.Atoi(port)
	host = normHost(host)

	if host == "" || perr != nil || n < 1 || n > 65535 {
		return "", "", errors.New("proxy: bad request target")
	}

	return host, strconv.Itoa(n), nil
}

// listsHost reports whether an entry names exactly this host or IP, on any port.
func (l allowlist) listsHost(host string) bool {
	for _, e := range l {
		if !e.wild && e.host == host {
			return true
		}
	}

	return false
}

// permits reports whether host:port is allowed; defPort is the port an entry
// without one stands for. Wildcards never match IP literals.
func (l allowlist) permits(host, port, defPort string) bool {
	_, ipErr := netip.ParseAddr(host)
	isIP := ipErr == nil

	for _, e := range l {
		want := e.port
		if want == "" {
			want = defPort
		}

		if want != port {
			continue
		}

		if e.wild {
			if !isIP && strings.HasSuffix(host, "."+e.host) {
				return true
			}

			continue
		}

		if host == e.host {
			return true
		}
	}

	return false
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func stripHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for name := range strings.SplitSeq(v, ",") {
			h.Del(strings.TrimSpace(name))
		}
	}

	for _, name := range hopHeaders {
		h.Del(name)
	}
}

const dialTimeout = 30 * time.Second

var errRefusedAddress = errors.New("proxy: address refused after resolution")

// control runs on every resolved address just before connect, so a second
// resolution cannot slip past. Link-local and unspecified addresses (cloud
// metadata among them) are refused unless that IP itself is on the allowlist.
func (h *handler) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return errRefusedAddress
	}

	ip := ap.Addr().Unmap().WithZone("")
	if (ip.IsLinkLocalUnicast() || ip.IsUnspecified()) && !h.allow.listsHost(ip.String()) {
		return errRefusedAddress
	}

	return nil
}

func (h *handler) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if h.lookup != nil {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}

		if _, perr := netip.ParseAddr(host); perr != nil {
			ips, lerr := h.lookup(ctx, host)
			if lerr != nil || len(ips) == 0 {
				return nil, errors.New("proxy: lookup failed")
			}

			addr = net.JoinHostPort(ips[0], port)
		}
	}

	return (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second, Control: h.control}).DialContext(ctx, network, addr)
}

func (h *handler) deny(w http.ResponseWriter, host, port string) {
	h.denied(net.JoinHostPort(host, port))
	http.Error(w, "forbidden by the egress allowlist", http.StatusForbidden)
}

// connect opens a raw tunnel to an allowed host:port.
func (h *handler) connect(w http.ResponseWriter, r *http.Request) {
	host, port, err := splitAuthority(r.Host, "443")
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)

		return
	}

	if !h.allow.permits(host, port, "443") {
		h.deny(w, host, port)

		return
	}

	up, err := h.dialContext(r.Context(), "tcp", net.JoinHostPort(host, port))
	if err != nil {
		if errors.Is(err, errRefusedAddress) {
			h.deny(w, host, port)

			return
		}

		http.Error(w, "bad gateway", http.StatusBadGateway)

		return
	}
	defer up.Close()

	client, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		http.Error(w, "tunnel unsupported", http.StatusInternalServerError)

		return
	}
	defer client.Close()

	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}

	tunnel(client, rw.Reader, up)
}

// tunnel copies both ways. Once one direction ends the other gets a short
// grace period so a stuck peer cannot hold the tunnel open.
func tunnel(client net.Conn, clientIn io.Reader, up net.Conn) {
	done := make(chan struct{}, 2)

	pipe := func(dst net.Conn, src io.Reader) {
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}

		done <- struct{}{}
	}

	go pipe(up, clientIn)
	go pipe(client, up)

	<-done

	deadline := time.Now().Add(30 * time.Second)
	_ = client.SetDeadline(deadline)
	_ = up.SetDeadline(deadline)

	<-done
}

// forward relays an absolute-form http request to an allowed host:port without following redirects.
func (h *handler) forward(w http.ResponseWriter, r *http.Request) {
	defPort := "80"
	if r.URL.Scheme == "https" {
		defPort = "443"
	}

	host, port, err := splitAuthority(r.URL.Host, defPort)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)

		return
	}

	if r.URL.Scheme != "http" || !h.allow.permits(host, port, "80") {
		h.deny(w, host, port)

		return
	}

	out := r.Clone(r.Context())
	out.RequestURI = ""
	stripHop(out.Header)

	if _, ok := out.Header["User-Agent"]; !ok {
		out.Header.Set("User-Agent", "")
	}

	resp, err := h.egress.RoundTrip(out)
	if err != nil {
		if errors.Is(err, errRefusedAddress) {
			h.deny(w, host, port)

			return
		}

		http.Error(w, "bad gateway", http.StatusBadGateway)

		return
	}
	defer resp.Body.Close()

	stripHop(resp.Header)

	maps.Copy(w.Header(), resp.Header)

	w.WriteHeader(resp.StatusCode)
	flushCopy(w, resp.Body)
}

func flushCopy(w http.ResponseWriter, src io.Reader) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)

	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}

			_ = rc.Flush()
		}

		if err != nil {
			return
		}
	}
}
