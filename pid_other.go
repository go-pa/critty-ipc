//go:build !linux

package ipc

import (
	"context"
	"errors"
	"fmt"
)

// PID returns the process ID of the Alacritty process listening on the
// socket. It asks the operating system rather than Alacritty, so it works
// with every Alacritty version and without --remote-control.
//
// PID is supported only on Linux; elsewhere it returns an error wrapping
// [errors.ErrUnsupported].
func (c *Client) PID(ctx context.Context) (int, error) {
	return 0, fmt.Errorf("alacritty ipc: PID: %w", errors.ErrUnsupported)
}
