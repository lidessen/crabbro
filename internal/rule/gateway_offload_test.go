package rule

import (
	"strings"
	"testing"
)

func TestGatewayOffloadRuleValidation(t *testing.T) {
	base := `version: 1
id: offload
on:
  request:
    method: POST
request:
  pipeline:
    - uses: crabbro.gateway-offload
      with:
        protocol: responses
        model: plan/model
`
	for _, tc := range []struct {
		name, script string
		valid        bool
	}{
		{"valid", base, true},
		{"missing model", strings.Replace(base, "        model: plan/model\n", "", 1), false},
		{"invalid protocol", strings.Replace(base, "protocol: responses", "protocol: unknown", 1), false},
		{"empty model", strings.Replace(base, "model: plan/model", "model: ''", 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRuleStore().Create("offload", 1, tc.script)
			if (err == nil) != tc.valid {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
}
