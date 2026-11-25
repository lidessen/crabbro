// Package provider describes built-in gateway providers and their authentication methods.
package provider

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
)

type AuthKind string

const (
	APIKey    AuthKind = "api-key"
	KeyPool   AuthKind = "key-pool"
	OAuthSIWC AuthKind = "oauth-siwc"
)

type AuthMethodSpec struct {
	Kind AuthKind `json:"kind"`
}

type ProviderSpec struct {
	ID               string           `json:"id"`
	DisplayName      string           `json:"displayName"`
	DefaultBaseURL   string           `json:"defaultBaseUrl,omitempty"`
	DefaultEndpoints []string         `json:"defaultEndpoints,omitempty"`
	DefaultModels    []string         `json:"defaultModels,omitempty"`
	SupportedAuth    []AuthMethodSpec `json:"authMethods"`
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

var identifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidatePluginProvider validates a declaration without restricting custom auth kinds.
func ValidatePluginProvider(p ProviderSpec) error {
	if !identifierPattern.MatchString(p.ID) {
		return fmt.Errorf("invalid provider id %q", p.ID)
	}
	if strings.TrimSpace(p.DisplayName) == "" {
		return fmt.Errorf("provider %q requires displayName", p.ID)
	}
	if p.DefaultBaseURL != "" {
		u, err := url.Parse(p.DefaultBaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("provider %q has invalid defaultBaseUrl", p.ID)
		}
	}
	for _, values := range [][]string{p.DefaultEndpoints, p.DefaultModels} {
		for _, v := range values {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("provider %q contains an empty default", p.ID)
			}
		}
	}
	if len(p.SupportedAuth) == 0 {
		return fmt.Errorf("provider %q requires authMethods", p.ID)
	}
	seen := map[AuthKind]bool{}
	for _, a := range p.SupportedAuth {
		if !identifierPattern.MatchString(string(a.Kind)) || seen[a.Kind] {
			return fmt.Errorf("provider %q has invalid or duplicate auth kind %q", p.ID, a.Kind)
		}
		seen[a.Kind] = true
	}
	return nil
}

func clone(p ProviderSpec) ProviderSpec {
	p.SupportedAuth = append([]AuthMethodSpec(nil), p.SupportedAuth...)
	p.DefaultEndpoints = append([]string(nil), p.DefaultEndpoints...)
	p.DefaultModels = append([]string(nil), p.DefaultModels...)
	return p
}

type registeredProvider struct {
	owner string
	spec  ProviderSpec
}

// Registry holds built-ins and independently owned plugin providers.
type Registry struct {
	mu      sync.RWMutex
	plugins map[string]registeredProvider
}

func NewRegistry() *Registry { return &Registry{plugins: make(map[string]registeredProvider)} }

// RegisterPluginProvider rejects all collisions, including with built-ins.
func (r *Registry) RegisterPluginProvider(pluginID string, p ProviderSpec) error {
	return r.RegisterPluginProviders(pluginID, []ProviderSpec{p})
}

// RegisterPluginProviders registers a plugin's declarations atomically.
func (r *Registry) RegisterPluginProviders(pluginID string, specs []ProviderSpec) error {
	if strings.TrimSpace(pluginID) == "" {
		return fmt.Errorf("plugin id is required")
	}
	seen := make(map[string]bool, len(specs))
	for _, p := range specs {
		if err := ValidatePluginProvider(p); err != nil {
			return err
		}
		if seen[p.ID] {
			return fmt.Errorf("provider id %q declared more than once", p.ID)
		}
		seen[p.ID] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range specs {
		for _, b := range catalog {
			if b.ID == p.ID {
				return fmt.Errorf("provider id %q collides with built-in provider", p.ID)
			}
		}
		if existing, ok := r.plugins[p.ID]; ok {
			return fmt.Errorf("provider id %q already registered by plugin %q", p.ID, existing.owner)
		}
	}
	if r.plugins == nil {
		r.plugins = make(map[string]registeredProvider)
	}
	for _, p := range specs {
		r.plugins[p.ID] = registeredProvider{pluginID, clone(p)}
	}
	return nil
}

func (r *Registry) UnregisterPlugin(pluginID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, p := range r.plugins {
		if p.owner == pluginID {
			delete(r.plugins, id)
		}
	}
}

// List returns built-ins in catalog order followed by plugins sorted by ID.
func (r *Registry) List() []ProviderSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ProviderSpec, 0, len(catalog)+len(r.plugins))
	for _, p := range catalog {
		if len(p.SupportedAuth) == 0 {
			p.SupportedAuth = []AuthMethodSpec{{APIKey}, {KeyPool}}
		}
		out = append(out, clone(p))
	}
	ids := make([]string, 0, len(r.plugins))
	for id := range r.plugins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		out = append(out, clone(r.plugins[id].spec))
	}
	return out
}

func (r *Registry) Lookup(id string) (ProviderSpec, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, p := range catalog {
		if p.ID == id {
			if len(p.SupportedAuth) == 0 {
				p.SupportedAuth = []AuthMethodSpec{{APIKey}, {KeyPool}}
			}
			return clone(p), true
		}
	}
	p, ok := r.plugins[id]
	if !ok {
		return ProviderSpec{}, false
	}
	return clone(p.spec), true
}

var DefaultRegistry = NewRegistry()

// List returns independent copies from the default registry.
func List() []ProviderSpec                  { return DefaultRegistry.List() }
func Lookup(id string) (ProviderSpec, bool) { return DefaultRegistry.Lookup(id) }
