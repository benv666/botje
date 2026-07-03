package irc

import (
	"slices"
	"testing"
	"time"

	"go-botje/internal/bus"
)

type sessFixture struct {
	s      *Session
	events []*bus.Event
	sent   []string
	high   []string
}

func newSess() *sessFixture {
	f := &sessFixture{}
	f.s = NewSession("junerules", "Meretrix", func() time.Time {
		return time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	})
	f.s.Send = func(l string) { f.sent = append(f.sent, l) }
	f.s.SendHigh = func(l string) { f.high = append(f.high, l) }
	f.s.Emit = func(ev *bus.Event) { f.events = append(f.events, ev) }
	return f
}

func (f *sessFixture) one(t *testing.T, want string) *bus.Event {
	t.Helper()
	if len(f.events) != 1 || f.events[0].Name != want {
		t.Fatalf("events = %+v, want one %s", f.events, want)
	}
	ev := f.events[0]
	f.events = nil
	return ev
}

func TestRegisterSendsNickUser(t *testing.T) {
	f := newSess()
	f.s.Register()
	want := []string{"NICK Meretrix", "USER Botje i * :BenV's Test Bot"}
	if !slices.Equal(f.sent, want) {
		t.Fatalf("sent = %q, want %q", f.sent, want)
	}
}

func TestPingPongHighPriority(t *testing.T) {
	f := newSess()
	f.s.HandleLine("PING :irc.benv.junerules.com")
	if !slices.Equal(f.high, []string{"PONG irc.benv.junerules.com"}) {
		t.Fatalf("high = %q", f.high)
	}
	if len(f.events) != 0 {
		t.Fatalf("PING produced events: %+v", f.events)
	}
	f.high = nil
	f.s.HandleLine("PING :has space")
	if !slices.Equal(f.high, []string{"PONG :has space"}) {
		t.Fatalf("high = %q, want colon-prefixed magic", f.high)
	}
}

func TestPrivmsgChannel(t *testing.T) {
	f := newSess()
	f.s.HandleLine(":BenV!benv@host.example PRIVMSG #testing :hello world")
	ev := f.one(t, "IRC_PRIVMSG")
	if ev.Channel != "#testing" || ev.Msg != "hello world" || ev.Query || ev.TargetMe || ev.SenderMe {
		t.Fatalf("event = %+v", ev)
	}
	if ev.Sender.Nick != "BenV" || ev.Sender.User != "benv" || ev.Sender.Host != "host.example" {
		t.Fatalf("sender = %+v", ev.Sender)
	}
	if ev.BotNick != "Meretrix" || ev.Server != "junerules" {
		t.Fatalf("botnick/server = %q/%q", ev.BotNick, ev.Server)
	}
	if ev.Raw.Cmd != "PRIVMSG" || ev.Raw.Params != "#testing :hello world" {
		t.Fatalf("raw = %+v", ev.Raw)
	}
}

func TestPrivmsgQueryRewrite(t *testing.T) {
	f := newSess()
	f.s.HandleLine(":BenV!benv@host PRIVMSG Meretrix :psst")
	ev := f.one(t, "IRC_PRIVMSG")
	if !ev.TargetMe || !ev.Query {
		t.Fatalf("query flags not set: %+v", ev)
	}
	if ev.Channel != "BenV" {
		t.Fatalf("channel = %q, want rewritten to sender nick", ev.Channel)
	}
}

func TestSenderMe(t *testing.T) {
	f := newSess()
	f.s.HandleLine(":Meretrix!bot@host PRIVMSG #testing :i said this")
	if ev := f.one(t, "IRC_PRIVMSG"); !ev.SenderMe {
		t.Fatalf("SenderMe not set: %+v", ev)
	}
}

func TestPrefixlessInheritsLastSender(t *testing.T) {
	f := newSess()
	f.s.HandleLine(":BenV!benv@host PRIVMSG #testing :first")
	f.events = nil
	f.s.HandleLine("PRIVMSG #testing :prefixless")
	ev := f.one(t, "IRC_PRIVMSG")
	if ev.Sender.Nick != "BenV" || ev.Sender.User != "benv" {
		t.Fatalf("sender = %+v, want inherited BenV", ev.Sender)
	}
}

