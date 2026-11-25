package quota

import (
	"testing"

	"github.com/lidessen/crabbro/internal/gateway/config"
)

func TestSIWCIsNotAnAPIKeyQuotaProvider(t *testing.T) {
	for _, id := range []string{"zcode", "vercel", "deepseek"} {
		p := &config.Plan{ID: id, SIWC: &config.SIWCSpec{SessionRef: "session", HostIDRef: "host"}}
		if got := provider(p); got != "" {
			t.Fatalf("SIWC plan %q matched quota provider %q", id, got)
		}
	}
}
