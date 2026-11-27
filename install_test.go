package gaia

// Tests for scripts/install.sh. Each test serves a fake release over HTTP
// and runs the script with a PATH holding only the tools it needs, plus a
// fake cosign (and a fake uname) where a test asks for one.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const (
	installTag  = "v1.2.3"
	installStub = "#!/bin/sh\necho gaia stub\n"
)

// installTools are the commands install.sh runs, found on the real PATH.
var installTools = []string{
	"sh", "curl", "tar", "gzip", "awk", "cut", "sha256sum", "shasum",
	"mktemp", "chmod", "mkdir", "cp", "mv", "rm", "uname", "cat",
}

// fakeRelease is one release served by an httptest server.
type fakeRelease struct {
	files map[string][]byte // download/<tag>/<name> -> body
}

func installArchiveName() string {
	return fmt.Sprintf("gaia_%s_%s_%s.tar.gz", strings.TrimPrefix(installTag, "v"), runtime.GOOS, runtime.GOARCH)
}

// newFakeRelease returns a release for this platform whose checksums.txt
// matches its archive.
func newFakeRelease(t *testing.T) *fakeRelease {
	t.Helper()
	archive := tarGz(t, map[string]string{"gaia": installStub, "LICENSE": "MIT\n"})
	sum := sha256.Sum256(archive)
	checksums := fmt.Sprintf("%s  %s\n%s  gaia_other_windows_amd64.zip\n",
		hex.EncodeToString(sum[:]), installArchiveName(), strings.Repeat("0", 64))
	return &fakeRelease{files: map[string][]byte{
		installArchiveName():          archive,
		"checksums.txt":               []byte(checksums),
		"checksums.txt.sigstore.json": []byte(`{"fake":"bundle"}`),
	}}
}

func tarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// serve starts the fake release server and returns its releases base URL.
func (r *fakeRelease) serve(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, "/releases/tag/"+installTag, http.StatusFound)
	})
	mux.HandleFunc("/releases/tag/"+installTag, func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "release page")
	})
	mux.HandleFunc("/releases/download/"+installTag+"/", func(w http.ResponseWriter, req *http.Request) {
		b, ok := r.files[filepath.Base(req.URL.Path)]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(b)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/releases"
}

type installOpts struct {
	cosign string // "" none, else "ok" or "fail"
	uname  string // "" real, else the fake `uname -s` output
	env    []string
}

type installResult struct {
	code       int
	out        string
	dir        string // GAIA_INSTALL_DIR
	cosignArgs string
}

// runInstall runs scripts/install.sh against base with a minimal PATH.
func runInstall(t *testing.T, base string, o installOpts) installResult {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("install.sh supports linux and darwin only")
	}
	bin := t.TempDir()
	for _, tool := range installTools {
		if tool == "uname" && o.uname != "" {
			continue
		}
		p, err := exec.LookPath(tool)
		if err != nil {
			continue // e.g. sha256sum on darwin, shasum on some linux
		}
		if err := os.Symlink(p, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	argsFile := filepath.Join(t.TempDir(), "cosign-args")
	if o.cosign != "" {
		code := 0
		if o.cosign == "fail" {
			code = 1
		}
		writeScript(t, filepath.Join(bin, "cosign"),
			fmt.Sprintf("#!/bin/sh\necho \"$@\" > %q\nexit %d\n", argsFile, code))
	}
	if o.uname != "" {
		writeScript(t, filepath.Join(bin, "uname"),
			fmt.Sprintf("#!/bin/sh\ncase $1 in -s) echo %s ;; -m) echo x86_64 ;; esac\n", o.uname))
	}
	dir := filepath.Join(t.TempDir(), "bin")
	cmd := exec.Command("sh", "scripts/install.sh")
	cmd.Env = append([]string{
		"PATH=" + bin,
		"HOME=" + t.TempDir(),
		"TMPDIR=" + t.TempDir(),
		"GAIA_RELEASES_URL=" + base,
		"GAIA_INSTALL_DIR=" + dir,
	}, o.env...)
	out, err := cmd.CombinedOutput()
	res := installResult{out: string(out), dir: dir}
	if ee, ok := err.(*exec.ExitError); ok {
		res.code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(argsFile); err == nil {
		res.cosignArgs = string(b)
	}
	return res
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func assertInstalled(t *testing.T, r installResult) {
	t.Helper()
	if r.code != 0 {
		t.Fatalf("exit %d, want 0\n%s", r.code, r.out)
	}
	b, err := os.ReadFile(filepath.Join(r.dir, "gaia"))
	if err != nil {
		t.Fatalf("gaia not installed: %v\n%s", err, r.out)
	}
	if string(b) != installStub {
		t.Errorf("installed binary %q, want the archive's", b)
	}
	if fi, err := os.Stat(filepath.Join(r.dir, "gaia")); err == nil && fi.Mode().Perm() != 0o755 {
		t.Errorf("mode %s, want 0755", fi.Mode().Perm())
	}
}

func assertRefused(t *testing.T, r installResult, msg string) {
	t.Helper()
	if r.code == 0 {
		t.Fatalf("exit 0, want a refusal\n%s", r.out)
	}
	if !strings.Contains(r.out, msg) || !strings.Contains(r.out, "nothing was installed") {
		t.Errorf("output does not say %q and that nothing was installed:\n%s", msg, r.out)
	}
	entries, err := os.ReadDir(r.dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) > 0 {
		t.Errorf("install dir is not empty: %v", entries)
	}
}

func TestInstallVerifiesAndInstalls(t *testing.T) {
	base := newFakeRelease(t).serve(t)
	r := runInstall(t, base, installOpts{cosign: "ok", env: []string{"GAIA_VERSION=" + installTag}})
	assertInstalled(t, r)
	for _, want := range []string{
		"verify-blob", "--bundle", "checksums.txt.sigstore.json", "checksums.txt",
		"--certificate-oidc-issuer https://token.actions.githubusercontent.com",
		`--certificate-identity-regexp ^https://github\.com/techspeque/gaia/\.github/workflows/release\.yml@refs/tags/v`,
	} {
		if !strings.Contains(r.cosignArgs, want) {
			t.Errorf("cosign args %q lack %q", r.cosignArgs, want)
		}
	}
	if !strings.Contains(r.out, "signature of checksums.txt verified") || !strings.Contains(r.out, "checksum of") {
		t.Errorf("output does not report both checks:\n%s", r.out)
	}
}

func TestInstallResolvesLatest(t *testing.T) {
	base := newFakeRelease(t).serve(t)
	r := runInstall(t, base, installOpts{cosign: "ok"})
	assertInstalled(t, r)
	if !strings.Contains(r.out, installTag) {
		t.Errorf("output does not name %s:\n%s", installTag, r.out)
	}
}

func TestInstallRefusesChecksumMismatch(t *testing.T) {
	rel := newFakeRelease(t)
	rel.files[installArchiveName()] = tarGz(t, map[string]string{"gaia": "#!/bin/sh\necho tampered\n"})
	r := runInstall(t, rel.serve(t), installOpts{cosign: "ok"})
	assertRefused(t, r, "checksum mismatch")
}

func TestInstallRefusesArchiveNotInChecksums(t *testing.T) {
	rel := newFakeRelease(t)
	rel.files["checksums.txt"] = []byte(strings.Repeat("0", 64) + "  gaia_other_windows_amd64.zip\n")
	r := runInstall(t, rel.serve(t), installOpts{cosign: "ok"})
	assertRefused(t, r, "is not listed in checksums.txt")
}

func TestInstallRefusesBadSignature(t *testing.T) {
	r := runInstall(t, newFakeRelease(t).serve(t), installOpts{cosign: "fail"})
	assertRefused(t, r, "signature verification failed")
}

func TestInstallRefusesMissingSignatureBundle(t *testing.T) {
	rel := newFakeRelease(t)
	delete(rel.files, "checksums.txt.sigstore.json")
	r := runInstall(t, rel.serve(t), installOpts{cosign: "ok"})
	assertRefused(t, r, "download failed")
}

func TestInstallWithoutCosignWarns(t *testing.T) {
	r := runInstall(t, newFakeRelease(t).serve(t), installOpts{})
	assertInstalled(t, r)
	if !strings.Contains(r.out, "signature was NOT verified") {
		t.Errorf("no warning that the signature was not verified:\n%s", r.out)
	}
}

func TestInstallWithoutCosignRefusedWhenRequired(t *testing.T) {
	r := runInstall(t, newFakeRelease(t).serve(t), installOpts{env: []string{"GAIA_REQUIRE_SIGNATURE=1"}})
	assertRefused(t, r, "cosign is not installed")
}

func TestInstallRefusesUnsupportedOS(t *testing.T) {
	r := runInstall(t, newFakeRelease(t).serve(t), installOpts{cosign: "ok", uname: "FreeBSD"})
	assertRefused(t, r, "unsupported OS FreeBSD")
}

func TestInstallRefusesMissingRelease(t *testing.T) {
	r := runInstall(t, newFakeRelease(t).serve(t), installOpts{cosign: "ok", env: []string{"GAIA_VERSION=v9.9.9"}})
	assertRefused(t, r, "download failed")
}
