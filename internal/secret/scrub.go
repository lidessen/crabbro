package secret

import (
	"github.com/lidessen/crabbro/internal/scrub"
	"net/http"
)

const RedactedPlaceholder = scrub.RedactedPlaceholder

func StripAuthHeaders(h http.Header, extra ...string) http.Header {
	return scrub.StripAuthHeaders(h, extra...)
}
func ScrubHeaders(h http.Header, values []string, extra ...string) http.Header {
	return scrub.ScrubHeaders(h, values, extra...)
}
func ScrubURL(raw string) string                  { return scrub.ScrubURL(raw) }
func ScrubText(s string, values ...string) string { return scrub.ScrubText(s, values...) }
func ScrubAny(v any, values ...string) any        { return scrub.ScrubAny(v, values...) }
