package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"proxio/internal/tokens"
)

// echo is what the test target reports about the request it received.
type echo struct {
	Method           string      `json:"method"`
	URI              string      `json:"uri"`
	Host             string      `json:"host"`
	Header           http.Header `json:"header"`
	TransferEncoding []string    `json:"transfer_encoding"`
	Body             string      `json:"body"`
}

func echoTarget(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(echo{
			Method:           r.Method,
			URI:              r.URL.RequestURI(),
			Host:             r.Host,
			Header:           r.Header,
			TransferEncoding: r.TransferEncoding,
			Body:             string(body),
		})
	}))
	t.Cleanup(s.Close)
	return s
}

// lockedBuffer collects log output from the handler's goroutine.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// harness is proxio, standing up, with one token minted.
type harness struct {
	server *httptest.Server
	store  *tokens.Store
	dir    string
	secret string
	logs   *lockedBuffer
}

func front(t *testing.T, tune ...func(*Options)) *harness {
	t.Helper()
	dir := t.TempDir()
	st, err := tokens.Open(dir)
	if err != nil {
		t.Fatalf("tokens.Open: %v", err)
	}
	_, secret, err := st.Create("test_token")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	logs := &lockedBuffer{}
	opts := Options{
		Tokens:          st,
		Log:             slog.New(slog.NewJSONHandler(logs, nil)),
		DialTimeout:     5 * time.Second,
		ResponseTimeout: 5 * time.Second,
		MaxRedirects:    10,
	}
	for _, f := range tune {
		f(&opts)
	}
	s := httptest.NewServer(New(opts))
	t.Cleanup(s.Close)
	return &harness{server: s, store: st, dir: dir, secret: secret, logs: logs}
}

// link builds a /proxy URL out of proxio's own parameters.
func (h *harness) link(params url.Values) string {
	return h.server.URL + "/proxy?" + params.Encode()
}

func (h *harness) url(target string) string {
	return h.link(url.Values{ParamURL: {target}, ParamToken: {h.secret}})
}

func (h *harness) request(t *testing.T, method, target string, body io.Reader) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, h.url(target), body)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// get is a request built from proxio's parameters directly, for the cases that are about
// those parameters rather than about what gets relayed.
func (h *harness) get(t *testing.T, params url.Values) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.link(params), nil)
	if err != nil {
		t.Fatal(err)
	}
	return do(t, req)
}

// noFollow is what every test uses, so a 3xx reaching the caller is visible rather than
// quietly resolved by the test client.
var noFollow = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func do(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := noFollow.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// waitForLog returns the log once a line has actually been written.
//
// The line goes out after the response body has been copied, so a caller that has finished
// reading its body routinely gets here before the handler's last statement runs. Reading the
// buffer straight away passes or fails depending on which goroutine wins, which is the kind
// of test that is green until it is red in CI for no reason anybody can reproduce.
func (h *harness) waitForLog(t *testing.T) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if s := h.logs.String(); strings.Contains(s, `"msg":`) {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatal("no log line was written")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func decode(t *testing.T, resp *http.Response) echo {
	t.Helper()
	var e echo
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decoding the echo: %v", err)
	}
	return e
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the body: %v", err)
	}
	return string(raw)
}

// The core promise.
func TestMethodHeadersAndBodyAreInherited(t *testing.T) {
	target := echoTarget(t)
	h := front(t)

	req := h.request(t, http.MethodPut, target.URL+"/thing", strings.NewReader("the payload"))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Custom", "kept")
	req.Header.Add("X-Multi", "one")
	req.Header.Add("X-Multi", "two")

	e := decode(t, do(t, req))
	if e.Method != http.MethodPut {
		t.Errorf("method = %q, want PUT", e.Method)
	}
	if e.Body != "the payload" {
		t.Errorf("body = %q, want %q", e.Body, "the payload")
	}
	if e.URI != "/thing" {
		t.Errorf("uri = %q, want /thing", e.URI)
	}
	if got := e.Header.Get("X-Custom"); got != "kept" {
		t.Errorf("X-Custom = %q, want kept", got)
	}
	if got := e.Header.Values("X-Multi"); len(got) != 2 {
		t.Errorf("X-Multi = %v, want both values", got)
	}
	if got := e.Header.Get("Content-Type"); got != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", got)
	}
	// The authority the target answers on is its own, not proxio's.
	if strings.Contains(e.Host, h.server.Listener.Addr().String()) {
		t.Errorf("host = %q, which is proxio's rather than the target's", e.Host)
	}
}

