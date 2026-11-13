// Package critty is a client for the IPC socket of critty, a fork of the
// Alacritty terminal emulator that adds remote control, and of Alacritty
// itself. It speaks the protocol of "critty msg" and "alacritty msg".
//
// A [Client] addresses one process, whose ID [Client.PID] returns on Linux.
// [RemoteControlSocket] gives the socket of a critty process started with
// --remote-control, [RemoteControlNames] lists the running ones, and
// [FindSocket] locates a socket the way "alacritty msg" does without one.
// Only critty processes started with --remote-control accept
// [Client.ReadScreen] and [Client.SendText]:
//
//	c := critty.Client{Socket: critty.RemoteControlSocket("build")}
//	err := c.SendText(ctx, 0, "make test\r")
//	screen, err := c.ReadScreen(ctx, 0)
//
// [Client.Do] sends any command, including ones this package does not wrap:
//
//	reply, err := c.Do(ctx, "ReadScreen", map[string]any{"window_id": nil})
//	err = reply.Decode(&screen)
package critty

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// ErrNoReply is returned by [Reply.Decode] when Alacritty closed the
// connection without a reply, as it does for a command that has none.
var ErrNoReply = errors.New("alacritty ipc: no reply")

// Error is a command failure reported by Alacritty, such as a remote control
// command sent to a process started without --remote-control.
type Error struct {
	Message string
}

func (e *Error) Error() string { return "alacritty: " + e.Message }

// Window identifies an Alacritty window; its value is the window's
// ALACRITTY_WINDOW_ID. The zero Window identifies no window, and each
// command documents what it targets instead. A Window encodes as a JSON
// number, or as null if zero, so command arguments for [Client.Do] can use it
// for a window_id field.
type Window uint64

// MarshalJSONTo implements [encoding/json/v2.MarshalerTo].
func (w Window) MarshalJSONTo(enc *jsontext.Encoder) error {
	if w == 0 {
		return enc.WriteToken(jsontext.Null)
	}
	return enc.WriteToken(jsontext.Uint(uint64(w)))
}

// Client sends commands to the IPC socket of one Alacritty process.
// Each command uses its own connection, so a Client is safe for concurrent
// use by multiple goroutines.
type Client struct {
	// Socket is the path of the IPC socket.
	Socket string
}

// Reply is Alacritty's answer to a command, a value tagged with its kind.
type Reply struct {
	// Kind names the reply, such as "ReadScreen". It is empty if Alacritty
	// closed the connection without a reply.
	Kind string

	// Value is the reply's JSON value.
	Value jsontext.Value
}

// Decode stores the reply's value in the value pointed to by v, following
// the rules of [encoding/json/v2.Unmarshal], which matches object
// names to struct fields case-sensitively. If there is no reply, Decode
// returns [ErrNoReply].
func (r *Reply) Decode(v any) error {
	if r.Kind == "" {
		return ErrNoReply
	}
	return json.Unmarshal(r.Value, v)
}

// Do sends the command name to Alacritty and returns its reply. Alacritty
// receives the JSON object {name: args}: name is a variant of its
// SocketMessage enum, such as "ReadScreen", and args, encoded as by
// [encoding/json/v2.Marshal], are that variant's fields. The other methods
// of Client are wrappers around Do for the commands Alacritty currently
// knows.
//
// A command without a reply returns a [Reply] with an empty Kind. If
// Alacritty replies with an error, as it does for an unknown command or
// invalid args, Do returns it as an [*Error]. If ctx is done before the reply
// arrives, Do returns [context.Cause] of ctx.
func (c *Client) Do(ctx context.Context, name string, args any) (*Reply, error) {
	msg, err := json.Marshal(map[string]any{name: args})
	if err != nil {
		return nil, err
	}

	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Now()) })
	defer stop()

	// Alacritty reads the command until the end of the stream, then replies
	// on the same connection and closes it.
	data, err := func() ([]byte, error) {
		if _, err := conn.Write(msg); err != nil {
			return nil, err
		}
		if err := conn.(*net.UnixConn).CloseWrite(); err != nil {
			return nil, err
		}
		return io.ReadAll(conn)
	}()
	if err != nil {
		if ctx.Err() != nil {
			return nil, context.Cause(ctx)
		}
		return nil, err
	}
	if len(data) == 0 {
		return &Reply{}, nil
	}

	// A reply is an externally tagged enum: an object with a single member.
	var tagged map[string]jsontext.Value
	if err := json.Unmarshal(data, &tagged); err != nil || len(tagged) != 1 {
		return nil, fmt.Errorf("alacritty ipc: invalid reply %s", data)
	}
	for kind, value := range tagged {
		if kind == "Error" {
			var message string
			if json.Unmarshal(value, &message) != nil {
				message = string(value)
			}
			return nil, &Error{message}
		}
		return &Reply{Kind: kind, Value: value}, nil
	}
	panic("unreachable")
}

