package codec

import (
	"bytes"
	"strings"

	"github.com/lidessen/crabbro/internal/gateway/ir"
	"github.com/lidessen/crabbro/internal/gateway/protocol"
)

// StreamDeltaTap detects the first substantive event in downstream SSE bytes.
type StreamDeltaTap struct {
	decoder streamDecoder
	proto   string
	buf     []byte
	event   string
	data    []byte
	found   bool
}

func NewStreamDeltaTap(proto string) *StreamDeltaTap {
	t := &StreamDeltaTap{proto: proto}
	if c, err := codecFor(proto); err == nil {
		t.decoder = c.NewStreamDecoder()
	}
	return t
}

// Write observes successfully written bytes without delaying their delivery.
func (t *StreamDeltaTap) Write(p []byte) (int, error) {
	if t.found || t.decoder == nil {
		return len(p), nil
	}
	t.buf = append(t.buf, p...)
	for {
		idx := bytes.IndexByte(t.buf, '\n')
		if idx < 0 {
			break
		}
		line := string(bytes.TrimSuffix(t.buf[:idx], []byte("\r")))
		t.buf = t.buf[idx+1:]
		switch {
		case line == "":
			t.flushEvent()
		case strings.HasPrefix(line, "event:"):
			t.event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			if len(t.data) > 0 {
				t.data = append(t.data, '\n')
			}
			t.data = append(t.data, strings.TrimSpace(strings.TrimPrefix(line, "data:"))...)
		}
		if t.found {
			t.buf = nil
			break
		}
	}
	return len(p), nil
}

func (t *StreamDeltaTap) flushEvent() {
	if len(t.data) > 0 {
		events, _, _ := t.decoder.Feed(t.event, string(t.data))
		for _, event := range events {
			switch event.Kind {
			case ir.EventTextDelta, ir.EventThinkingDelta, ir.EventToolCallDelta:
				t.found = true
			case ir.EventToolCallStart:
				// Anthropic block starts are metadata; wait for an actual delta.
				t.found = t.proto != protocol.ProtocolAnthropic
			}
			if t.found {
				break
			}
		}
	}
	t.event = ""
	t.data = nil
}

func (t *StreamDeltaTap) Found() bool { return t.found }
