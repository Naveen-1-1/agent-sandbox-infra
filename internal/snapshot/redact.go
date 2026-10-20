package snapshot

import (
	"net/url"
	"regexp"
)

var urlUserinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/@\s]+@`)

// Redact removes credentials embedded in URLs anywhere in s, so they never
// reach logs, errors or status.
func Redact(s string) string { return urlUserinfo.ReplaceAllString(s, "${1}") }

// StripCredentials returns rawURL without its user:password part.
func StripCredentials(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.User == nil {
		return Redact(rawURL)
	}
	u.User = nil
	return u.String()
}
