package rvf

import (
	"strings"
	"testing"

	"go-botje/internal/bus"
	"go-botje/internal/format"
	"go-botje/internal/storage"
)

func (f *fixture) join(nick, channel string) {
	ev := &bus.Event{Name: "IRC_JOIN", Server: "junerules", Channel: channel,
		SenderMe: strings.EqualFold(nick, "Meretrix"), Extra: map[string]any{}}
	ev.Sender.Nick = nick
	f.b.Submit(ev)
}

func (f *fixture) topicChange(nick, channel, topic string) {
	ev := &bus.Event{Name: "IRC_TOPIC", Server: "junerules", Channel: channel,
		SenderMe: strings.EqualFold(nick, "Meretrix"),
		Extra:    map[string]any{"topic": topic}}
	ev.Sender.Nick = nick
	f.b.Submit(ev)
}

// joining an owned game channel asserts the help topic; other channels
// are left alone.
func TestTopicSetOnJoin(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.join("Meretrix", "#radvanfortuin")
	if len(f.raw) != 1 || !strings.HasPrefix(f.raw[0], "TOPIC #radvanfortuin :") ||
		!strings.Contains(f.raw[0], "draai") {
		t.Fatalf("nl topic raw = %q", f.raw)
	}
	f.raw = nil
	f.join("Meretrix", "#wheeloffortune")
	if len(f.raw) != 1 || !strings.Contains(f.raw[0], "spin") {
		t.Fatalf("en topic raw = %q", f.raw)
	}
	f.raw = nil
	f.join("Meretrix", "#testing")
	if len(f.raw) != 0 {
		t.Fatalf("non-game channel got a topic: %q", f.raw)
	}
	// someone else joining changes nothing
	f.join("BenV", "#radvanfortuin")
	if len(f.raw) != 0 {
		t.Fatalf("other joins triggered a topic: %q", f.raw)
	}
}

// a changed topic on an owned channel is put back; our own topic (as it
// echoes back, mIRC-colored) and unowned channels are left alone.
func TestTopicSticks(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.topicChange("Verty", "#radvanfortuin", "lol gaming")
	if len(f.raw) != 1 || !strings.Contains(f.raw[0], "TOPIC #radvanfortuin :") {
		t.Fatalf("changed topic not restored: %q", f.raw)
	}
	f.raw = nil
	// the bot's own set echoes back with SenderMe: no loop
	f.topicChange("Meretrix", "#radvanfortuin", format.ToIRC(langs["nl"].topic))
	if len(f.raw) != 0 {
		t.Fatalf("own echo re-triggered: %q", f.raw)
	}
	// someone re-setting the exact wanted topic: nothing to fix
	f.topicChange("Verty", "#radvanfortuin", format.ToIRC(langs["nl"].topic))
	if len(f.raw) != 0 {
		t.Fatalf("correct topic still reset: %q", f.raw)
	}
	f.topicChange("Verty", "#testing", "whatever")
	if len(f.raw) != 0 {
		t.Fatalf("unowned channel topic touched: %q", f.raw)
	}
}

// hufterproof: garbage and trolling get fun refusals, not silence or
// broken state.
func TestHufterproof(t *testing.T) {
	f := newFixture(t, storage.NewMemory())

	// nine players: the studio is full
	f.msg("BenV", "#testing", "!start a,b,c,d,e,f,g,h,i")
	if out := f.all(); !strings.Contains(out, "8") {
		t.Fatalf("player cap reply: %q", out)
	}
	if len(f.m.games) != 0 {
		t.Fatal("overfull game was created")
	}

	// a novelty nick
	f.msg("BenV", "#testing", "!start "+strings.Repeat("x", 40))
	if out := f.all(); out == "" || strings.Contains(out, "beurt") {
		t.Fatalf("silly nick reply: %q", out)
	}
	if len(f.m.games) != 0 {
		t.Fatal("silly-nick game was created")
	}

	f.startGame("#testing", "BenV")
	f.take()
	// a letter before any spin
	f.rolls = []int{0}
	f.msg("BenV", "#testing", "t")
	if out := f.all(); !strings.Contains(out, "draai") {
		t.Fatalf("letter-before-spin: %q", out)
	}
	// a consonant is owed, but the player tries to spin/buy/solve/pass
	f.rolls = []int{20}
	f.msg("BenV", "#testing", "draai")
	f.take()
	for _, move := range []string{"draai", "koop e", "los op: iets", "pas"} {
		f.rolls = []int{0}
		f.msg("BenV", "#testing", move)
		if out := f.all(); !strings.Contains(out, "medeklinker") {
			t.Fatalf("%q while a letter is owed: %q", move, out)
		}
	}
	// the state never moved: the consonant still works
	f.msg("BenV", "#testing", "t")
	if out := f.all(); !strings.Contains(out, "7x T") {
		t.Fatalf("letter after garbage: %q", out)
	}
}

// winning paints ascii art: multi-line, colorized, with the nick in it.
func TestVictoryArt(t *testing.T) {
	f := newFixture(t, storage.NewMemory())
	f.startGame("#testing", "SomeVeryLongNickName12345")
	f.take()
	f.rolls = []int{20}
	f.msg("BenV", "#testing", "!stop") // not a player; keeps the game
	f.take()
	f.msg("SomeVeryLongNickName12345", "#testing", "draai")
	f.take()
	f.msg("SomeVeryLongNickName12345", "#testing", "t")
	f.take()
	f.rolls = []int{1} // art variant
	f.msg("SomeVeryLongNickName12345", "#testing", "los op: wie het laatst lacht lacht het best")
	out := f.all()
	if lines := strings.Count(out, "\n"); lines < 3 {
		t.Fatalf("win output has %d newlines, want art + win line: %q", lines, out)
	}
	// the art shows the nick display-truncated to 20 chars
	if !strings.Contains(out, "SomeVeryLongNickName…") {
		t.Fatalf("truncated nick missing from art: %q", out)
	}
	if !strings.Contains(out, "JUIST") {
		t.Fatalf("win line missing: %q", out)
	}
}

// every art variant renders to clean IRC bytes: exactly one banner
// placeholder, and no stray braces that colorize would mangle.
func TestVictoryArtVariants(t *testing.T) {
	if len(victoryArts) < 4 {
		t.Fatalf("only %d art variants, want >= 4", len(victoryArts))
	}
	for i, art := range victoryArts {
		if strings.Count(art, "%s") != 1 {
			t.Errorf("art %d has %d banner placeholders", i, strings.Count(art, "%s"))
		}
		lines := strings.Count(art, "\n") + 1
		if lines < 3 || lines > 4 {
			t.Errorf("art %d is %d lines, want 3-4", i, lines)
		}
		for _, lang := range []string{"nl", "en"} {
			roll := func(int) int { return i }
			rendered := format.ToIRC(victory(langs[lang], roll, "BenV"))
			if strings.ContainsAny(rendered, "{}") {
				t.Errorf("art %d (%s) leaves braces on the wire: %q", i, lang, rendered)
			}
			if !strings.Contains(rendered, "BenV") {
				t.Errorf("art %d (%s) lost the nick: %q", i, lang, rendered)
			}
		}
	}
}