// ReadScreen returns the text visible in window w, a line per screen row:
// soft-wrapped rows are joined and trailing whitespace is dropped. The zero
// Window reads the window with the lowest ID.
//
// Like [Client.SendText], ReadScreen requires a process started with
// --remote-control and returns an [*Error] otherwise, or if w does not
// exist.
func (c *Client) ReadScreen(ctx context.Context, w Window) (string, error) {
	return c.readScreen(ctx, readScreenArgs{Window: w})
}

// ReadScreenANSI is like [Client.ReadScreen] but includes colors and text
// attributes as ANSI SGR escape sequences, reset at the end of each line.
// Colors keep their form (palette entry, 256-color index or RGB), tabs become
// spaces, and trailing blanks with a background color are kept.
func (c *Client) ReadScreenANSI(ctx context.Context, w Window) (string, error) {
	return c.readScreen(ctx, readScreenArgs{Window: w, ANSI: true})
}

func (c *Client) readScreen(ctx context.Context, args readScreenArgs) (string, error) {
	var screen string
	err := c.call(ctx, "ReadScreen", args, &screen)
	return screen, err
}

// SendText writes text to the terminal input of window w, as if it was
// typed: "\r" is Enter and "\x03" is Ctrl-C. The zero Window writes to the
// window with the lowest ID. SendText requires a process started with
// --remote-control and returns an [*Error] otherwise, or if w does not exist.
//
// SendText returns once Alacritty has queued the text. The program in the
// terminal reacts asynchronously, so a following [Client.ReadScreen] may
// not show its output yet.
func (c *Client) SendText(ctx context.Context, w Window, text string) error {
	_, err := c.Do(ctx, "SendText", sendTextArgs{Text: text, Window: w})
	return err
}

// GetConfig returns the runtime configuration of window w as JSON. The zero
// Window, or an unknown one, returns the configuration used for new windows.
func (c *Client) GetConfig(ctx context.Context, w Window) ([]byte, error) {
	var config string
	err := c.call(ctx, "GetConfig", windowArgs{w}, &config)
	return []byte(config), err
}

// SetConfig overrides configuration options of window w at runtime. Each
// option is a TOML assignment such as "font.size=12". The zero Window
// changes every window and the configuration used for new windows.
func (c *Client) SetConfig(ctx context.Context, w Window, options ...string) error {
	_, err := c.Do(ctx, "Config", configArgs{Options: options, Window: w})
	return err
}

// ResetConfig discards the runtime configuration changes made by
// [Client.SetConfig] to window w. The zero Window resets every window and
// the configuration used for new windows.
func (c *Client) ResetConfig(ctx context.Context, w Window) error {
	_, err := c.Do(ctx, "Config", configArgs{Window: w, Reset: true})
	return err
}

// CreateWindow opens a new window in the Alacritty process.
func (c *Client) CreateWindow(ctx context.Context, opts WindowOptions) error {
	_, err := c.Do(ctx, "CreateWindow", opts.args())
	return err
}

// WindowOptions configures a window opened by [Client.CreateWindow]. The
// zero value opens a window with the configured defaults.
type WindowOptions struct {
	// Command is the program and its arguments to run in the window.
	// If empty, the configured shell is used.
	Command []string

	// WorkingDirectory is the directory the command starts in.
	// If empty, the configured working directory is used.
	WorkingDirectory string

	// Hold, if true, keeps the window open after the command exits.
	Hold bool

	// Title is the window title. If empty, the configured title is used.
	Title string

	// ClassGeneral and ClassInstance are the X11 WM_CLASS of the window; on
	// Wayland, ClassGeneral is the app_id. Each that is empty keeps its
	// configured value.
	ClassGeneral, ClassInstance string

	// Options overrides configuration options for the window. Each option is
	// a TOML assignment such as "font.size=12".
	Options []string

	// ActivationToken optionally specifies the XDG activation token that
	// lets the new window take focus on Wayland, usually the value of
	// XDG_ACTIVATION_TOKEN.
	ActivationToken string
}

// RemoteControlSocket returns the IPC socket path of the Alacritty process
// started with --remote-control name.
func RemoteControlSocket(name string) string {
	return filepath.Join(socketDir(), socketPrefix()+remoteControlMarker+strings.ReplaceAll(name, "/", "-")+".sock")
}

