package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

func nodeID(b byte) core.NodeID {
	var r [16]byte
	r[0] = b
	return core.NewNodeID(r)
}

func testNode(b byte, name string) Node {
	t0 := time.Date(2026, 11, 27, 12, 0, 0, 123456789, time.UTC)
	return Node{
		ID:              nodeID(b),
		Name:            name,
		TailnetIdentity: "ident-" + name,
		OS:              "linux",
		Arch:            "arm64",
		GaiaVersion:     "v0.0.1",
		APIVersion:      core.APIVersion,
		CreatedAt:       t0,
		LastSeen:        t0.Add(time.Second),
	}
}

func sameNode(t *testing.T, got, want Node) {
	t.Helper()
	if got.ID != want.ID || got.Name != want.Name || got.TailnetIdentity != want.TailnetIdentity ||
		got.OS != want.OS || got.Arch != want.Arch || got.GaiaVersion != want.GaiaVersion ||
		got.APIVersion != want.APIVersion || !got.CreatedAt.Equal(want.CreatedAt) ||
		!got.LastSeen.Equal(want.LastSeen) {
		t.Errorf("node = %+v, want %+v", got, want)
	}
}

func TestNodeCRUD(t *testing.T) {
	ctx := context.Background()
	dir := stateDir(t)
	s := mustOpen(t, dir)

	if l, err := s.ListNodes(ctx); err != nil || l == nil || len(l) != 0 {
		t.Fatalf("empty list = %v, %v; want empty non-nil", l, err)
	}

	b, a := testNode(1, "bravo"), testNode(2, "alpha")
	for _, n := range []Node{b, a} {
		if err := s.CreateNode(ctx, n); err != nil {
			t.Fatalf("CreateNode(%s): %v", n.Name, err)
		}
	}

	got, err := s.GetNode(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	sameNode(t, got, a)

	l, err := s.ListNodes(ctx)
	if err != nil || len(l) != 2 {
		t.Fatalf("list = %v, %v", l, err)
	}
	sameNode(t, l[0], a)
	sameNode(t, l[1], b)

	seen := a.LastSeen.Add(time.Minute)
	if err := s.UpdateNodeLastSeen(ctx, a.ID, seen); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetNode(ctx, a.ID)
	if !got.LastSeen.Equal(seen) || !got.CreatedAt.Equal(a.CreatedAt) {
		t.Errorf("after update: last_seen %v created %v", got.LastSeen, got.CreatedAt)
	}

	if err := s.RemoveNode(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	_, err = s.GetNode(ctx, b.ID)
	wantCode(t, err, core.CodeNotFound)

	// Data survives a reopen.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := mustOpen(t, dir)
	l, err = s2.ListNodes(ctx)
	if err != nil || len(l) != 1 {
		t.Fatalf("after reopen list = %v, %v", l, err)
	}
	a.LastSeen = seen
	sameNode(t, l[0], a)
}

func TestNodeNotFound(t *testing.T) {
	ctx := context.Background()
	s := mustOpen(t, stateDir(t))
	id := nodeID(9)
	_, err := s.GetNode(ctx, id)
	ce := wantCode(t, err, core.CodeNotFound)
	if ce.Details["id"] != id.String() {
		t.Errorf("details = %v", ce.Details)
	}
	wantCode(t, s.UpdateNodeLastSeen(ctx, id, time.Now()), core.CodeNotFound)
	wantCode(t, s.RemoveNode(ctx, id), core.CodeNotFound)
	if !errors.Is(s.RemoveNode(ctx, id), core.NewError(core.CodeNotFound, "")) {
		t.Error("errors.Is does not match not_found")
	}
}

func TestNodeConflicts(t *testing.T) {
	ctx := context.Background()
	s := mustOpen(t, stateDir(t))
	base := testNode(1, "alpha")
	if err := s.CreateNode(ctx, base); err != nil {
		t.Fatal(err)
	}
	sameID := testNode(1, "other")
	sameName := testNode(2, "alpha")
	sameName.TailnetIdentity = "ident-unique"
	sameIdent := testNode(3, "third")
	sameIdent.TailnetIdentity = base.TailnetIdentity
	for name, n := range map[string]Node{"id": sameID, "name": sameName, "identity": sameIdent} {
		t.Run(name, func(t *testing.T) { wantCode(t, s.CreateNode(ctx, n), core.CodeConflict) })
	}
	if l, _ := s.ListNodes(ctx); len(l) != 1 {
		t.Errorf("conflicting inserts left %d rows", len(l))
	}
}

func TestNodeInvalid(t *testing.T) {
	ctx := context.Background()
	s := mustOpen(t, stateDir(t))
	cases := map[string]func(*Node){
		"zero id":       func(n *Node) { n.ID = core.NodeID{} },
		"empty name":    func(n *Node) { n.Name = "" },
		"empty ident":   func(n *Node) { n.TailnetIdentity = "" },
		"zero created":  func(n *Node) { n.CreatedAt = time.Time{} },
		"zero lastseen": func(n *Node) { n.LastSeen = time.Time{} },
	}
	for name, mut := range cases {
		n := testNode(1, "alpha")
		mut(&n)
		if err := s.CreateNode(ctx, n); err == nil {
			t.Errorf("%s: accepted", name)
		} else {
			wantCode(t, err, core.CodeInvalidArgument)
		}
	}
	if err := s.CreateNode(ctx, testNode(1, "alpha")); err != nil {
		t.Fatal(err)
	}
	wantCode(t, s.UpdateNodeLastSeen(ctx, nodeID(1), time.Time{}), core.CodeInvalidArgument)
}

// TestConcurrentWriters checks writes from many goroutines serialise on the
// one writer without busy errors, while reads run alongside.
func TestConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	s := mustOpen(t, stateDir(t))
	n := testNode(1, "alpha")
	if err := s.CreateNode(ctx, n); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := range 32 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs <- s.UpdateNodeLastSeen(ctx, n.ID, n.LastSeen.Add(time.Duration(i)*time.Second))
		}()
		go func() {
			defer wg.Done()
			_, err := s.ListNodes(ctx)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestUpdateNode(t *testing.T) {
	ctx := context.Background()
	s := mustOpen(t, stateDir(t))
	a, b := testNode(1, "alpha"), testNode(2, "bravo")
	for _, n := range []Node{a, b} {
		if err := s.CreateNode(ctx, n); err != nil {
			t.Fatal(err)
		}
	}

	u := a
	u.Name, u.OS, u.Arch, u.GaiaVersion, u.APIVersion = "alpha2", "darwin", "amd64", "v0.0.2", core.APIVersion+1
	u.LastSeen = a.LastSeen.Add(time.Hour)
	u.CreatedAt = time.Time{} // ignored: created_at is kept
	if err := s.UpdateNode(ctx, u); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetNode(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	u.CreatedAt = a.CreatedAt
	sameNode(t, got, u)

	dup := u
	dup.Name = b.Name
	wantCode(t, s.UpdateNode(ctx, dup), core.CodeConflict)
	dup = u
	dup.TailnetIdentity = b.TailnetIdentity
	wantCode(t, s.UpdateNode(ctx, dup), core.CodeConflict)

	wantCode(t, s.UpdateNode(ctx, testNode(9, "zulu")), core.CodeNotFound)

	bad := u
	bad.Name = ""
	wantCode(t, s.UpdateNode(ctx, bad), core.CodeInvalidArgument)
	bad = u
	bad.LastSeen = time.Time{}
	wantCode(t, s.UpdateNode(ctx, bad), core.CodeInvalidArgument)

	got, _ = s.GetNode(ctx, a.ID)
	sameNode(t, got, u)
}
