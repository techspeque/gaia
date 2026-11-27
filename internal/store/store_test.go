package store

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

func stateDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "state")
}

func mustOpen(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatalf("Open(%s): %v", dir, err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func backups(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, dbFileName+".v*.bak"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// snapshot maps every file name in dir to its contents.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[e.Name()] = string(b)
	}
	return out
}

// rawDB opens the database file directly, bypassing Open.
func rawDB(t *testing.T, file string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(file, false))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func setUserVersion(t *testing.T, file string, v int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+file)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA user_version = " + itoa(v)); err != nil {
		t.Fatal(err)
	}
}

var itoa = strconv.Itoa

func wantCode(t *testing.T, err error, code core.ErrorCode) *core.Error {
	t.Helper()
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("err = %v, want core.Error with code %s", err, code)
	}
	return ce
}

func TestFreshOpenMigratesAndReopens(t *testing.T) {
	dir := stateDir(t)
	s := mustOpen(t, dir)

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o700 {
			t.Errorf("state dir mode = %04o, want 0700", got)
		}
		fi, err = os.Stat(filepath.Join(dir, dbFileName))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("database mode = %04o, want 0600", got)
		}
	}

	ctx := context.Background()
	v, err := userVersion(ctx, s.r)
	if err != nil {
		t.Fatal(err)
	}
	if v != SchemaVersion() || v < 1 {
		t.Errorf("user_version = %d, want %d", v, SchemaVersion())
	}
	var mode string
	if err := s.r.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	if got := s.w.Stats().MaxOpenConnections; got != 1 {
		t.Errorf("writer MaxOpenConnections = %d, want 1", got)
	}
	if _, err := s.r.ExecContext(ctx, "DELETE FROM nodes"); err == nil {
		t.Error("reader pool accepted a write")
	}
	if b := backups(t, dir); len(b) != 0 {
		t.Errorf("fresh database was backed up: %v", b)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := mustOpen(t, dir)
	if v, _ := userVersion(ctx, s2.r); v != SchemaVersion() {
		t.Errorf("reopened user_version = %d", v)
	}
	if b := backups(t, dir); len(b) != 0 {
		t.Errorf("reopen at current version made a backup: %v", b)
	}
}

func TestNewerSchemaRefusedAndUntouched(t *testing.T) {
	dir := stateDir(t)
	s := mustOpen(t, dir)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, dbFileName)
	newer := SchemaVersion() + 1
	setUserVersion(t, file, newer)

	before := snapshot(t, dir)
	_, err := Open(context.Background(), dir)
	ce := wantCode(t, err, core.CodeVersionMismatch)
	if ce.Details["db_version"] != itoa(newer) || ce.Details["binary_version"] != itoa(SchemaVersion()) {
		t.Errorf("details = %v", ce.Details)
	}
	after := snapshot(t, dir)
	if !maps.Equal(before, after) {
		t.Errorf("state directory changed: before %v files, after %v files",
			slices.Sorted(maps.Keys(before)), slices.Sorted(maps.Keys(after)))
	}
}

// TestNewerSchemaInWALRefusedAndUntouched covers a crash that left a newer
// user_version committed in the WAL only: the header still shows the
// current version, yet Open must refuse and must not checkpoint or remove
// the WAL.
func TestNewerSchemaInWALRefusedAndUntouched(t *testing.T) {
	src := stateDir(t)
	s := mustOpen(t, src)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(src, dbFileName)
	db := rawDB(t, file)
	newer := SchemaVersion() + 1
	for _, q := range []string{"PRAGMA wal_autocheckpoint = 0", "PRAGMA user_version = " + itoa(newer)} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	// Copy the database and WAL while the connection is open, as a crash
	// would leave them.
	dir := stateDir(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal"} {
		b, err := os.ReadFile(file + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, dbFileName+suffix), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if hv, _, err := headerVersion(filepath.Join(dir, dbFileName)); err != nil || hv != SchemaVersion() {
		t.Fatalf("header version = %d, %v; want %d", hv, err, SchemaVersion())
	}

	before := snapshot(t, dir)
	_, err := Open(context.Background(), dir)
	ce := wantCode(t, err, core.CodeVersionMismatch)
	if ce.Details["db_version"] != itoa(newer) || ce.Details["binary_version"] != itoa(SchemaVersion()) {
		t.Errorf("details = %v", ce.Details)
	}
	after := snapshot(t, dir)
	if !maps.Equal(before, after) {
		t.Errorf("state directory changed: before %v files, after %v files",
			slices.Sorted(maps.Keys(before)), slices.Sorted(maps.Keys(after)))
	}
}

// TestNewerSchemaInsideMigration covers the second check: a version the
// file header does not show yet (still in the WAL) is caught in the
// migration transaction.
func TestNewerSchemaInsideMigration(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), dbFileName)
	db := rawDB(t, file)
	if _, err := db.Exec("PRAGMA user_version = 5"); err != nil {
		t.Fatal(err)
	}
	migs := []migration{{1, "0001_a.sql", "CREATE TABLE a (x)"}}
	err := migrate(ctx, db, migs, true, func(int) string { t.Fatal("backup attempted"); return "" })
	wantCode(t, err, core.CodeVersionMismatch)
}

