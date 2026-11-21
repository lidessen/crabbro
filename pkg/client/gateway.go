package client

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/lidessen/crabbro/pkg/api"
)

// ListSecrets is GET /v1/secrets. Secret values are never returned.
func (c *Client) ListSecrets(ctx context.Context) ([]api.SecretSummary, error) {
	var out api.SecretListResponse
	err := c.getJSON(ctx, "/v1/secrets", &out)
	if out.Secrets == nil {
		out.Secrets = []api.SecretSummary{}
	}
	return out.Secrets, err
}

// SetSecret is POST /v1/secrets.
func (c *Client) SetSecret(ctx context.Context, name, value string) error {
	return c.postJSON(ctx, "/v1/secrets", map[string]string{
		"name":  name,
		"value": value,
	}, nil)
}

// DeleteSecret is DELETE /v1/secrets/{name}.
func (c *Client) DeleteSecret(ctx context.Context, name string) error {
	resp, err := c.Do(ctx, http.MethodDelete, "/v1/secrets/"+url.PathEscape(name), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decode(resp, nil)
}

// GatewayStatus is GET /v1/gateway/status.
func (c *Client) GatewayStatus(ctx context.Context) (api.GatewayStatusResponse, error) {
	var out api.GatewayStatusResponse
	err := c.getJSON(ctx, "/v1/gateway/status", &out)
	return out, err
}

// ListGatewayPlans is GET /v1/gateway/plans.
func (c *Client) ListGatewayPlans(ctx context.Context) ([]api.GatewayPlanStatus, error) {
	var out api.GatewayPlanListResponse
	err := c.getJSON(ctx, "/v1/gateway/plans", &out)
	if out.Plans == nil {
		out.Plans = []api.GatewayPlanStatus{}
	}
	return out.Plans, err
}

// ResetGatewayPlanCooldown is POST /v1/gateway/plans/{plan}/reset-cooldown.
func (c *Client) ResetGatewayPlanCooldown(ctx context.Context, planID string) error {
	return c.postJSON(ctx, "/v1/gateway/plans/"+url.PathEscape(planID)+"/reset-cooldown", nil, nil)
}

// ListGatewayRoutes is GET /v1/gateway/routes.
func (c *Client) ListGatewayRoutes(ctx context.Context) ([]api.GatewayRouteStatus, error) {
	var out api.GatewayRouteListResponse
	err := c.getJSON(ctx, "/v1/gateway/routes", &out)
	if out.Routes == nil {
		out.Routes = []api.GatewayRouteStatus{}
	}
	return out.Routes, err
}

// GatewayUsage is GET /v1/gateway/usage.
func (c *Client) GatewayUsage(ctx context.Context, window, plan string) (api.GatewayUsageResponse, error) {
	q := url.Values{}
	if window != "" {
		q.Set("window", window)
	}
	if plan != "" {
		q.Set("plan", plan)
	}
	path := "/v1/gateway/usage"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var out api.GatewayUsageResponse
	err := c.getJSON(ctx, path, &out)
	if out.Buckets == nil {
		out.Buckets = []api.GatewayUsageBucket{}
	}
	return out, err
}

// PreviewGatewayRoute is POST /v1/gateway/preview.
func (c *Client) PreviewGatewayRoute(ctx context.Context, req api.GatewayPreviewRequest) (api.GatewayPreviewResponse, error) {
	var out api.GatewayPreviewResponse
	err := c.postJSON(ctx, "/v1/gateway/preview", req, &out)
	return out, err
}

// ReloadGateway is POST /v1/gateway/reload.
func (c *Client) ReloadGateway(ctx context.Context) (complete bool, diags []api.Diagnostic, err error) {
	var out struct {
		Complete    bool             `json:"complete"`
		Diagnostics []api.Diagnostic `json:"diagnostics"`
	}
	err = c.postJSON(ctx, "/v1/gateway/reload", nil, &out)
	return out.Complete, out.Diagnostics, err
}

func configQueryPath(op, kind, id string) string {
	q := url.Values{}
	if id != "" {
		q.Set("id", id)
	}
	if kind != "" {
		q.Set("kind", kind)
	}
	if enc := q.Encode(); enc != "" {
		return fmt.Sprintf("/v1/config/%s?%s", op, enc)
	}
	return "/v1/config/" + op
}

// DraftConfig stages raw YAML config bytes under /v1/config/draft.
func (c *Client) DraftConfig(ctx context.Context, kind, id string, body []byte) error {
	resp, err := c.Do(ctx, http.MethodPost, configQueryPath("draft", kind, id), bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decode(resp, nil)
}

// ValidateConfig checks a staged or live config file via POST /v1/config/validate.
func (c *Client) ValidateConfig(ctx context.Context, kind, id string) (ok bool, diags []api.Diagnostic, err error) {
	var out struct {
		OK          bool             `json:"ok"`
		Diagnostics []api.Diagnostic `json:"diagnostics"`
	}
	err = c.postJSON(ctx, configQueryPath("validate", kind, id), nil, &out)
	return out.OK, out.Diagnostics, err
}

// CommitConfig promotes a validated draft via POST /v1/config/commit.
func (c *Client) CommitConfig(ctx context.Context, kind, id string) error {
	return c.postJSON(ctx, configQueryPath("commit", kind, id), nil, nil)
}

// DiscardConfig removes a staged draft via POST /v1/config/discard.
func (c *Client) DiscardConfig(ctx context.Context, kind, id string) error {
	return c.postJSON(ctx, configQueryPath("discard", kind, id), nil, nil)
}

// RefreshGatewayQuotas synchronizes provider quota snapshots.
func (c *Client) RefreshGatewayQuotas(ctx context.Context) ([]api.GatewayPlanStatus, error) {
	var out api.GatewayPlanListResponse
	err := c.postJSON(ctx, "/v1/gateway/quotas/refresh", nil, &out)
	return out.Plans, err
}
