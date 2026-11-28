package supervise

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

// endedDir writes an ended session's state with the given logical length,
// and the scrollback files: cur always, prev when not nil.
func endedDir(t *testing.T, end int64, prev, cur string, ended bool) string {
	t.Helper()
	dir := t.TempDir()
	start := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	st := core.SupervisorState{
		Version: core.SupervisorStateVersion, SessionID: core.NewSessionID([16]byte{3}), PID: 42,
		Mode: core.SessionHeadless, Argv: []string{"agent"}, StartedAt: start,
	}
	if ended {
		code, endT := 0, start.Add(time.Minute)
		st.ExitCode, st.EndedAt, st.ScrollbackEnd = &code, &endT, end
	}
	if err := WriteState(dir, st); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ScrollbackFile), []byte(cur), 0o600); err != nil {
		t.Fatal(err)
	}
	if prev != "" {
		if err := os.WriteFile(filepath.Join(dir, ScrollbackPrevFile), []byte(prev), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func readAll(t *testing.T, dir string, off int64) (string, *Scrollback) {
	t.Helper()
	sb, err := OpenScrollback(dir, off)
	if err != nil {
		t.Fatalf("offset %d: %v", off, err)
	}
	defer sb.Close()
	b, err := io.ReadAll(sb)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), sb
}

func TestOpenScrollback(t *testing.T) {
	t.Run("one segment", func(t *testing.T) {
		dir := endedDir(t, 11, "", "hello world", true)
		for off, want := range map[int64]string{0: "hello world", 6: "world", 11: ""} {
			if got, sb := readAll(t, dir, off); got != want || sb.Start != 0 || sb.End != 11 {
				t.Errorf("offset %d: %q [%d,%d), want %q", off, got, sb.Start, sb.End, want)
			}
		}
	})
	t.Run("rotated", func(t *testing.T) {
		// 100 bytes produced: prev "abcde" is [90,95), cur "fghij" [95,100).
		dir := endedDir(t, 100, "abcde", "fghij", true)
		for off, want := range map[int64]string{90: "abcdefghij", 93: "defghij", 95: "fghij", 99: "j", 100: ""} {
			if got, sb := readAll(t, dir, off); got != want || sb.Start != 90 {
				t.Errorf("offset %d: %q start %d, want %q start 90", off, got, sb.Start, want)
			}
		}
		_, err := OpenScrollback(dir, 89)
		var ce *core.Error
		if !errors.As(err, &ce) || ce.Code != core.CodeInvalidArgument || ce.Details["start"] != "90" {
			t.Errorf("before start: %v", err)
		}
		_, err = OpenScrollback(dir, 101)
		if !errors.As(err, &ce) || ce.Code != core.CodeInvalidArgument || ce.Details["end"] != "100" {
			t.Errorf("past end: %v", err)
		}
	})
	t.Run("prev missing", func(t *testing.T) {
		dir := endedDir(t, 100, "", "fghij", true)
		if got, sb := readAll(t, dir, 95); got != "fghij" || sb.Start != 95 {
			t.Errorf("%q start %d", got, sb.Start)
		}
	})
	t.Run("running", func(t *testing.T) {
		dir := endedDir(t, 0, "", "abc", false)
		if _, err := OpenScrollback(dir, 0); !errors.Is(err, core.NewError(core.CodeConflict, "")) {
			t.Errorf("running session: %v, want conflict", err)
		}
	})
	t.Run("gone", func(t *testing.T) {
		dir := endedDir(t, 3, "", "abc", true)
		if err := os.Remove(filepath.Join(dir, ScrollbackFile)); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenScrollback(dir, 0); !errors.Is(err, core.NewError(core.CodeNotFound, "")) {
			t.Errorf("no scrollback: %v, want not_found", err)
		}
	})
}
