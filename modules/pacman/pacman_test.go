package pacman

import (
	"strings"
	"testing"

	"go-botje/internal/bus"
	"go-botje/internal/module"
)

type fixture struct {
	m    *Module
	b    *bus.Bus
	sent []string
	rand []float64 // consumed in order
}

func newFixture(t *testing.T, randSeq ...float64) *fixture {
	t.Helper()
	f := &fixture{rand: randSeq}
	f.b = bus.New()
	f.b.RegisterEvent("IRC_PRIVMSG")
	f.m = New()
	f.m.Rand = func() float64 {
		if len(f.rand) == 0 {
			return 0
		}
		v := f.rand[0]
		f.rand = f.rand[1:]
		return v
	}
	err := f.m.Load(&module.Context{
		Bus:     f.b,
		Privmsg: func(ch, msg string) { f.sent = append(f.sent, ch+"|"+msg) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) msg(text string) {
	ev := &bus.Event{Name: "IRC_PRIVMSG", BotNick: "Meretrix", Channel: "#testing",
		Msg: text, Extra: map[string]any{}}
	ev.Sender.Nick = "Verty"
	f.b.Submit(ev)
}

func TestThreeDotsAlwaysTriggers(t *testing.T) {
	f := newFixture(t, 0) // template 0
	f.msg("... :)")
	if len(f.sent) != 1 {
		t.Fatalf("sent = %q, want one pacman", f.sent)
	}
	if !strings.Contains(f.sent[0], "( o <") || !strings.Contains(f.sent[0], "Verty") {
		t.Fatalf("art = %q, want template 0 with nick", f.sent[0])
	}
}

func TestTwoDotsUsuallyIgnored(t *testing.T) {
	f := newFixture(t, 0.5) // rand > 0.3: ignore
	f.msg(".. huh")
	if len(f.sent) != 0 {
		t.Fatalf("sent = %q, want silence at rand 0.5", f.sent)
	}
}

func TestTwoDotsSurpriseAttack(t *testing.T) {
	f := newFixture(t, 0.1, 0.99) // rand <= 0.3: attack; template last
	f.msg(".. huh")
	if len(f.sent) != 1 {
		t.Fatalf("sent = %q, want surprise pacman", f.sent)
	}
	if !strings.Contains(f.sent[0], "_..._") {
		t.Fatalf("art = %q, want last template", f.sent[0])
	}
}

func TestIgnores(t *testing.T) {
	f := newFixture(t, 0, 0, 0, 0, 0)
	for _, m := range []string{"!command ..", "Meretrix: ...", "meretrix: ...", "no dots here", ". single"} {
		f.msg(m)
	}
	if len(f.sent) != 0 {
		t.Fatalf("sent = %q for non-triggers", f.sent)
	}
}

func TestOwnMessageIgnored(t *testing.T) {
	f := newFixture(t, 0)
	ev := &bus.Event{Name: "IRC_PRIVMSG", BotNick: "Meretrix", Channel: "#testing",
		Msg: "....", SenderMe: true, Extra: map[string]any{}}
	f.b.Submit(ev)
	if len(f.sent) != 0 {
		t.Fatalf("replied to own message: %q", f.sent)
	}
}

func TestChompingVariant(t *testing.T) {
	f := newFixture(t, 0.2) // int(0.2*6)=1: the added chomping pac-man
	f.msg("...")
	if len(f.sent) != 1 {
		t.Fatalf("sent = %q", f.sent)
	}
	if !strings.Contains(f.sent[0], "▝◣") || !strings.Contains(f.sent[0], "●") || !strings.Contains(f.sent[0], "Verty") {
		t.Fatalf("art = %q, want fancy variant with eye/mouth, dots, and nick", f.sent[0])
	}
	// no more than 3 art lines
	if n := strings.Count(strings.TrimRight(f.sent[0], "\n"), "\n") + 1; n > 3 {
		t.Fatalf("variant has %d lines, want <= 3", n)
	}
}
