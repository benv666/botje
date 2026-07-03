package core

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
	"go-botje/internal/storage"
)

// echoModule registers a !ping command that replies through the pager.
type echoModule struct{ ctx *module.Context }

func (m *echoModule) Name() string { return "echo" }
func (m *echoModule) Load(ctx *module.Context) error {
	m.ctx = ctx
	ctx.Cmd.Register("echo", "ping", func(d *cmd.Data) bool {
		ctx.Pager.EventMsg(d.Event, "ping", "pong "+d.Data)
		return true
	})
	return nil
}
func (m *echoModule) Unload() error { return nil }

// harness runs a Core against an in-memory pipe and scripts the server
// side line by line.
type harness struct {
	t      *testing.T
	server net.Conn
	r      *bufio.Reader
	cancel context.CancelFunc
	done   chan error
}

func newHarness(t *testing.T, mods ...module.Module) *harness {
	t.Helper()
	client, server := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{
		t: t, server: server, r: bufio.NewReader(server),
		cancel: cancel, done: make(chan error, 1),
	}
	go func() {
		h.done <- Run(ctx, Config{
			Network:   "test",
			Nick:      "Meretrix",
			Channels:  []string{"#testing"},
			Store:     storage.NewMemory(),
			Modules:   mods,
			Dial:      func() (net.Conn, error) { return client, nil },
			JoinDelay: 10 * time.Millisecond,
		})
	}()
	t.Cleanup(func() {
		cancel()
		server.Close()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			t.Error("core did not stop")
		}
	})
	return h
}

// expect reads wire lines until one contains want (or fails).
func (h *harness) expect(want string) string {
	h.t.Helper()
	h.server.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		line, err := h.r.ReadString('\n')
		if err != nil {
			h.t.Fatalf("waiting for %q: %v", want, err)
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.Contains(line, want) {
			return line
		}
	}
}

func (h *harness) send(line string) {
	h.t.Helper()
	if _, err := h.server.Write([]byte(line + "\r\n")); err != nil {
		h.t.Fatal(err)
	}
}

func TestCoreRegistersAndJoins(t *testing.T) {
	h := newHarness(t)
	h.expect("NICK Meretrix")
	h.expect("USER Botje")
	h.expect("JOIN #testing")
}

func TestCorePingPong(t *testing.T) {
	h := newHarness(t)
	h.expect("NICK Meretrix")
	h.send("PING :tok123")
	h.expect("PONG tok123")
}

func TestCoreCommandThroughModule(t *testing.T) {
	h := newHarness(t, &echoModule{})
	h.expect("JOIN #testing")
	h.send(":Meretrix!b@h JOIN #testing")
	h.send(":BenV!benv@host PRIVMSG #testing :!ping hello")
	if got := h.expect("PRIVMSG #testing :"); got != "PRIVMSG #testing :pong hello" {
		t.Fatalf("reply = %q", got)
	}
}

func TestCoreMoreWithNothing(t *testing.T) {
	h := newHarness(t)
	h.expect("JOIN #testing")
	h.send(":BenV!benv@host PRIVMSG #testing :!more")
	h.expect("PRIVMSG #testing :There is nothing more to display for you.")
}

func TestCoreSuggestion(t *testing.T) {
	h := newHarness(t, &echoModule{})
	h.expect("JOIN #testing")
	h.send(":BenV!benv@host PRIVMSG #testing :!pign")
	got := h.expect("Maybe you meant")
	if !strings.Contains(got, "PRIVMSG #testing :BenV: Maybe you meant") ||
		!strings.Contains(got, "ping") {
		t.Fatalf("suggestion = %q", got)
	}
}

func TestCoreGracefulQuit(t *testing.T) {
	h := newHarness(t)
	h.expect("JOIN #testing")
	h.cancel()
	h.expect("QUIT :")
}
