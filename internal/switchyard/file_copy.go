package switchyard

import (
	"errors"
	"io"
	"os"
	"syscall"
)

type fileCloner func(output, input *os.File) error

// Unsupported filesystems, cross-device copies and restricted ioctl access use
// the ordinary copy path. Real I/O/storage failures must still reach callers.
func prepareClone(output, input *os.File, clone fileCloner) (bool, error) {
	if clone == nil {
		return false, nil
	}
	err := clone(output, input)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, syscall.EOPNOTSUPP) && !errors.Is(err, syscall.EXDEV) &&
		!errors.Is(err, syscall.ENOTTY) && !errors.Is(err, syscall.EINVAL) &&
		!errors.Is(err, syscall.ENOSYS) && !errors.Is(err, syscall.EPERM) {
		return false, err
	}
	// Reset a partial attempt, but do not truncate an already-empty file. On
	// ext4 even truncate(0) -> size 0 marks it for allocation on close, making
	// the otherwise ordinary copy path much more expensive.
	info, err := output.Stat()
	if err != nil {
		return false, err
	}
	if info.Size() != 0 {
		if err := output.Truncate(0); err != nil {
			return false, err
		}
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	_, err = input.Seek(0, io.SeekStart)
	return false, err
}
