// Package proxy is the relay itself: one endpoint that makes somebody else's request.
//
// It is not built on httputil.ReverseProxy, which is the obvious thing to reach for and is
// the wrong shape. That type exists to put a fixed backend behind a fixed front, and its
// Rewrite hook is a place to adjust a request whose destination is already known. Here the
// destination arrives in a query parameter and changes every request, and the rules that
// matter — what an unrecognised hide value does, which errors are proxio's own, whether a
// 3xx is followed — are all things ReverseProxy would be talked out of rather than helped
// with.
package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"proxio/internal/app"
	"proxio/internal/tokens"
)

// What goes in [HeaderError]: whose fault the failure was, in one word.
const (
	kindRequest  = "request"
	kindAuth     = "auth"
	kindUpstream = "upstream"
)

// copyBuffer is the chunk the response is relayed in. Big enough that a fast download is not
// a syscall per few kilobytes, small enough that a slow trickle is not held back waiting to
// fill it.
const copyBuffer = 32 * 1024

// Options is everything the handler is built from.
type Options struct {
	Tokens          *tokens.Store
	Log             *slog.Logger
	DialTimeout     time.Duration
	ResponseTimeout time.Duration

	// MaxRedirects of 0 hands a 3xx back to the caller untouched.
	MaxRedirects int
}

// Handler serves /proxy and /healthz, and nothing else.
type Handler struct {
	tokens *tokens.Store
	log    *slog.Logger
	client *http.Client
	mux    *http.ServeMux
}

// New builds the handler and the client it proxies with.
func New(opts Options) *Handler {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	h := &Handler{
		tokens: opts.Tokens,
		log:    log,
		client: &http.Client{
			Transport:     transport(opts),
			CheckRedirect: checkRedirect(opts.MaxRedirects),
			// No Timeout, deliberately. Client.Timeout covers reading the body, so any value
			// here is a ceiling on how long a proxied response may last — and the failure,
			// a download cut at exactly N seconds, gets blamed on the network for a week.
			// Time to first byte is bounded by the transport instead, which is the part that
			// can be bounded without breaking a stream.
		},
		mux: http.NewServeMux(),
	}

	// Exact patterns, so /proxy/anything is a 404 rather than a second way in.
	h.mux.HandleFunc("/proxy", h.relay)
	h.mux.HandleFunc("/healthz", h.healthz)
	h.mux.HandleFunc("/", h.notFound)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func transport(opts Options) *http.Transport {
	return &http.Transport{
		// Deliberately no Proxy. http.DefaultTransport reads HTTP_PROXY from the
		// environment; proxio *is* the proxy, and chaining it through another one because a
		// variable was set for some unrelated build step is not a thing to find out from a
		// log line.
		DialContext: (&net.Dialer{
			Timeout:   opts.DialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   opts.DialTimeout,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: opts.ResponseTimeout,

		// The caller's own Accept-Encoding crosses untouched, so proxio neither asks for an
		// encoding nobody wanted nor transparently decompresses one that was. A caller that
		// asked for gzip gets gzip bytes and the Content-Encoding that describes them.
		DisableCompression: true,
	}
}

// hopsKey carries a per-request redirect counter through the client, which shares one
// CheckRedirect across every request in flight.
type hopsKey struct{}

func checkRedirect(max int) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if max == 0 {
			// Hand the 3xx straight back, Location and all. Re-encoding it is the caller's
			// business, which is the transparent behaviour for anyone who wants it.
			return http.ErrUseLastResponse
		}
		if len(via) >= max {
			return fmt.Errorf("stopped after %d redirects", max)
		}
		if c, ok := req.Context().Value(hopsKey{}).(*atomic.Int64); ok {
			c.Store(int64(len(via)))
		}
		return nil
	}
}

func (h *Handler) relay(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	rec := record{method: r.Method, client: clientIP(r)}
	q := r.URL.Query()

	secret, err := token(q)
	if err != nil {
		h.fail(w, rec, started, http.StatusBadRequest, kindRequest, err)
		return
	}
	// Authenticated before the target is even parsed, so a stranger learns nothing about
	// what proxio would or would not have been willing to fetch.
	label, ok, terr := h.tokens.Verify(secret)
	if terr != nil {
		// Not fatal: the set already in memory is still used, so a botched hand-edit of
		// tokens.json does not take the proxy down. Loud, because it means the file on disk
		// and the tokens being honoured have stopped agreeing.
		h.log.Error("the token file could not be re-read; carrying on with the tokens already loaded",
			"path", h.tokens.Path(), "error", terr)
	}
	if !ok {
		// One answer for "no token" and for "wrong token". The difference tells whoever is
		// guessing which half they got right.
		h.fail(w, rec, started, http.StatusUnauthorized, kindAuth,
			fmt.Errorf("the %s parameter is missing or names no token", ParamToken))
		return
	}
	rec.label = label

	target, err := targetURL(q)
	if err != nil {
		h.fail(w, rec, started, http.StatusBadRequest, kindRequest, err)
		return
	}
	rec.target = loggableURL(target)

	hide, err := hidden(q)
	if err != nil {
		h.fail(w, rec, started, http.StatusBadRequest, kindRequest, err)
		return
	}
	rec.hidden = hide

	// A body only when there is one. NewRequest handed a reader of unknown length sends
	// Transfer-Encoding: chunked, and a chunked GET is a request some servers answer with a
	// 400 — so a request whose ContentLength is 0 goes out with no body at all. A chunked
	// upload arrives as -1 and stays -1.
	var body io.Reader
	if r.Body != nil && r.ContentLength != 0 {
		body = r.Body
	}

	hops := new(atomic.Int64)
	ctx := context.WithValue(r.Context(), hopsKey{}, hops)
	out, err := http.NewRequestWithContext(ctx, r.Method, target.String(), body)
	if err != nil {
		h.fail(w, rec, started, http.StatusBadRequest, kindRequest, err)
		return
	}
	out.ContentLength = r.ContentLength
	out.Header = requestHeaders(r, hide)

	resp, err := h.client.Do(out)
	if err != nil {
		h.upstreamFailed(w, r, rec, started, err)
		return
	}
	defer resp.Body.Close()

	rec.status = resp.StatusCode
	rec.redirects = hops.Load()
	if resp.Request != nil && resp.Request.URL != nil {
		if final := loggableURL(resp.Request.URL); final != rec.target {
			rec.final = final
		}
	}

	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	n, cerr := flushingCopy(w, resp.Body)
	rec.bytes = n
	if cerr != nil {
		// The status went out long ago and there is nothing left to say with it. The
		// connection closes without a terminating chunk, so the caller sees a truncated body
		// rather than a complete one, and this line is where the reason lives.
		h.log.Warn("the response stopped part way", rec.attrs(started, cerr)...)
		return
	}
	h.log.Info("proxied", rec.attrs(started, nil)...)
}

// loggedPathMax bounds the one part of a logged URL a caller controls the length of.
//
// With the query gone the rest is short by construction, so this almost never fires. It is
// here because "almost never" is not the same as never, and a log line is not the place to
// find out.
const loggedPathMax = 256

// loggableURL is a target URL with everything after the path left out.
//
//	https://api.example.com/things?page=2#top  →  https://api.example.com/things?{redacted}
//
// The query is dropped rather than filtered, which is what makes this safe without proxio
// having to know whose secret is whose. A chained target is another proxio's /proxy URL and
// carries the next hop's token — but that token is *in the query*, along with the entire
// nested URL, so omitting the query omits both the credential and the nesting that would
// otherwise make the line thousands of characters long.
//
// Building the string from the parts rather than trimming the whole also drops userinfo, so
// a target written https://user:password@example.com/ does not put a password in the log.
//
// The marker matters as much as the omission: without it a reader cannot tell a URL that had
// no query from one whose query was taken out, and would go looking for a request that does
// not match anything they sent.
func loggableURL(u *url.URL) string {
	out := u.Scheme + "://" + u.Host + truncate(u.EscapedPath(), loggedPathMax)
	if u.RawQuery != "" {
		out += "?{redacted}"
	}
	if u.Fragment != "" {
		out += "#{redacted}"
	}
	return out
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// Cut on a rune boundary. A path is normally ASCII, but nothing stops a caller putting
	// anything in one, and half a rune in a JSON log is a parse error somewhere downstream.
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// upstreamFailed answers a request that never got a response.
func (h *Handler) upstreamFailed(w http.ResponseWriter, r *http.Request, rec record, started time.Time, err error) {
	// The caller hung up. There is nobody to answer, and writing a status onto a dead
	// connection turns an ordinary event into a log line that reads like a fault.
	if r.Context().Err() != nil {
		h.log.Info("the caller hung up", rec.attrs(started, nil)...)
		return
	}

	status := http.StatusBadGateway
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			status = http.StatusGatewayTimeout
		}
		// The url.Error wrapper repeats the method and the URL the caller supplied. What is
		// worth handing back is the cause.
		err = ue.Err
	}
	h.fail(w, rec, started, status, kindUpstream, err)
}

