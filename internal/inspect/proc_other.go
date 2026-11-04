//go:build !linux && !darwin && !windows

package inspect

import "runtime"

var osPlatform = runtime.GOOS
