package mcpsrv

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Mengzhex/shao/internal/config"
)

// These tests drive the real listener over a real TCP connection rather than
// httptest handlers, because the parts most likely to break are the streaming
// ones: flushing, keepalives, session lifetime. A recorded handler would not
// exercise any of them.

const testToken = "test-token-0123456789"

func newTestHTTP(t *testing.T) *HTTPServer {
	t.Helper()
	root := t.TempDir()
	buildFixture(t, root)

	cfg, err := config.Load(root)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	h, err := srv.ListenHTTP(config.HTTPConfig{Bind: "127.0.0.1", Port: 0}, testToken)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = h.Close(ctx)
	})
	return h
}

// sseStream reads server-sent events off a live response body.
type sseStream struct {
	body io.ReadCloser
	sc   *bufio.Scanner
}

func openSSE(t *testing.T, rawurl string, header http.Header) (*sseStream, *http.Response) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawurl, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	// No client timeout: the point of the stream is that it stays open.
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatalf("open sse: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type is %q, want text/event-stream", ct)
	}
	s := &sseStream{body: resp.Body, sc: bufio.NewScanner(resp.Body)}
	t.Cleanup(func() { resp.Body.Close() })
	return s, resp
}

// next returns the next event, skipping keepalive comments. It reports
// comments seen so a test can assert on them.
func (s *sseStream) next(t *testing.T) (event, data string, comments int) {
	t.Helper()
	for s.sc.Scan() {
		line := s.sc.Text()
		switch {
		case line == "":
			if event != "" || data != "" {
				return event, data, comments
			}
		case strings.HasPrefix(line, ":"):
			comments++
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if data != "" {
				data += "\n"
			}
			data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
	t.Fatalf("stream ended before a complete event arrived (scanner err: %v)", s.sc.Err())
	return "", "", comments
}

func bearer() http.Header {
	return http.Header{"Authorization": []string{"Bearer " + testToken}}
}

// TestSSERoundTrip is the whole point of the SSE transport: the reply to a
// POST arrives on a separate, already-open stream.
func TestSSERoundTrip(t *testing.T) {
	h := newTestHTTP(t)

	stream, _ := openSSE(t, h.SSEURL(), bearer())
	if stream == nil {
		t.Fatal("stream did not open")
	}

	// The server must name the POST endpoint before anything else.
	event, data, _ := stream.next(t)
	if event != "endpoint" {
		t.Fatalf("first event is %q, want %q", event, "endpoint")
	}
	if !strings.HasPrefix(data, messagesPath+"?sessionId=") {
		t.Fatalf("endpoint data is %q, want a %s URL with a sessionId", data, messagesPath)
	}

	// Post a request to the advertised endpoint. The 202 says only that it was
	// accepted; the answer comes back on the stream.
	postURL := "http://" + h.Addr + data
	resp, err := http.Post(postURL, "application/json",
		strings.NewReader(`{"jsonrpc":"2.0","id":7,"method":"tools/list"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("POST %s returned %d, want 202", messagesPath, resp.StatusCode)
	}

	event, data, _ = stream.next(t)
	if event != "message" {
		t.Fatalf("reply event is %q, want %q", event, "message")
	}
	var reply struct {
		ID     int `json:"id"`
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(data), &reply); err != nil {
		t.Fatalf("reply is not JSON-RPC: %q: %v", data, err)
	}
	if reply.ID != 7 {
		t.Errorf("reply id is %d, want 7", reply.ID)
	}
	if len(reply.Result.Tools) < 8 {
		t.Errorf("got %d tools over SSE, want at least 8", len(reply.Result.Tools))
	}
}

// The endpoint event carries the token because a browser EventSource cannot
// set headers, so a client that reached the stream can also reach the POST.
func TestSSEEndpointCarriesToken(t *testing.T) {
	h := newTestHTTP(t)
	stream, _ := openSSE(t, h.SSEURL()+"?token="+url.QueryEscape(testToken), nil)
	if stream == nil {
		t.Fatal("stream did not open with a query token")
	}
	_, data, _ := stream.next(t)
	if !strings.Contains(data, "token="+url.QueryEscape(testToken)) {
		t.Errorf("endpoint %q does not carry the token", data)
	}
}

func TestSSEAuthAndOrigin(t *testing.T) {
	h := newTestHTTP(t)
	cases := []struct {
		name   string
		url    string
		header http.Header
		want   int
	}{
		{"no token", h.SSEURL(), nil, http.StatusUnauthorized},
		{"wrong header token", h.SSEURL(), http.Header{"Authorization": []string{"Bearer nope"}}, http.StatusUnauthorized},
		{"wrong query token", h.SSEURL() + "?token=nope", nil, http.StatusUnauthorized},
		{"browser origin", h.SSEURL(), http.Header{
			"Authorization": []string{"Bearer " + testToken},
			"Origin":        []string{"https://evil.example.com"},
		}, http.StatusForbidden},
		{"loopback origin is fine", h.SSEURL(), http.Header{
			"Authorization": []string{"Bearer " + testToken},
			"Origin":        []string{"http://127.0.0.1:1234"},
		}, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, tc.url, nil)
			for k, v := range tc.header {
				req.Header[k] = v
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			resp, err := (&http.Client{}).Do(req.WithContext(ctx))
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("got %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
}

func TestMessagesRejectsUnknownSession(t *testing.T) {
	h := newTestHTTP(t)
	resp, err := http.Post(
		fmt.Sprintf("http://%s%s?sessionId=deadbeef&token=%s", h.Addr, messagesPath, testToken),
		"application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("got %d, want 404 for an unknown sessionId", resp.StatusCode)
	}
}

func TestMessagesRequiresAuth(t *testing.T) {
	h := newTestHTTP(t)
	resp, err := http.Post(
		fmt.Sprintf("http://%s%s?sessionId=whatever", h.Addr, messagesPath),
		"application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("got %d, want 401", resp.StatusCode)
	}
}

// A closed stream must release its session, otherwise a client reconnecting
// repeatedly would exhaust the cap.
func TestSSESessionReleasedOnDisconnect(t *testing.T) {
	h := newTestHTTP(t)

	req, _ := http.NewRequest(http.MethodGet, h.SSEURL(), nil)
	req.Header = bearer()
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// Read the endpoint event so the handler is certainly running.
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if sc.Text() == "" {
			break
		}
	}

	h.mu.Lock()
	open := len(h.sessions)
	h.mu.Unlock()
	if open != 1 {
		t.Fatalf("%d sessions registered, want 1", open)
	}

	resp.Body.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		open = len(h.sessions)
		h.mu.Unlock()
		if open == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("session still registered %v after the client disconnected", 3*time.Second)
}

func TestSSESessionCapEnforced(t *testing.T) {
	h := newTestHTTP(t)

	var bodies []io.ReadCloser
	t.Cleanup(func() {
		for _, b := range bodies {
			b.Close()
		}
	})

	for i := 0; i < maxSSESessions; i++ {
		req, _ := http.NewRequest(http.MethodGet, h.SSEURL(), nil)
		req.Header = bearer()
		resp, err := (&http.Client{}).Do(req)
		if err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d returned %d, want 200", i, resp.StatusCode)
		}
		bodies = append(bodies, resp.Body)
		// Read the endpoint event so the session is definitely registered.
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if sc.Text() == "" {
				break
			}
		}
	}

	req, _ := http.NewRequest(http.MethodGet, h.SSEURL(), nil)
	req.Header = bearer()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := (&http.Client{}).Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("request beyond the cap: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("stream %d returned %d, want 503 once the cap is reached",
			maxSSESessions+1, resp.StatusCode)
	}
}

// An idle stream must emit keepalive comments, or a proxy in front of it will
// eventually drop a connection that is merely waiting for a question.
func TestSSEKeepalive(t *testing.T) {
	saved := sseKeepalive
	sseKeepalive = 120 * time.Millisecond
	t.Cleanup(func() { sseKeepalive = saved })

	h := newTestHTTP(t)
	stream, _ := openSSE(t, h.SSEURL(), bearer())
	if stream == nil {
		t.Fatal("stream did not open")
	}
	if event, _, _ := stream.next(t); event != "endpoint" {
		t.Fatalf("first event is %q", event)
	}

	// Ask nothing, and wait. The only thing that can arrive is a keepalive.
	done := make(chan int, 1)
	go func() {
		count := 0
		for stream.sc.Scan() {
			if strings.HasPrefix(stream.sc.Text(), ":") {
				count++
				if count >= 2 {
					done <- count
					return
				}
			}
		}
		done <- count
	}()

	select {
	case n := <-done:
		if n < 2 {
			t.Errorf("saw %d keepalives, want at least 2", n)
		}
	case <-time.After(3 * time.Second):
		t.Error("no keepalives arrived on an idle stream")
	}
}

// Streamable HTTP may answer with either shape; the client's Accept header
// decides, and a client that requires SSE must not be handed JSON.
func TestStreamableHTTPRespectsAccept(t *testing.T) {
	h := newTestHTTP(t)
	body := `{"jsonrpc":"2.0","id":3,"method":"ping"}`

	cases := []struct {
		accept     string
		wantPrefix string
		wantCT     string
	}{
		{"text/event-stream", "event: message", "text/event-stream"},
		{"application/json", `{"jsonrpc"`, "application/json"},
		// Accepting both is the common case; JSON is simpler, so JSON wins.
		{"application/json, text/event-stream", `{"jsonrpc"`, "application/json"},
		{"", `{"jsonrpc"`, "application/json"},
	}
	for _, tc := range cases {
		t.Run("accept="+tc.accept, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, h.URL(), strings.NewReader(body))
			req.Header = bearer()
			if tc.accept != "" {
				req.Header.Set("Accept", tc.accept)
			}
			resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			got, _ := io.ReadAll(resp.Body)
			if !strings.HasPrefix(resp.Header.Get("Content-Type"), tc.wantCT) {
				t.Errorf("Content-Type is %q, want %q", resp.Header.Get("Content-Type"), tc.wantCT)
			}
			if !strings.HasPrefix(strings.TrimSpace(string(got)), tc.wantPrefix) {
				t.Errorf("body starts %q, want prefix %q", string(got), tc.wantPrefix)
			}
		})
	}
}

// GET on the Streamable HTTP endpoint has no stream to offer, and should point
// at the one that does rather than just refusing.
func TestStreamableHTTPGetPointsAtSSE(t *testing.T) {
	h := newTestHTTP(t)
	req, _ := http.NewRequest(http.MethodGet, h.URL(), nil)
	req.Header = bearer()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("got %d, want 405", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), ssePath) {
		t.Errorf("405 body %q does not mention %s", string(body), ssePath)
	}
}

func TestRefusesNonLoopbackBind(t *testing.T) {
	root := t.TempDir()
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ListenHTTP(config.HTTPConfig{Bind: "0.0.0.0", Port: 0}, testToken); err == nil {
		t.Fatal("binding to 0.0.0.0 was allowed; it serves terminal history and must stay on loopback")
	}
}

// Publishing recorded terminal history to a network must be an explicit act.
// A value sitting in a config file is not consent, so AllowRemote gates it.
func TestNonLoopbackBindNeedsAllowRemote(t *testing.T) {
	root := t.TempDir()
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := srv.ListenHTTP(config.HTTPConfig{Bind: "0.0.0.0", Port: 0}, testToken); err == nil {
		t.Error("bound to 0.0.0.0 without AllowRemote; that must be refused")
	}

	h, err := srv.ListenHTTP(config.HTTPConfig{Bind: "127.0.0.1", Port: 0, AllowRemote: true}, testToken)
	if err != nil {
		t.Fatalf("loopback bind should always work: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h.Close(ctx)
}

func TestAllowListParsing(t *testing.T) {
	cases := []struct {
		in      []string
		wantErr bool
	}{
		{[]string{"192.168.1.0/24"}, false},
		{[]string{"192.168.1.42"}, false}, // a bare host is accepted
		{[]string{"10.0.0.0/8", " 192.168.1.42 "}, false},
		{[]string{"not-an-ip"}, true},
		{[]string{"192.168.1.0/99"}, true},
		{nil, false},
	}
	for _, tc := range cases {
		_, err := parseAllow(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseAllow(%v) error = %v, wantErr %v", tc.in, err, tc.wantErr)
		}
	}
}

func TestAllowListBlocksOtherClients(t *testing.T) {
	root := t.TempDir()
	buildFixture(t, root)
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// An allow list naming only a network this machine is not on. Loopback is
	// always permitted, otherwise the endpoint would lock out the host it runs
	// on, so this checks the rule rather than the connection.
	h, err := srv.ListenHTTP(config.HTTPConfig{
		Bind: "127.0.0.1", Port: 0, Allow: []string{"203.0.113.0/24"},
	}, testToken)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		h.Close(ctx)
	})

	if !h.permitted("127.0.0.1:5000") {
		t.Error("loopback must always be permitted, or the host locks itself out")
	}
	if !h.permitted("203.0.113.7:5000") {
		t.Error("an address inside the allow list was rejected")
	}
	if h.permitted("192.168.1.9:5000") {
		t.Error("an address outside the allow list was permitted")
	}
	if h.permitted("garbage") {
		t.Error("an unparseable address was permitted")
	}

	// With a list configured, a real request from loopback still works.
	req, _ := http.NewRequest(http.MethodPost, h.URL(),
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header = bearer()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("loopback request got %d, want 200", resp.StatusCode)
	}
}

// An empty allow list means anything that reaches the port, which is the
// default and needs to stay true so loopback use is not accidentally broken.
func TestEmptyAllowListPermitsAll(t *testing.T) {
	h := newTestHTTP(t)
	for _, addr := range []string{"127.0.0.1:1", "192.168.50.9:1", "10.1.2.3:1"} {
		if !h.permitted(addr) {
			t.Errorf("%s rejected with an empty allow list", addr)
		}
	}
}

// The Origin check is an anti-DNS-rebinding measure specific to localhost
// servers. On an endpoint deliberately published to the network it is the
// wrong control and would break browser-based clients elsewhere, so it is
// dropped there while the token still applies.
func TestOriginCheckOnlyAppliesToLoopbackBind(t *testing.T) {
	loopbackOnly := newTestHTTP(t)
	if loopbackOnly.originOK(withOrigin("https://evil.example.com")) {
		t.Error("a browser origin was accepted on a loopback endpoint")
	}
	if !loopbackOnly.originOK(withOrigin("")) {
		t.Error("a request with no Origin was rejected")
	}

	published := &HTTPServer{loopback: false}
	if !published.originOK(withOrigin("https://agent.example.com")) {
		t.Error("origin was enforced on a deliberately published endpoint")
	}
}

func withOrigin(origin string) *http.Request {
	r, _ := http.NewRequest(http.MethodPost, "http://127.0.0.1/mcp", nil)
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

// The endpoint records where it is so it can be stopped precisely.
//
// This exists because of a real incident: without it, the obvious way to stop
// the server is to kill it by image name, and every recorder wrapping a live
// terminal shares that image name. Doing so killed terminals that had nothing
// to do with the server.
func TestServeStateLifecycle(t *testing.T) {
	root := t.TempDir()
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}

	if _, running := ReadServeState(cfg); running {
		t.Fatal("reported a running endpoint before one was started")
	}

	h, err := srv.ListenHTTP(config.HTTPConfig{Bind: "127.0.0.1", Port: 0}, testToken)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		h.Close(ctx)
	})

	if err := WriteServeState(cfg, h, ""); err != nil {
		t.Fatalf("write state: %v", err)
	}

	state, running := ReadServeState(cfg)
	if !running {
		t.Fatal("endpoint not reported running after its state was written")
	}
	if state.URL != h.URL() {
		t.Errorf("recorded URL %q, want %q", state.URL, h.URL())
	}
	if state.PID != os.Getpid() {
		t.Errorf("recorded pid %d, want %d", state.PID, os.Getpid())
	}

	// Writing the state must clear any leftover stop request, or a newly
	// started server would shut itself down at once.
	if ServeStopRequested(cfg) {
		t.Error("a stop was already pending on a freshly written state")
	}

	if err := RequestServeStop(cfg); err != nil {
		t.Fatal(err)
	}
	if !ServeStopRequested(cfg) {
		t.Error("stop request was not visible")
	}

	ClearServeState(cfg)
	if _, running := ReadServeState(cfg); running {
		t.Error("endpoint still reported running after its state was cleared")
	}
	if ServeStopRequested(cfg) {
		t.Error("stop request survived clearing the state")
	}
}

// A state file left behind by a killed server must not be mistaken for a
// running one.
func TestServeStateFromDeadProcessIsNotRunning(t *testing.T) {
	root := t.TempDir()
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.EnsurePrivateDir(root); err != nil {
		t.Fatal(err)
	}
	// A pid that cannot exist.
	stale := `{"pid":2147483632,"addr":"127.0.0.1:1","url":"http://127.0.0.1:1/mcp"}`
	if err := os.WriteFile(filepath.Join(root, serveStateFile), []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}

	state, running := ReadServeState(cfg)
	if running {
		t.Error("a state file from a dead process was reported as running")
	}
	// The pid is still returned so the caller can explain what it cleaned up.
	if state.PID == 0 {
		t.Error("the stale record was not parsed at all")
	}
}

// A fresh start must not inherit a stop request from a previous run.
func TestWriteServeStateClearsStaleStop(t *testing.T) {
	root := t.TempDir()
	cfg, err := config.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := RequestServeStop(cfg); err != nil {
		t.Fatal(err)
	}

	h, err := srv.ListenHTTP(config.HTTPConfig{Bind: "127.0.0.1", Port: 0}, testToken)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		h.Close(ctx)
	})
	if err := WriteServeState(cfg, h, ""); err != nil {
		t.Fatal(err)
	}
	if ServeStopRequested(cfg) {
		t.Error("a new endpoint inherited a stop request from a previous run and would exit immediately")
	}
}
