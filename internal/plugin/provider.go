package plugin

import (
	"context"
	"fmt"
	"time"

	"github.com/lidessen/crabbro/internal/gateway/provider"
	"github.com/lidessen/crabbro/internal/secret"
)

// ProviderDeclarations returns independent copies of an installed plugin's declarations.
func (m *Manager) ProviderDeclarations(pluginID string) []provider.ProviderSpec {
	m.mu.Lock()
	defer m.mu.Unlock()
	specs := m.state.Providers[pluginID]
	out := make([]provider.ProviderSpec, len(specs))
	for i, p := range specs {
		p.DefaultEndpoints = append([]string(nil), p.DefaultEndpoints...)
		p.DefaultModels = append([]string(nil), p.DefaultModels...)
		p.SupportedAuth = append([]provider.AuthMethodSpec(nil), p.SupportedAuth...)
		out[i] = p
	}
	return out
}

// RegisterPluginProviders adds an enabled installed plugin's providers to r.
func (m *Manager) RegisterPluginProviders(r *provider.Registry, pluginID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pkg := m.packageByID(pluginID)
	if pkg == nil || !pkg.Enabled {
		return fmt.Errorf("plugin is not enabled")
	}
	return r.RegisterPluginProviders(pluginID, m.state.Providers[pluginID])
}

// CallProviderAuth invokes a declared plugin auth method with an assigned ref only.
func (m *Manager) CallProviderAuth(ctx context.Context, method string, req provider.AuthRequest) (provider.AuthResult, error) {
	if method != "provider.auth.prepare" && method != "provider.auth.refresh" {
		return provider.AuthResult{}, fmt.Errorf("unknown provider auth method")
	}
	if secret.ValidRef(req.CredentialRef) != nil {
		return provider.AuthResult{}, fmt.Errorf("invalid credential ref")
	}
	m.mu.Lock()
	pkg := m.packageByID(req.PluginID)
	inst := m.instanceByName(req.PluginID, req.Instance)
	var rec PackageRecord
	allowed := false
	if pkg != nil && pkg.Enabled && inst != nil && inst.Enabled {
		rec = *pkg
		assigned := false
		for _, ref := range inst.Credentials {
			if ref == req.CredentialRef {
				assigned = true
				break
			}
		}
		if assigned {
			for _, p := range m.state.Providers[req.PluginID] {
				if p.ID == req.ProviderID {
					for _, a := range p.SupportedAuth {
						if a.Kind == req.Kind {
							allowed = true
						}
					}
				}
			}
		}
	}
	m.mu.Unlock()
	if !allowed {
		return provider.AuthResult{}, fmt.Errorf("provider auth is not authorized")
	}
	if err := m.ensureHost(); err != nil {
		return provider.AuthResult{}, err
	}
	var reply map[string]any
	for attempt := 0; ; attempt++ {
		if err := m.activate(rec, nil); err != nil {
			return provider.AuthResult{}, err
		}
		var err error
		reply, err = m.roundTrip(ctx, map[string]any{"type": method, "package": req.PluginID, "instance": req.Instance, "providerId": req.ProviderID, "kind": req.Kind, "credentialRef": req.CredentialRef}, 5*time.Second)
		if err != nil {
			return provider.AuthResult{}, err
		}
		errMsg, _ := reply["error"].(string)
		if attempt == 0 && (errMsg == "plugin is not active" || errMsg == "plugin exited") {
			m.forget(rec.ID)
			continue
		}
		if errMsg != "" {
			return provider.AuthResult{}, fmt.Errorf("provider auth failed")
		}
		break
	}
	result, ok := reply["result"].(map[string]any)
	if !ok {
		return provider.AuthResult{}, fmt.Errorf("invalid provider auth result")
	}
	ref, ok := result["credentialRef"].(string)
	if !ok || ref != req.CredentialRef {
		return provider.AuthResult{}, fmt.Errorf("invalid provider auth result")
	}
	retry, _ := result["retry"].(bool)
	return provider.AuthResult{CredentialRef: ref, Retry: retry}, nil
}
