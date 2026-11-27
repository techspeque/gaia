package core

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// deniedImports are standard-library packages that do I/O or reach the
// process environment. internal/core must stay pure (OVERVIEW §2.3).
var deniedImports = map[string]bool{
	"bufio": true, "crypto/rand": true, "database/sql": true, "embed": true,
	"io": true, "io/fs": true, "io/ioutil": true, "log": true, "log/slog": true,
	"net": true, "net/http": true, "os": true, "os/exec": true, "os/signal": true,
	"os/user": true, "path/filepath": true, "plugin": true, "runtime/cgo": true,
	"syscall": true, "unsafe": true,
}

func TestCoreImportsNoModulePackageAndNoIO(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if !strings.Contains(strings.SplitN(path, "/", 2)[0], ".") {
				if deniedImports[path] || strings.HasPrefix(path, "net/") || strings.HasPrefix(path, "os/") {
					t.Errorf("%s imports %s, which does I/O", name, path)
				}
				continue
			}
			t.Errorf("%s imports non-standard package %s", name, path)
		}
	}
	if checked == 0 {
		t.Fatal("no source files checked")
	}
}
