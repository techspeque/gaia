package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strconv"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// migration is one numbered, forward-only schema step.
type migration struct {
	version int
	name    string
	sql     string
}

var migrationName = regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+\.sql$`)

// builtinMigrations returns the migrations embedded in this binary.
func builtinMigrations() ([]migration, error) {
	sub, err := fs.Sub(embeddedMigrations, "migrations")
	if err != nil {
		return nil, err
	}
	return loadMigrations(sub)
}

// SchemaVersion is the schema version this binary migrates to.
func SchemaVersion() int {
	m, err := builtinMigrations()
	if err != nil {
		panic(err) // the embedded set is checked by tests
	}
	return len(m)
}

// loadMigrations reads every *.sql file at the root of fsys. Names must be
// NNNN_name.sql and the numbers must run 1..N with no gap or duplicate.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries { // ReadDir returns entries sorted by name
		if e.IsDir() || path.Ext(e.Name()) != ".sql" {
			continue
		}
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration %q: want NNNN_name.sql", e.Name())
		}
		v, _ := strconv.Atoi(m[1])
		if want := len(out) + 1; v != want {
			return nil, fmt.Errorf("migration %q: want version %04d next", e.Name(), want)
		}
		b, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(b)})
	}
	if len(out) == 0 {
		return nil, errors.New("no migrations")
	}
	return out, nil
}

// sqliteMagic starts every SQLite database file.
const sqliteMagic = "SQLite format 3\x00"

// headerVersion reads PRAGMA user_version straight from the database file
// header (big-endian int32 at offset 60), without opening a connection, so
// checking a database changes nothing. A missing file reports exists false;
// an empty file is a new database at version 0.
func headerVersion(file string) (version int, exists bool, err error) {
	f, err := os.Open(file)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	var h [100]byte
	n, err := io.ReadFull(f, h[:])
	if n == 0 && errors.Is(err, io.EOF) {
		return 0, true, nil
	}
	if err != nil || string(h[:len(sqliteMagic)]) != sqliteMagic {
		return 0, true, &core.Error{
			Code:    core.CodeInvalidArgument,
			Message: fmt.Sprintf("%s is not a SQLite database", file),
			Details: map[string]string{"path": file},
		}
	}
	return int(int32(binary.BigEndian.Uint32(h[60:64]))), true, nil
}

func versionMismatch(dbVersion, binaryVersion int) *core.Error {
	return &core.Error{
		Code: core.CodeVersionMismatch,
		Message: fmt.Sprintf("database schema version %d is newer than this binary's %d: upgrade gaia",
			dbVersion, binaryVersion),
		Details: map[string]string{
			"db_version":     strconv.Itoa(dbVersion),
			"binary_version": strconv.Itoa(binaryVersion),
		},
	}
}

func userVersion(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (int, error) {
	var v int
	err := q.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

// migrate brings db up to the last of migs. When existed is true and a
// migration is pending, the database is first copied to backupPath. Each
// migration runs in its own transaction that also sets user_version, so a
// failure leaves the database at the previous version.
func migrate(ctx context.Context, db *sql.DB, migs []migration, existed bool, backupPath func(from int) string) error {
	target := len(migs)
	v, err := userVersion(ctx, db)
	if err != nil {
		return err
	}
	if v > target {
		return versionMismatch(v, target)
	}
	if v == target {
		return nil
	}
	if existed {
		if err := backup(ctx, db, backupPath(v)); err != nil {
			return err
		}
	}
	for _, m := range migs[v:] {
		if err := apply(ctx, db, m, target); err != nil {
			return err
		}
	}
	return nil
}

func apply(ctx context.Context, db *sql.DB, m migration, target int) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	v, err := userVersion(ctx, tx)
	if err != nil {
		return err
	}
	if v > target {
		return versionMismatch(v, target)
	}
	if v != m.version-1 {
		return fmt.Errorf("migration %s: database at version %d, want %d", m.name, v, m.version-1)
	}
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	// PRAGMA takes no bound parameters; m.version is an int.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return fmt.Errorf("migration %s: %w", m.name, err)
	}
	return tx.Commit()
}

// backup writes a consistent copy of db to file at mode 0600.
func backup(ctx context.Context, db *sql.DB, file string) error {
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", file); err != nil {
		return fmt.Errorf("back up database to %s: %w", file, err)
	}
	return os.Chmod(file, 0o600)
}

// backupName is the backup file name for a database at version from.
func backupName(from int, now time.Time) string {
	return fmt.Sprintf("%s.v%d-%s.bak", dbFileName, from, now.UTC().Format("20060102T150405.000000000Z"))
}
