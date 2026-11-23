package codec

import (
	"testing"

	"github.com/lidessen/crabbro/internal/gateway/protocol"
)

func TestStreamDeltaTap(t *testing.T) {
	tests := []struct {
		name, proto, metadata, delta string
	}{
		{"anthropic text", protocol.ProtocolAnthropic, "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"text\"}}\n\n", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n"},
		{"anthropic tool", protocol.ProtocolAnthropic, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"tool_use\"}}\n\n", "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\"}}\n\n"},
		{"chat thinking", protocol.ProtocolOpenAIChat, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n", "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"x\"}}]}\n\n"},
		{"chat tool", protocol.ProtocolOpenAIChat, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n", "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"f\"}}]}}]}\n\n"},
		{"responses", protocol.ProtocolOpenAIResponses, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n", "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n"},
		{"gemini", protocol.ProtocolGemini, "data: {\"candidates\":[{\"content\":{\"role\":\"model\"}}]}\n\n", "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"x\"}]}}]}\n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tap := NewStreamDeltaTap(tt.proto)
			_, _ = tap.Write([]byte(tt.metadata))
			if tap.Found() {
				t.Fatal("metadata marked as a delta")
			}
			for _, b := range []byte(tt.delta) {
				_, _ = tap.Write([]byte{b})
			}
			if !tap.Found() {
				t.Fatal("delta was not detected")
			}
		})
	}
}
