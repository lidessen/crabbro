package inspect

import (
	"net/http"
	"net/url"
	"strings"
)

// Class is what the proxy can infer from headers it already has.
type Class struct {
	ResourceType        string
	Origin              string
	PageURL             string
	InitiatorURL        string
	Confidence          string
	InitiatorConfidence string
}

// Classify reads resource type, page, and initiator from request headers.
// Sec-Fetch-Dest: empty is xhr. A missing Sec-Fetch-Dest is unknown.
// x-chiling-initiator and x-chiling-page are extension stamps. Callers strip
// them before the request is forwarded.
func Classify(h http.Header, rawURL string) Class {
	if h == nil {
		h = http.Header{}
	}
	dest := strings.ToLower(strings.TrimSpace(h.Get("Sec-Fetch-Dest")))
	mode := strings.ToLower(strings.TrimSpace(h.Get("Sec-Fetch-Mode")))
	out := Class{
		ResourceType:        resourceType(dest, mode, h),
		Confidence:          "header",
		InitiatorConfidence: "none",
	}
	origin := strings.TrimSpace(h.Get("Origin"))
	if origin == "null" {
		origin = ""
	}
	referer := strings.TrimSpace(h.Get("Referer"))
	if referer == "" {
		referer = strings.TrimSpace(h.Get("Referrer"))
	}
	switch {
	case dest == "document" || mode == "navigate":
		out.PageURL = rawURL
		if origin == "" {
			origin = originOf(rawURL)
		}
	case referer != "":
		out.PageURL = referer
		if origin == "" {
			origin = originOf(referer)
		}
	}
	if origin == "" && out.PageURL != "" {
		origin = originOf(out.PageURL)
	}
	out.Origin = origin
	if page := strings.TrimSpace(h.Get("X-Chiling-Page")); page != "" {
		out.PageURL = page
		out.Confidence = "extension"
		if out.Origin == "" {
			out.Origin = originOf(page)
		}
	}
	if init := strings.TrimSpace(h.Get("X-Chiling-Initiator")); init != "" {
		out.InitiatorURL = init
		out.InitiatorConfidence = "extension"
	}
	return out
}

func resourceType(dest, mode string, h http.Header) string {
	if strings.EqualFold(h.Get("Upgrade"), "websocket") || mode == "websocket" {
		return "websocket"
	}
	switch dest {
	case "":
		if h.Get("Sec-Fetch-Dest") == "" && mode == "" && h.Get("Sec-Fetch-Site") == "" && h.Get("Sec-Fetch-Mode") == "" {
			// Header absent, not the explicit empty token.
			if _, ok := h["Sec-Fetch-Dest"]; ok {
				return "xhr"
			}
			return "unknown"
		}
		return "xhr"
	case "empty":
		return "xhr"
	case "document":
		return "document"
	case "script":
		return "script"
	case "style":
		return "stylesheet"
	case "image":
		return "image"
	case "font":
		return "font"
	case "video", "audio":
		return "media"
	case "worker", "serviceworker", "sharedworker":
		return "worker"
	case "websocket":
		return "websocket"
	default:
		return "other"
	}
}

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host)
}

// StripStamps removes extension hints so the origin never sees them.
func StripStamps(h http.Header) {
	if h == nil {
		return
	}
	h.Del("X-Chiling-Initiator")
	h.Del("X-Chiling-Page")
}
