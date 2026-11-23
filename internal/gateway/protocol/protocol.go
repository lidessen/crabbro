// Package protocol defines the gateway's stateless wire protocol identifiers.
package protocol

const (
	ProtocolOpenAIChat      = "openai-chat"
	ProtocolOpenAIResponses = "openai-responses"
	ProtocolAnthropic       = "anthropic"
	ProtocolGemini          = "gemini"
	ProtocolTypeSafeJev     = "typesafe-jev"
	ProtocolOpenAIDecisions = "openai-decisions"
	ProtocolGeminiDecisions = "gemini-decisions"
	FamilyGenerative        = "generative"
	FamilyDecision          = "decision"
)

// ProtocolFamily returns the family of a recognized protocol.
func ProtocolFamily(p string) string {
	switch p {
	case ProtocolTypeSafeJev, ProtocolOpenAIDecisions, ProtocolGeminiDecisions:
		return FamilyDecision
	case ProtocolOpenAIChat, ProtocolOpenAIResponses, ProtocolAnthropic, ProtocolGemini:
		return FamilyGenerative
	default:
		return ""
	}
}
