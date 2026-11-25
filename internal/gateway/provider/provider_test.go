package provider

import (
	"context"
	"testing"
)

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

func TestRegistryPluginLifecycle(t *testing.T) {
	r := NewRegistry()
	p := ProviderSpec{ID: "example", DisplayName: "Example", DefaultEndpoints: []string{"openai-chat"}, DefaultModels: []string{"one"}, SupportedAuth: []AuthMethodSpec{{Kind: "plugin-oauth"}}}
	if err := r.RegisterPluginProvider("plugin-a", p); err != nil {
		t.Fatal(err)
	}
	p.DefaultModels[0] = "mutated"
	got, ok := r.Lookup("example")
	if !ok || got.DefaultModels[0] != "one" || got.SupportedAuth[0].Kind != "plugin-oauth" {
		t.Fatalf("unexpected provider: %+v", got)
	}
	got.DefaultEndpoints[0] = "mutated"
	again, _ := r.Lookup("example")
	if again.DefaultEndpoints[0] != "openai-chat" {
		t.Fatal("lookup returned shared data")
	}
	if _, ok := r.Lookup("openai"); !ok || len(r.List()) != len(catalog)+1 {
		t.Fatal("missing built-in or plugin provider")
	}
	if err := r.RegisterPluginProvider("plugin-b", p); err == nil {
		t.Fatal("duplicate accepted")
	}
	p.ID = "openai"
	if err := r.RegisterPluginProvider("plugin-b", p); err == nil {
		t.Fatal("built-in collision accepted")
	}
	r.UnregisterPlugin("plugin-b")
	if _, ok := r.Lookup("example"); !ok {
		t.Fatal("removed another plugin's provider")
	}
	r.UnregisterPlugin("plugin-a")
	if _, ok := r.Lookup("example"); ok {
		t.Fatal("provider not removed")
	}
	if _, ok := r.Lookup("openai"); !ok {
		t.Fatal("built-in removed")
	}
}

func TestRegistryBatchIsAtomic(t *testing.T) {
	r := NewRegistry()
	good := ProviderSpec{ID: "valid", DisplayName: "Valid", SupportedAuth: []AuthMethodSpec{{Kind: "plugin-token"}}}
	bad := good
	bad.ID = "openai"
	if err := r.RegisterPluginProviders("p", []ProviderSpec{good, bad}); err == nil {
		t.Fatal("collision accepted")
	}
	if _, ok := r.Lookup("valid"); ok {
		t.Fatal("partial batch registered")
	}
}

type authRPCStub struct {
	method string
	req    AuthRequest
}

func (s *authRPCStub) CallProviderAuth(_ context.Context, method string, req AuthRequest) (AuthResult, error) {
	s.method, s.req = method, req
	return AuthResult{CredentialRef: req.CredentialRef}, nil
}
func TestPluginAuthenticatorBridge(t *testing.T) {
	rpc := &authRPCStub{}
	request := AuthRequest{PluginID: "p", Instance: "local", ProviderID: "partner", Kind: "plugin-oauth", CredentialRef: "assigned"}
	bridge := PluginAuthenticator{RPC: rpc, Request: request}
	if _, err := bridge.Prepare(context.Background()); err != nil || rpc.method != "provider.auth.prepare" || rpc.req != request {
		t.Fatalf("prepare: %q %+v %v", rpc.method, rpc.req, err)
	}
	if _, err := bridge.Refresh(context.Background()); err != nil || rpc.method != "provider.auth.refresh" || rpc.req != request {
		t.Fatalf("refresh: %q %+v %v", rpc.method, rpc.req, err)
	}
}
