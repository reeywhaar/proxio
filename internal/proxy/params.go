package proxy

import (
	"fmt"
	"net/url"
	"strings"
)

// The three query parameters proxio has of its own. Everything else in a request belongs
// either to the caller or to the target.
const (
	ParamURL   = "url"
	ParamToken = "token"
	ParamHide  = "hide"
)

// one reads a parameter that may appear at most once.
//
// Repeating it is refused rather than resolved, for all three: which copy wins is a
// request-smuggling question, and every answer to it is a bug somebody eventually finds. It
// matters most for token, where "first wins" and "last wins" are two different access
// decisions from the same URL.
func one(q url.Values, name string) (string, bool, error) {
	vs, ok := q[name]
	if !ok || len(vs) == 0 {
		return "", false, nil
	}
	if len(vs) > 1 {
		return "", false, fmt.Errorf("the %s parameter was given more than once", name)
	}
	return vs[0], true, nil
}

// targetURL reads the URL proxio was asked to fetch.
//
// The target's own query string survives because url= carries a single percent-encoded
// value: a target with ?a=1&b=2 arrives as %3Fa%3D1%26b%3D2 and cannot be confused for
// proxio's own parameters, nor they for it.
func targetURL(q url.Values) (*url.URL, error) {
	raw, ok, err := one(q, ParamURL)
	switch {
	case err != nil:
		return nil, err
	case !ok:
		return nil, fmt.Errorf("the %s parameter is required: /proxy?%s=<percent-encoded absolute URL>&%s=<token>", ParamURL, ParamURL, ParamToken)
	case raw == "":
		return nil, fmt.Errorf("the %s parameter is empty", ParamURL)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("the %s parameter is not a URL: %w", ParamURL, err)
	}
	switch {
	case !u.IsAbs():
		// Resolved against proxio's own origin, a relative URL would make proxio fetch
		// itself. That is a loop, not a proxy.
		return nil, fmt.Errorf("the %s parameter %q is not absolute; it needs a scheme and a host", ParamURL, raw)
	case u.Scheme != "http" && u.Scheme != "https":
		return nil, fmt.Errorf("the %s parameter %q is %s, and proxio speaks only http and https", ParamURL, raw, u.Scheme)
	case u.Host == "":
		return nil, fmt.Errorf("the %s parameter %q names no host", ParamURL, raw)
	}
	return u, nil
}

// token reads the credential. An absent one comes back empty and is refused by the caller as
// an unknown token would be, so "no token" and "wrong token" stay indistinguishable.
func token(q url.Values) (string, error) {
	v, _, err := one(q, ParamToken)
	return v, err
}

// hidden reads whether the target should be told anything about who asked.
//
// Both vocabularies are enumerated and everything else is refused, **including an empty
// value**, which is the case a query parameter has and a header did not: `?hide` with no
// value is the natural way to write a flag, and under a truthy-only rule it would be
// silently off with the caller's address going out. A parameter whose only job is
// concealment has to fail loudly when it is not understood, so `?hide` is a 400 that says to
// write `hide=1`.
func hidden(q url.Values) (bool, error) {
	v, ok, err := one(q, ParamHide)
	if err != nil || !ok {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off":
		return false, nil
	case "1", "true", "yes", "on":
		return true, nil
	default:
		return false, fmt.Errorf("the %s parameter %q is not a value; write one of %s=1, true, yes, on, 0, false, no, off", ParamHide, v, ParamHide)
	}
}
