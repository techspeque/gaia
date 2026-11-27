package core

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const launcherContract = "../../docs/contracts/launcher-v1.md"

// contractExitCodes parses the table under "## Exit codes" in the launcher
// contract: every row whose first cell is a number, code -> backticked name.
func contractExitCodes(t *testing.T, doc string) map[int]string {
	t.Helper()
	_, section, ok := strings.Cut(doc, "\n## Exit codes\n")
	if !ok {
		t.Fatal(`launcher contract has no "## Exit codes" section`)
	}
	if i := strings.Index(section, "\n## "); i >= 0 {
		section = section[:i]
	}
	codes := map[int]string{}
	for line := range strings.SplitSeq(section, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 2 {
			continue
		}
		code, err := strconv.Atoi(strings.TrimSpace(cells[0]))
		if err != nil {
			continue // header or separator row
		}
		name := strings.TrimSpace(cells[1])
		if !strings.HasPrefix(name, "`") || !strings.HasSuffix(name, "`") || len(name) < 3 {
			t.Fatalf("exit code %d: name cell %q is not a backticked name", code, name)
		}
		if _, dup := codes[code]; dup {
			t.Fatalf("exit code %d listed twice", code)
		}
		codes[code] = strings.Trim(name, "`")
	}
	if len(codes) == 0 {
		t.Fatal("no exit-code rows parsed from the launcher contract")
	}
	return codes
}

func TestExitCodesMatchLauncherContract(t *testing.T) {
	b, err := os.ReadFile(filepath.FromSlash(launcherContract))
	if err != nil {
		t.Fatal(err)
	}
	doc := contractExitCodes(t, string(b))

	for _, c := range ExitCodes {
		name, ok := doc[int(c)]
		if !ok {
			t.Errorf("exit code %d (%s) is not in the launcher contract", c, c.Name())
			continue
		}
		if name != c.Name() {
			t.Errorf("exit code %d: contract says %q, code says %q", c, name, c.Name())
		}
	}
	consts := map[int]bool{}
	for _, c := range ExitCodes {
		consts[int(c)] = true
	}
	for code, name := range doc {
		if !consts[code] {
			t.Errorf("contract exit code %d (%s) has no constant", code, name)
		}
	}
	if len(exitCodeNames) != len(ExitCodes) {
		t.Errorf("exitCodeNames has %d entries, ExitCodes %d", len(exitCodeNames), len(ExitCodes))
	}
}

func TestExitCodesReservedRange(t *testing.T) {
	for _, c := range ExitCodes {
		if !c.Reserved() {
			t.Errorf("%d outside reserved range", c)
		}
	}
	if ExitCode(0).Reserved() || ExitCode(89).Reserved() || ExitCode(100).Reserved() || ExitCode(130).Reserved() {
		t.Error("code outside 90..99 reported reserved")
	}
	if ExitCode(1).Name() != "" {
		t.Error("unreserved code has a name")
	}
	for _, c := range ExitCodes {
		want := c == ExitNodeUnavailable || c == ExitSessionLost
		if c.Retryable() != want {
			t.Errorf("%d Retryable = %v", c, c.Retryable())
		}
	}
}

// The parser itself must catch a drifted document, or the contract test
// proves nothing.
func TestContractParserDetectsDrift(t *testing.T) {
	doc := "x\n## Exit codes\n\n| Code | Name | Meaning |\n|---|---|---|\n| 90 | `no_daemon` | m |\n| 91 | `renamed` | m |\n\n## Next\n| 92 | `other` | m |\n"
	got := contractExitCodes(t, doc)
	if len(got) != 2 || got[90] != "no_daemon" || got[91] != "renamed" {
		t.Fatalf("parsed %v", got)
	}
}