func TestJoinTracking(t *testing.T) {
	f := newSess()
	f.s.HandleLine(":Meretrix!bot@host JOIN #testing")
	ev := f.one(t, "IRC_JOIN")
	if !ev.TargetMe || ev.Channel != "#testing" {
		t.Fatalf("self-join event = %+v", ev)
	}
	if got := f.s.Channels(); !slices.Equal(got, []string{"#testing"}) {
		t.Fatalf("Channels = %v", got)
	}

	f.s.HandleLine(":Someone!x@y JOIN #testing")
	ev = f.one(t, "IRC_JOIN")
	if ev.TargetMe || ev.Channel != "#testing" {
		t.Fatalf("other-join event = %+v", ev)
	}
	if got := f.s.Channels(); len(got) != 1 {
		t.Fatalf("Channels = %v, other people's joins must not add", got)
	}
}

// The Perl efPart has a typo ($event->{$channel} instead of {channel}),
// so IRC_PART events never carry the channel. Fixed here.
func TestPartTracking(t *testing.T) {
	f := newSess()
	f.s.HandleLine(":Meretrix!bot@host JOIN #testing")
	f.events = nil
	f.s.HandleLine(":Meretrix!bot@host PART #testing")
	ev := f.one(t, "IRC_PART")
	if ev.Channel != "#testing" {
		t.Fatalf("part channel = %q", ev.Channel)
	}
	if got := f.s.Channels(); len(got) != 0 {
		t.Fatalf("Channels after self-part = %v", got)
	}
}

func TestKickEvent(t *testing.T) {
	f := newSess()
	f.s.HandleLine(":BenV!b@h KICK #testing Meretrix :begone")
	ev := f.one(t, "IRC_KICK")
	if !ev.TargetMe {
		t.Fatal("TargetMe not set for own kick")
	}
	// parity: the Perl leaves event channel unset for KICK, only extra
	if ev.Channel != "" {
		t.Fatalf("channel = %q, want empty (perl parity)", ev.Channel)
	}
	if ev.Extra["channel"] != "#testing" || ev.Extra["target"] != "Meretrix" || ev.Extra["reason"] != "begone" {
		t.Fatalf("extra = %+v", ev.Extra)
	}
	f.s.HandleLine(":BenV!b@h KICK #testing Someone :bye")
	if ev := f.one(t, "IRC_KICK"); ev.TargetMe {
		t.Fatal("TargetMe set for someone else's kick")
	}
}

func TestQuitClassification(t *testing.T) {
	for _, tc := range []struct {
		params   string
		wantMsg  string
		netsplit bool
	}{
		{":Quit: bye folks", "bye folks", false},
		{":Ping timeout: 240 seconds", "Ping timeout", false},
		{":EOF from client", "EOF from client", false},
		{":irc1.example.net irc2.example.net", "irc1.example.net irc2.example.net", true},
		{":leaving", "leaving", false},
	} {
		f := newSess()
		f.s.HandleLine(":Someone!x@y QUIT " + tc.params)
		ev := f.one(t, "IRC_QUIT")
		if ev.Extra["msg"] != tc.wantMsg {
			t.Errorf("QUIT %q: msg = %q, want %q", tc.params, ev.Extra["msg"], tc.wantMsg)
		}
		gotSplit := ev.Extra["netsplit"] == 1
		if gotSplit != tc.netsplit {
			t.Errorf("QUIT %q: netsplit = %v, want %v", tc.params, gotSplit, tc.netsplit)
		}
	}
}

func TestTopicTracking(t *testing.T) {
	f := newSess()
	f.s.HandleLine(":BenV!b@h TOPIC #testing :new topic here")
	ev := f.one(t, "IRC_TOPIC")
	if ev.Channel != "#testing" || ev.Extra["topic"] != "new topic here" {
		t.Fatalf("topic event = %+v", ev)
	}
	if got := f.s.Topic("#testing"); got != "new topic here" {
		t.Fatalf("Topic = %q", got)
	}
	// RPL_TOPIC tracks silently, no event
	f.s.HandleLine(":srv 332 Meretrix #testing :from the server")
	if len(f.events) != 0 {
		t.Fatalf("RPL_TOPIC produced events: %+v", f.events)
	}
	if got := f.s.Topic("#testing"); got != "from the server" {
		t.Fatalf("Topic after 332 = %q", got)
	}
}

