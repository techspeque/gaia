package supervise

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"github.com/techspeque/gaia/internal/core"
)

// Scrollback reads the stored scrollback of an ended session from a
// logical offset. Start is the first offset still kept, End the logical
// length.
type Scrollback struct {
	Start, End int64
	r          io.Reader
	files      []*os.File
}

// Read reads the next scrollback bytes.
func (s *Scrollback) Read(p []byte) (int, error) { return s.r.Read(p) }

// Close closes the scrollback files.
func (s *Scrollback) Close() error {
	var err error
	for _, f := range s.files {
		err = errors.Join(err, f.Close())
	}
	return err
}

// OpenScrollback opens the scrollback of the ended session in dir for
// reading from logical offset off to its end. A session that has not ended
// is core.CodeConflict (its supervisor streams it); an offset before the
// first kept byte is core.CodeInvalidArgument with the detail "start"; an
// offset past the end is core.CodeInvalidArgument with the detail "end".
// A missing scrollback file is core.CodeNotFound.
//
// The files are mapped as the supervisor writes them: ScrollbackFile holds
// the last bytes up to ScrollbackEnd, and ScrollbackPrevFile, when present,
// the bytes just before.
func OpenScrollback(dir string, off int64) (*Scrollback, error) {
	st, err := ReadState(dir)
	if err != nil {
		return nil, err
	}
	if !st.Ended() {
		return nil, core.NewError(core.CodeConflict, "the session is still running; stream it from its supervisor")
	}
	sb := &Scrollback{End: st.ScrollbackEnd}
	cur, err := os.Open(filepath.Join(dir, ScrollbackFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, core.NewError(core.CodeNotFound, "the session's scrollback is gone")
	}
	if err != nil {
		return nil, err
	}
	sb.files = append(sb.files, cur)
	curSize, err := fileSize(cur)
	if err != nil {
		_ = sb.Close()
		return nil, err
	}
	curBase := sb.End - curSize
	if curBase < 0 {
		_ = sb.Close()
		return nil, fmt.Errorf("the scrollback holds %d bytes, more than the %d recorded", curSize, sb.End)
	}
	sb.Start = curBase
	var prev *os.File
	var prevSize int64
	if curBase > 0 {
		prev, err = os.Open(filepath.Join(dir, ScrollbackPrevFile))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			prev = nil
		case err != nil:
			_ = sb.Close()
			return nil, err
		default:
			sb.files = append(sb.files, prev)
			if prevSize, err = fileSize(prev); err != nil {
				_ = sb.Close()
				return nil, err
			}
			sb.Start = max(0, curBase-prevSize)
		}
	}
	switch {
	case off < sb.Start:
		_ = sb.Close()
		return nil, &core.Error{
			Code:    core.CodeInvalidArgument,
			Message: fmt.Sprintf("offset %d was rotated out of the scrollback; the first byte kept is at %d", off, sb.Start),
			Details: map[string]string{"start": strconv.FormatInt(sb.Start, 10)},
		}
	case off > sb.End:
		_ = sb.Close()
		return nil, &core.Error{
			Code:    core.CodeInvalidArgument,
			Message: fmt.Sprintf("offset %d is past the end of the scrollback, %d", off, sb.End),
			Details: map[string]string{"end": strconv.FormatInt(sb.End, 10)},
		}
	}
	var readers []io.Reader
	if prev != nil && off < curBase {
		// prev holds [curBase-prevSize, curBase).
		readers = append(readers, io.NewSectionReader(prev, off-(curBase-prevSize), curBase-off))
	}
	from := max(off, curBase) - curBase
	readers = append(readers, io.NewSectionReader(cur, from, curSize-from))
	sb.r = io.MultiReader(readers...)
	return sb, nil
}

func fileSize(f *os.File) (int64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}