type errorBody struct {
	Error string `json:"error"`
}

// fail answers with a response proxio generated itself, and says so.
func (h *Handler) fail(w http.ResponseWriter, rec record, started time.Time, status int, kind string, err error) {
	rec.status = status
	w.Header().Set(HeaderError, kind)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: err.Error()})
	h.log.Warn("refused", rec.attrs(started, err)...)
}

func (h *Handler) healthz(w http.ResponseWriter, _ *http.Request) {
	// Unauthenticated, so a container healthcheck and a load balancer can both reach it. It
	// reports that the process is answering, which is the only thing proxio can honestly
	// promise: it has no database to check and no upstream of its own.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "version": app.Version})
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	h.fail(w, record{method: r.Method, client: clientIP(r)}, time.Now(),
		http.StatusNotFound, kindRequest, errors.New("proxio serves /proxy and /healthz"))
}

// flushingCopy relays the body a chunk at a time, flushing each one.
//
// The flush is the whole of what "streaming" means here. Without it Go's response buffer
// holds up to four kilobytes before anything reaches the caller, which turns a
// server-sent-event stream into silence and looks exactly like a slow origin — a failure
// that regresses invisibly, because every test that only checks the final body still passes.
func flushingCopy(w http.ResponseWriter, src io.Reader) (int64, error) {
	rc := http.NewResponseController(w)
	buf := make([]byte, copyBuffer)

	var total int64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			written, werr := w.Write(buf[:n])
			total += int64(written)
			if werr != nil {
				return total, werr
			}
			if ferr := rc.Flush(); ferr != nil && !errors.Is(ferr, http.ErrNotSupported) {
				return total, ferr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return total, nil
			}
			return total, rerr
		}
	}
}