func TestModeChannelAndSelf(t *testing.T) {
	f := newSess()
	f.s.HandleLine(":BenV!b@h MODE #testing +o Meretrix")
	ev := f.one(t, "IRC_MODE")
	if ev.Channel != "#testing" || ev.TargetMe || ev.Extra["mode"] != "+o" {
		t.Fatalf("mode event = %+v", ev)
	}
	if un, ok := ev.Extra["unparsed"].([]string); !ok || !slices.Equal(un, []string{"Meretrix"}) {
		t.Fatalf("unparsed = %#v", ev.Extra["unparsed"])
	}

	f.s.HandleLine(":Meretrix MODE Meretrix :+i")
	ev = f.one(t, "IRC_MODE")
	if !ev.TargetMe {
		t.Fatalf("own mode event = %+v", ev)
	}
	if got := f.s.Mode(); got != "+i" {
		t.Fatalf("Mode = %q", got)
	}
}

func TestErrorEmitsAndQuits(t *testing.T) {
	f := newSess()
	f.s.HandleLine("ERROR :Closing Link: too many hoes")
	ev := f.one(t, "IRC_ERROR")
	if ev.Extra["msg"] != "Closing Link: too many hoes" {
		t.Fatalf("error msg = %q", ev.Extra["msg"])
	}
	if !slices.Equal(f.sent, []string{"QUIT :I can't deal with ERRORs, Bye bye!"}) {
		t.Fatalf("sent = %q", f.sent)
	}
}

func TestNoticeChannelIsTarget(t *testing.T) {
	f := newSess()
	f.s.HandleLine(":srv NOTICE Meretrix :look at me")
	ev := f.one(t, "IRC_NOTICE")
	// perl parity: notice channel is the target, not rewritten to sender
	if ev.Channel != "Meretrix" || !ev.TargetMe || ev.Msg != "look at me" {
		t.Fatalf("notice event = %+v", ev)
	}
}

func TestInvite(t *testing.T) {
	f := newSess()
	f.s.HandleLine(":BenV!b@h INVITE Meretrix :#secret")
	ev := f.one(t, "IRC_INVITE")
	if ev.Channel != "#secret" || !ev.TargetMe || ev.Extra["channel"] != "#secret" {
		t.Fatalf("invite event = %+v", ev)
	}
}

func TestMotdAccumulates(t *testing.T) {
	f := newSess()
	f.s.HandleLine(":srv 375 Meretrix :- srv Message of the day -")
	f.s.HandleLine(":srv 372 Meretrix :- Welcome to junerules")
	f.s.HandleLine(":srv 372 Meretrix :- Behave.")
	f.s.HandleLine(":srv 376 Meretrix :End of /MOTD command.")
	if got := f.s.Motd(); got != "- Welcome to junerules\n- Behave.\n" {
		t.Fatalf("Motd = %q", got)
	}
	if len(f.events) != 0 {
		t.Fatalf("MOTD numerics produced events: %+v", f.events)
	}
}

func TestUnhandledNumericIgnored(t *testing.T) {
	f := newSess()
	f.s.HandleLine(":srv 001 Meretrix :Welcome to the network")
	f.s.HandleLine("garbage with no valid structure \x01")
	if len(f.events) != 0 || len(f.sent) != 0 || len(f.high) != 0 {
		t.Fatalf("unexpected output: %+v %q %q", f.events, f.sent, f.high)
	}
}

func TestJoinChannels(t *testing.T) {
	f := newSess()
	f.s.JoinChannels([]string{"#testing", "#other"})
	want := []string{"JOIN #testing", "JOIN #other"}
	if !slices.Equal(f.sent, want) {
		t.Fatalf("sent = %q, want %q", f.sent, want)
	}
}

func TestBackoff(t *testing.T) {
	t0 := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	var b Backoff
	// escalating: 3, 60, 180, 300 with quick successive failures
	now := t0
	for _, want := range []time.Duration{3 * time.Second, 60 * time.Second, 180 * time.Second, 300 * time.Second} {
		got := b.Next(now)
		if got != want {
			t.Fatalf("Next = %v, want %v", got, want)
		}
		now = now.Add(time.Second) // failed again right away
	}
	// still failing fast: stays at 300 (the perl falls off the list and
	// stops reconnecting; fixed here)
	if got := b.Next(now); got != 300*time.Second {
		t.Fatalf("Next past end = %v, want 300s", got)
	}
	// after more than 300s of peace the ladder resets
	now = now.Add(302 * time.Second)
	if got := b.Next(now); got != 3*time.Second {
		t.Fatalf("Next after quiet period = %v, want reset to 3s", got)
	}
}
