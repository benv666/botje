package core

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"go-botje/internal/auth"
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

func TestCoreAdminPort(t *testing.T) {
	client, server := net.Pipe()
	adminLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(storage.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	a.AddUser("benv", "geheim")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			Network: "test", Nick: "Meretrix", Channels: []string{"#testing"},
			Store: storage.NewMemory(), Auth: a, AdminListener: adminLn,
			Dial:      func() (net.Conn, error) { return client, nil },
			JoinDelay: 10 * time.Millisecond,
		})
	}()
	t.Cleanup(func() {
		cancel()
		server.Close()
		<-done
	})

	tc, err := net.Dial("tcp", adminLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	tc.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(tc)
	expectTelnet := func(marker string) string {
		t.Helper()
		var buf strings.Builder
		b := make([]byte, 1)
		for !strings.Contains(buf.String(), marker) {
			if _, err := r.Read(b); err != nil {
				t.Fatalf("waiting for %q, got %q: %v", marker, buf.String(), err)
			}
			buf.WriteByte(b[0])
		}
		return buf.String()
	}
	expectTelnet("login: ")
	tc.Write([]byte("benv\r\n"))
	expectTelnet("password: ")
	tc.Write([]byte("geheim\r\n"))
	expectTelnet("Welcome to botje!")
	tc.Write([]byte("status\r\n"))
	out := expectTelnet("Modules with hooks:")
	if !strings.Contains(out, "test") {
		t.Fatalf("status = %q", out)
	}
}

func TestCoreGracefulQuit(t *testing.T) {
	h := newHarness(t)
	h.expect("JOIN #testing")
	h.cancel()
	h.expect("QUIT :")
}

