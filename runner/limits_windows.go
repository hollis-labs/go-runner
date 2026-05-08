//go:build windows

package runner

import (
	"errors"
	"os/exec"
)

// applyResourceLimitsImpl is unsupported on Windows. ResourceLimits
// configured at Run time on Windows returns an error. Callers that
// need cross-platform limits should detect runtime.GOOS == "windows"
// and skip ResourceLimits configuration on that platform.
func applyResourceLimitsImpl(cmd *exec.Cmd, limits ResourceLimits) (func(), error) {
	_ = cmd
	_ = limits
	return nil, errors.New("ResourceLimits not supported on windows")
}
