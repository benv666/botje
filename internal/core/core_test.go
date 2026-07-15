package core

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"go-botje/internal/auth"
	"go-botje/internal/bus"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/metrics"
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

// newHarnessCfg is the one place that boots a core for a test: an
// in-memory pipe as the IRC wire and a default Config the caller can
// mutate. It does not confirm registration; use newHarness unless the
// test is about pre-welcome behavior.
func newHarnessCfg(t *testing.T, mutate func(*Config)) *harness {
	t.Helper()
	client, server := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	h := &harness{
		t: t, server: server, r: bufio.NewReader(server),
		cancel: cancel, done: make(chan error, 1),
	}
	cfg := Config{
		Network:  "test",
		Nick:     "Meretrix",
		Channels: []string{"#testing"},
		Store:    storage.NewMemory(),
		Dial:     func() (net.Conn, error) { return client, nil },
	}
	if mutate != nil {
		mutate(&cfg)
	}
	go func() { h.done <- Run(ctx, cfg) }()
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

func newHarnessRaw(t *testing.T, mods ...module.Module) *harness {
	return newHarnessCfg(t, func(cfg *Config) { cfg.Modules = mods })
}

func newHarness(t *testing.T, mods ...module.Module) *harness {
	t.Helper()
	h := newHarnessRaw(t, mods...)
	h.welcome()
	return h
}

// welcome confirms registration; the core joins channels on this.
func (h *harness) welcome() {
	h.send(":srv 001 Meretrix :Welcome to test")
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
	h.send(":Meretrix!b@h JOIN #testing")
	h.send(":BenV!benv@host PRIVMSG #testing :!more")
	h.expect("PRIVMSG #testing :There is nothing more to display for you.")
}

func TestCoreSuggestion(t *testing.T) {
	h := newHarness(t, &echoModule{})
	h.expect("JOIN #testing")
	h.send(":Meretrix!b@h JOIN #testing")
	h.send(":BenV!benv@host PRIVMSG #testing :!pign")
	got := h.expect("Maybe you meant")
	if !strings.Contains(got, "PRIVMSG #testing :BenV: Maybe you meant") ||
		!strings.Contains(got, "ping") {
		t.Fatalf("suggestion = %q", got)
	}
}

// adminHarness is a harness with a telnet client logged in on the
// admin port as benv/geheim (a superuser when su is set).
type adminHarness struct {
	*harness
	tc net.Conn
	tr *bufio.Reader
}

func newAdminHarness(t *testing.T, store storage.Store, su bool, mods ...module.Module) *adminHarness {
	t.Helper()
	a, err := auth.New(storage.NewMemory())
	if err != nil {
		t.Fatal(err)
	}
	a.AddUser("benv", "geheim")
	if su {
		hash, _ := bcrypt.GenerateFromPassword([]byte("geheim"), bcrypt.MinCost)
		a.SetSuperuser("benv", string(hash))
	}
	adminLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessCfg(t, func(cfg *Config) {
		cfg.Store = store
		cfg.Modules = mods
		cfg.Auth = a
		cfg.AdminListener = adminLn
	})

	tc, err := net.Dial("tcp", adminLn.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tc.Close() })
	tc.SetDeadline(time.Now().Add(10 * time.Second))
	ah := &adminHarness{harness: h, tc: tc, tr: bufio.NewReader(tc)}
	ah.expectTelnet("login: ")
	ah.telnet("benv")
	ah.expectTelnet("password: ")
	ah.telnet("geheim")
	ah.expectTelnet("Welcome to botje!")
	return ah
}

func (h *adminHarness) telnet(line string) {
	h.t.Helper()
	if _, err := h.tc.Write([]byte(line + "\r\n")); err != nil {
		h.t.Fatal(err)
	}
}

// expectTelnet reads the telnet stream byte by byte (the prompts have
// no trailing newline) until marker appears.
func (h *adminHarness) expectTelnet(marker string) string {
	h.t.Helper()
	var buf strings.Builder
	b := make([]byte, 1)
	for !strings.Contains(buf.String(), marker) {
		if _, err := h.tr.Read(b); err != nil {
			h.t.Fatalf("waiting for %q, got %q: %v", marker, buf.String(), err)
		}
		buf.WriteByte(b[0])
	}
	return buf.String()
}

