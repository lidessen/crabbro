// Package provider describes built-in gateway providers and their authentication methods.
package provider

type AuthKind string

const (
	APIKey    AuthKind = "api-key"
	KeyPool   AuthKind = "key-pool"
	OAuthSIWC AuthKind = "oauth-siwc"
)

type AuthMethodSpec struct{ Kind AuthKind }

type ProviderSpec struct {
	ID               string
	DisplayName      string
	DefaultBaseURL   string
	DefaultEndpoints []string
	DefaultModels    []string
	SupportedAuth    []AuthMethodSpec
}

var catalog = []ProviderSpec{
	{ID: "openai", DisplayName: "OpenAI", DefaultBaseURL: "https://api.openai.com/v1", DefaultEndpoints: []string{"openai-chat", "openai-responses"}, SupportedAuth: []AuthMethodSpec{{APIKey}, {KeyPool}, {OAuthSIWC}}},
	{ID: "anthropic", DisplayName: "Anthropic", DefaultBaseURL: "https://api.anthropic.com", DefaultEndpoints: []string{"anthropic"}},
	{ID: "google", DisplayName: "Google", DefaultBaseURL: "https://generativelanguage.googleapis.com", DefaultEndpoints: []string{"gemini"}},
	{ID: "deepseek", DisplayName: "DeepSeek", DefaultBaseURL: "https://api.deepseek.com", DefaultEndpoints: []string{"openai-chat"}},
	{ID: "vercel", DisplayName: "Vercel"},
	{ID: "zcode", DisplayName: "Zcode"},
	{ID: "minimax-cn", DisplayName: "MiniMax China"},
	{ID: "kimi-code-cn", DisplayName: "Kimi Code China"},
	{ID: "xiaomi", DisplayName: "Xiaomi"},
	{ID: "opencode-go", DisplayName: "OpenCode Go"},
	{ID: "zenmux", DisplayName: "ZenMux"},
	{ID: "commandcode", DisplayName: "CommandCode"},
	{ID: "typesafe", DisplayName: "TypeSafe", DefaultBaseURL: "https://api.typesafe.ai", DefaultEndpoints: []string{"typesafe-jev"}},
}

// List returns independent copies so callers cannot mutate the catalog.
func List() []ProviderSpec {
	out := make([]ProviderSpec, len(catalog))
	for i, p := range catalog {
		if len(p.SupportedAuth) == 0 {
			p.SupportedAuth = []AuthMethodSpec{{APIKey}, {KeyPool}}
		}
		p.SupportedAuth = append([]AuthMethodSpec(nil), p.SupportedAuth...)
		p.DefaultEndpoints = append([]string(nil), p.DefaultEndpoints...)
		p.DefaultModels = append([]string(nil), p.DefaultModels...)
		out[i] = p
	}
	return out
}

func Lookup(id string) (ProviderSpec, bool) {
	for _, p := range List() {
		if p.ID == id {
			return p, true
		}
	}
	return ProviderSpec{}, false
}
