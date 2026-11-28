//go:build linux || darwin

package supervise

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

// TestScrollbackRotationBound: with MaxScrollback set, the scrollback files
// never hold more than the bound together while the agent writes several
// times as much; they keep the newest bytes; a stream from before the
// first kept offset is refused, one from it gets every byte after it; and
// the logical length is recorded on exit.
func TestScrollbackRotationBound(t *testing.T) {
	const bound = 1000
	const lines = 400
	var want strings.Builder
	for i := 1; i <= lines; i++ {
		fmt.Fprintf(&want, "line %d\n", i)
	}
	spec := newSpec(t, core.SessionHeadless, fmt.Sprintf("stream:%d:2ms", lines), "exit:0")
	spec.MaxScrollback = bound
	s := start(t, spec)

	diskSize := func() int64 {
		var n int64
		for _, f := range []string{ScrollbackFile, ScrollbackPrevFile} {
			if fi, err := os.Stat(filepath.Join(s.dir, f)); err == nil {
				n += fi.Size()
			}
		}
		return n
	}
	sampled := make(chan int64)
	stopSampling := make(chan struct{})
	go func() {
		var most int64
		for {
			select {
			case <-stopSampling:
				sampled <- most
				return
			default:
			}
			most = max(most, diskSize())
			time.Sleep(time.Millisecond)
		}
	}()

	// Wait for the first rotation, then check the refusal and stream the rest.
	var st Status
	deadline := time.Now().Add(15 * time.Second)
	for {
		var err error
		if st, err = s.client.Status(ctxT(t)); err != nil {
			t.Fatal(err)
		}
		if st.Start > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no rotation: %+v", st)
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, err := s.client.Stream(ctxT(t), 0, io.Discard)
	wantCode(t, err, core.CodeInvalidArgument)
	var got bytes.Buffer
	from := st.Start
	if _, err := s.client.Stream(ctxT(t), from, &got); err != nil {
		t.Fatalf("stream from start %d: %v", from, err)
	}
	end := s.wait()
	close(stopSampling)
	if most := <-sampled; most > bound {
		t.Errorf("scrollback on disk reached %d bytes, bound %d", most, bound)
	}
	if end.ScrollbackEnd != int64(want.Len()) {
		t.Errorf("scrollback_end %d, want %d", end.ScrollbackEnd, want.Len())
	}
	if got.String() != want.String()[from:] {
		t.Errorf("stream from %d differs from the output after it", from)
	}
	prev, _ := os.ReadFile(filepath.Join(s.dir, ScrollbackPrevFile))
	kept := string(prev) + s.scrollback()
	if len(kept) > bound || len(kept) < bound/2 || !strings.HasSuffix(want.String(), kept) {
		t.Errorf("kept %d bytes, want the last %d to %d bytes of the output", len(kept), bound/2, bound)
	}
}

func TestSpecMaxScrollbackValidated(t *testing.T) {
	for _, m := range []int64{-1, 1} {
		spec := newSpec(t, core.SessionHeadless, "exit:0")
		spec.MaxScrollback = m
		if err := spec.Validate(); err == nil {
			t.Errorf("max_scrollback %d accepted", m)
		}
	}
}
