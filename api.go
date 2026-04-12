package reflink

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Always will perform a reflink operation and fail on error if reflink is not supported.
func Always(src, dst string) error {
	return reflinkFile(src, dst, false)
}

// Auto will attempt to perform a reflink operation and fallback to normal data
// copy if reflink is not supported. This is the safer option for general use.
func Auto(src, dst string) error {
	return reflinkFile(src, dst, true)
}

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

	// try FICLONE
	err = reflinkInternal(tmp, s)

	// verify FICLONE actually cloned — tmpfs returns success but empty file
	if err == nil {
		if info, stErr := tmp.Stat(); stErr == nil && info.Size() != st.Size() {
			err = ErrReflinkFailed
		}
	}

	// fallback: sparse-aware copy (SEEK_DATA/SEEK_HOLE + copy_file_range),
	// then io.Copy as last resort
	if (err != nil) && fallback {
		err = sparseCopy(tmp, s, st.Size())
	}
	if (err != nil) && fallback {
		s.Seek(0, io.SeekStart)
		tmp.Seek(0, io.SeekStart)
		tmp.Truncate(0)
		_, err = io.Copy(tmp, s)
	}

	// ensure logical size matches source (trailing sparse holes)
	if err == nil {
		if info, stErr := tmp.Stat(); stErr == nil && info.Size() < st.Size() {
			err = tmp.Truncate(st.Size())
		}
	}

	tmp.Close()

	if err != nil {
		os.Remove(tmp.Name())
		return err
	}

	// keep src file mode if possible
	if st, err := s.Stat(); err == nil {
		os.Chmod(tmp.Name(), st.Mode())
	}

	err = os.Rename(tmp.Name(), dst)
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}

	return nil
}

// Reflink performs the reflink operation on the passed files.
func Reflink(dst, src *os.File, fallback bool) error {
	err := reflinkInternal(dst, src)

	// verify FICLONE
	if err == nil {
		srcSt, _ := src.Stat()
		dstSt, _ := dst.Stat()
		if srcSt != nil && dstSt != nil && dstSt.Size() != srcSt.Size() {
			err = ErrReflinkFailed
		}
	}

	if (err != nil) && fallback {
		var st fs.FileInfo
		st, err = src.Stat()
		if err != nil {
			return fmt.Errorf("failed to stat source: %w", err)
		}
		err = sparseCopy(dst, src, st.Size())
		if err != nil {
			reader := io.NewSectionReader(src, 0, st.Size())
			writer := &sectionWriter{w: dst}
			dst.Truncate(0)
			_, err = io.Copy(writer, reader)
		}
		// ensure logical size
		if err == nil {
			if dstSt, stErr := dst.Stat(); stErr == nil && dstSt.Size() < st.Size() {
				err = dst.Truncate(st.Size())
			}
		}
	}
	return err
}

// Partial performs a range reflink operation on the passed files.
func Partial(dst, src *os.File, dstOffset, srcOffset, n int64, fallback bool) error {
	err := reflinkRangeInternal(dst, src, dstOffset, srcOffset, n)
	if (err != nil) && fallback {
		_, err = copyFileRange(dst, src, dstOffset, srcOffset, n)
	}
	if (err != nil) && fallback {
		reader := io.NewSectionReader(src, srcOffset, n)
		writer := &sectionWriter{w: dst, base: dstOffset}
		_, err = io.CopyN(writer, reader, n)
	}
	return err
}
