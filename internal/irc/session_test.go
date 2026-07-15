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
	f.s.HandleLine("PING :irc.example.com")
	if !slices.Equal(f.high, []string{"PONG irc.example.com"}) {
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
	// divergence from perl (which left the channel in extra only): the
	// typed-fields refactor puts it in Channel like every other event
	if ev.Channel != "#testing" || ev.Target != "Meretrix" || ev.Reason != "begone" {
		t.Fatalf("kick fields = %q %q %q", ev.Channel, ev.Target, ev.Reason)
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
		if ev.Msg != tc.wantMsg {
			t.Errorf("QUIT %q: msg = %q, want %q", tc.params, ev.Msg, tc.wantMsg)
		}
		gotSplit := ev.NetSplit
		if gotSplit != tc.netsplit {
			t.Errorf("QUIT %q: netsplit = %v, want %v", tc.params, gotSplit, tc.netsplit)
		}
	}
}

func TestTopicTracking(t *testing.T) {
	f := newSess()
	f.s.HandleLine(":BenV!b@h TOPIC #testing :new topic here")
	ev := f.one(t, "IRC_TOPIC")
	if ev.Channel != "#testing" || ev.Topic != "new topic here" {
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
	if ev.Channel != "#testing" || ev.TargetMe || ev.Mode != "+o" {
		t.Fatalf("mode event = %+v", ev)
	}
	if !slices.Equal(ev.Args, []string{"Meretrix"}) {
		t.Fatalf("args = %#v", ev.Args)
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
	if ev.Msg != "Closing Link: too many hoes" {
		t.Fatalf("error msg = %q", ev.Msg)
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
	if ev.Channel != "#secret" || !ev.TargetMe {
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

// The Perl ignores 433 and sits nickless forever; here the session
// retries with an underscore suffix and follows its own new nick.
func TestNickInUseRetriesWithUnderscore(t *testing.T) {
	f := newSess()
	f.s.HandleLine(":srv 433 * Meretrix :Nickname is already in use")
	if !slices.Equal(f.sent, []string{"NICK Meretrix_"}) {
		t.Fatalf("sent = %q, want underscore retry", f.sent)
	}
	if len(f.events) != 0 {
		t.Fatalf("433 produced events: %+v", f.events)
	}
	// the bot now answers to the new nick: queries rewrite against it
	f.s.HandleLine(":BenV!b@h PRIVMSG Meretrix_ :psst")
	ev := f.one(t, "IRC_PRIVMSG")
	if !ev.Query || ev.BotNick != "Meretrix_" {
		t.Fatalf("event after nick change = %+v", ev)
	}
	// and again: another collision appends another underscore
	f.sent = nil
	f.s.HandleLine(":srv 433 * Meretrix_ :Nickname is already in use")
	if !slices.Equal(f.sent, []string{"NICK Meretrix__"}) {
		t.Fatalf("sent = %q", f.sent)
	}
}

func TestJoinChannels(t *testing.T) {
	f := newSess()
	f.s.JoinChannels([]string{"#testing", "#other"})
	want := []string{"JOIN #testing", "NAMES #testing", "JOIN #other", "NAMES #other"}
	if !slices.Equal(f.sent, want) {
		t.Fatalf("sent = %q, want %q", f.sent, want)
	}
}

// JoinChannels marks membership optimistically. Under a keeper a
// resuming core re-JOINs channels the live session is already in; the
// ircd sends no JOIN echo for those, so without this the core would
// think it is in no channels (and drop all channel output).
func TestJoinChannelsMarksMembership(t *testing.T) {
	f := newSess()
	f.s.JoinChannels([]string{"#testing", "#other"})
	if !f.s.InChannel("#testing") || !f.s.InChannel("#other") {
		t.Fatalf("channels not tracked after JoinChannels: %v", f.s.Channels())
	}
	// a later real JOIN echo must not break anything
	f.s.HandleLine(":Meretrix!bot@host JOIN #testing")
	if !f.s.InChannel("#testing") {
		t.Fatal("echo cleared membership")
	}
}

// Registration confirmation: 001 on a fresh connection, 462 when a
// restarted core re-registers over a keeper's live session. Joining is
// anchored on this, not on a timer: JOINs sent before the server
// confirms registration die with ERR_NOTREGISTERED (2026-07-13 live).
func TestSessionWelcome(t *testing.T) {
	for _, tc := range []struct {
		name, line string
	}{
		{"fresh 001", ":srv 001 Meretrix :Welcome to junerules"},
		{"resume 462", ":srv 462 Meretrix :You may not reregister"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSess()
			calls := 0
			f.s.Welcome = func() { calls++ }
			if f.s.Welcomed() {
				t.Fatal("welcomed before any server line")
			}
			f.s.HandleLine(tc.line)
			if !f.s.Welcomed() || calls != 1 {
				t.Fatalf("welcomed=%v calls=%d after %q", f.s.Welcomed(), calls, tc.line)
			}
			// a second confirmation must not re-fire (001 after a 462
			// resume, or vice versa)
			f.s.HandleLine(":srv 001 Meretrix :Welcome again")
			f.s.HandleLine(":srv 462 Meretrix :You may not reregister")
			if calls != 1 {
				t.Fatalf("Welcome fired %d times, want once", calls)
			}
		})
	}
}

func TestBackoff(t *testing.T) {
	t0 := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	var b Backoff
	// escalating: 3, 60, 180, 300 with the caller sleeping each returned
	// delay before the attempt fails again (the real call pattern)
	now := t0
	for _, want := range []time.Duration{3 * time.Second, 60 * time.Second, 180 * time.Second, 300 * time.Second} {
		got := b.Next(now)
		if got != want {
			t.Fatalf("Next = %v, want %v", got, want)
		}
		now = now.Add(got + time.Second) // slept the delay, attempt failed a second later
	}
	// still failing: stays at 300 (the perl falls off the list and stops
	// reconnecting; fixed here). The 300s sleep itself must NOT count as
	// a quiet period: that cycled the ladder back to 3s live on
	// 2026-07-13 and kept hammering a connectban'd ircd.
	for range 3 {
		got := b.Next(now)
		if got != 300*time.Second {
			t.Fatalf("Next past end = %v, want 300s", got)
		}
		now = now.Add(got + time.Second)
	}
	// a connection that lived >300s past the attempt is real peace: the
	// ladder resets
	now = now.Add(301 * time.Second)
	if got := b.Next(now); got != 3*time.Second {
		t.Fatalf("Next after quiet period = %v, want reset to 3s", got)
	}
}

// --- channel member tracking (NAMES) ---

// NAMES replies build the member list: 353 accumulates, 366 swaps it
// in, status prefixes are stripped, and a later refresh REPLACES the
// list instead of merging into it.
func TestNamesReplyBuildsMembers(t *testing.T) {
	f := newSess()
	f.s.HandleLine(":Meretrix!bot@host JOIN #testing")
	f.events = nil
	f.s.HandleLine(":srv 353 Meretrix = #testing :BenV @Verty +Lotjuh")
	f.s.HandleLine(":srv 353 Meretrix = #testing :~Bram %Ventiel")
	f.s.HandleLine(":srv 366 Meretrix #testing :End of /NAMES list.")
	want := []string{"BenV", "Bram", "Lotjuh", "Ventiel", "Verty"}
	if got := f.s.Members("#testing"); !slices.Equal(got, want) {
		t.Fatalf("Members = %q, want %q", got, want)
	}
	if len(f.events) != 0 {
		t.Fatalf("NAMES numerics produced events: %+v", f.events)
	}
	// a refresh replaces: Verty is gone from the new list
	f.s.HandleLine(":srv 353 Meretrix = #testing :BenV")
	f.s.HandleLine(":srv 366 Meretrix #testing :End of /NAMES list.")
	if got := f.s.Members("#testing"); !slices.Equal(got, []string{"BenV"}) {
		t.Fatalf("refreshed Members = %q, want just BenV", got)
	}
}

// join/part/kick/quit/nick keep the member list current between NAMES
// refreshes.
func TestMembersTrackedThroughEvents(t *testing.T) {
	f := newSess()
	f.s.HandleLine(":Meretrix!bot@host JOIN #testing")
	f.s.HandleLine(":Meretrix!bot@host JOIN #other")
	f.s.HandleLine(":srv 353 Meretrix = #testing :BenV Verty")
	f.s.HandleLine(":srv 366 Meretrix #testing :End of /NAMES list.")
	f.s.HandleLine(":srv 353 Meretrix = #other :BenV")
	f.s.HandleLine(":srv 366 Meretrix #other :End of /NAMES list.")

	f.s.HandleLine(":Bram!b@h JOIN #testing")
	if !f.s.NickIn("#testing", "bram") { // nicks are case-insensitive
		t.Fatal("joined nick not tracked")
	}
	f.s.HandleLine(":Verty!v@h PART #testing")
	if f.s.NickIn("#testing", "Verty") {
		t.Fatal("parted nick still tracked")
	}
	f.s.HandleLine(":op!o@h KICK #testing Bram :out")
	if f.s.NickIn("#testing", "Bram") {
		t.Fatal("kicked nick still tracked")
	}
	// a quit disappears from every shared channel
	f.s.HandleLine(":BenV!b@h QUIT :Quit: weg")
	if f.s.NickIn("#testing", "BenV") || f.s.NickIn("#other", "BenV") {
		t.Fatal("quit nick still tracked")
	}
	// a nick change renames, it does not add
	f.s.HandleLine(":srv 353 Meretrix = #testing :Lotjuh")
	f.s.HandleLine(":srv 366 Meretrix #testing :End of /NAMES list.")
	f.s.HandleLine(":Lotjuh!l@h NICK :Slotjuh")
	if f.s.NickIn("#testing", "Lotjuh") || !f.s.NickIn("#testing", "Slotjuh") {
		t.Fatalf("nick change not applied: %q", f.s.Members("#testing"))
	}
}

// JoinChannels asks for NAMES explicitly: a core resuming a keeper's
// live session gets no JOIN echo for already-joined channels (the
// 2026-07-08 InChannel lesson), so without this the member list would
// stay empty until the next full reconnect.
func TestJoinChannelsRequestsNames(t *testing.T) {
	f := newSess()
	f.s.JoinChannels([]string{"#testing"})
	if !slices.Contains(f.sent, "NAMES #testing") {
		t.Fatalf("no NAMES request: %q", f.sent)
	}
}

// The bot's own re-join resets the list: whoever was tracked before a
// kick is stale by the time we are back.
func TestOwnRejoinResetsMembers(t *testing.T) {
	f := newSess()
	f.s.HandleLine(":Meretrix!bot@host JOIN #testing")
	f.s.HandleLine(":srv 353 Meretrix = #testing :BenV")
	f.s.HandleLine(":srv 366 Meretrix #testing :End of /NAMES list.")
	f.s.HandleLine(":Meretrix!bot@host JOIN #testing")
	if got := f.s.Members("#testing"); len(got) != 0 {
		t.Fatalf("members survived own re-join: %q", got)
	}
}
