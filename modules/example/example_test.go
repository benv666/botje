package example

import (
	"strings"
	"testing"
	"time"

	"go-botje/internal/admin"
	"go-botje/internal/bus"
	"go-botje/internal/conf"
	"go-botje/internal/fetch"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/irc/pager"
	"go-botje/internal/module"
	"go-botje/internal/sched"
	"go-botje/internal/storage"
)

// this test keeps the example module honest: it must load against the
// real module API and every documented feature must actually work.
func TestExampleModuleWorks(t *testing.T) {
	clk := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	b := bus.New()
	for _, ev := range []string{"IRC_PRIVMSG", "IRC_JOIN", "COMMAND"} {
		b.RegisterEvent(ev)
	}
	cmds := cmd.New()
	cf := conf.New()
	sch := sched.New(func() time.Time { return clk })
	store := storage.NewMemory()
	var sent []string
	fetcher := fetch.New(func(fn func()) { fn() })

	m := New()
	ctx := &module.Context{
		Bus: b, Cmd: cmds, Conf: cf, Store: store, Sched: sch, Fetch: fetcher,
		Privmsg: func(ch, msg string) { sent = append(sent, ch+"|"+msg) },
	}
	ctx.Pager = pager.New(sch, func(ch, line string) { ctx.Privmsg(ch, line) })
	ctx.Pager.MaxLines = func() int { return 4 }
	if err := m.Load(ctx); err != nil {
		t.Fatal(err)
	}

	say := func(msg string) {
		ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: "#testing",
			Msg: msg, Extra: map[string]any{}}
		ev.Sender.Nick = "BenV"
		b.Submit(ev)
		cmds.Handle(ev)
	}

	// storage counter
	say("!example count")
	say("!example count")
	if len(sent) != 2 || !strings.Contains(sent[1], "2") {
		t.Fatalf("counter replies = %q", sent)
	}
	var st stored
	if _, err := store.Get("example", "state", &st); err != nil || st.Counter != 2 {
		t.Fatalf("stored counter = %+v (%v)", st, err)
	}

	// pager holds lines past the flood budget
	sent = nil
	say("!example lines")
	if len(sent) != 4 || !strings.Contains(sent[3], "line 4 of 8") {
		t.Fatalf("paged reply = %q", sent)
	}

	// sched timer fires on the fake clock
	sent = nil
	say("!example remind lunch")
	clk = clk.Add(6 * time.Second)
	sch.RunDue()
	if len(sent) != 2 || sent[1] != "#testing|BenV: lunch" {
		t.Fatalf("remind = %q", sent)
	}

	// default handler + conf setting
	sent = nil
	cf.Set("example_greeting", "Yo")
	say("!greet")
	if len(sent) != 1 || sent[0] != "#testing|Yo, BenV!" {
		t.Fatalf("greet = %q", sent)
	}

	// admin spec via COMMAND event
	var specs []admin.Spec
	for _, payload := range b.Submit(&bus.Event{Name: "COMMAND", Extra: map[string]any{}}) {
		if s, ok := payload.(admin.Spec); ok {
			specs = append(specs, s)
		}
	}
	if len(specs) != 1 || specs[0].Name != "example" {
		t.Fatalf("admin specs = %+v", specs)
	}
	if out := specs[0].Run("", "example"); !strings.Contains(out, "2") {
		t.Fatalf("admin example = %q", out)
	}

	// unload deregisters everything
	if err := m.Unload(); err != nil {
		t.Fatal(err)
	}
	sent = nil
	say("!example count")
	if len(sent) != 0 {
		t.Fatalf("command still live after unload: %q", sent)
	}
}
