package critty

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

// fakeServer accepts one connection per request, records the message and
// answers with the next reply; an empty reply closes without answering.
func fakeServer(t *testing.T, replies ...string) (*Client, <-chan string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ipc.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	got := make(chan string, len(replies))
	go func() {
		for _, reply := range replies {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			msg, _ := io.ReadAll(conn)
			got <- string(msg)
			io.WriteString(conn, reply)
			conn.Close()
		}
	}()
	return &Client{Socket: path}, got
}

func TestRequests(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, *Client) error
		want string
	}{
		{
			name: "SendText",
			call: func(ctx context.Context, c *Client) error { return c.SendText(ctx, 0, "a\"\x03\r") },
			want: `{"SendText":{"text":"a\"\u0003\r","window_id":null}}`,
		},
		{
			name: "SetConfig",
			call: func(ctx context.Context, c *Client) error { return c.SetConfig(ctx, 42, "font.size=12") },
			want: `{"Config":{"options":["font.size=12"],"window_id":42,"reset":false}}`,
		},
		{
			name: "ResetConfig",
			call: func(ctx context.Context, c *Client) error { return c.ResetConfig(ctx, 0) },
			want: `{"Config":{"options":[],"window_id":null,"reset":true}}`,
		},
		{
			name: "CreateWindow/zero",
			call: func(ctx context.Context, c *Client) error { return c.CreateWindow(ctx, WindowOptions{}) },
			want: `{"CreateWindow":{"terminal_options":{"hold":false,"command":[]},"window_identity":{},"option":[]}}`,
		},
		{
			name: "CreateWindow/full",
			call: func(ctx context.Context, c *Client) error {
				return c.CreateWindow(ctx, WindowOptions{
					Command:          []string{"htop", "-d"},
					WorkingDirectory: "/tmp",
					Hold:             true,
					Title:            "T",
					ClassGeneral:     "gen",
					ClassInstance:    "inst",
					Options:          []string{"font.size=9"},
				})
			},
			want: `{"CreateWindow":{"terminal_options":{"working_directory":"/tmp","hold":true,"command":["htop","-d"]},` +
				`"window_identity":{"title":"T","class":{"general":"gen","instance":"inst"}},"option":["font.size=9"]}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, got := fakeServer(t, "")
			if err := tt.call(t.Context(), c); err != nil {
				t.Fatal(err)
			}
			if msg := <-got; msg != tt.want {
				t.Errorf("sent %s\nwant %s", msg, tt.want)
			}
		})
	}
}

func TestReadScreen(t *testing.T) {
	c, got := fakeServer(t, `{"ReadScreen":"$ ls\nfoo"}`, "", `{"Error":"no window with ID 9"}`)

	screen, err := c.ReadScreen(t.Context(), 7)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"ReadScreen":{"window_id":7}}`; <-got != want {
		t.Errorf("request does not match %s", want)
	}
	if screen != "$ ls\nfoo" {
		t.Errorf("ReadScreen = %q", screen)
	}

	if _, err := c.ReadScreen(t.Context(), 8); !errors.Is(err, ErrNoReply) {
		t.Errorf("ReadScreen without reply: err = %v, want ErrNoReply", err)
	}
	if _, err := c.ReadScreen(t.Context(), 9); err == nil {
		t.Error("ReadScreen of unknown window: err = nil, want *Error")
	}
}

func TestReadScreenANSI(t *testing.T) {
	c, got := fakeServer(t, `{"ReadScreen":"\u001b[0;31mred\u001b[0m"}`)

	screen, err := c.ReadScreenANSI(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"ReadScreen":{"window_id":null,"ansi":true}}`; <-got != want {
		t.Errorf("request does not match %s", want)
	}
	if screen != "\x1b[0;31mred\x1b[0m" {
		t.Errorf("ReadScreenANSI = %q", screen)
	}
}

func TestReadText(t *testing.T) {
	tests := []struct {
		opts ReadOptions
		want string
	}{
		{ReadOptions{}, `{"ReadScreen":{"window_id":null}}`},
		{ReadOptions{Lines: 500}, `{"ReadScreen":{"window_id":null,"lines":500}}`},
		{ReadOptions{Lines: -1, ANSI: true}, `{"ReadScreen":{"window_id":null,"ansi":true,"all":true}}`},
	}
	for _, tt := range tests {
		c, got := fakeServer(t, `{"ReadScreen":"299\n300"}`)
		text, err := c.ReadText(t.Context(), 0, tt.opts)
		if err != nil {
			t.Fatal(err)
		}
		if msg := <-got; msg != tt.want {
			t.Errorf("ReadText(%+v) sent %s, want %s", tt.opts, msg, tt.want)
		}
		if text != "299\n300" {
			t.Errorf("ReadText(%+v) = %q", tt.opts, text)
		}
	}
}

func TestGetConfig(t *testing.T) {
	c, _ := fakeServer(t, `{"GetConfig":"{\"font\":{\"size\":12}}"}`, `{"ReadScreen":""}`)

	config, err := c.GetConfig(t.Context(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(config) != `{"font":{"size":12}}` {
		t.Errorf("GetConfig = %s", config)
	}

	if _, err := c.GetConfig(t.Context(), 0); err == nil {
		t.Error("GetConfig accepted a ReadScreen reply")
	}
}

func TestContextCancel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ipc.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan struct{})
	defer close(done)
	go func() {
		// Accept but never reply.
		conn, err := ln.Accept()
		if err == nil {
			defer conn.Close()
			io.ReadAll(conn)
			<-done
		}
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	c := Client{Socket: path}
	if _, err := c.ReadScreen(ctx, 0); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want context.DeadlineExceeded", err)
	}
}

func TestRemoteControlSocket(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS sockets have no display in their name")
	}
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	t.Setenv("DISPLAY", ":0")
	t.Setenv("WAYLAND_DISPLAY", "wayland-1")
	if got, want := RemoteControlSocket("a/b"), "/run/user/1000/Alacritty-wayland-1-remote-control-a-b.sock"; got != want {
		t.Errorf("RemoteControlSocket = %s, want %s", got, want)
	}
}

func TestRemoteControlNames(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("macOS sockets live in the shared temporary directory")
	}
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv("DISPLAY", ":0")
	t.Setenv("WAYLAND_DISPLAY", "") // Restores the variable after the test.
	os.Unsetenv("WAYLAND_DISPLAY")

	listen := func(file string) *net.UnixListener {
		ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, file), Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { ln.Close() })
		return ln
	}
	listen("Alacritty-:0-remote-control-build.sock")
	listen("Alacritty-:0-remote-control-a-b.sock")
	listen("Alacritty-:0-4242.sock")                 // No remote control.
	listen("Alacritty-:1-remote-control-other.sock") // Another display.
	stale := listen("Alacritty-:0-remote-control-dead.sock")
	stale.SetUnlinkOnClose(false)
	stale.Close()

	got, err := RemoteControlNames()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a-b", "build"}; !slices.Equal(got, want) {
		t.Errorf("RemoteControlNames = %q, want %q", got, want)
	}
}

func TestErrorReply(t *testing.T) {
	const message = "remote control requires starting Alacritty with --remote-control NAME"
	reply := `{"Error":"` + message + `"}`
	c, _ := fakeServer(t, reply, reply)

	err := c.SendText(t.Context(), 0, "ls\r")
	if e, ok := errors.AsType[*Error](err); !ok || e.Message != message {
		t.Errorf("SendText: err = %v, want *Error %q", err, message)
	}
	if _, err := c.ReadScreen(t.Context(), 0); err == nil {
		t.Error("ReadScreen: err = nil, want *Error")
	}
}

func TestDo(t *testing.T) {
	type customArgs struct {
		Window Window `json:"window_id"`
		Lines  int    `json:"lines"`
	}
	c, got := fakeServer(t,
		`{"Custom":{"rows":["a","b"]}}`,
		"",
		`{"Error":"unknown command"}`,
		`{"A":1,"B":2}`,
		`not json`,
	)

	reply, err := c.Do(t.Context(), "Custom", customArgs{Window: 3, Lines: 2})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"Custom":{"window_id":3,"lines":2}}`; <-got != want {
		t.Errorf("request does not match %s", want)
	}
	var value struct {
		Rows []string `json:"rows"`
	}
	if err := reply.Decode(&value); err != nil || reply.Kind != "Custom" || !slices.Equal(value.Rows, []string{"a", "b"}) {
		t.Errorf("reply %s %s decoded to %v, %v", reply.Kind, reply.Value, value, err)
	}

	reply, err = c.Do(t.Context(), "NoReply", map[string]any{"window_id": nil})
	if want := `{"NoReply":{"window_id":null}}`; <-got != want {
		t.Errorf("request does not match %s", want)
	}
	if err != nil || reply.Kind != "" {
		t.Errorf("Do without reply = %+v, %v", reply, err)
	}
	if err := reply.Decode(new(string)); !errors.Is(err, ErrNoReply) {
		t.Errorf("Decode without reply: err = %v, want ErrNoReply", err)
	}

	if _, err := c.Do(t.Context(), "Bad", nil); err == nil {
		t.Error("Do accepted an error reply")
	} else if e, ok := errors.AsType[*Error](err); !ok || e.Message != "unknown command" {
		t.Errorf("err = %v, want *Error", err)
	}
	if want := `{"Bad":null}`; <-got != want {
		t.Errorf("request does not match %s", want)
	}

	for _, invalid := range []string{"two members", "not JSON"} {
		if _, err := c.Do(t.Context(), "X", nil); err == nil {
			t.Errorf("Do accepted a reply with %s", invalid)
		}
		<-got
	}
}

func TestUnexpectedReplyKind(t *testing.T) {
	c, _ := fakeServer(t, `{"GetConfig":"{}"}`)
	if _, err := c.ReadScreen(t.Context(), 0); err == nil {
		t.Error("ReadScreen accepted a GetConfig reply")
	}
}
