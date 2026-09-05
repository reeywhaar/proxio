package proxy

import (
	"net"
	"net/http"
	"slices"
	"strings"

	"proxio/internal/app"
)

// HeaderError marks a response proxio generated itself.
//
// A response header rather than a body convention, because on a successful proxy the body
// belongs to the target and the status code does too — a caller cannot otherwise tell "the
// proxy failed" from "the target returned 502". proxio never adds this to a relayed
// response, so its presence means the bytes below it are proxio's.
//
// In a chain of proxios it says that the failure belongs to a hop rather than to the final
// target — not which hop. The body says what went wrong, and the hop that generated it is
// the first one whose own request failed.
const HeaderError = "X-Proxio-Error"

// hopByHop belong to a single transport-level connection and must not be forwarded, per RFC
// 9110 §7.6.1. Written in textproto's canonical form, which is why TE is "Te".
var hopByHop = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// forwarding are the headers that say where a request came from. proxio adds them, and the
// hide parameter is the request not to.
//
// Upgrade being hop-by-hop above is also why a WebSocket cannot be relayed through proxio: a
// target never sees the request to switch protocols.
var forwarding = []string{
	"Forwarded",
	"Via",
	"X-Forwarded-For",
	"X-Forwarded-Host",
	"X-Forwarded-Proto",
	"X-Real-Ip",
}

// dropped is every header that must not cross this hop: the fixed list, plus everything the
// sender named in Connection.
//
// The second half is the part that gets forgotten. A proxy that drops the fixed list but
// forwards a header the sender explicitly asked to have consumed here is passing on
// something nobody meant to send onward.
func dropped(h http.Header) map[string]bool {
	out := make(map[string]bool, len(hopByHop)+4)
	for _, k := range hopByHop {
		out[k] = true
	}
	for _, v := range h.Values("Connection") {
		for _, token := range strings.Split(v, ",") {
			if token = strings.TrimSpace(token); token != "" {
				out[http.CanonicalHeaderKey(token)] = true
			}
		}
	}
	return out
}

// requestHeaders builds what the target will see.
//
// Host is not here and does not need to be: Go's server moves the inbound authority to
// Request.Host and deletes it from the map, and the outbound Host comes from the target URL.
func requestHeaders(r *http.Request, hide bool) http.Header {
	skip := dropped(r.Header)
	out := make(http.Header, len(r.Header))
	for k, vs := range r.Header {
		switch {
		case skip[k]:
		case hide && slices.Contains(forwarding, k):
			// Stripped, not merely not-added. A caller asking to be hidden while sitting
			// behind something that already wrote an X-Forwarded-For — another proxio, very
			// possibly — would otherwise have its address forwarded by the header it
			// arrived in.
		default:
			out[k] = slices.Clone(vs)
		}
	}

	// Content-Length travels on Request.ContentLength. A copy here would be a second,
	// possibly disagreeing, statement of the same fact.
	out.Del("Content-Length")

	if hide {
		return out
	}

	ip := clientIP(r)
	// Appended to whatever chain arrived rather than replacing it — proxio is very likely
	// behind caddy, or behind another proxio, and those entries are the earlier hops. Set
	// collapses a header that arrived split over several lines into one well-formed value.
	chain := strings.Join(out.Values("X-Forwarded-For"), ", ")
	if chain != "" {
		chain += ", "
	}
	out.Set("X-Forwarded-For", chain+ip)

	// The peer proxio is actually talking to, not the head of the chain above. proxio cannot
	// tell a genuine entry from one a caller invented, and a header claiming to name the real
	// client should not be built out of a string a stranger typed.
	out.Set("X-Real-Ip", ip)

	// Add, not Set, so a chain of proxios reads as a chain rather than as the last one.
	out.Add("Via", "1.1 "+app.Name)
	return out
}

// copyResponseHeaders relays everything the target said except what belongs to its
// connection with proxio. Set-Cookie, Content-Encoding, Content-Length and Content-Type all
// cross unchanged.
//
// Trailers are not relayed. That is a known gap rather than an oversight.
func copyResponseHeaders(dst, src http.Header) {
	skip := dropped(src)
	for k, vs := range src {
		if skip[k] {
			continue
		}
		dst[k] = slices.Clone(vs)
	}
}

// clientIP is the peer, with the port dropped.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
