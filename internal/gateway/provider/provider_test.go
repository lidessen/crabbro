package provider

import "testing"

func TestCatalog(t *testing.T) {
	ids := []string{"openai", "anthropic", "google", "deepseek", "vercel", "zcode", "minimax-cn", "kimi-code-cn", "xiaomi", "opencode-go", "zenmux", "commandcode", "typesafe"}
	if len(List()) != len(ids) {
		t.Fatalf("unexpected catalog size")
	}
	for _, id := range ids {
		p, ok := Lookup(id)
		if !ok || p.ID != id || p.DisplayName == "" || len(p.SupportedAuth) == 0 {
			t.Fatalf("missing provider %s: %+v", id, p)
		}
	}
	p, _ := Lookup("openai")
	methods := map[AuthKind]bool{}
	for _, method := range p.SupportedAuth {
		methods[method.Kind] = true
	}
	if !methods[APIKey] || !methods[OAuthSIWC] {
		t.Fatal("OpenAI methods missing")
	}
	p.SupportedAuth[0].Kind = "changed"
	again, _ := Lookup("openai")
	if again.SupportedAuth[0].Kind == "changed" {
		t.Fatal("catalog was mutated")
	}
	if _, ok := Lookup("unknown"); ok {
		t.Fatal("unknown provider found")
	}
}
