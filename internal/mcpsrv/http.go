package mcpsrv

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Mengzhex/shao/internal/config"
)

// The HTTP transport exists for AI platforms that connect to a URL rather
// than launching a process.
//
// By default it binds to loopback, requires a bearer token held in an
// owner-only file, and refuses requests carrying a browser Origin.
//
// It can also be published to a network, because an agent on another machine
// has to reach it somehow, but only when asked for explicitly: see
// HTTPConfig.AllowRemote. Doing so is a real change in exposure. The traffic
// is plain HTTP, so recorded terminal output and the token itself are readable
// by anything on the path, and an allow list of client networks is the only
// coarse filter available. An SSH tunnel is the better answer where the
// remote end can arrange one.
//
// Two shapes are served, because platforms differ on which they support:
// Streamable HTTP at /mcp, and the older HTTP+SSE transport at /sse. The SSE
// stream is a response channel only. Nothing is ever written to it that the
// client did not ask for, so shao remains pull-only despite holding a
// long-lived connection open.

const (
	// httpPath is the Streamable HTTP endpoint: POST a request, get a reply
	// as JSON, or as an SSE frame when the client asks for one.
	httpPath = "/mcp"
	// ssePath and messagesPath implement the older HTTP+SSE transport, where
	// a GET opens a long-lived stream and replies come back over it while
	// requests go to a separate POST endpoint. It is deprecated in the spec
	// but is still the only transport a lot of hosted AI platforms offer, and
	// "the MCP SSE URL" almost always means this one.
	ssePath      = "/sse"
	messagesPath = "/messages"

	readHeaderTimeout = 10 * time.Second
	requestTimeout    = 120 * time.Second
	maxRequestBytes   = 4 << 20

	// maxSSESessions bounds how many streams can be held open at once, so a
	// misbehaving client cannot accumulate goroutines indefinitely.
	maxSSESessions = 8
	// sseQueue is how many replies may be buffered for one stream.
	sseQueue = 32
)

// sseKeepalive stops idle proxies and load balancers from dropping a stream
// that is simply waiting for the user to ask something. A variable rather than
// a constant only so tests need not wait fifteen seconds for one.
var sseKeepalive = 15 * time.Second

// sseSession is one open SSE stream. Replies for it are queued on ch by the
// POST handler and written out by the GET handler.
//
// Note what this does not do: nothing is ever queued that the client did not
// ask for. The stream is a response channel, not a push channel, so shao
// stays strictly pull-only even though the connection is long-lived.
type sseSession struct {
	id string
	ch chan []byte
}

// LoadOrCreateToken returns the bearer token for the local endpoint,
// generating one on first use.
func LoadOrCreateToken(cfg *config.Config) (string, error) {
	path := cfg.TokenPath()
	if data, err := os.ReadFile(path); err == nil {
		if token := strings.TrimSpace(string(data)); token != "" {
			return token, nil
		}
	}
	return RotateToken(cfg)
}

// RotateToken replaces the bearer token, invalidating any client still using
// the old one.
func RotateToken(cfg *config.Config) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	if err := config.WritePrivateFile(cfg.TokenPath(), []byte(token+"\n")); err != nil {
		return "", err
	}
	return token, nil
}

// HTTPServer is a running local MCP endpoint.
type HTTPServer struct {
	Addr  string
	Token string

	srv *http.Server
	ln  net.Listener

	// loopback records whether this endpoint is reachable only from this
	// machine. It relaxes nothing; it decides what the caller is told.
	loopback bool
	// allow, when non-empty, is the set of client networks permitted to
	// connect at all, checked before the token.
	allow []*net.IPNet

	mu       sync.Mutex
	sessions map[string]*sseSession
}

