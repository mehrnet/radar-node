//go:build !linux && !darwin && !windows

package netbind

import (
	"fmt"
	"runtime"
)

func controlInterface(string, uintptr, string) error {
	return fmt.Errorf("interface binding is not supported on %s", runtime.GOOS)
}
