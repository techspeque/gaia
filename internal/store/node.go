package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/techspeque/gaia/internal/core"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Node is a registered machine as stored (OVERVIEW §2.2). Whether it is
// online is derived from LastSeen by the host and is not stored.
type Node struct {
	ID              core.NodeID
	Name            string
	TailnetIdentity string
	// TailnetName is the MagicDNS short name the control server assigned
	// the node, which may differ from Name; "" when unknown.
	TailnetName string
	OS          string
	Arch        string
	GaiaVersion string
	APIVersion  int
	CreatedAt   time.Time
	LastSeen    time.Time
}

const nodeColumns = `id, name, tailnet_identity, tailnet_name, os, arch, gaia_version, api_version, created_at, last_seen`

// CreateNode inserts n. A duplicate id, name or tailnet identity is a
// core.CodeConflict error.
func (s *Store) CreateNode(ctx context.Context, n Node) error {
	if err := validateNode(n); err != nil {
		return err
	}
	_, err := s.w.ExecContext(ctx,
		`INSERT INTO nodes (`+nodeColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID.String(), n.Name, n.TailnetIdentity, n.TailnetName, n.OS, n.Arch, n.GaiaVersion, n.APIVersion,
		n.CreatedAt.UnixNano(), n.LastSeen.UnixNano())
	if isUniqueViolation(err) {
		return &core.Error{
			Code:    core.CodeConflict,
			Message: fmt.Sprintf("a node with id %s, name %q or this tailnet identity already exists", n.ID, n.Name),
			Details: map[string]string{"id": n.ID.String(), "name": n.Name},
		}
	}
	return err
}

// GetNode returns the node with id, or a core.CodeNotFound error.
func (s *Store) GetNode(ctx context.Context, id core.NodeID) (Node, error) {
	row := s.r.QueryRowContext(ctx, `SELECT `+nodeColumns+` FROM nodes WHERE id = ?`, id.String())
	n, err := scanNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Node{}, nodeNotFound(id)
	}
	return n, err
}

// ListNodes returns every node, ordered by name.
func (s *Store) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+nodeColumns+` FROM nodes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nodes := []Node{}
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, n)
	}
	return nodes, rows.Err()
}

// UpdateNodeLastSeen sets the node's last_seen to t, or returns a
// core.CodeNotFound error.
func (s *Store) UpdateNodeLastSeen(ctx context.Context, id core.NodeID, t time.Time) error {
	if t.IsZero() {
		return invalidNode("last_seen is zero")
	}
	res, err := s.w.ExecContext(ctx, `UPDATE nodes SET last_seen = ? WHERE id = ?`, t.UnixNano(), id.String())
	return affectedOne(res, err, id)
}

// UpdateNode stores n's name, tailnet identity, tailnet name, os, arch, gaia
// version, API version and last_seen on the row with n's id; created_at is
// kept. A
// missing row is core.CodeNotFound; a name or identity another node holds
// is core.CodeConflict.
func (s *Store) UpdateNode(ctx context.Context, n Node) error {
	if n.CreatedAt.IsZero() {
		n.CreatedAt = n.LastSeen // not stored; satisfies validateNode
	}
	if err := validateNode(n); err != nil {
		return err
	}
	res, err := s.w.ExecContext(ctx,
		`UPDATE nodes SET name = ?, tailnet_identity = ?, tailnet_name = ?, os = ?, arch = ?,
		 gaia_version = ?, api_version = ?, last_seen = ? WHERE id = ?`,
		n.Name, n.TailnetIdentity, n.TailnetName, n.OS, n.Arch, n.GaiaVersion, n.APIVersion, n.LastSeen.UnixNano(), n.ID.String())
	if isUniqueViolation(err) {
		return &core.Error{
			Code:    core.CodeConflict,
			Message: fmt.Sprintf("another node already has name %q or this tailnet identity", n.Name),
			Details: map[string]string{"id": n.ID.String(), "name": n.Name},
		}
	}
	return affectedOne(res, err, n.ID)
}

// RemoveNode deletes the node with id, or returns a core.CodeNotFound error.
func (s *Store) RemoveNode(ctx context.Context, id core.NodeID) error {
	res, err := s.w.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, id.String())
	return affectedOne(res, err, id)
}

func affectedOne(res sql.Result, err error, id core.NodeID) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return nodeNotFound(id)
	}
	return nil
}

func scanNode(row interface{ Scan(...any) error }) (Node, error) {
	var (
		n             Node
		id            string
		created, seen int64
	)
	if err := row.Scan(&id, &n.Name, &n.TailnetIdentity, &n.TailnetName, &n.OS, &n.Arch, &n.GaiaVersion,
		&n.APIVersion, &created, &seen); err != nil {
		return Node{}, err
	}
	var err error
	if n.ID, err = core.ParseNodeID(id); err != nil {
		return Node{}, fmt.Errorf("stored node id: %w", err)
	}
	n.CreatedAt = time.Unix(0, created).UTC()
	n.LastSeen = time.Unix(0, seen).UTC()
	return n, nil
}

func validateNode(n Node) error {
	switch {
	case n.ID.IsZero():
		return invalidNode("id is empty")
	case n.Name == "":
		return invalidNode("name is empty")
	case n.TailnetIdentity == "":
		return invalidNode("tailnet identity is empty")
	case n.CreatedAt.IsZero():
		return invalidNode("created_at is zero")
	case n.LastSeen.IsZero():
		return invalidNode("last_seen is zero")
	}
	return nil
}

func invalidNode(reason string) *core.Error {
	return core.NewError(core.CodeInvalidArgument, "invalid node: "+reason)
}

func nodeNotFound(id core.NodeID) *core.Error {
	return &core.Error{
		Code:    core.CodeNotFound,
		Message: fmt.Sprintf("node %s not found", id),
		Details: map[string]string{"id": id.String()},
	}
}

func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	c := se.Code()
	return c == sqlite3.SQLITE_CONSTRAINT_UNIQUE || c == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY
}
