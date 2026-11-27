// Package store is the host's SQLite access (ADR-0006).
//
// A Store owns one state directory, created at mode 0700 and refused if
// group or others can reach it. The database is pure-Go SQLite in WAL mode
// with one writer connection and a read-only reader pool. Numbered,
// embedded, forward-only migrations run at Open; an existing database is
// backed up before any migration, and a database newer than this binary is
// refused without being touched.
package store