func parseAllow(cidrs []string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, c := range cidrs {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		// A bare address is accepted and treated as a single host, since that
		// is what someone naming one machine will write.
		if !strings.Contains(c, "/") {
			ip := net.ParseIP(c)
			if ip == nil {
				return nil, fmt.Errorf("allow: %q is not an IP address or CIDR", c)
			}
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			c = fmt.Sprintf("%s/%d", ip.String(), bits)
		}
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("allow: %q: %w", c, err)
		}
		out = append(out, n)
	}
	return out, nil
}

// permitted reports whether a client address may connect. An empty allow list
// permits anything that can reach the port.
func (h *HTTPServer) permitted(remoteAddr string) bool {
	if len(h.allow) == 0 {
		return true
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, n := range h.allow {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// LANURLs lists the addresses this endpoint can be reached at from other
// machines, so they can be printed rather than looked up by hand.
func (h *HTTPServer) LANURLs() []string {
	if h.loopback {
		return nil
	}
	_, port, err := net.SplitHostPort(h.Addr)
	if err != nil {
		return nil
	}
	// When bound to a wildcard address the listener cannot say which
	// interface to advertise, so enumerate them.
	bindHost, _, _ := net.SplitHostPort(h.Addr)
	if ip := net.ParseIP(bindHost); ip != nil && !ip.IsUnspecified() {
		return []string{"http://" + net.JoinHostPort(bindHost, port)}
	}

	var out []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok || n.IP.To4() == nil || !n.IP.IsGlobalUnicast() {
				continue
			}
			out = append(out, "http://"+net.JoinHostPort(n.IP.String(), port))
		}
	}
	return out
}

// gate applies every access check shared by the endpoints, writing the
// response itself and reporting whether the request may proceed.
//
// Keeping it in one place matters more than the few lines it saves: three
// endpoints each re-implementing the checks is how one of them ends up missing
// the token check.
func (h *HTTPServer) gate(w http.ResponseWriter, r *http.Request, token string) bool {
	if !h.permitted(r.RemoteAddr) {
		http.Error(w, "client address not in the allow list", http.StatusForbidden)
		return false
	}
	if !h.originOK(r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return false
	}
	if !checkAuth(r, token) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="shao"`)
		http.Error(w, "missing or invalid bearer token", http.StatusUnauthorized)
		return false
	}
	return true
}

// originOK guards against DNS rebinding, which is a threat specific to a
// server reachable at localhost: a page on any website can resolve a name to
// 127.0.0.1 and post to it. Refusing a non-loopback Origin closes that.
//
// On an endpoint deliberately published to the network the check is dropped,
// because it is no longer the right control and it would break browser-based
// clients on other machines for no gain. There the bearer token is the gate,
// and it is one a rebinding page cannot obtain.
func (h *HTTPServer) originOK(r *http.Request) bool {
	if !h.loopback {
		return true
	}
	return checkOrigin(r)
}

// URL is the Streamable HTTP endpoint, the one to prefer.
func (h *HTTPServer) URL() string { return "http://" + h.Addr + httpPath }

// SSEURL is the endpoint for clients that only support the older HTTP+SSE
// transport.
func (h *HTTPServer) SSEURL() string { return "http://" + h.Addr + ssePath }

func (h *HTTPServer) addSession() (*sseSession, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.sessions) >= maxSSESessions {
		return nil, fmt.Errorf("too many open streams")
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	s := &sseSession{id: hex.EncodeToString(buf), ch: make(chan []byte, sseQueue)}
	h.sessions[s.id] = s
	return s, nil
}

func (h *HTTPServer) session(id string) (*sseSession, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s, ok := h.sessions[id]
	return s, ok
}

func (h *HTTPServer) removeSession(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessions, id)
}

// ListenHTTP starts the local endpoint. A port of 0 lets the OS choose, and
// the chosen address is reported back so it can be printed.
func (s *Server) ListenHTTP(cfg config.HTTPConfig, token string) (*HTTPServer, error) {
	bind := cfg.Bind
	if bind == "" {
		bind = "127.0.0.1"
	}
	// A non-loopback bind publishes recorded terminal output to the network.
	// That is a legitimate thing to want -- an agent on another machine has to
	// reach it somehow -- but it must be asked for explicitly, so a value
	// sitting in a config file cannot do it by accident.
	if !isLoopback(bind) && !cfg.AllowRemote {
		return nil, fmt.Errorf(
			"refusing to bind the MCP endpoint to %s: it serves your terminal history, "+
				"so reaching it from another machine has to be requested explicitly with --bind", bind)
	}

	allow, err := parseAllow(cfg.Allow)
	if err != nil {
		return nil, err
	}

	ln, err := net.Listen("tcp", net.JoinHostPort(bind, fmt.Sprint(cfg.Port)))
	if err != nil {
		return nil, err
	}

	h := &HTTPServer{
		Addr:     ln.Addr().String(),
		Token:    token,
		ln:       ln,
		sessions: map[string]*sseSession{},
		loopback: isLoopback(bind),
		allow:    allow,
	}
	mux := http.NewServeMux()
	mux.HandleFunc(httpPath, s.httpHandler(h, token))
	mux.HandleFunc(ssePath, s.sseHandler(h, token))
	mux.HandleFunc(messagesPath, s.messagesHandler(h, token))
	h.srv = &http.Server{
		Handler: mux,
		// No write timeout: an SSE stream is meant to stay open, and a
		// timeout here would sever it mid-conversation.
		ReadHeaderTimeout: readHeaderTimeout,
	}
	go func() { _ = h.srv.Serve(ln) }()
	return h, nil
}

// Close stops the endpoint.
func (h *HTTPServer) Close(ctx context.Context) error {
	if h.srv == nil {
		return nil
	}
	return h.srv.Shutdown(ctx)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (s *Server) httpHandler(h *HTTPServer, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.gate(w, r, token) {
			return
		}

		switch r.Method {
		case http.MethodPost:
			s.httpPost(w, r)
		case http.MethodGet:
			// Streamable HTTP uses GET only for server-initiated messages,
			// and shao never sends any, so refusing is correct here. Clients
			// that need a stream should use the SSE transport at /sse.
			http.Error(w, "no server-to-client stream on this endpoint; use "+ssePath+" for the SSE transport", http.StatusMethodNotAllowed)
		case http.MethodDelete:
			// No server-side session state to discard.
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}
}

func (s *Server) httpPost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
	if err != nil {
		http.Error(w, "could not read request body", http.StatusBadRequest)
		return
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		http.Error(w, "empty request", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	type result struct {
		payload any
		empty   bool
	}
	done := make(chan result, 1)
	go func() {
		if trimmed[0] == '[' {
			responses := s.handleBatch([]byte(trimmed))
			done <- result{payload: responses, empty: len(responses) == 0}
			return
		}
		resp := s.handle([]byte(trimmed))
		done <- result{payload: resp, empty: resp == nil}
	}()

	select {
	case <-ctx.Done():
		http.Error(w, "request timed out", http.StatusGatewayTimeout)
	case res := <-done:
		if res.empty {
			// Everything in the request was a notification.
			w.WriteHeader(http.StatusAccepted)
			return
		}
		// Streamable HTTP lets the server answer with an SSE frame instead of
		// plain JSON. Some clients require that, so honour their Accept
		// header rather than always sending JSON.
		if wantsSSE(r) {
			if flusher, ok := w.(http.Flusher); ok {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("X-Accel-Buffering", "no")
				w.WriteHeader(http.StatusOK)
				data, err := json.Marshal(res.payload)
				if err == nil {
					fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
					flusher.Flush()
				}
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(res.payload)
	}
}

// wantsSSE reports whether the client asked for an event stream and did not
// also accept plain JSON. When it accepts both, JSON is simpler and is used.
func wantsSSE(r *http.Request) bool {
	accept := strings.ToLower(r.Header.Get("Accept"))
	if !strings.Contains(accept, "text/event-stream") {
		return false
	}
	return !strings.Contains(accept, "application/json")
}

// sseHandler opens a stream and tells the client where to post requests.
//
// This is the deprecated HTTP+SSE transport, kept because many hosted AI
// platforms accept only an "SSE URL". The handshake is: the server opens the
// stream and immediately sends an `endpoint` event naming the POST target,
// including a session id that ties the two halves together.
func (s *Server) sseHandler(h *HTTPServer, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.gate(w, r, token) {
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}

		sess, err := h.addSession()
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		defer h.removeSession(sess.id)

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		// Tells nginx and friends not to buffer, which would otherwise hold
		// events until the stream closed.
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)

		// The token has to travel in the POST URL because a browser
		// EventSource cannot set headers, and a client that had to use this
		// transport often cannot either.
		endpoint := fmt.Sprintf("%s?sessionId=%s&token=%s", messagesPath, sess.id, url.QueryEscape(token))
		fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", endpoint)
		flusher.Flush()

		keepalive := time.NewTicker(sseKeepalive)
		defer keepalive.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case msg := <-sess.ch:
				// SSE data must not contain bare newlines; the JSON encoder
				// emits none, but split defensively rather than trust that.
				fmt.Fprint(w, "event: message\n")
				for _, line := range strings.Split(strings.TrimRight(string(msg), "\n"), "\n") {
					fmt.Fprintf(w, "data: %s\n", line)
				}
				fmt.Fprint(w, "\n")
				flusher.Flush()
			case <-keepalive.C:
				// A comment line: valid SSE, ignored by clients.
				fmt.Fprint(w, ": keepalive\n\n")
				flusher.Flush()
			}
		}
	}
}

// messagesHandler receives requests for an open SSE stream and queues the
// replies onto it, answering the POST itself with 202.
func (s *Server) messagesHandler(h *HTTPServer, token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !h.gate(w, r, token) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		sess, ok := h.session(r.URL.Query().Get("sessionId"))
		if !ok {
			http.Error(w, "unknown or closed sessionId; reopen the SSE stream", http.StatusNotFound)
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes))
		if err != nil {
			http.Error(w, "could not read request body", http.StatusBadRequest)
			return
		}
		trimmed := strings.TrimSpace(string(body))
		if trimmed == "" {
			http.Error(w, "empty request", http.StatusBadRequest)
			return
		}

		var replies []*rpcResponse
		if trimmed[0] == '[' {
			replies = s.handleBatch([]byte(trimmed))
		} else if resp := s.handle([]byte(trimmed)); resp != nil {
			replies = []*rpcResponse{resp}
		}

		for _, resp := range replies {
			data, err := json.Marshal(resp)
			if err != nil {
				continue
			}
			select {
			case sess.ch <- data:
			default:
				// The stream is not draining. Failing the POST is better than
				// blocking it forever or silently dropping the answer.
				http.Error(w, "stream is not keeping up", http.StatusServiceUnavailable)
				return
			}
		}
		// The reply travels on the SSE stream, so there is nothing to return.
		w.WriteHeader(http.StatusAccepted)
	}
}

// checkAuth accepts the token as a bearer header or, as a fallback, a query
// parameter.
//
// The query form exists because a browser EventSource cannot set headers at
// all, so for some clients it is the only way to authenticate. It is a weaker
// place to put a secret, since URLs end up in logs and history. That is
// tolerable only because this endpoint is bound to the loopback interface, so
// the URL never leaves the machine.
func checkAuth(r *http.Request, token string) bool {
	if q := strings.TrimSpace(r.URL.Query().Get("token")); q != "" {
		return subtle.ConstantTimeCompare([]byte(q), []byte(token)) == 1
	}
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return false
	}
	got := strings.TrimSpace(header[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

// checkOrigin rejects browser-originated requests.
//
// A local HTTP server is reachable by any web page the user has open, and a
// page that could post here would be able to read their terminal history. MCP
// clients are not browsers and send no Origin header, so requiring its
// absence (or a loopback value) costs nothing and closes that door.
func checkOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	host := origin
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return isLoopback(host)
}
