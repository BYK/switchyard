//go:build !linux || (!amd64 && !arm64 && !riscv64)

package switchyard

import (
	"os"
	"syscall"
)

func cloneFile(_, _ *os.File) error { return syscall.EOPNOTSUPP }
