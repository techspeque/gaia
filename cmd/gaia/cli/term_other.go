//go:build windows

package cli

// Resized never delivers: there is no resize signal on this platform.
func (t fdTerminal) Resized() (<-chan struct{}, func()) { return nil, func() {} }