// A target's own query string has to survive the round trip through url=.
func TestTheTargetsQueryStringSurvives(t *testing.T) {
	target := echoTarget(t)
	h := front(t)

	e := decode(t, do(t, h.request(t, http.MethodGet, target.URL+"/search?a=1&b=two+words&c=%26", nil)))
	if e.URI != "/search?a=1&b=two+words&c=%26" {
		t.Errorf("uri = %q, want the query string intact", e.URI)
	}
}

// A credential handed to proxio must not be handed on to a stranger.
// proxio's parameters live in proxio's own URL, and the target's live in url=. Nothing of
// proxio's should show up on the other side.
func TestProxiosOwnParametersNeverReachTheTarget(t *testing.T) {
	target := echoTarget(t)
	h := front(t)

	resp := h.get(t, url.Values{
		ParamURL:   {target.URL + "/thing?mine=kept"},
		ParamToken: {h.secret},
		ParamHide:  {"0"},
	})

	e := decode(t, resp)
	if e.URI != "/thing?mine=kept" {
		t.Errorf("uri = %q, want the target's own query string and nothing else", e.URI)
	}
	if strings.Contains(e.URI, h.secret) {
		t.Error("the token reached the target")
	}
	for _, name := range []string{ParamToken, ParamHide} {
		if strings.Contains(e.URI, name+"=") {
			t.Errorf("the %s parameter reached the target: %q", name, e.URI)
		}
	}
	// And it is not in a header either, from any leftover of the old interface.
	for name := range e.Header {
		if strings.HasPrefix(name, "X-Proxio-") {
			t.Errorf("the target was sent %s", name)
		}
	}
}

func TestHopByHopHeadersAreDropped(t *testing.T) {
	target := echoTarget(t)
	h := front(t)

	req := h.request(t, http.MethodGet, target.URL, nil)
	req.Header.Set("Proxy-Authorization", "Basic nope")
	req.Header.Set("Te", "trailers")
	// Named in Connection, so the sender is asking for it to be consumed at this hop. This
	// is the half of the rule that gets forgotten.
	req.Header.Set("Connection", "X-Hop-Header")
	req.Header.Set("X-Hop-Header", "should not cross")

	e := decode(t, do(t, req))
	for _, name := range []string{"Proxy-Authorization", "Te", "Connection", "X-Hop-Header"} {
		if got := e.Header.Get(name); got != "" {
			t.Errorf("%s crossed to the target: %q", name, got)
		}
	}
}

func TestForwardedForIsAppended(t *testing.T) {
	target := echoTarget(t)
	h := front(t)

	req := h.request(t, http.MethodGet, target.URL, nil)
	req.Header.Set("X-Forwarded-For", "203.0.113.9")

	e := decode(t, do(t, req))
	xff := e.Header.Get("X-Forwarded-For")
	if !strings.HasPrefix(xff, "203.0.113.9, ") {
		t.Errorf("X-Forwarded-For = %q, want the inbound chain kept and proxio's peer appended", xff)
	}
	if e.Header.Get("X-Real-Ip") == "" {
		t.Error("X-Real-Ip was not set")
	}
	// X-Real-Ip names the peer proxio is talking to, never a value a caller supplied.
	if e.Header.Get("X-Real-Ip") == "203.0.113.9" {
		t.Error("X-Real-Ip was built from a header a stranger sent")
	}
	if !strings.Contains(e.Header.Get("Via"), "proxio") {
		t.Errorf("Via = %q, want proxio named", e.Header.Get("Via"))
	}
}