func TestBackupBeforeMigratingExisting(t *testing.T) {
	dir := stateDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, dbFileName)
	// An existing database at version 0 holding data of its own.
	db := rawDB(t, file)
	if _, err := db.Exec("CREATE TABLE keep (x TEXT); INSERT INTO keep VALUES ('old')"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	mustOpen(t, dir)
	b := backups(t, dir)
	if len(b) != 1 {
		t.Fatalf("backups = %v, want one", b)
	}
	if !strings.Contains(filepath.Base(b[0]), ".v0-") {
		t.Errorf("backup name %s does not carry the old version", b[0])
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(b[0])
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("backup mode = %04o, want 0600", got)
		}
	}
	hv, _, err := headerVersion(b[0])
	if err != nil || hv != 0 {
		t.Errorf("backup header version = %d, %v; want 0", hv, err)
	}
	bdb := rawDB(t, b[0])
	var x string
	if err := bdb.QueryRow("SELECT x FROM keep").Scan(&x); err != nil || x != "old" {
		t.Errorf("backup row = %q, %v", x, err)
	}
	var n int
	if err := bdb.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name = 'nodes'").Scan(&n); err != nil || n != 0 {
		t.Errorf("backup has the nodes table (%d, %v); want the pre-migration schema", n, err)
	}
}

func TestMigrateRunsOnlyPendingWithBackup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := rawDB(t, filepath.Join(dir, dbFileName))
	migs := []migration{
		{1, "0001_a.sql", "CREATE TABLE a (x)"},
		{2, "0002_b.sql", "CREATE TABLE b (x); INSERT INTO b VALUES (1)"},
	}
	var backedUp []int
	bp := func(from int) string {
		backedUp = append(backedUp, from)
		return filepath.Join(dir, backupName(from, time.Now()))
	}
	if err := migrate(ctx, db, migs[:1], false, bp); err != nil {
		t.Fatal(err)
	}
	if len(backedUp) != 0 {
		t.Fatalf("fresh migrate backed up: %v", backedUp)
	}
	if err := migrate(ctx, db, migs, true, bp); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(backedUp, []int{1}) {
		t.Errorf("backups taken at versions %v, want [1]", backedUp)
	}
	if v, _ := userVersion(ctx, db); v != 2 {
		t.Errorf("user_version = %d, want 2", v)
	}
	if err := migrate(ctx, db, migs, true, bp); err != nil {
		t.Fatal(err)
	}
	if len(backedUp) != 1 {
		t.Errorf("migrate at current version backed up again")
	}
}

func TestFailedMigrationKeepsPreviousVersion(t *testing.T) {
	ctx := context.Background()
	db := rawDB(t, filepath.Join(t.TempDir(), dbFileName))
	migs := []migration{
		{1, "0001_a.sql", "CREATE TABLE a (x)"},
		{2, "0002_bad.sql", "CREATE TABLE b (x); THIS IS NOT SQL"},
	}
	if err := migrate(ctx, db, migs, false, nil); err == nil {
		t.Fatal("bad migration succeeded")
	}
	if v, _ := userVersion(ctx, db); v != 1 {
		t.Errorf("user_version = %d, want 1", v)
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_schema WHERE name = 'b'").Scan(&n); err != nil || n != 0 {
		t.Errorf("partial migration left table b (%d, %v)", n, err)
	}
}

func TestStateDirRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode bits")
	}
	for _, mode := range []os.FileMode{0o755, 0o750, 0o701, 0o770, 0o707} {
		dir := stateDir(t)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		_, err := Open(context.Background(), dir)
		ce := wantCode(t, err, core.CodeInvalidArgument)
		if ce.Details["path"] != dir {
			t.Errorf("mode %04o: details = %v", mode, ce.Details)
		}
		if _, err := os.Stat(filepath.Join(dir, dbFileName)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("mode %04o: database created in a refused directory", mode)
		}
	}
}

func TestStateDirNotADirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), file)
	wantCode(t, err, core.CodeInvalidArgument)
}

func TestNotASQLiteFile(t *testing.T) {
	dir := stateDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, dbFileName), []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(context.Background(), dir)
	wantCode(t, err, core.CodeInvalidArgument)
}

func TestLoadMigrations(t *testing.T) {
	ok := fstest.MapFS{
		"0001_nodes.sql": {Data: []byte("a")},
		"0002_more.sql":  {Data: []byte("b")},
		"README.md":      {Data: []byte("ignored")},
	}
	m, err := loadMigrations(ok)
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || m[0].version != 1 || m[1].version != 2 || m[1].sql != "b" {
		t.Errorf("migrations = %+v", m)
	}

	bad := map[string]fstest.MapFS{
		"gap":       {"0001_a.sql": {}, "0003_c.sql": {}},
		"duplicate": {"0001_a.sql": {}, "0001_b.sql": {}},
		"from zero": {"0000_a.sql": {}},
		"bad name":  {"1_a.sql": {}},
		"uppercase": {"0001_A.sql": {}},
		"empty":     {},
	}
	for name, fsys := range bad {
		if _, err := loadMigrations(fsys); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	if _, err := builtinMigrations(); err != nil {
		t.Errorf("embedded migrations: %v", err)
	}
}

func TestHeaderVersion(t *testing.T) {
	dir := t.TempDir()
	if _, exists, err := headerVersion(filepath.Join(dir, "missing")); exists || err != nil {
		t.Errorf("missing file: exists %v, err %v", exists, err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if v, exists, err := headerVersion(empty); v != 0 || !exists || err != nil {
		t.Errorf("empty file: %d %v %v", v, exists, err)
	}
	short := filepath.Join(dir, "short")
	if err := os.WriteFile(short, []byte("SQLite format 3\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := headerVersion(short); err == nil {
		t.Error("short file accepted")
	}
}