// newHarnessStore is newHarness with a caller-owned store, for
// persistence tests that span two runs.
func newHarnessStore(t *testing.T, store storage.Store, mods ...module.Module) *harness {
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
			Store:     store,
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

// first boot seeds the channel set from the flags; a stored set from a
// previous run wins over the flags afterwards.
func TestCoreChannelsPersistAcrossRuns(t *testing.T) {
	store := storage.NewMemory()
	var chans []string
	if _, err := store.Get("core", "channels", &chans); err != nil {
		t.Fatal(err)
	}

	h := newHarnessStore(t, store)
	h.expect("JOIN #testing")

	if ok, _ := store.Get("core", "channels", &chans); !ok || len(chans) != 1 || chans[0] != "#testing" {
		t.Fatalf("stored channels after first run = %v", chans)
	}

	// simulate a telnet-era change: stored set differs from the flags
	if err := store.Put("core", "channels", []string{"#testing", "#other"}); err != nil {
		t.Fatal(err)
	}
	h2 := newHarnessStore(t, store)
	h2.expect("JOIN #testing")
	h2.expect("JOIN #other")
}

// an invite joins the channel and persists it for the next boot.
func TestCoreInviteAutoJoin(t *testing.T) {
	store := storage.NewMemory()
	h := newHarnessStore(t, store)
	h.expect("JOIN #testing")
	h.send(":BenV!benv@host INVITE Meretrix :#newhome")
	h.expect("JOIN #newhome")

	var chans []string
	deadline := time.Now().Add(5 * time.Second)
	for {
		chans = nil
		store.Get("core", "channels", &chans)
		if len(chans) == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(chans) != 2 || chans[1] != "#newhome" {
		t.Fatalf("stored channels after invite = %v", chans)
	}
}

// conf values set at runtime survive a restart through storage.
func TestCoreConfPersistAcrossRuns(t *testing.T) {
	store := storage.NewMemory()
	if err := store.Put("core", "conf", map[string]string{"anti_flood_max_lines": "7"}); err != nil {
		t.Fatal(err)
	}
	a, err := auth.New(storage.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	a.AddUser("benv", "geheim")
	hash, _ := bcrypt.GenerateFromPassword([]byte("geheim"), bcrypt.MinCost)
	a.SetSuperuser("benv", string(hash))

	client, server := net.Pipe()
	adminLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			Network: "test", Nick: "Meretrix", Channels: []string{"#testing"},
			Store: store, Auth: a, AdminListener: adminLn,
			Dial:      func() (net.Conn, error) { return client, nil },
			JoinDelay: 10 * time.Millisecond,
		})
	}()
	t.Cleanup(func() {
		cancel()
		server.Close()
		<-done
	})

	tc, err := net.Dial("tcp", adminLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	tc.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(tc)
	expectTelnet := func(marker string) string {
		t.Helper()
		var buf strings.Builder
		b := make([]byte, 1)
		for !strings.Contains(buf.String(), marker) {
			if _, err := r.Read(b); err != nil {
				t.Fatalf("waiting for %q, got %q: %v", marker, buf.String(), err)
			}
			buf.WriteByte(b[0])
		}
		return buf.String()
	}
	expectTelnet("login: ")
	tc.Write([]byte("benv\r\n"))
	expectTelnet("password: ")
	tc.Write([]byte("geheim\r\n"))
	expectTelnet("Welcome to botje!")

	// the stored value survived the restart-equivalent (fresh core, old store)
	tc.Write([]byte("conf anti_flood_max_lines\r\n"))
	expectTelnet("= 7")

	// a new Set lands in storage
	tc.Write([]byte("conf anti_flood_max_lines=2\r\n"))
	expectTelnet("anti_flood_max_lines = 2")
	var stored map[string]string
	deadline := time.Now().Add(5 * time.Second)
	for {
		stored = nil
		store.Get("core", "conf", &stored)
		if stored["anti_flood_max_lines"] == "2" || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if stored["anti_flood_max_lines"] != "2" {
		t.Fatalf("stored conf = %v", stored)
	}
}

// telnet join/part manage the channel set at runtime.
func TestCoreAdminJoinPart(t *testing.T) {
	store := storage.NewMemory()
	a, err := auth.New(storage.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	a.AddUser("benv", "geheim")
	hash, _ := bcrypt.GenerateFromPassword([]byte("geheim"), bcrypt.MinCost)
	a.SetSuperuser("benv", string(hash))

	client, server := net.Pipe()
	adminLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			Network: "test", Nick: "Meretrix", Channels: []string{"#testing"},
			Store: store, Auth: a, AdminListener: adminLn,
			Dial:      func() (net.Conn, error) { return client, nil },
			JoinDelay: 10 * time.Millisecond,
		})
	}()
	t.Cleanup(func() {
		cancel()
		server.Close()
		<-done
	})

	wire := bufio.NewReader(server)
	expectWire := func(want string) string {
		t.Helper()
		server.SetReadDeadline(time.Now().Add(10 * time.Second))
		for {
			line, err := wire.ReadString('\n')
			if err != nil {
				t.Fatalf("waiting for %q: %v", want, err)
			}
			line = strings.TrimRight(line, "\r\n")
			if strings.Contains(line, want) {
				return line
			}
		}
	}
	expectWire("JOIN #testing")

	tc, err := net.Dial("tcp", adminLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	tc.SetDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(tc)
	expectTelnet := func(marker string) string {
		t.Helper()
		var buf strings.Builder
		b := make([]byte, 1)
		for !strings.Contains(buf.String(), marker) {
			if _, err := r.Read(b); err != nil {
				t.Fatalf("waiting for %q, got %q: %v", marker, buf.String(), err)
			}
			buf.WriteByte(b[0])
		}
		return buf.String()
	}
	expectTelnet("login: ")
	tc.Write([]byte("benv\r\n"))
	expectTelnet("password: ")
	tc.Write([]byte("geheim\r\n"))
	expectTelnet("Welcome to botje!")

	tc.Write([]byte("join #extra\r\n"))
	expectTelnet("#extra")
	expectWire("JOIN #extra")

	tc.Write([]byte("part #extra\r\n"))
	expectTelnet("#extra")
	expectWire("PART #extra")

	var chans []string
	store.Get("core", "channels", &chans)
	if len(chans) != 1 || chans[0] != "#testing" {
		t.Fatalf("stored channels after join+part = %v", chans)
	}

	// part of an unknown channel errors
	tc.Write([]byte("part #nope\r\n"))
	expectTelnet("Error")
}