// The header's entire purpose: with it on, the target learns nothing about who asked.
func TestHideRemovesEveryTraceOfTheCaller(t *testing.T) {
	target := echoTarget(t)
	h := front(t)

	for _, on := range []string{"1", "true", "yes", "on", "TRUE", "On"} {
		t.Run(on, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, h.link(url.Values{
				ParamURL: {target.URL}, ParamToken: {h.secret}, ParamHide: {on},
			}), nil)
			if err != nil {
				t.Fatal(err)
			}
			// Arrived already carrying a chain, which is the case that a "do not add"
			// implementation gets wrong: the address goes out in the header it came in.
			// A proxio in front of another proxio is exactly this.
			req.Header.Set("X-Forwarded-For", "203.0.113.9")
			req.Header.Set("X-Real-Ip", "203.0.113.9")
			req.Header.Set("X-Forwarded-Host", "somewhere.example")
			req.Header.Set("X-Forwarded-Proto", "https")
			req.Header.Set("Forwarded", "for=203.0.113.9")
			req.Header.Set("Via", "1.1 someone-else")

			e := decode(t, do(t, req))
			for _, name := range forwarding {
				if got := e.Header.Get(name); got != "" {
					t.Errorf("%s reached the target as %q", name, got)
				}
			}
		})
	}
}

func TestHideOffStillForwards(t *testing.T) {
	target := echoTarget(t)
	h := front(t)

	for _, off := range []string{"0", "false", "no", "off"} {
		t.Run(off, func(t *testing.T) {
			e := decode(t, h.get(t, url.Values{
				ParamURL: {target.URL}, ParamToken: {h.secret}, ParamHide: {off},
			}))
			if e.Header.Get("X-Forwarded-For") == "" {
				t.Errorf("%s=%q switched forwarding off", ParamHide, off)
			}
		})
	}

	t.Run("absent", func(t *testing.T) {
		e := decode(t, do(t, h.request(t, http.MethodGet, target.URL, nil)))
		if e.Header.Get("X-Forwarded-For") == "" {
			t.Error("forwarding was off with no hide parameter at all")
		}
	})
}

// The silent-off failure. A typo in a parameter whose only job is concealment has to be loud.
//
// The empty case is the one a query parameter has and a header did not: `?hide` with no value
// is the natural way to write a flag, and taking it as off would send the address of somebody
// who believed they had hidden it.
func TestHideRefusesAValueItDoesNotKnow(t *testing.T) {
	target := echoTarget(t)
	h := front(t)

	for _, bad := range []string{"ture", "yep", "2", "hide", ""} {
		t.Run("value_"+bad, func(t *testing.T) {
			resp := h.get(t, url.Values{
				ParamURL: {target.URL}, ParamToken: {h.secret}, ParamHide: {bad},
			})
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %q was accepted and forwarding stayed on", resp.StatusCode, bad)
			}
			if resp.Header.Get(HeaderError) != kindRequest {
				t.Errorf("%s = %q, want %q", HeaderError, resp.Header.Get(HeaderError), kindRequest)
			}
		})
	}
}

