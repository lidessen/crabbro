// Package quota synchronizes sanitized provider quota state in memory.
package quota

import (
	"github.com/lidessen/crabbro/pkg/api"
)

type RemoteQuota = api.RemoteQuotaJSON
type QuotaWindow = api.RemoteQuotaWindow
type ResetCoupon = api.RemoteResetCoupon
type apiResetOutcome = api.GatewayQuotaResetResponse
