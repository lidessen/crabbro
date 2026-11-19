package codec

import (
	"encoding/json"
	"fmt"

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