func TestHideGivenTwiceIsRefused(t *testing.T) {
	target := echoTarget(t)
	h := front(t)

	resp := h.get(t, url.Values{
		ParamURL: {target.URL}, ParamToken: {h.secret}, ParamHide: {"1", "0"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// Which copy of a token wins is an access decision, so there is no answer to it but refusal.
func TestTokenGivenTwiceIsRefused(t *testing.T) {
	target := echoTarget(t)
	h := front(t)

	resp := h.get(t, url.Values{
		ParamURL: {target.URL}, ParamToken: {h.secret, "px_something_else"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAuthIsRequired(t *testing.T) {
	target := echoTarget(t)
	h := front(t)

	cases := map[string]string{
		"missing": "",
		"wrong":   "px_not-a-real-token",
		"prefix":  h.secret[:len(h.secret)-1],
	}
	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			params := url.Values{ParamURL: {target.URL}}
			if token != "" {
				params.Set(ParamToken, token)
			}

			resp := h.get(t, params)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if resp.Header.Get(HeaderError) != kindAuth {
				t.Errorf("%s = %q, want %q", HeaderError, resp.Header.Get(HeaderError), kindAuth)
			}
		})
	}
}

// The same answer for "no token" and "wrong token": the difference tells whoever is guessing
// which half they got right.
func TestAuthFailuresAreIndistinguishable(t *testing.T) {
	target := echoTarget(t)
	h := front(t)

	a := h.get(t, url.Values{ParamURL: {target.URL}})
	b := h.get(t, url.Values{ParamURL: {target.URL}, ParamToken: {"px_wrong"}})

	if a.StatusCode != b.StatusCode {
		t.Errorf("statuses differ: %d and %d", a.StatusCode, b.StatusCode)
	}
	if body(t, a) != body(t, b) {
		t.Error("the two failures answer with different bodies")
	}
}

func TestTargetURLIsValidated(t *testing.T) {
	h := front(t)

	cases := map[string][]string{
		"missing":    nil,
		"empty":      {""},
		"duplicated": {"https://a.example", "https://b.example"},
		"relative":   {"/just/a/path"},
		"schemeless": {"example.com/thing"},
		"ftp":        {"ftp://example.com/thing"},
		"file":       {"file:///etc/passwd"},
		"nohost":     {"http:///path"},
	}
	for name, values := range cases {
		t.Run(name, func(t *testing.T) {
			params := url.Values{ParamToken: {h.secret}}
			if values != nil {
				params[ParamURL] = values
			}

			resp := h.get(t, params)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if resp.Header.Get(HeaderError) != kindRequest {
				t.Errorf("%s = %q, want %q", HeaderError, resp.Header.Get(HeaderError), kindRequest)
			}
		})
	}
}

// The one that regresses invisibly: every test that only checks the final body still passes
// when the response is buffered instead of streamed.
//
// Both halves are asserted separately because they fail differently. Without the flush the
// status line itself is held in Go's response buffer, so the failure is that Do never
// returns — which is why this test bounds the request with a context rather than trusting a
// read deadline further down.
func TestTheResponseIsStreamed(t *testing.T) {
	release := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "first\n")
		_ = http.NewResponseController(w).Flush()
		<-release
		_, _ = io.WriteString(w, "second\n")
	}))
	t.Cleanup(target.Close)
	released := false
	t.Cleanup(func() {
		if !released {
			close(release)
		}
	})

	h := front(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := noFollow.Do(h.request(t, http.MethodGet, target.URL, nil).WithContext(ctx))
	if err != nil {
		t.Fatalf("no response arrived while the target was still holding it open, so even the status line is being buffered rather than streamed: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, len("first\n"))
	read := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(resp.Body, buf)
		read <- err
	}()

	select {
	case err := <-read:
		if err != nil {
			t.Fatalf("reading the first chunk: %v", err)
		}
		if string(buf) != "first\n" {
			t.Fatalf("first chunk = %q, want %q", buf, "first\n")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first chunk never arrived while the target was still holding the response open: it is being buffered rather than streamed")
	}

	released = true
	close(release)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading the rest: %v", err)
	}
	if string(rest) != "second\n" {
		t.Errorf("rest = %q, want %q", rest, "second\n")
	}
}

// Without this, proxio keeps pulling a file nobody is reading.
func TestTheCallerHangingUpCancelsTheUpstream(t *testing.T) {
	gone := make(chan struct{})
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "x")
		_ = http.NewResponseController(w).Flush()
		<-r.Context().Done()
		close(gone)
	}))
	t.Cleanup(target.Close)

	h := front(t)
	ctx, cancel := context.WithCancel(context.Background())
	req := h.request(t, http.MethodGet, target.URL, nil).WithContext(ctx)

	resp, err := noFollow.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("request: %v", err)
	}
	// Read the first byte, so the chain is established all the way through before the hang-up.
	if _, err := io.ReadFull(resp.Body, make([]byte, 1)); err != nil {
		cancel()
		t.Fatalf("reading the first byte: %v", err)
	}
	cancel()
	resp.Body.Close()

	select {
	case <-gone:
	case <-time.After(5 * time.Second):
		t.Fatal("the target was still serving after the caller hung up: the cancellation did not cross")
	}
}

