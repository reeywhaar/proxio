package proxy

import "time"

// record is what one request turns into in the log.
//
// Built up as the request is handled, so a failure halfway through still logs everything
// known by then rather than nothing.
type record struct {
	label     string
	method    string
	target    string
	final     string
	client    string
	status    int
	bytes     int64
	redirects int64
	hidden    bool
}

// attrs is the log line.
//
// Two fields are worth explaining because both look like leaks and are not:
//
// The token field is the label, never the secret — which is what labels are for, and why a
// token has one. And the client address is logged even when hidden is true: HIDE governs
// what the *target* is told, not what the operator can see in their own logs.
//
// The target URL is logged whole, query string included. It is the single most useful field
// a proxy has, and it can carry a secret somebody put in a query parameter. That is a known
// property, written down in docs/deploy.md, rather than a surprise.
func (rec record) attrs(started time.Time, err error) []any {
	attrs := make([]any, 0, 22)
	if rec.label != "" {
		attrs = append(attrs, "token", rec.label)
	}
	attrs = append(attrs, "method", rec.method)
	if rec.target != "" {
		attrs = append(attrs, "url", rec.target)
	}
	if rec.status != 0 {
		attrs = append(attrs, "status", rec.status)
	}
	attrs = append(attrs,
		"bytes", rec.bytes,
		"dur_ms", time.Since(started).Milliseconds(),
		"hidden", rec.hidden,
	)
	if rec.redirects > 0 {
		attrs = append(attrs, "redirects", rec.redirects)
	}
	// Only when a redirect actually moved it. Repeating the target on every line would be a
	// column that is noise on all but a few of them.
	if rec.final != "" {
		attrs = append(attrs, "final_url", rec.final)
	}
	attrs = append(attrs, "client", rec.client)
	if err != nil {
		attrs = append(attrs, "error", err.Error())
	}
	return attrs
}