func TestCoreAdminPort(t *testing.T) {
	h := newAdminHarness(t, storage.NewMemory(), false)
	h.telnet("status")
	out := h.expectTelnet("Modules with hooks:")
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
	return newHarnessStoreInterval(t, store, 20*time.Millisecond, mods...)
}

func newHarnessStoreInterval(t *testing.T, store storage.Store, saveInterval time.Duration, mods ...module.Module) *harness {
	t.Helper()
	h := newHarnessCfg(t, func(cfg *Config) {
		cfg.Store = store
		cfg.Modules = mods
		cfg.SaveInterval = saveInterval
	})
	h.welcome()
	return h
}

// saverModule marks a value through the shared Saver at load time.
type saverModule struct{}

func (saverModule) Name() string { return "saverm" }
func (saverModule) Load(ctx *module.Context) error {
	ctx.Saver.Mark("saverm", "k", func() any { return 42 })
	return nil
}
func (saverModule) Unload() error { return nil }

// the core flushes the saver on its cadence...
func TestCoreSaverFlushCadence(t *testing.T) {
	store := storage.NewMemory()
	newHarnessStore(t, store, saverModule{})
	deadline := time.Now().Add(5 * time.Second)
	for {
		var v int
		if ok, _ := store.Get("saverm", "k", &v); ok && v == 42 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("saver mark never flushed on the cadence")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ...and once more, synchronously, at shutdown (the default cadence is
// a minute, so only the shutdown flush can have written this).
func TestCoreSaverFlushOnShutdown(t *testing.T) {
	store := storage.NewMemory()
	h := newHarnessStoreInterval(t, store, time.Minute, saverModule{})
	h.cancel()
	select {
	case err := <-h.done:
		h.done <- err // the harness cleanup waits on this too
	case <-time.After(5 * time.Second):
		t.Fatal("core did not stop")
	}
	var v int
	if ok, _ := store.Get("saverm", "k", &v); !ok || v != 42 {
		t.Fatalf("saverm/k = %v %d after shutdown", ok, v)
	}
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
	h := newAdminHarness(t, store, true)

	// the stored value survived the restart-equivalent (fresh core, old store)
	h.telnet("conf anti_flood_max_lines")
	h.expectTelnet("= 7")

	// a new Set lands in storage
	h.telnet("conf anti_flood_max_lines=2")
	h.expectTelnet("anti_flood_max_lines = 2")
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
	h := newAdminHarness(t, store, true)
	h.welcome()
	h.expect("JOIN #testing")

	h.telnet("join #extra")
	h.expectTelnet("#extra")
	h.expect("JOIN #extra")

	h.telnet("part #extra")
	h.expectTelnet("#extra")
	h.expect("PART #extra")

	var chans []string
	store.Get("core", "channels", &chans)
	if len(chans) != 1 || chans[0] != "#testing" {
		t.Fatalf("stored channels after join+part = %v", chans)
	}

	// part of an unknown channel errors
	h.telnet("part #nope")
	h.expectTelnet("Error")
}

// sentCapture records IRC_SENT events (what the logger module will hook).
type sentCapture struct {
	mu   sync.Mutex
	seen []bus.Event
}

func (m *sentCapture) Name() string { return "sentcap" }
func (m *sentCapture) Load(ctx *module.Context) error {
	return ctx.Bus.RegisterHook("sentcap", "IRC_SENT", func(ev *bus.Event) (bus.Handled, any) {
		m.mu.Lock()
		m.seen = append(m.seen, *ev)
		m.mu.Unlock()
		return bus.None, nil
	})
}
func (m *sentCapture) Unload() error { return nil }

// every outbound privmsg emits IRC_SENT so the logger can record the
// bot's own lines; multi-line replies emit one event per line.
func TestCoreEmitsIRCSent(t *testing.T) {
	cap := &sentCapture{}
	h := newHarness(t, &echoModule{}, cap)
	h.expect("JOIN #testing")
	h.send(":Meretrix!b@h JOIN #testing")
	h.send(":BenV!benv@host PRIVMSG #testing :!ping hi\nthere")
	h.expect("PRIVMSG #testing :")

	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.seen) == 0 {
		t.Fatal("no IRC_SENT events")
	}
	ev := cap.seen[0]
	if ev.Channel != "#testing" || !ev.SenderMe || ev.Sender.Nick != "Meretrix" ||
		!strings.HasPrefix(ev.Msg, "pong hi") {
		t.Fatalf("IRC_SENT = %+v", ev)
	}
}

// rawAndMembership exercises the SendRaw + InChannel + Members module
// API.
type rawProbe struct {
	ctx     *module.Context
	inTest  bool
	inOther bool
	members []string
}

func (m *rawProbe) Name() string { return "rawprobe" }
func (m *rawProbe) Load(ctx *module.Context) error {
	m.ctx = ctx
	return ctx.Bus.RegisterHook("rawprobe", "IRC_PRIVMSG", func(ev *bus.Event) (bus.Handled, any) {
		if ev.Msg == "!probe" {
			m.inTest = ctx.InChannel("#testing")
			m.inOther = ctx.InChannel("#nope")
			m.members = ctx.Members("#testing")
			ctx.SendRaw("GLINE *@evil.example 1h :spam")
		}
		return bus.None, nil
	})
}
func (m *rawProbe) Unload() error { return nil }

func TestCoreSendRawAndInChannel(t *testing.T) {
	p := &rawProbe{}
	h := newHarness(t, p)
	h.expect("JOIN #testing")
	h.send(":Meretrix!b@h JOIN #testing")
	h.send(":srv 353 Meretrix = #testing :BenV @Verty")
	h.send(":srv 366 Meretrix #testing :End of /NAMES list.")
	h.send(":BenV!benv@host PRIVMSG #testing :!probe")
	got := h.expect("GLINE")
	if got != "GLINE *@evil.example 1h :spam" {
		t.Fatalf("raw line = %q", got)
	}
	if !p.inTest {
		t.Error("InChannel(#testing) = false, want true")
	}
	if p.inOther {
		t.Error("InChannel(#nope) = true, want false")
	}
	if want := []string{"BenV", "Verty"}; !slices.Equal(p.members, want) {
		t.Errorf("Members(#testing) = %q, want %q", p.members, want)
	}
}

// channel messages to a channel the bot is not in are dropped (not
// queued into the flood budget on doomed ERR_CANNOTSENDTOCHAN);
// queries to nicks and messages to joined channels still go out.
func TestCoreDropsMessagesToUnjoinedChannels(t *testing.T) {
	h := newHarness(t, &echoModule{})
	h.expect("JOIN #testing")
	h.send(":Meretrix!b@h JOIN #testing")

	// reply into a channel we ARE in: goes out
	h.send(":BenV!benv@host PRIVMSG #testing :!ping a")
	if got := h.expect("PRIVMSG #testing :"); got != "PRIVMSG #testing :pong a" {
		t.Fatalf("joined-channel reply = %q", got)
	}
	// a query (reply target is the nick) goes out even though it is not a channel
	h.send(":BenV!benv@host PRIVMSG Meretrix :!ping b")
	if got := h.expect("PRIVMSG BenV :"); got != "PRIVMSG BenV :pong b" {
		t.Fatalf("query reply = %q", got)
	}
}

// broadcaster is an rss-like module: on !cast it fires a message at an
// un-joined channel and then one at a joined channel. Only the second
// must reach the wire.
type broadcaster struct{ ctx *module.Context }

func (m *broadcaster) Name() string { return "broadcaster" }
func (m *broadcaster) Load(ctx *module.Context) error {
	m.ctx = ctx
	return ctx.Bus.RegisterHook("broadcaster", "IRC_PRIVMSG", func(ev *bus.Event) (bus.Handled, any) {
		if ev.Msg == "!cast" {
			ctx.Privmsg("#notjoined", "into the void")
			ctx.Privmsg("#testing", "this one goes")
		}
		return bus.None, nil
	})
}
func (m *broadcaster) Unload() error { return nil }

func TestCorePrivmsgDropUnjoined(t *testing.T) {
	h := newHarness(t, &broadcaster{})
	h.expect("JOIN #testing")
	h.send(":Meretrix!b@h JOIN #testing")
	h.send(":BenV!benv@host PRIVMSG #testing :!cast")
	if got := h.expect("PRIVMSG #"); got != "PRIVMSG #testing :this one goes" {
		t.Fatalf("first channel wire line = %q, want only the joined-channel message", got)
	}
}

// the metrics endpoint reflects live bus activity and connection state.
func TestCoreMetricsEndpoint(t *testing.T) {
	reg := metrics.New()
	metricsLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// the listener is only for a free Addr(); close it so core's own
	// listen on the same addr can bind
	metricsLn.Close()
	h := newHarnessCfg(t, func(cfg *Config) {
		cfg.Modules = []module.Module{&echoModule{}, &broadcaster{}}
		cfg.Metrics = reg
		cfg.MetricsAddr = metricsLn.Addr().String()
	})
	h.welcome()
	// drive a command so a hook records a call
	h.expect("JOIN #testing")
	h.send(":Meretrix!b@h JOIN #testing")
	h.send(":BenV!benv@host PRIVMSG #testing :!ping x")
	h.expect("pong x")

	// scrape
	var body string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://" + metricsLn.Addr().String() + "/metrics")
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		body = string(b)
		if strings.Contains(body, "botje_connected") {
			break
		}
	}
	if !strings.Contains(body, "botje_connected 1") {
		t.Errorf("no connected gauge:\n%s", body)
	}
	if !strings.Contains(body, `botje_hook_calls_total{event="IRC_PRIVMSG",module="broadcaster"}`) {
		t.Errorf("no hook call counter:\n%s", body)
	}
	// runtime memory + dispatcher backlog gauges (backlog item 3a)
	for _, want := range []string{
		"go_goroutines ",
		"go_memstats_heap_alloc_bytes ",
		"go_gc_cycles_total ",
		"botje_work_backlog ",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q:\n%s", want, body)
		}
	}
	// the boot-time channel seeding did storage puts through the
	// instrumented store
	if !strings.Contains(body, `botje_storage_op_seconds_count{ns="core",op="put"}`) {
		t.Errorf("no storage op latency series:\n%s", body)
	}
}