// DisableCompression: what the caller asked for is what the caller gets, encoded as the
// target encoded it.
func TestCompressedResponsesCrossUndecoded(t *testing.T) {
	const payload = "the quick brown fox, repeated enough to be worth compressing. "
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			t.Errorf("Accept-Encoding did not cross: %q", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "text/plain")
		gz := gzip.NewWriter(w)
		_, _ = io.WriteString(gz, strings.Repeat(payload, 20))
		gz.Close()
	}))
	t.Cleanup(target.Close)

	h := front(t)
	req := h.request(t, http.MethodGet, target.URL, nil)
	req.Header.Set("Accept-Encoding", "gzip")

	resp := do(t, req)
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip: proxio decompressed on the caller's behalf", got)
	}
	gz, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("the body is not gzip: %v", err)
	}
	raw, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != strings.Repeat(payload, 20) {
		t.Error("the decompressed body does not match what the target sent")
	}
}

func redirectTarget(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/start", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/end", http.StatusFound)
	})
	mux.HandleFunc("/end", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "arrived")
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestRedirectsAreFollowed(t *testing.T) {
	target := redirectTarget(t)
	h := front(t)

	resp := do(t, h.request(t, http.MethodGet, target.URL+"/start", nil))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := body(t, resp); got != "arrived" {
		t.Errorf("body = %q, want %q", got, "arrived")
	}
}

func TestTooManyRedirectsIsRefused(t *testing.T) {
	var target *httptest.Server
	target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/again", http.StatusFound)
	}))
	t.Cleanup(target.Close)

	h := front(t, func(o *Options) { o.MaxRedirects = 3 })
	resp := do(t, h.request(t, http.MethodGet, target.URL, nil))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if got := body(t, resp); !strings.Contains(got, "3 redirects") {
		t.Errorf("body = %q, want it to name the limit", got)
	}
}

// Zero is the transparent behaviour, for anybody who wants the other answer.
func TestZeroRedirectsPassThe3xxThrough(t *testing.T) {
	target := redirectTarget(t)
	h := front(t, func(o *Options) { o.MaxRedirects = 0 })

	resp := do(t, h.request(t, http.MethodGet, target.URL+"/start", nil))
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want 302", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/end" {
		t.Errorf("Location = %q, want it untouched", got)
	}
}

func TestUnreachableTargetIsBadGateway(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := dead.URL
	dead.Close() // nothing is listening there any more

	h := front(t)
	resp := do(t, h.request(t, http.MethodGet, addr, nil))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	if resp.Header.Get(HeaderError) != kindUpstream {
		t.Errorf("%s = %q, want %q", HeaderError, resp.Header.Get(HeaderError), kindUpstream)
	}
	// The caller must be able to tell a proxy failure from a 502 the target itself returned.
	if !strings.Contains(body(t, resp), "connect") && !strings.Contains(body(t, resp), "refused") {
		t.Logf("body = %q", body(t, resp))
	}
}

func TestATargets502IsNotMarkedAsProxios(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "the target's own words")
	}))
	t.Cleanup(target.Close)

	h := front(t)
	resp := do(t, h.request(t, http.MethodGet, target.URL, nil))
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want the target's 502", resp.StatusCode)
	}
	if got := resp.Header.Get(HeaderError); got != "" {
		t.Errorf("%s = %q on a relayed response, which makes the target's failure look like proxio's", HeaderError, got)
	}
	if got := body(t, resp); got != "the target's own words" {
		t.Errorf("body = %q, want the target's", got)
	}
}

