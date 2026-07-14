package guard

import (
	"strings"
	"testing"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/conf"
	"go-botje/internal/module"
	"go-botje/internal/sched"
	"go-botje/internal/storage"
)

// efixture is the guard fixture wired with SendRaw + InChannel capture,
// for enforcement tests.
type efixture struct {
	*fixture
	raw []string
	in  map[string]bool
}

func newEnforce(t *testing.T) *efixture {
	t.Helper()
	f := &fixture{st: storage.NewMemory(), clk: time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)}
	f.b = bus.New()
	for _, ev := range []string{"IRC_PRIVMSG", "IRC_JOIN", "QUIT", "COMMAND"} {
		f.b.RegisterEvent(ev)
	}
	f.cf = conf.New()
	f.sch = sched.New(func() time.Time { return f.clk })
	f.m = New()
	f.m.Now = func() time.Time { return f.clk }
	ef := &efixture{fixture: f, in: map[string]bool{"#testing": true, "#bvs": true, "#rss": true}}
	if err := f.m.Load(&module.Context{
		Bus: f.b, Conf: f.cf, Store: f.st, Sched: f.sch,
		SendRaw:   func(line string) { ef.raw = append(ef.raw, line) },
		InChannel: func(ch string) bool { return ef.in[ch] },
		Privmsg:   func(ch, msg string) {},
	}); err != nil {
		t.Fatal(err)
	}
	return ef
}

func (f *efixture) join(nick, user, host, channel string) {
	ev := &bus.Event{Name: "IRC_JOIN", Server: "junerules", Channel: channel}
	ev.Sender.Nick, ev.Sender.User, ev.Sender.Host = nick, user, host
	f.b.Submit(ev)
}

func (f *efixture) say(nick, user, host, channel, msg string) {
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: channel, Msg: msg}
	ev.Sender.Nick, ev.Sender.User, ev.Sender.Host = nick, user, host
	f.b.Submit(ev)
}

func (f *efixture) query(nick, user, host, msg string) {
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: nick, Query: true, Msg: msg}
	ev.Sender.Nick, ev.Sender.User, ev.Sender.Host = nick, user, host
	f.b.Submit(ev)
}

func (f *efixture) glined() bool {
	for _, l := range f.raw {
		if strings.HasPrefix(l, "GLINE") {
			return true
		}
	}
	return false
}

func (f *efixture) kicks() (n int) {
	for _, l := range f.raw {
		if strings.HasPrefix(l, "KICK") {
			n++
		}
	}
	return
}

func TestNoEnforcementWhileOff(t *testing.T) {
	f := newEnforce(t)
	// same line to 3 channels, but guard is off
	f.say("spammer", "x", "evil", "#testing", "buy coins")
	f.say("spammer", "x", "evil", "#bvs", "buy coins")
	f.say("spammer", "x", "evil", "#rss", "buy coins")
	if f.glined() || f.kicks() > 0 {
		t.Fatalf("enforced while off: %v", f.raw)
	}
}

func TestDuplicateAcrossChannelsGlines(t *testing.T) {
	f := newEnforce(t)
	f.cf.Set("guard_enabled", "true")
	f.say("spammer", "x", "evil.host", "#testing", "buy coins at scam.example")
	if f.glined() {
		t.Fatal("glined on first channel")
	}
	f.say("spammer", "x", "evil.host", "#bvs", "buy coins at scam.example")
	if !f.glined() {
		t.Fatalf("no gline after duplicate across 2 channels: %v", f.raw)
	}
	// the gline targets the host, not the ident
	var gl string
	for _, l := range f.raw {
		if strings.HasPrefix(l, "GLINE") {
			gl = l
		}
	}
	if !strings.Contains(gl, "*@evil.host") {
		t.Fatalf("gline = %q, want *@evil.host", gl)
	}
}

