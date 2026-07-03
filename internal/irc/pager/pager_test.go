package pager

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/sched"
)

type fixture struct {
	clk  *time.Time
	sch  *sched.Sched
	p    *Pager
	sent []string // "channel|line"
}

func newFixture() *fixture {
	t0 := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	f := &fixture{clk: &t0}
	f.sch = sched.New(func() time.Time { return *f.clk })
	f.p = New(f.sch, func(channel, line string) {
		f.sent = append(f.sent, channel+"|"+line)
	})
	return f
}

func (f *fixture) advance(d time.Duration) {
	*f.clk = f.clk.Add(d)
	f.sch.RunDue()
}

func (f *fixture) take() []string {
	s := f.sent
	f.sent = nil
	return s
}

func ev(nick string) *bus.Event {
	e := &bus.Event{Name: "IRC_PRIVMSG", Channel: "#testing"}
	e.Sender.Nick = nick
	return e
}

func lines(n int) []string {
	var out []string
	for i := 1; i <= n; i++ {
		out = append(out, fmt.Sprintf("line%d", i))
	}
	return out
}

func TestFewLinesSentDirectly(t *testing.T) {
	f := newFixture()
	f.p.EventMsg(ev("BenV"), "karma", lines(3)...)
	want := []string{"#testing|line1", "#testing|line2", "#testing|line3"}
	if got := f.take(); !slices.Equal(got, want) {
		t.Fatalf("sent = %q, want %q", got, want)
	}
	f.p.More(ev("BenV"), "")
	if got := f.take(); !slices.Equal(got, []string{"#testing|There is nothing more to display for you."}) {
		t.Fatalf("More after full send = %q", got)
	}
}

func TestTruncationWithSuffix(t *testing.T) {
	f := newFixture()
	f.p.EventMsg(ev("BenV"), "karma", lines(10)...)
	want := []string{
		"#testing|line1", "#testing|line2", "#testing|line3",
		"#testing|line4 {W}(+6)",
	}
	if got := f.take(); !slices.Equal(got, want) {
		t.Fatalf("sent = %q, want %q", got, want)
	}
}

func TestNewlineSplitAndBlankDrop(t *testing.T) {
	f := newFixture()
	f.p.EventMsg(ev("BenV"), "karma", "a\n\nb", "   ", "c")
	want := []string{"#testing|a", "#testing|b", "#testing|c"}
	if got := f.take(); !slices.Equal(got, want) {
		t.Fatalf("sent = %q, want %q", got, want)
	}
}

func TestMorePagesThroughAndDrains(t *testing.T) {
	f := newFixture()
	f.p.EventMsg(ev("BenV"), "karma", lines(10)...)
	f.take()

	f.p.More(ev("BenV"), "")
	want := []string{
		"#testing|line5", "#testing|line6", "#testing|line7",
		"#testing|line8 {W}(+2)",
	}
	if got := f.take(); !slices.Equal(got, want) {
		t.Fatalf("first More = %q, want %q", got, want)
	}

	f.p.More(ev("BenV"), "")
	want = []string{"#testing|line9", "#testing|line10"}
	if got := f.take(); !slices.Equal(got, want) {
		t.Fatalf("second More = %q, want %q", got, want)
	}
	if f.sch.Len() != 0 {
		t.Fatal("expiry timer still pending after full drain")
	}

	f.p.More(ev("BenV"), "")
	if got := f.take(); !slices.Equal(got, []string{"#testing|There is nothing more to display for you."}) {
		t.Fatalf("More after drain = %q", got)
	}
}

func TestMultipleCommandsPrompt(t *testing.T) {
	f := newFixture()
	f.p.EventMsg(ev("BenV"), "karma", lines(6)...)
	f.p.EventMsg(ev("BenV"), "rss", lines(6)...)
	f.take()

	f.p.More(ev("BenV"), "")
	want := []string{"#testing|Multiple commands are waiting for you to display more. Use !more karma|rss (choose one) to specify for which one you want to see more!"}
	if got := f.take(); !slices.Equal(got, want) {
		t.Fatalf("prompt = %q, want %q", got, want)
	}

	f.p.More(ev("BenV"), "rss")
	if got := f.take(); !slices.Equal(got, []string{"#testing|line5", "#testing|line6"}) {
		t.Fatalf("More rss = %q", got)
	}
}

func TestUnknownCommandWhileOthersPending(t *testing.T) {
	f := newFixture()
	f.p.EventMsg(ev("BenV"), "karma", lines(6)...)
	f.take()
	f.p.More(ev("BenV"), "rss")
	want := []string{"#testing|There is nothing (more) to display for rss for you."}
	if got := f.take(); !slices.Equal(got, want) {
		t.Fatalf("More unknown = %q, want %q", got, want)
	}
}

func TestExpiryAfter600s(t *testing.T) {
	f := newFixture()
	f.p.EventMsg(ev("BenV"), "karma", lines(10)...)
	f.take()
	f.advance(600 * time.Second)
	f.p.More(ev("BenV"), "")
	if got := f.take(); !slices.Equal(got, []string{"#testing|There is nothing more to display for you."}) {
		t.Fatalf("More after expiry = %q", got)
	}
}

// The Perl deletes the old stash on replace but leaves its expiry timer
// running, which then kills the new stash early. Fixed here.
func TestReplaceCancelsOldExpiry(t *testing.T) {
	f := newFixture()
	f.p.EventMsg(ev("BenV"), "karma", lines(10)...)
	f.take()
	f.advance(400 * time.Second)
	f.p.EventMsg(ev("BenV"), "karma", lines(10)...)
	f.take()
	f.advance(300 * time.Second) // old timer would have fired at t=600
	f.p.More(ev("BenV"), "")
	if got := f.take(); len(got) != 4 {
		t.Fatalf("More after replace = %q, want 4 lines: old expiry must not kill new stash", got)
	}
}

func TestPerNickIsolation(t *testing.T) {
	f := newFixture()
	f.p.EventMsg(ev("BenV"), "karma", lines(6)...)
	f.take()
	f.p.More(ev("Someone"), "")
	want := []string{"#testing|There is nothing more to display for you."}
	if got := f.take(); !slices.Equal(got, want) {
		t.Fatalf("other nick More = %q, want %q", got, want)
	}
}

func TestConfigurableMaxLines(t *testing.T) {
	f := newFixture()
	f.p.MaxLines = func() int { return 2 }
	f.p.EventMsg(ev("BenV"), "karma", lines(5)...)
	want := []string{"#testing|line1", "#testing|line2 {W}(+3)"}
	if got := f.take(); !slices.Equal(got, want) {
		t.Fatalf("sent = %q, want %q", got, want)
	}
}

func TestEmptyChannelIgnored(t *testing.T) {
	f := newFixture()
	e := ev("BenV")
	e.Channel = " : "
	f.p.EventMsg(e, "karma", lines(3)...)
	if got := f.take(); len(got) != 0 {
		t.Fatalf("sent %q for empty channel", got)
	}
}