func TestResponseHeadersAndStatusCross(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Custom", "from the target")
		w.Header().Add("Set-Cookie", "a=1")
		w.Header().Add("Set-Cookie", "b=2")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "short and stout")
	}))
	t.Cleanup(target.Close)

	h := front(t)
	resp := do(t, h.request(t, http.MethodGet, target.URL, nil))
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want 418", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Custom"); got != "from the target" {
		t.Errorf("X-Custom = %q", got)
	}
	if got := resp.Header.Values("Set-Cookie"); len(got) != 2 {
		t.Errorf("Set-Cookie = %v, want both", got)
	}
	if got := body(t, resp); got != "short and stout" {
		t.Errorf("body = %q", got)
	}
}

// A GET with no body must not go out chunked: some servers answer that with a 400.
func TestABodylessRequestSendsNoBody(t *testing.T) {
	target := echoTarget(t)
	h := front(t)

	e := decode(t, do(t, h.request(t, http.MethodGet, target.URL, nil)))
	if len(e.TransferEncoding) != 0 {
		t.Errorf("Transfer-Encoding = %v on a GET with no body", e.TransferEncoding)
	}
	if e.Body != "" {
		t.Errorf("body = %q, want empty", e.Body)
	}
}

// The reason the token store is a file: `docker exec` is a second process.
func TestANewTokenIsAcceptedWithoutARestart(t *testing.T) {
	target := echoTarget(t)
	h := front(t)

	shell, err := tokens.Open(h.dir) // as `docker exec proxio proxio token create` would
	if err != nil {
		t.Fatal(err)
	}
	_, secret, err := shell.Create("added_later")
	if err != nil {
		t.Fatal(err)
	}

	added := url.Values{ParamURL: {target.URL}, ParamToken: {secret}}
	if resp := h.get(t, added); resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: the running server did not see a token created beside it", resp.StatusCode)
	}

	if err := shell.Delete("added_later"); err != nil {
		t.Fatal(err)
	}
	if resp := h.get(t, added); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: a withdrawn token still works", resp.StatusCode)
	}
}

func TestHealthzNeedsNoToken(t *testing.T) {
	h := front(t)

	resp, err := noFollow.Get(h.server.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["ok"] != true {
		t.Errorf("body = %v, want ok true", got)
	}
	if got["version"] == "" {
		t.Error("no version reported")
	}
}

// Exact patterns, so there is no second way in.
func TestOtherPathsAreNotFound(t *testing.T) {
	h := front(t)

	for _, path := range []string{"/", "/proxy/", "/proxy/extra", "/anything"} {
		t.Run(path, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, h.server.URL+path+"?"+ParamToken+"="+h.secret, nil)
			if err != nil {
				t.Fatal(err)
			}

			if resp := do(t, req); resp.StatusCode != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", resp.StatusCode)
			}
		})
	}
}

// OPTIONS is relayed like any other method rather than answered here, which is what rules
// out CORS handling and is worth having a test say out loud.
func TestOptionsIsRelayedRatherThanAnswered(t *testing.T) {
	target := echoTarget(t)
	h := front(t)

	resp := do(t, h.request(t, http.MethodOptions, target.URL, nil))
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("proxio answered the preflight itself: %q", got)
	}
	if e := decode(t, resp); e.Method != http.MethodOptions {
		t.Errorf("the target saw %q, want OPTIONS", e.Method)
	}
}

// Chaining is what the query-parameter interface buys, and it is not merely more convenient
// than a header: with the token in a header it is impossible. The first proxio would have to
// forward its own credential for the second to see one, and forwarding a credential to
// whatever host the caller named is the one thing a proxy must never do.
//
// With the token in the URL, each hop carries its own and neither knows the other's.
func TestProxiosChain(t *testing.T) {
	target := echoTarget(t)
	inner := front(t) // reached only by the outer one
	outer := front(t) // the one the caller talks to, with its own separate token store

	// The outer proxio's target is the inner proxio's own /proxy URL, token and all.
	resp := do(t, outer.request(t, http.MethodPost,
		inner.url(target.URL+"/thing"), strings.NewReader("through both")))

	e := decode(t, resp)
	if e.Method != http.MethodPost {
		t.Errorf("method = %q, want POST after two hops", e.Method)
	}
	if e.Body != "through both" {
		t.Errorf("body = %q, want it intact after two hops", e.Body)
	}
	if e.URI != "/thing" {
		t.Errorf("uri = %q, want /thing — the inner proxio should have unwrapped it", e.URI)
	}

	// One Via per hop, so a chain reads as a chain rather than as its last link.
	if via := e.Header.Values("Via"); len(via) != 2 {
		t.Errorf("Via = %v, want one entry per hop", via)
	}
	// And the forwarded-for chain grew at each hop rather than being overwritten.
	if xff := e.Header.Get("X-Forwarded-For"); !strings.Contains(xff, ",") {
		t.Errorf("X-Forwarded-For = %q, want an entry per hop", xff)
	}

	// Neither hop's credential ends up at the target.
	for name, secret := range map[string]string{"outer": outer.secret, "inner": inner.secret} {
		if strings.Contains(e.URI, secret) {
			t.Errorf("the %s proxio's token reached the target", name)
		}
	}
}