func TestResidentsImmune(t *testing.T) {
	f := newEnforce(t)
	// establish resident while off
	f.say("Reg", "reg", "good.host", "#testing", "hi")
	f.cf.Set("guard_enabled", "true")
	// resident repeats across channels: must NOT be actioned
	f.say("Reg", "reg", "good.host", "#testing", "same line")
	f.say("Reg", "reg", "good.host", "#bvs", "same line")
	f.say("Reg", "reg", "good.host", "#rss", "same line")
	if f.glined() {
		t.Fatalf("resident was glined: %v", f.raw)
	}
}

func TestMassJoinGlines(t *testing.T) {
	f := newEnforce(t)
	f.cf.Set("guard_enabled", "true")
	f.join("floody", "f", "flood.host", "#testing")
	f.join("floody", "f", "flood.host", "#bvs")
	f.join("floody", "f", "flood.host", "#rss")
	f.join("floody", "f", "flood.host", "#desman")
	if !f.glined() {
		t.Fatalf("no gline after mass-join: %v", f.raw)
	}
}

func TestMassJoinSlowIsFine(t *testing.T) {
	f := newEnforce(t)
	f.cf.Set("guard_enabled", "true")
	for _, ch := range []string{"#testing", "#bvs", "#rss", "#desman"} {
		f.join("newbie", "n", "slow.host", ch)
		f.clk = f.clk.Add(1 * time.Minute) // outside the join window
	}
	if f.glined() {
		t.Fatalf("slow joiner glined: %v", f.raw)
	}
}

func TestKicksSharedChannels(t *testing.T) {
	f := newEnforce(t)
	f.cf.Set("guard_enabled", "true")
	f.join("spammer", "x", "evil.host", "#testing")
	f.join("spammer", "x", "evil.host", "#bvs")
	f.say("spammer", "x", "evil.host", "#testing", "scam")
	f.say("spammer", "x", "evil.host", "#bvs", "scam")
	// duplicate across 2 channels triggers; kicks from both joined channels
	if f.kicks() < 2 {
		t.Fatalf("kicks = %d, want >= 2: %v", f.kicks(), f.raw)
	}
}

func TestActionedOnce(t *testing.T) {
	f := newEnforce(t)
	f.cf.Set("guard_enabled", "true")
	f.say("spammer", "x", "evil.host", "#testing", "scam")
	f.say("spammer", "x", "evil.host", "#bvs", "scam")
	n := len(f.raw)
	// keep spamming: no additional enforcement lines
	f.say("spammer", "x", "evil.host", "#rss", "scam")
	f.say("spammer", "x", "evil.host", "#desman", "scam")
	if len(f.raw) != n {
		t.Fatalf("re-fired enforcement: %v", f.raw[n:])
	}
}

func TestAuthWhitelistsMidWave(t *testing.T) {
	f := newEnforce(t)
	f.cf.Set("guard_auth_password", "sesam")
	f.cf.Set("guard_enabled", "true")
	// legit newcomer auths via query BEFORE tripping a heuristic
	f.query("Newbie", "new", "home.host", "auth sesam")
	// now they talk across channels like everyone does; must be immune
	f.say("Newbie", "new", "home.host", "#testing", "hello all")
	f.say("Newbie", "new", "home.host", "#bvs", "hello all")
	f.say("Newbie", "new", "home.host", "#rss", "hello all")
	if f.glined() {
		t.Fatalf("authed newcomer was glined: %v", f.raw)
	}
}

func TestAuthWrongPasswordNoWhitelist(t *testing.T) {
	f := newEnforce(t)
	f.cf.Set("guard_auth_password", "sesam")
	f.cf.Set("guard_enabled", "true")
	f.query("spammer", "x", "evil.host", "auth wrong")
	f.say("spammer", "x", "evil.host", "#testing", "scam")
	f.say("spammer", "x", "evil.host", "#bvs", "scam")
	if !f.glined() {
		t.Fatalf("wrong-password suspect escaped: %v", f.raw)
	}
}
