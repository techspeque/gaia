package supervise

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/techspeque/gaia/internal/core"
)

// File names inside a session directory.
const (
	StateFile      = "state.json"
	ScrollbackFile = "scrollback"
	SocketFile     = "sock"
)

// WriteState validates st and replaces dir's state file with it atomically:
// a 0600 temporary file in dir, synced, renamed over the old one, and the
// directory synced. A reader sees the old state or the new one, never part.
func WriteState(dir string, st core.SupervisorState) error {
	if err := st.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.CreateTemp(dir, ".state-*.tmp") // CreateTemp makes it 0600
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, StateFile)); err != nil {
		return err
	}
	ok = true
	return syncDir(dir)
}

// ReadState reads and validates dir's state file. A missing file is a
// CodeNotFound error.
func ReadState(dir string) (core.SupervisorState, error) {
	path := filepath.Join(dir, StateFile)
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return core.SupervisorState{}, &core.Error{
			Code:    core.CodeNotFound,
			Message: "no supervisor state file at " + path,
			Details: map[string]string{"path": path},
		}
	}
	if err != nil {
		return core.SupervisorState{}, err
	}
	var st core.SupervisorState
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return core.SupervisorState{}, fmt.Errorf("decoding %s: %w", path, err)
	}
	if err := st.Validate(); err != nil {
		return core.SupervisorState{}, err
	}
	return st, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
