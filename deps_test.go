// Package gaia holds module-wide tests only. This file enforces the package
// dependency rule of OVERVIEW §2.3 and ADR-0001:
//
//   - host and node may import the leaves;
//   - a leaf (any internal package other than core, host and node) imports
//     only core, except proxy, which may also import creds;
//   - core imports no package of this module;
//   - internal/metis is imported only by host, internal/ship only by node;
//   - nothing imports github.com/techspeque/metis.
//
// A package's group is its first path element under internal/, so
// sub-packages share their parent's rules.
package gaia

import (
	"bufio"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const metisModule = "github.com/techspeque/metis"

// importEdge is one import statement: package from imports package to, in file.
type importEdge struct {
	from, to, file string
}

// readModulePath returns the module path declared in dir/go.mod.
func readModulePath(dir string) (string, error) {
	f, err := os.Open(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("no module line in %s/go.mod", dir)
}

// collectImports parses every .go file, test files included, under the
// module's cmd/ and internal/ trees and returns one edge per import.
// testdata directories and directories starting with "." or "_" are skipped,
// as the go tool skips them.
func collectImports(root, modPath string) ([]importEdge, error) {
	var edges []importEdge
	fset := token.NewFileSet()
	for _, top := range []string{"cmd", "internal"} {
		base := filepath.Join(root, top)
		if _, err := os.Stat(base); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := d.Name()
			if d.IsDir() {
				if path != base && (name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_")) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(name, ".go") {
				return nil
			}
			f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			from := modPath + "/" + filepath.ToSlash(rel)
			for _, imp := range f.Imports {
				to, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					return fmt.Errorf("%s: %w", path, err)
				}
				edges = append(edges, importEdge{from: from, to: to, file: path})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return edges, nil
}

// group names the rule group of a module package: the first element under
// internal/, "cmd" for anything under cmd/, "" for anything else.
func group(modPath, pkg string) string {
	if rest, ok := strings.CutPrefix(pkg, modPath+"/internal/"); ok {
		g, _, _ := strings.Cut(rest, "/")
		return g
	}
	if pkg == modPath+"/cmd" || strings.HasPrefix(pkg, modPath+"/cmd/") {
		return "cmd"
	}
	return ""
}

func isLeaf(g string) bool {
	return g != "" && g != "cmd" && g != "core" && g != "host" && g != "node"
}

// checkEdges returns one message per edge that breaks the dependency rule.
func checkEdges(modPath string, edges []importEdge) []string {
	var bad []string
	for _, e := range edges {
		if why := violation(modPath, e.from, e.to); why != "" {
			bad = append(bad, fmt.Sprintf("%s: %s imports %s: %s", e.file, e.from, e.to, why))
		}
	}
	sort.Strings(bad)
	return bad
}

// violation explains why from may not import to, or returns "".
func violation(modPath, from, to string) string {
	if to == metisModule || strings.HasPrefix(to, metisModule+"/") {
		return "Gaia never imports Metis Go packages (ADR-0001)"
	}
	tg := group(modPath, to)
	inModule := to == modPath || strings.HasPrefix(to, modPath+"/")
	if inModule && group(modPath, from) == "core" && tg != "core" {
		return "internal/core imports no package of this module"
	}
	if !strings.HasPrefix(to, modPath+"/internal/") {
		return ""
	}
	fg := group(modPath, from)
	if fg == tg {
		return ""
	}
	switch {
	case tg == "metis" && fg != "host":
		return "only internal/host may import internal/metis"
	case tg == "ship" && fg != "node":
		return "only internal/node may import internal/ship"
	case fg == "core":
		return "internal/core imports no package of this module"
	case isLeaf(fg) && tg != "core" && !(fg == "proxy" && tg == "creds"):
		return "a leaf imports only internal/core (proxy → creds is the one exception)"
	}
	return ""
}

func TestDependencyRule(t *testing.T) {
	modPath, err := readModulePath(".")
	if err != nil {
		t.Fatal(err)
	}
	edges, err := collectImports(".", modPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) == 0 {
		t.Fatal("collected no imports; the walk found no Go files")
	}
	for _, msg := range checkEdges(modPath, edges) {
		t.Error(msg)
	}
}

func TestDependencyRuleRejects(t *testing.T) {
	const m = "example.com/mod"
	in := func(p string) string { return m + "/internal/" + p }
	cases := []struct {
		from, to string
		bad      bool
	}{
		// Forbidden.
		{in("store"), in("creds"), true},
		{in("creds"), in("proxy"), true},
		{in("repo"), in("supervise"), true},
		{in("net"), in("store/sub"), true},
		{in("store/sub"), in("net"), true},
		{in("core"), in("store"), true},
		{in("core"), in("host"), true},
		{in("core"), m + "/shared", true},
		{in("core"), m, true},
		{in("core"), m + "/cmd/gaia", true},
		{in("node"), in("metis"), true},
		{in("store"), in("metis"), true},
		{m + "/cmd/gaia", in("metis"), true},
		{in("host"), in("ship"), true},
		{in("proxy"), in("ship"), true},
		{in("store"), in("host"), true},
		{in("service"), in("store"), true},
		{in("store"), metisModule, true},
		{in("host"), metisModule + "/internal/ledger", true},
		{m + "/cmd/gaia", metisModule + "/cmd/metis", true},
		// Allowed.
		{in("proxy"), in("creds"), false},
		{in("proxy"), in("core"), false},
		{in("store"), in("core"), false},
		{in("host"), in("store"), false},
		{in("host"), in("metis"), false},
		{in("host"), in("node"), false},
		{in("node"), in("ship"), false},
		{in("node"), in("net"), false},
		{in("store"), in("store/migrations"), false},
		{m + "/cmd/gaia", in("host"), false},
		{in("store"), "fmt", false},
		{in("core"), "errors", false},
		{in("core"), in("core/sub"), false},
		{in("core"), m + "x/shared", false},
		{in("net"), "github.com/techspeque/metisx", false},
	}
	for _, c := range cases {
		got := violation(m, c.from, c.to) != ""
		if got != c.bad {
			t.Errorf("%s → %s: violation=%v, want %v", c.from, c.to, got, c.bad)
		}
	}
}

// TestCollectImports proves the real-repository check is not vacuous: a
// forbidden import written to a fake module is found and reported, from a
// regular file and from a test file, and testdata is skipped.
func TestCollectImports(t *testing.T) {
	root := t.TempDir()
	const m = "example.com/fake"
	write := func(rel, src string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module "+m+"\n\ngo 1.26\n")
	write("internal/creds/creds.go", "package creds\n")
	write("internal/store/store.go", "package store\n\nimport (\n\t\"fmt\"\n\t_ \""+m+"/internal/creds\"\n)\n\nvar _ = fmt.Sprint\n")
	write("internal/repo/repo_test.go", "package repo\n\nimport _ \""+metisModule+"\"\n")
	write("internal/net/testdata/x.go", "package x\n\nimport _ \""+m+"/internal/store\"\n")
	write("cmd/gaia/main.go", "package main\n\nimport _ \""+m+"/internal/store\"\n\nfunc main() {}\n")

	modPath, err := readModulePath(root)
	if err != nil {
		t.Fatal(err)
	}
	if modPath != m {
		t.Fatalf("module path %q, want %q", modPath, m)
	}
	edges, err := collectImports(root, modPath)
	if err != nil {
		t.Fatal(err)
	}
	bad := checkEdges(modPath, edges)
	if len(bad) != 2 {
		t.Fatalf("got %d violations, want 2: %q", len(bad), bad)
	}
	if !strings.Contains(bad[0], m+"/internal/repo imports "+metisModule) {
		t.Errorf("missing the Metis import from a test file: %q", bad)
	}
	if !strings.Contains(bad[1], m+"/internal/store imports "+m+"/internal/creds") {
		t.Errorf("missing the leaf-to-leaf import: %q", bad)
	}
}
