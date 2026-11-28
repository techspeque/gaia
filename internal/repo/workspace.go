package repo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Workspace is the node's directory of checkouts, one per remote URL.
type Workspace struct {
	root string

	mu    sync.Mutex
	slots map[string]*slot
}

// slot queues holders of one checkout, first come first served.
type slot struct {
	held    bool
	waiters []chan struct{}
}

// Open returns the workspace at root, creating it (0700) if needed.
func Open(root string) (*Workspace, error) {
	if root == "" {
		return nil, errors.New("repo: workspace root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("repo: workspace root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, fmt.Errorf("repo: creating workspace: %w", err)
	}
	return &Workspace{root: abs, slots: map[string]*slot{}}, nil
}

// Root is the workspace directory.
func (w *Workspace) Root() string { return w.root }

// Checkout is one held checkout. Only its holder may use it; Release hands
// it to the next in the queue.
type Checkout struct {
	w    *Workspace
	url  string
	name string
	dir  string
	once sync.Once
}

// Acquire validates url and waits until no other holder has its checkout.
// The checkout is cloned on the first Prepare, not here. A cancelled ctx
// ends the wait without taking the checkout.
func (w *Workspace) Acquire(ctx context.Context, url string) (*Checkout, error) {
	name, err := DirName(url)
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(w.root, name)
	if filepath.Dir(dir) != w.root || filepath.Base(dir) != name {
		return nil, invalid("repository URL maps outside the workspace")
	}
	if err := w.lock(ctx, name); err != nil {
		return nil, err
	}
	c := &Checkout{w: w, url: url, name: name, dir: dir}
	if fi, err := os.Lstat(dir); err == nil && !fi.IsDir() {
		c.Release()
		return nil, syncErr(StageBefore, ReasonNotACheckout, "", "checkout path is not a directory")
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		c.Release()
		return nil, fmt.Errorf("repo: checkout path: %w", err)
	}
	return c, nil
}

// Dir is the checkout's working tree, where the session runs.
func (c *Checkout) Dir() string { return c.dir }

// URL is the remote URL the checkout tracks as origin.
func (c *Checkout) URL() string { return c.url }

// Release gives the checkout to the next waiter. Calling it again is a no-op.
func (c *Checkout) Release() {
	c.once.Do(func() { c.w.unlock(c.name) })
}

func (w *Workspace) lock(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	s := w.slots[name]
	if s == nil {
		s = &slot{}
		w.slots[name] = s
	}
	if !s.held {
		s.held = true
		w.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	s.waiters = append(s.waiters, ch)
	w.mu.Unlock()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case <-ch:
		// Handed over while giving up: pass it on.
		w.handOffLocked(s)
	default:
		for i, c := range s.waiters {
			if c == ch {
				s.waiters = append(s.waiters[:i], s.waiters[i+1:]...)
				break
			}
		}
	}
	return ctx.Err()
}

func (w *Workspace) unlock(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handOffLocked(w.slots[name])
}

func (w *Workspace) handOffLocked(s *slot) {
	if len(s.waiters) == 0 {
		s.held = false
		return
	}
	next := s.waiters[0]
	s.waiters = s.waiters[1:]
	close(next)
}
