//go:build !windows

package runner

import (
	"os/exec"
	"testing"
)

// CW-20261001-0108: the ulimit -f block differs by shell, and the runner
// measures it rather than assuming. dash and POSIX-mode bash count 512
// bytes; bash outside POSIX mode (bash 3.2 as macOS's sh) counts 1024.
func TestMeasureFileSizeBlock(t *testing.T) {
	cases := []struct {
		shell string
		want  uint64
	}{
		{"dash", 512},
		{"bash", 1024}, // invoked as bash, not sh: no POSIX mode
	}
	for _, tc := range cases {
		path, err := exec.LookPath(tc.shell)
		if err != nil {
			t.Logf("%s not installed", tc.shell)
			continue
		}
		if got := measureFileSizeBlock(path); got != tc.want {
			t.Errorf("measureFileSizeBlock(%s) = %d, want %d", path, got, tc.want)
		}
	}
}
