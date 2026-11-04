package plugin

import "github.com/lidessen/chiling/pkg/api"

// PackageRecord and Match are the wire records for an installed plugin.
// Credentials are not stored on the package.
type (
	PackageRecord = api.Plugin
	Match         = api.PluginMatch
)
