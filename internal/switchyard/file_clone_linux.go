//go:build linux && (amd64 || arm64 || riscv64)

package switchyard

import (
	"os"
	"syscall"
)

// FICLONE from Linux's asm-generic ioctl ABI (linux/fs.h). Clones have
// independent inodes; later writes cannot modify the source's shared extents.
func cloneFile(output, input *os.File) error {
	const ficlone = 0x40049409
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, output.Fd(), ficlone, input.Fd())
	if errno != 0 {
		return errno
	}
	return nil
}
