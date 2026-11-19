package executor

import (
	"net/http"
	"testing"
)

func TestClassifyUnsupportedResponseFormatIsRetryable(t *testing.T) {
	body := []byte(`{"error":{"message":"This response_format type is unavailable now","type":"invalid_request_error"}}`)
	fc := ClassifyHTTPFailure(http.StatusBadRequest, body, make(http.Header), "deepseek", nil)
	if !fc.Retryable || fc.Kind != ErrKindModelUnavailable || fc.CooldownScope != "model" {
		t.Fatalf("unexpected classification: %+v", fc)
	}
}
