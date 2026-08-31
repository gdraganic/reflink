//go:build linux

package reflink

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// reflinkInternal performs the actual reflink action using the FICLONE ioctl
// without handling any fallback mechanism. On Linux, this uses the FICLONE ioctl
// which efficiently creates a copy-on-write clone of the entire source file.
//
// This operation requires both files to be on the same filesystem that supports
// reflinks (like btrfs or xfs with reflink=1 mount option).
func reflinkInternal(d, s *os.File) error {
	ss, err := s.SyscallConn()
	if err != nil {
		return err
	}
	sd, err := d.SyscallConn()
	if err != nil {
		return err
	}

	var err2, err3 error

	err = sd.Control(func(dfd uintptr) {
		err2 = ss.Control(func(sfd uintptr) {
			// int ioctl(int dest_fd, FICLONE, int src_fd);
			err3 = unix.IoctlFileClone(int(dfd), int(sfd))
		})
	})

	if err != nil {
		// sd.Control failed
		return err
	}
	if err2 != nil {
		// ss.Control failed
		return err2
	}

	if err3 != nil && errors.Is(err3, unix.ENOTSUP) {
		return ErrReflinkFailed
	}

	// err3 is ioctl() response
	return err3
}

// reflinkRangeInternal performs a partial reflink operation using the FICLONERANGE ioctl,
// which allows cloning a specific range of the source file to the destination file.
// This is more efficient than copying the data when supported by the filesystem.
func reflinkRangeInternal(dst, src *os.File, dstOffset, srcOffset, n int64) error {
	ss, err := src.SyscallConn()
	if err != nil {
		return err
	}
	sd, err := dst.SyscallConn()
	if err != nil {
		return err
	}

	var err2, err3 error

	err = sd.Control(func(dfd uintptr) {
		err2 = ss.Control(func(sfd uintptr) {
			req := &unix.FileCloneRange{
				Src_fd:      int64(sfd),
				Src_offset:  uint64(srcOffset),
				Src_length:  uint64(n),
				Dest_offset: uint64(dstOffset),
			}

			// int ioctl(int dest_fd, FICLONE, int src_fd);
			err3 = unix.IoctlFileCloneRange(int(dfd), req)
		})
	})

	if err != nil {
		// sd.Control failed
		return err
	}
	if err2 != nil {
		// ss.Control failed
		return err2
	}
	if err3 != nil && errors.Is(err3, unix.ENOTSUP) {
		return ErrReflinkFailed
	}

	// err3 is ioctl() response
	return err3
}

// copyFileRange uses the copy_file_range Linux syscall to efficiently copy data between files
// without involving userspace buffers. While not as efficient as reflink, it's still more
// efficient than regular userspace copying with io.Copy when available.
//
// This function returns the number of bytes copied and any error that occurred.
func copyFileRange(dst, src *os.File, dstOffset, srcOffset, n int64) (int64, error) {
	ss, err := src.SyscallConn()
	if err != nil {
		return 0, err
	}
	sd, err := dst.SyscallConn()
	if err != nil {
		return 0, err
	}

	var total int64
	var err2, err3 error

	err = sd.Control(func(dfd uintptr) {
		err2 = ss.Control(func(sfd uintptr) {
			// copy_file_range transfers at most 0x7ffff000 (2,147,479,552) bytes
			// per call, and may also perform a short copy for other reasons, so we
			// loop until all n bytes have been copied. The kernel advances srcOffset
			// and dstOffset on each call, so we only need to track the remaining count.
			for rn := n; rn > 0; {
				var resN int
				resN, err3 = unix.CopyFileRange(int(sfd), &srcOffset, int(dfd), &dstOffset, int(rn), 0)
				total += int64(resN)
				if err3 != nil || resN == 0 {
					// error, or end of source reached (avoids an infinite loop)
					break
				}
				rn -= int64(resN)
			}
		})
	})

	if err != nil {
		// sd.Control failed
		return total, err
	}
	if err2 != nil {
		// ss.Control failed
		return total, err2
	}

	// err3 is the copy_file_range() response
	return total, err3

}


// sparseCopy copies only data extents from src to dst using SEEK_DATA/SEEK_HOLE
// + copy_file_range. Holes are skipped, preserving sparsity. This matches cp(1)
// behavior and avoids copying multi-GB zero regions in sparse files.
func sparseCopy(dst, src *os.File, size int64) error {
	src.Seek(0, 0)
	dst.Seek(0, 0)
	dst.Truncate(0)

	sfd := int(src.Fd())
	dfd := int(dst.Fd())

	var cursor int64
	for {
		dataStart, err := unix.Seek(sfd, cursor, unix.SEEK_DATA)
		if err != nil {
			break // no more data extents (ENXIO) or SEEK_DATA unsupported
		}

		dataEnd, err := unix.Seek(sfd, dataStart, unix.SEEK_HOLE)
		if err != nil {
			dataEnd = size
		}

		srcOff := dataStart
		dstOff := dataStart
		for srcOff < dataEnd {
			chunk := dataEnd - srcOff
			if chunk > 128<<20 {
				chunk = 128 << 20
			}
			n, err := unix.CopyFileRange(sfd, &srcOff, dfd, &dstOff, int(chunk), 0)
			if err != nil || n == 0 {
				return err
			}
		}
		cursor = dataEnd
	}

	return dst.Truncate(size)
}
