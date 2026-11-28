package supervise

import (
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/techspeque/gaia/internal/core"
)

// feed writes chunks through a new redactor and returns everything emitted,
// flush included.
func feed(secrets []string, chunks ...string) string {
	r := NewRedactor(secrets)
	var b strings.Builder
	for _, c := range chunks {
		b.Write(r.Write([]byte(c)))
	}
	b.Write(r.Flush())
	return b.String()
}

func TestRedactorTable(t *testing.T) {
	const R = core.Redacted
	cases := []struct {
		name    string
		secrets []string
		chunks  []string
		want    string
	}{
		{"no secrets", nil, []string{"abc", "def"}, "abcdef"},
		{"empty secret ignored", []string{""}, []string{"abc"}, "abc"},
		{"whole", []string{"tok123"}, []string{"a tok123 b"}, "a " + R + " b"},
		{"twice", []string{"tok"}, []string{"toktok"}, R + R},
		{"split", []string{"tok123"}, []string{"a to", "k1", "23 b"}, "a " + R + " b"},
		{"byte by byte", []string{"tok"}, []string{"x", "t", "o", "k", "y"}, "x" + R + "y"},
		{"prefix only kept", []string{"tok123"}, []string{"a tok12", " b"}, "a tok12 b"},
		{"prefix at end flushed", []string{"tok123"}, []string{"a tok1"}, "a tok1"},
		{"longest wins", []string{"abc", "abcdef"}, []string{"xabcdefy"}, "x" + R + "y"},
		{"longest wins split", []string{"abc", "abcdef"}, []string{"xabc", "def", "y"}, "x" + R + "y"},
		{"shorter when longer fails", []string{"abc", "abcdef"}, []string{"xabc", "dy"}, "x" + R + "dy"},
		{"adjacent secrets", []string{"aa", "bb"}, []string{"aab", "b"}, R + R},
		{"self overlap", []string{"aab"}, []string{"aaa", "ab"}, "aa" + R},
		{"duplicate secret", []string{"k", "k"}, []string{"kk"}, R + R},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := feed(c.secrets, c.chunks...); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestRedactorEverySplit splits a text holding a secret at every pair of
// positions: the secret never comes out and the rest is unchanged.
func TestRedactorEverySplit(t *testing.T) {
	const secret = "sk-test-0123456789"
	text := "before " + secret + " middle " + secret + "\nafter"
	want := strings.ReplaceAll(text, secret, core.Redacted)
	for i := 0; i <= len(text); i++ {
		for j := i; j <= len(text); j++ {
			got := feed([]string{secret}, text[:i], text[i:j], text[j:])
			if got != want {
				t.Fatalf("split at %d,%d: got %q, want %q", i, j, got, want)
			}
		}
	}
}

// TestRedactorHoldsOnlyPossiblePrefix proves bytes that cannot start a
// secret are emitted at once.
func TestRedactorHoldsOnlyPossiblePrefix(t *testing.T) {
	r := NewRedactor([]string{"secret"})
	if got := string(r.Write([]byte("hello "))); got != "hello " {
		t.Errorf("got %q, want everything at once", got)
	}
	if got := string(r.Write([]byte("wor s"))); got != "wor " {
		t.Errorf("got %q, want all but the possible prefix", got)
	}
	if got := string(r.Write([]byte("x"))); got != "sx" {
		t.Errorf("got %q, want the released prefix", got)
	}
}

// TestRedactorRandom checks random texts over a small alphabet, so partial
// matches are frequent, cut into random chunks, against strings.ReplaceAll.
func TestRedactorRandom(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	for _, secret := range []string{"abab", "aab", "abc", "ba"} {
		for range 2000 {
			var b strings.Builder
			for range rng.IntN(40) {
				b.WriteByte("abc"[rng.IntN(3)])
			}
			text := b.String()
			var chunks []string
			for rest := text; rest != ""; {
				n := 1 + rng.IntN(len(rest))
				chunks = append(chunks, rest[:n])
				rest = rest[n:]
			}
			got := feed([]string{secret}, chunks...)
			want := strings.ReplaceAll(text, secret, core.Redacted)
			if got != want {
				t.Fatalf("secret %q, chunks %q: got %q, want %q", secret, chunks, got, want)
			}
		}
	}
}
