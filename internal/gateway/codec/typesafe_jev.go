package codec

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image/png"

	"github.com/lidessen/crabbro/internal/gateway/ir"
)

// DecodeDecisionRequest parses a TypeSafe System One (/v1/systemone) request body.
func DecodeDecisionRequest(body []byte) (*ir.DecisionRequest, error) {
	var req ir.DecisionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("invalid typesafe-jev decision request: %w", err)
	}
	if len(req.Questions) == 0 {
		return nil, fmt.Errorf("typesafe-jev decision request requires questions")
	}
	return &req, nil
}

// EncodeDecisionRequest serializes a DecisionRequest for upstream dispatch.
func EncodeDecisionRequest(req *ir.DecisionRequest) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("decision request is nil")
	}
	return json.Marshal(req)
}

// EncodeDecisionResponse serializes a DecisionResponse back to HTTP JSON.
func EncodeDecisionResponse(resp *ir.DecisionResponse) ([]byte, error) {
	if resp == nil {
		return nil, fmt.Errorf("decision response is nil")
	}
	return json.Marshal(resp)
}

// decisionImage resolves the supported Jev image state without discarding fields.
func decisionImage(req *ir.DecisionRequest) (string, string, bool, error) {
	state, object := req.State.(map[string]any)
	_, hasImage := state["image"]
	if len(req.Assets) == 0 && !hasImage {
		return "", "", false, nil
	}
	if !object || len(state) != 2 {
		return "", "", false, fmt.Errorf("unsupported image state")
	}
	text, ok := state["text"].(string)
	if !ok {
		return "", "", false, fmt.Errorf("image state requires text")
	}
	ref, ok := state["image"].(string)
	if !ok || ref == "" {
		return "", "", false, fmt.Errorf("image state requires an asset reference")
	}
	asset, ok := req.Assets[ref]
	if !ok || len(req.Assets) != 1 {
		return "", "", false, fmt.Errorf("unresolved or unused image asset")
	}
	if asset.Type != "image" || asset.Format != "png" || len(asset.DataBase64) > 8*1024*1024 {
		return "", "", false, fmt.Errorf("unsupported or oversized image asset")
	}
	data, err := base64.StdEncoding.Strict().DecodeString(asset.DataBase64)
	if err != nil || len(data) == 0 || len(data) > 6*1024*1024 {
		return "", "", false, fmt.Errorf("invalid image data")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 4096 || cfg.Height > 4096 || int64(cfg.Width)*int64(cfg.Height) > 16000000 {
		return "", "", false, fmt.Errorf("invalid or oversized PNG")
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		return "", "", false, fmt.Errorf("invalid PNG")
	}
	return text, asset.DataBase64, true, nil
}
