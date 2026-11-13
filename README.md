# critty-ipc

A Go client for the IPC socket of the [Alacritty](https://alacritty.org)
terminal emulator and of critty, an Alacritty fork that adds remote control:
reading a window's screen and typing into it from another program.

```sh
go get github.com/go-pa/critty-ipc
```

```go
import ipc "github.com/go-pa/critty-ipc"

// Start the terminal with: critty --remote-control build
c := ipc.Client{Socket: ipc.RemoteControlSocket("build")}
err := c.SendText(ctx, 0, "make test\r")
screen, err := c.ReadScreen(ctx, 0)
pid, err := c.PID(ctx)
```

The configuration and window commands also work with stock Alacritty; reading
the screen and sending text need critty started with `--remote-control NAME`.
`Client.PID` is Linux only.

Requires Go 1.27 or later.