// Each hop decides for itself, so hiding at the front does not stop the next hop identifying
// itself to the one after.
func TestHideAppliesPerHop(t *testing.T) {
	target := echoTarget(t)
	inner := front(t)
	outer := front(t)

	resp := outer.get(t, url.Values{
		ParamURL:   {inner.url(target.URL)},
		ParamToken: {outer.secret},
		ParamHide:  {"1"},
	})

	e := decode(t, resp)
	// The outer hop told the inner one nothing, so the chain the target sees starts at the
	// outer proxio rather than at the caller: one entry, not two.
	xff := e.Header.Get("X-Forwarded-For")
	if xff == "" {
		t.Fatal("the inner proxio forwarded nothing, though it was not asked to hide")
	}
	if strings.Contains(xff, ",") {
		t.Errorf("X-Forwarded-For = %q, want only the outer proxio — the caller was hidden from it", xff)
	}
	if via := e.Header.Values("Via"); len(via) != 1 {
		t.Errorf("Via = %v, want only the inner hop's entry", via)
	}
}

// The url field is the *target's* URL. proxio's own /proxy request URI — which is where its
// own token actually is — is never logged in any form, and the target's query string is left
// out whole rather than filtered.
//
// Dropping the query rather than picking secrets out of it is what makes this safe without
// proxio having to know whose secret is whose: a chained target carries the next hop's token
// and the entire nested URL, and both live in the query.
func TestTheTargetsQueryStringIsNeverLogged(t *testing.T) {
	target := echoTarget(t)
	h := front(t)

	// Shaped like a chained hop's target: another proxio's /proxy URL with a token on it.
	chained := target.URL + "/proxy?keep=this&" + ParamToken + "=px_INNER_SECRET"
	e := decode(t, do(t, h.request(t, http.MethodGet, chained, nil)))

	// Leaving it out of the log must not leave it out of the request, or the next hop would
	// be handed a token called {redacted}.
	if !strings.Contains(e.URI, "px_INNER_SECRET") || !strings.Contains(e.URI, "keep=this") {
		t.Errorf("uri = %q: the log rendering changed what was actually sent", e.URI)
	}

	logs := h.waitForLog(t)
	for what, secret := range map[string]string{
		"the next hop's token":        "px_INNER_SECRET",
		"proxio's own token":          h.secret,
		"an ordinary query parameter": "keep=this",
	} {
		if strings.Contains(logs, secret) {
			t.Errorf("%s is in the access log:\n%s", what, logs)
		}
	}

	// Enough is kept to know what was asked for, and that something was left out — without
	// the marker a reader cannot tell this from a URL that had no query at all.
	if !strings.Contains(logs, "/proxy?{redacted}") {
		t.Errorf("the log does not show the path and that a query was omitted:\n%s", logs)
	}
	// The label is how a token appears in a log. That is what labels are for.
	if !strings.Contains(logs, "test_token") {
		t.Errorf("the token's label is missing, so the line names nobody:\n%s", logs)
	}
	// The method is its own field rather than something to read out of the URL.
	if !strings.Contains(logs, `"method":"GET"`) {
		t.Errorf("the method is missing from the line:\n%s", logs)
	}
}

