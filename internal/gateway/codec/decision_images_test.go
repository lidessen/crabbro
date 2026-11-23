package codec

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/lidessen/crabbro/internal/gateway/config"
	"github.com/lidessen/crabbro/internal/gateway/router"
)

func TestDecisionImageTranslation(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	data := base64.StdEncoding.EncodeToString(buf.Bytes())
	raw, _ := json.Marshal(map[string]any{"model": "decision", "state": map[string]any{"text": "hello", "image": "window"}, "assets": map[string]any{"window": map[string]any{"type": "image", "format": "png", "data_base64": data}}, "questions": map[string]any{"ok": map[string]any{"type": "noul", "instructions": "ok?"}}})
	ingress := IngressInfo{Family: config.FamilyDecision, Protocol: config.ProtocolTypeSafeJev, Action: "systemone"}
	for _, tc := range []struct{ proto, expected string }{{config.ProtocolOpenAIDecisions, "data:image/png;base64," + data}, {config.ProtocolGeminiDecisions, data}} {
		t.Run(tc.proto, func(t *testing.T) {
			b, _, err := NewBridge().BuildUpstreamBody(router.Candidate{EndpointProto: tc.proto}, ingress, raw)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), tc.expected) || !strings.Contains(string(b), "hello") || !strings.Contains(string(b), "ok?") {
				t.Fatalf("lost content: %s", b)
			}
		})
	}
	for _, bad := range [][]byte{bytes.Replace(raw, []byte(`"window"`), []byte(`"missing"`), 1), bytes.Replace(raw, []byte(data), []byte("bad"), 1)} {
		if _, _, err := NewBridge().BuildUpstreamBody(router.Candidate{EndpointProto: config.ProtocolOpenAIDecisions}, ingress, bad); err == nil {
			t.Fatal("invalid image accepted")
		}
	}
	if _, _, err := NewBridge().BuildUpstreamBody(router.Candidate{EndpointProto: config.ProtocolAnthropic}, ingress, raw); err == nil {
		t.Fatal("unsupported image route accepted")
	}
}