// RemoteControlNames returns the names of the Alacritty processes on the
// current display that were started with --remote-control and accept
// connections, in sorted order. A "/" in a name is returned as "-".
func RemoteControlNames() ([]string, error) {
	dir := socketDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	prefix := socketPrefix() + remoteControlMarker
	var names []string
	for _, entry := range entries {
		name, ok := strings.CutPrefix(entry.Name(), prefix)
		if !ok {
			continue
		}
		if name, ok = strings.CutSuffix(name, ".sock"); ok && reachable(filepath.Join(dir, entry.Name())) {
			names = append(names, name)
		}
	}
	return names, nil
}

// FindSocket returns the path of an IPC socket that accepts connections: the
// one in ALACRITTY_SOCKET, which Alacritty sets for the programs it runs,
// or else the first one found for the current display.
func FindSocket() (string, error) {
	if path := os.Getenv("ALACRITTY_SOCKET"); path != "" && reachable(path) {
		return path, nil
	}

	dir := socketDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	prefix := socketPrefix()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".sock") {
			continue
		}
		if path := filepath.Join(dir, name); reachable(path) {
			return path, nil
		}
	}
	return "", fmt.Errorf("alacritty ipc: no socket found in %s", dir)
}

// call sends the command name and decodes its reply, which must be of the
// same kind, into v.
func (c *Client) call(ctx context.Context, name string, args, v any) error {
	r, err := c.Do(ctx, name, args)
	if err != nil {
		return err
	}
	if r.Kind != "" && r.Kind != name {
		return fmt.Errorf("alacritty ipc: %s reply to %s", r.Kind, name)
	}
	return r.Decode(v)
}

// Arguments of the commands Alacritty knows, mirroring the fields of the
// SocketMessage variants in critty's alacritty/src/cli.rs. Fields that are
// not optional in Rust are always sent; nil slices encode as [].

type windowArgs struct {
	Window Window `json:"window_id"`
}

type readScreenArgs struct {
	Window Window `json:"window_id"`
	ANSI   bool   `json:"ansi,omitzero"`
}

type sendTextArgs struct {
	Text   string `json:"text"`
	Window Window `json:"window_id"`
}

type configArgs struct {
	Options []string `json:"options"`
	Window  Window   `json:"window_id"`
	Reset   bool     `json:"reset"`
}

type createWindowArgs struct {
	TerminalOptions struct {
		WorkingDirectory string   `json:"working_directory,omitempty"`
		Hold             bool     `json:"hold"`
		Command          []string `json:"command"`
	} `json:"terminal_options"`
	WindowIdentity struct {
		Title string     `json:"title,omitempty"`
		Class *classArgs `json:"class,omitzero"`
	} `json:"window_identity"`
	ActivationToken string   `json:"activation_token,omitempty"`
	Option          []string `json:"option"`
}

type classArgs struct {
	General  string `json:"general,omitempty"`
	Instance string `json:"instance,omitempty"`
}

func (o WindowOptions) args() createWindowArgs {
	var a createWindowArgs
	a.TerminalOptions.WorkingDirectory = o.WorkingDirectory
	a.TerminalOptions.Hold = o.Hold
	a.TerminalOptions.Command = o.Command
	a.WindowIdentity.Title = o.Title
	if o.ClassGeneral != "" || o.ClassInstance != "" {
		a.WindowIdentity.Class = &classArgs{o.ClassGeneral, o.ClassInstance}
	}
	a.ActivationToken = o.ActivationToken
	a.Option = o.Options
	return a
}

// reachable reports whether a process accepts connections on the socket at path.
func reachable(path string) bool {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// remoteControlMarker separates the socket prefix from the remote control
// name, keeping these sockets apart from the ones named after a process ID;
// it mirrors remote_control_socket_path in critty's
// alacritty/src/polling/ipc.rs.
const remoteControlMarker = "-remote-control-"

// socketDir mirrors socket_dir in critty's alacritty/src/polling/ipc.rs.
func socketDir() string {
	if runtime.GOOS != "darwin" {
		if dir := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(dir) {
			return dir
		}
	}
	return os.TempDir()
}

// socketPrefix mirrors socket_prefix in critty's alacritty/src/polling/ipc.rs.
func socketPrefix() string {
	if runtime.GOOS == "darwin" {
		return "Alacritty"
	}
	display, ok := os.LookupEnv("WAYLAND_DISPLAY")
	if !ok {
		display = os.Getenv("DISPLAY")
	}
	return "Alacritty-" + strings.ReplaceAll(display, "/", "-")
}
