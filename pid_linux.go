package ipc

import (
	"context"
	"net"
	"syscall"
)

// PID returns the process ID of the Alacritty process listening on the
// socket. It asks the operating system rather than Alacritty, so it works
// with every Alacritty version and without --remote-control.
//
// PID is supported only on Linux; elsewhere it returns an error wrapping
// [errors.ErrUnsupported].
func (c *Client) PID(ctx context.Context) (int, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	raw, err := conn.(*net.UnixConn).SyscallConn()
	if err != nil {
		return 0, err
	}

	// For a connected stream socket, SO_PEERCRED holds the credentials of
	// the process that called listen on the other end.
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	return int(cred.Pid), nil
}