// Userinfo is a credential too, and building the logged URL from its parts drops it.
func TestUserinfoIsNotLogged(t *testing.T) {
	h := front(t)

	// Nothing is listening; the log line is written either way, which is the point.
	h.get(t, url.Values{
		ParamURL:   {"https://someone:hunter2@127.0.0.1:1/private"},
		ParamToken: {h.secret},
	})

	logs := h.waitForLog(t)
	if strings.Contains(logs, "hunter2") {
		t.Errorf("a password in the target URL is in the access log:\n%s", logs)
	}
	if !strings.Contains(logs, "127.0.0.1:1/private") {
		t.Errorf("the host and path were dropped along with the userinfo:\n%s", logs)
	}
}

// With the query gone the rest is short by construction, so this almost never fires — which
// is not the same as never.
func TestALongPathIsTruncatedInTheLog(t *testing.T) {
	target := echoTarget(t)
	h := front(t)

	padding := strings.Repeat("x", 2*loggedPathMax)
	do(t, h.request(t, http.MethodGet, target.URL+"/"+padding, nil))

	logs := h.waitForLog(t)
	if strings.Contains(logs, padding) {
		t.Error("a path longer than the limit was logged whole")
	}
	if !strings.Contains(logs, "/xxx") {
		t.Errorf("the log kept too little to be useful:\n%s", logs)
	}
}

// CORS is the target's business, and proxio neither adds it nor takes it away.
//
// This is worth a test because it is the one place where "every header crosses" has a
// consequence nobody predicts from the rule: a browser checks the response it received for
// Access-Control-Allow-Origin without caring which server sent it, so relaying the target's
// header is what makes fetch() through proxio work at all. Strip it and every CORS-permissive
// API becomes unreachable from a page; synthesise it and proxio would be overriding a policy
// that is not its to set.
func TestTheTargetsCORSHeadersCross(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			// The preflight is answered by the target, because proxio relays OPTIONS like
			// any other method rather than intercepting it.
			w.Header().Set("Access-Control-Allow-Origin", "https://page.example")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if got := r.Header.Get("Origin"); got != "https://page.example" {
			t.Errorf("the target saw Origin %q, want it forwarded", got)
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Expose-Headers", "X-Total")
		w.Header().Set("X-Total", "7")
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(target.Close)

	h := front(t)

	req := h.request(t, http.MethodGet, target.URL, nil)
	req.Header.Set("Origin", "https://page.example")
	resp := do(t, req)
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the target's; a browser cannot read the response without it", got)
	}
	if got := resp.Header.Get("Access-Control-Expose-Headers"); got != "X-Total" {
		t.Errorf("Access-Control-Expose-Headers = %q, want the target's", got)
	}

	// A preflight goes to the target and its answer comes back intact, so a fetch() that
	// needs one completes end to end.
	pre := h.request(t, http.MethodOptions, target.URL, nil)
	pre.Header.Set("Origin", "https://page.example")
	pre.Header.Set("Access-Control-Request-Method", "POST")
	pr := do(t, pre)
	if pr.StatusCode != http.StatusNoContent {
		t.Errorf("preflight status = %d, want the target's 204", pr.StatusCode)
	}
	if got := pr.Header.Get("Access-Control-Allow-Methods"); got != "GET, POST" {
		t.Errorf("Access-Control-Allow-Methods = %q, want the target's", got)
	}
}

// The other half: a target with no CORS policy does not acquire one by being proxied.
func TestProxioInventsNoCORSPolicy(t *testing.T) {
	target := echoTarget(t) // sends no Access-Control-* at all
	h := front(t)

	req := h.request(t, http.MethodGet, target.URL, nil)
	req.Header.Set("Origin", "https://page.example")

	resp := do(t, req)
	for _, name := range []string{
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Credentials",
		"Access-Control-Expose-Headers",
	} {
		if got := resp.Header.Get(name); got != "" {
			t.Errorf("proxio added %s: %q — that is the target's policy to set, not proxio's", name, got)
		}
	}
}