// multi-line output keeps intentional leading whitespace on every line
// (ASCII art like pacman) and drops blank lines. The Perl cmd_privmsg
// stripped continuation-line whitespace, mangling art; fixed to match
// Perl's own better cmd_eventmsg path.
type artModule struct{ ctx *module.Context }

func (m *artModule) Name() string { return "art" }
func (m *artModule) Load(ctx *module.Context) error {
	m.ctx = ctx
	return ctx.Bus.RegisterHook("art", "IRC_PRIVMSG", func(ev *bus.Event) (bus.Handled, any) {
		if ev.Msg == "!art" {
			ctx.Privmsg(ev.Channel, "   /~~\\\n   ( o <\n\n    \\__/")
		}
		return bus.None, nil
	})
}
func (m *artModule) Unload() error { return nil }

func TestCorePreservesArtWhitespace(t *testing.T) {
	h := newHarness(t, &artModule{})
	h.expect("JOIN #testing")
	h.send(":Meretrix!b@h JOIN #testing")
	h.send(":BenV!benv@host PRIVMSG #testing :!art")
	if got := h.expect(`/~~\`); got != `PRIVMSG #testing :   /~~\` {
		t.Fatalf("line 1 = %q", got)
	}
	if got := h.expect("( o <"); got != "PRIVMSG #testing :   ( o <" {
		t.Fatalf("line 2 lost its margin = %q", got)
	}
	if got := h.expect(`\__/`); got != `PRIVMSG #testing :    \__/` {
		t.Fatalf("line 3 (after a blank line) = %q", got)
	}
}

