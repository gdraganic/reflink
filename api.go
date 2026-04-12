package reflink

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Always will perform a reflink operation and fail on error if reflink is not supported.
// It creates a copy of the source file at the destination using the filesystem's
// copy-on-write mechanism, which is extremely fast and space-efficient.
//
// This is equivalent to command `cp --reflink=always` on Linux systems.
// Both files must be on the same filesystem that supports reflinks (btrfs, xfs).
//
// Returns ErrReflinkUnsupported if the OS doesn't support reflinks,
// or ErrReflinkFailed if the specific filesystem doesn't support reflinks.
func Always(src, dst string) error {
	return reflinkFile(src, dst, false)
}

// Auto will attempt to perform a reflink operation and fallback to normal data
// copy if reflink is not supported. This is the safer option for general use.
//
// The fallback mechanism follows this priority:
// 1. Try reflink (FICLONE ioctl)
// 2. Try copy_file_range syscall (more efficient than userspace copy)
// 3. Fallback to regular io.Copy
//
// All fallback paths correctly preserve the logical size of sparse files
// (files with holes) by truncating the destination to match the source size.
//
// This is equivalent to `cp --reflink=auto` on Linux systems.
func Auto(src, dst string) error {
	return reflinkFile(src, dst, true)
}

// reflinkFile performs the reflink operation to copy src into dst using
// the underlying filesystem's copy-on-write reflink system.
//
// The function creates a temporary file in the same directory as dst, performs the
// copy operation to this temporary file, and then renames it to dst. This ensures
// atomic replacement of the destination file.
//
// If reflink fails (for example, if the filesystem does not support reflinks) and
// fallback is true, then copy_file_range will be used. If copy_file_range also fails,
// io.Copy will be used as a final fallback to copy the data.
//
// The function preserves the file mode of the source file when possible.
func reflinkFile(src, dst string, fallback bool) error {
	s, err := os.Open(src)
	if err != nil {
		return err
	}
	defer s.Close()

	st, err := s.Stat()
	if err != nil {
		return err
	}

	// generate temporary file for output
	tmp, err := os.CreateTemp(filepath.Dir(dst), "")
	if err != nil {
		return err
	}

	// copy to temp file
	err = reflinkInternal(tmp, s)

	// Verify FICLONE actually worked — some filesystems (e.g. tmpfs) return
	// success from FICLONE but produce an empty destination file.
	if err == nil {
		if dstInfo, stErr := tmp.Stat(); stErr == nil && dstInfo.Size() != st.Size() {
			err = ErrReflinkFailed
		}
	}

	// if reflink failed but we allow fallback, first attempt using copyFileRange (will actually clone bytes on some filesystems)
	if (err != nil) && fallback {
		// Reset file positions after failed FICLONE.
		s.Seek(0, io.SeekStart)
		tmp.Seek(0, io.SeekStart)

		_, err = copyFileRange(tmp, s, 0, 0, st.Size())
	}

	// if everything failed and we fallback, attempt io.Copy
	if (err != nil) && fallback {
		// reflink failed but fallback enabled, perform a normal copy instead
		s.Seek(0, io.SeekStart)
		tmp.Seek(0, io.SeekStart)
		tmp.Truncate(0)
		_, err = io.Copy(tmp, s)
	}

	if err == nil {
		// Ensure the destination file has the same logical size as the source.
		// copy_file_range does not preserve trailing holes in sparse files —
		// only data extents are copied, so the destination may be smaller than
		// the source's logical size.
		if dstInfo, stErr := tmp.Stat(); stErr == nil && dstInfo.Size() < st.Size() {
			err = tmp.Truncate(st.Size())
		}
	}

	tmp.Close() // we're not writing to this anymore

	// if an error happened, remove temp file and signal error
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}

	// keep src file mode if possible
	if st, err := s.Stat(); err == nil {
		os.Chmod(tmp.Name(), st.Mode())
	}

	// replace dst file
	err = os.Rename(tmp.Name(), dst)
	if err != nil {
		// failed to rename (dst is not writable?)
		os.Remove(tmp.Name())
		return err
	}

	return nil
}

// Reflink performs the reflink operation on the passed files, replacing
// dst's contents with src. This function works with already-open file handles.
//
// If fallback is true and reflink fails (on unsupported filesystems),
// copy_file_range will be tried first, and if that fails too, io.Copy will
// be used to copy the data. When using io.Copy, the destination file will
// be truncated first.
//
// Note: Unlike Always() and Auto(), this function requires you to open and
// close the file handles yourself, which gives more control but requires more
// careful handling.
func Reflink(dst, src *os.File, fallback bool) error {
	err := reflinkInternal(dst, src)

	// Verify FICLONE worked (tmpfs returns success but empty file).
	if err == nil {
		srcSt, _ := src.Stat()
		dstSt, _ := dst.Stat()
		if srcSt != nil && dstSt != nil && dstSt.Size() != srcSt.Size() {
			err = ErrReflinkFailed
		}
	}

	if (err != nil) && fallback {
		// reflink failed, but we can fallback, but first we need to know the file's size
		var st fs.FileInfo
		st, err = src.Stat()
		if err != nil {
			// couldn't stat source, this can't be helped
			return fmt.Errorf("failed to stat source: %w", err)
		}
		_, err = copyFileRange(dst, src, 0, 0, st.Size())
		if err != nil {
			// copyFileRange failed too, switch to simple io copy
			reader := io.NewSectionReader(src, 0, st.Size())
			writer := &sectionWriter{w: dst}
			dst.Truncate(0) // assuming any error in truncate will result in copy error
			_, err = io.Copy(writer, reader)
		}

		// Ensure logical size matches source (sparse file trailing holes).
		if err == nil {
			if dstSt, stErr := dst.Stat(); stErr == nil && dstSt.Size() < st.Size() {
				err = dst.Truncate(st.Size())
			}
		}
	}
	return err
}

// Partial performs a range reflink operation on the passed files, replacing
// part of dst's contents with data from src. This allows for more fine-grained
// control over which parts of the file are copied.
//
// Parameters:
//   - dst: Destination file handle
//   - src: Source file handle
//   - dstOffset: Offset in the destination file where data should be written
//   - srcOffset: Offset in the source file where data should be read from
//   - n: Number of bytes to copy
//   - fallback: Whether to fall back to regular copy methods if reflink fails
//
// If fallback is true and reflink fails, copy_file_range will be tried first,
// and if that fails too, io.CopyN with appropriate readers/writers will be used.
//
// This function is useful for selectively copying parts of large files without
// having to read and write the entire file contents.
func Partial(dst, src *os.File, dstOffset, srcOffset, n int64, fallback bool) error {
	err := reflinkRangeInternal(dst, src, dstOffset, srcOffset, n)
	if (err != nil) && fallback {
		_, err = copyFileRange(dst, src, dstOffset, srcOffset, n)
	}

	if (err != nil) && fallback {
		// seek both src & dst
		reader := io.NewSectionReader(src, srcOffset, n)
		writer := &sectionWriter{w: dst, base: dstOffset}
		_, err = io.CopyN(writer, reader, n)
	}
	return err
}
