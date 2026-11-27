package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/techspeque/gaia/internal/core"

	_ "modernc.org/sqlite" // registers the "sqlite" driver; pure Go, CGO off
)

// dbFileName is the database file inside the state directory.
const dbFileName = "gaia.db"

// Store is the host's database. It is safe for concurrent use.
type Store struct {
	w *sql.DB // the one writer: a single connection
	r *sql.DB // readers: query_only connections
}

// Open prepares the state directory dir, opens its database and migrates it
// to SchemaVersion. A database newer than this binary is refused with a
// core.CodeVersionMismatch error and left untouched.
func Open(ctx context.Context, dir string) (*Store, error) {
	if err := ensureStateDir(dir); err != nil {
		return nil, err
	}
	migs, err := builtinMigrations()
	if err != nil {
		return nil, err
	}
	file := filepath.Join(dir, dbFileName)
	hv, existed, err := headerVersion(file)
	if err != nil {
		return nil, err
	}
	if hv > len(migs) {
		return nil, versionMismatch(hv, len(migs))
	}
	if wv, ok, err := walVersion(ctx, file); err != nil {
		return nil, err
	} else if ok && wv > len(migs) {
		return nil, versionMismatch(wv, len(migs))
	}
	if !existed {
		// Create the file ourselves so it, and the -wal and -shm files
		// SQLite derives from it, are 0600.
		f, err := os.OpenFile(file, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, err
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	}

	w, err := sql.Open("sqlite", dsn(file, false))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetConnMaxLifetime(0)
	w.SetConnMaxIdleTime(0)
	w.SetMaxIdleConns(1)
	s := &Store{w: w}
	if err := s.checkWAL(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	backupPath := func(from int) string { return filepath.Join(dir, backupName(from, time.Now())) }
	if err := migrate(ctx, w, migs, existed, backupPath); err != nil {
		_ = s.Close()
		return nil, err
	}

	r, err := sql.Open("sqlite", dsn(file, true))
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	s.r = r
	if err := r.PingContext(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// Close closes every connection.
func (s *Store) Close() error {
	var errs []error
	if s.r != nil {
		errs = append(errs, s.r.Close())
	}
	if s.w != nil {
		errs = append(errs, s.w.Close())
	}
	return errors.Join(errs...)
}

// checkWAL confirms the writer connection put the database in WAL mode.
func (s *Store) checkWAL(ctx context.Context) error {
	var mode string
	if err := s.w.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return err
	}
	if mode != "wal" {
		return fmt.Errorf("database journal mode is %q, want wal", mode)
	}
	return nil
}

// dsn builds the modernc.org/sqlite connection string for file. Writer
// transactions take the write lock at BEGIN, so concurrent writers queue on
// busy_timeout instead of failing at their first write.
func dsn(file string, readOnly bool) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	if readOnly {
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
		q.Set("_txlock", "immediate")
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(file), RawQuery: q.Encode()}
	return u.String()
}

// ensureStateDir creates dir at mode 0700 if it is missing, and refuses it
// if it is not a directory or grants any permission to group or others.
func ensureStateDir(dir string) error {
	fi, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		fi, err = os.Stat(dir)
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return unsafeStateDir(dir, fi.Mode(), "is not a directory")
	}
	// Unix mode bits do not describe access on Windows, where the daemon
	// roles do not run yet (phase 0 plan).
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return unsafeStateDir(dir, fi.Mode(), "is readable by group or others: chmod 700 it")
	}
	return nil
}

func unsafeStateDir(dir string, mode fs.FileMode, reason string) *core.Error {
	return &core.Error{
		Code:    core.CodeInvalidArgument,
		Message: fmt.Sprintf("state directory %s %s", dir, reason),
		Details: map[string]string{"path": dir, "mode": fmt.Sprintf("%04o", mode.Perm())},
	}
}
