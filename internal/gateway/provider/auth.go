package provider

import "context"

// AuthRequest identifies the one credential already assigned to this plugin
// instance. Only its secret-store reference crosses the plugin RPC boundary.
type AuthRequest struct {
	PluginID      string   `json:"pluginId"`
	Instance      string   `json:"instance"`
	ProviderID    string   `json:"providerId"`
	Kind          AuthKind `json:"kind"`
	CredentialRef string   `json:"credentialRef"`
}

// AuthResult refers to the same credential; the host resolves it locally.
type AuthResult struct {
	CredentialRef string `json:"credentialRef"`
	Retry         bool   `json:"retry,omitempty"`
}

// AuthRPC transports provider.auth.prepare and provider.auth.refresh calls.
// Implementations must authorize the reference and enforce transport limits.
type AuthRPC interface {
	CallProviderAuth(context.Context, string, AuthRequest) (AuthResult, error)
}

// PluginAuthenticator bridges authentication lifecycle calls to a plugin host.
type PluginAuthenticator struct {
	RPC     AuthRPC
	Request AuthRequest
}

func (a PluginAuthenticator) Prepare(ctx context.Context) (AuthResult, error) {
	return a.RPC.CallProviderAuth(ctx, "provider.auth.prepare", a.Request)
}
func (a PluginAuthenticator) Refresh(ctx context.Context) (AuthResult, error) {
	return a.RPC.CallProviderAuth(ctx, "provider.auth.refresh", a.Request)
}
