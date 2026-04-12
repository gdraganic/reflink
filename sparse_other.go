//go:build !linux

package reflink

import (
	"os"
)

// sparseCopy is not supported on non-Linux platforms.
func sparseCopy(dst, src *os.File, size int64) error {
	return ErrReflinkFailed
}
