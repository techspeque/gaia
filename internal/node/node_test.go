package node

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/techspeque/gaia/internal/core"
)

type fakeRegistrar struct {
	mu          sync.Mutex
	registers   int
	heartbeats  int
	registerErr error
	// forget makes the next heartbeat return not_found.
	forget bool
	beat   chan struct{}
}

func id(b byte) core.NodeID {
	var r [16]byte
	r[0] = b
	return core.NewNodeID(r)
}

func (f *fakeRegistrar) Register(_ context.Context, req core.RegisterNodeRequest) (core.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.registerErr != nil {
		return core.Node{}, f.registerErr
	}
	f.registers++
	return core.Node{ID: id(byte(f.registers)), Name: req.Name}, nil
}

func (f *fakeRegistrar) Heartbeat(_ context.Context, req core.HeartbeatRequest) (core.Node, error) {
	f.mu.Lock()
	f.heartbeats++
	forget := f.forget
	f.forget = false
	f.mu.Unlock()
	defer func() { f.beat <- struct{}{} }()
	if forget {
		return core.Node{}, core.NewError(core.CodeNotFound, "gone")
	}
	return core.Node{ID: req.ID}, nil
}

func (f *fakeRegistrar) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registers, f.heartbeats
}

func TestRunLocalRegistersThenHeartbeats(t *testing.T) {
	f := &fakeRegistrar{beat: make(chan struct{}, 16), forget: false}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunLocal(ctx, f, LocalRequest("v1"), time.Millisecond, nil) }()
	for range 3 {
		select {
		case <-f.beat:
		case <-time.After(5 * time.Second):
			t.Fatal("no heartbeat")
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunLocal returned %v", err)
	}
	if reg, hb := f.counts(); reg != 1 || hb < 3 {
		t.Errorf("registers %d heartbeats %d", reg, hb)
	}
}

func TestRunLocalReregistersOnNotFound(t *testing.T) {
	f := &fakeRegistrar{beat: make(chan struct{}, 16), forget: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = RunLocal(ctx, f, LocalRequest("v1"), time.Millisecond, nil) }()
	for range 2 {
		select {
		case <-f.beat:
		case <-time.After(5 * time.Second):
			t.Fatal("no heartbeat")
		}
	}
	if reg, _ := f.counts(); reg != 2 {
		t.Errorf("registers %d, want 2", reg)
	}
}

func TestRunLocalRegisterFailure(t *testing.T) {
	want := errors.New("store closed")
	f := &fakeRegistrar{registerErr: want, beat: make(chan struct{}, 1)}
	if err := RunLocal(context.Background(), f, LocalRequest("v1"), time.Millisecond, nil); !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestLocalRequest(t *testing.T) {
	r := LocalRequest("v9")
	if r.TailnetIdentity != LocalIdentity || r.OS != runtime.GOOS || r.Arch != runtime.GOARCH ||
		r.GaiaVersion != "v9" || r.APIVersion != core.APIVersion || r.Name == "" || strings.Contains(r.Name, ".") {
		t.Errorf("LocalRequest = %+v", r)
	}
}