// keeper-resume: the ircd rejects the re-registration with 462 (no 001
// ever comes) and echoes no JOIN (the live session is already in the
// channel). The core must join on the 462 and output to that channel
// must still go out, not get dropped by the un-joined-channel guard.
func TestCoreResumeNoJoinEchoStillSends(t *testing.T) {
	h := newHarnessRaw(t, &broadcaster{})
	h.expect("USER ")
	h.send(":srv 462 Meretrix :You may not reregister")
	h.expect("JOIN #testing") // core sends JOIN...
	// ...but we deliberately send NO ":Meretrix JOIN #testing" echo,
	// mimicking a resume under the keeper
	h.send(":BenV!benv@host PRIVMSG #testing :!cast")
	// broadcaster targets #notjoined then #testing; #testing must arrive
	if got := h.expect("PRIVMSG #"); got != "PRIVMSG #testing :this one goes" {
		t.Fatalf("resume send = %q, want the #testing message through", got)
	}
}

// The 2026-07-13 lost-joins bug: the core JOINed on a timer, so a core
// attached to a keeper whose IRC side was still down queued JOINs
// behind NICK/USER; the whole burst flushed on reconnect and the ircd
// answered the pipelined JOINs with ERR_NOTREGISTERED (registration
// completes asynchronously), silently eating them. JOIN must wait for
// the server to confirm registration.
func TestCoreJoinWaitsForWelcome(t *testing.T) {
	h := newHarnessRaw(t)
	h.expect("NICK Meretrix")
	h.expect("USER ")
	// no welcome yet: nothing further may hit the wire
	h.server.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if line, err := h.r.ReadString('\n'); err == nil {
		t.Fatalf("wire got %q before the welcome", line)
	}
	h.welcome()
	h.expect("JOIN #testing")
}

// querier fires a query (nick target) on any NOTICE, so a test can
// trigger it with the pre-registration "*** Looking up your hostname"
// notice a real ircd sends.
type querier struct{}

func (querier) Name() string { return "querier" }
func (querier) Load(ctx *module.Context) error {
	return ctx.Bus.RegisterHook("querier", "IRC_NOTICE", func(ev *bus.Event) (bus.Handled, any) {
		ctx.Privmsg("BenV", "too early")
		return bus.None, nil
	})
}
func (querier) Unload() error { return nil }

// Queries sent before the welcome are dropped, not queued: the server
// would eat them with ERR_NOTREGISTERED anyway, after they burn flood
// budget and keeper buffer.
func TestCorePrivmsgDroppedBeforeWelcome(t *testing.T) {
	h := newHarnessRaw(t, &querier{})
	h.expect("NICK Meretrix")
	h.expect("USER ")
	h.send(":srv NOTICE * :*** Looking up your hostname...")
	h.server.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if line, err := h.r.ReadString('\n'); err == nil {
		t.Fatalf("wire got %q before the welcome", line)
	}
	h.welcome()
	h.expect("JOIN #testing") // and the dropped query never shows up
}

// messages replayed from the keeper's buffer while the core is still
// registering are held until the welcome, then processed normally.
// Before this they mutated module state while every reply was eaten by
// the pre-welcome outbound guard (BenV's silent "draai", 2026-07-14).
func TestCoreHoldsPreWelcomePrivmsg(t *testing.T) {
	h := newHarnessRaw(t, &echoModule{})
	h.expect("USER ")
	// the keeper flushes buffered chat before registration completes
	h.send(":BenV!benv@host PRIVMSG Meretrix :!ping vroeg")
	h.server.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if line, err := h.r.ReadString('\n'); err == nil {
		t.Fatalf("wire got %q before the welcome", line)
	}
	h.welcome()
	h.expect("JOIN #testing")
	if got := h.expect("PRIVMSG BenV :"); got != "PRIVMSG BenV :pong vroeg" {
		t.Fatalf("held message not replayed after welcome: %q", got)
	}
}
