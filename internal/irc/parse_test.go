package irc

import (
	"slices"
	"testing"
)

func TestParseLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want Line
		ok   bool
	}{
		{
			"numeric with prefix",
			":irc.benv.junerules.com 332 Meretrix #testing :the topic",
			Line{Prefix: "irc.benv.junerules.com", Cmd: "332", Name: "RPL_TOPIC", Code: "332",
				Params: "Meretrix #testing :the topic"},
			true,
		},
		{
			"privmsg",
			":BenV!benv@host.example PRIVMSG #testing :hello world",
			Line{Prefix: "BenV!benv@host.example", Cmd: "PRIVMSG", Name: "PRIVMSG",
				Params: "#testing :hello world"},
			true,
		},
		{
			"no prefix",
			"PING :irc.benv.junerules.com",
			Line{Cmd: "PING", Name: "PING", Params: ":irc.benv.junerules.com"},
			true,
		},
		{
			"no params",
			":nick!u@h QUIT",
			Line{Prefix: "nick!u@h", Cmd: "QUIT", Name: "QUIT"},
			true,
		},
		{
			"unknown numeric stays numeric",
			":srv 999 x :y",
			Line{Prefix: "srv", Cmd: "999", Name: "999", Code: "999", Params: "x :y"},
			true,
		},
		{
			"known error numeric",
			":srv 433 * Meretrix :Nickname is already in use",
			Line{Prefix: "srv", Cmd: "433", Name: "ERR_NICKNAMEINUSE", Code: "433",
				Params: "* Meretrix :Nickname is already in use"},
			true,
		},
		// ERR_NOSUCHNICK has a trailing space in the Perl table; fixed here
		{
			"nosuchnick name has no trailing space",
			":srv 401 me ghost :No such nick",
			Line{Prefix: "srv", Cmd: "401", Name: "ERR_NOSUCHNICK", Code: "401",
				Params: "me ghost :No such nick"},
			true,
		},
		{"two-digit cmd invalid", ":x 12 y", Line{}, false},
		{"four-digit cmd invalid", ":x 1234 y", Line{}, false},
		{"mixed alnum cmd invalid", ":x PRIV1MSG y", Line{}, false},
		{"empty line invalid", "", Line{}, false},
		{"prefix only invalid", ":onlyprefix", Line{}, false},
	} {
		got, ok := ParseLine(tc.in)
		if ok != tc.ok {
			t.Errorf("%s: ParseLine(%q) ok = %v, want %v", tc.name, tc.in, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("%s: ParseLine(%q)\n got %+v\nwant %+v", tc.name, tc.in, got, tc.want)
		}
	}
}

func TestParsePrefix(t *testing.T) {
	for _, tc := range []struct {
		in               string
		nick, user, host string
	}{
		{"BenV!benv@host.example", "BenV", "benv", "host.example"},
		{"nick@host.only", "nick", "", "host.only"},
		{"nick!user.only", "nick", "user.only", ""},
		{"justnick", "justnick", "", ""},
		{"irc.benv.junerules.com", "irc.benv.junerules.com", "", ""},
		// perl's non-greedy backtracking puts everything before @ in user
		{"a!b!c@d", "a", "b!c", "d"},
		{"", "", "", ""},
	} {
		nick, user, host := ParsePrefix(tc.in)
		if nick != tc.nick || user != tc.user || host != tc.host {
			t.Errorf("ParsePrefix(%q) = %q %q %q, want %q %q %q",
				tc.in, nick, user, host, tc.nick, tc.user, tc.host)
		}
	}
}

func TestSplitParams(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
	}{
		{"Meretrix #testing :the topic here", []string{"Meretrix", "#testing", "the topic here"}},
		{"#chan +o BenV", []string{"#chan", "+o", "BenV"}},
		{":only trailing", []string{"only trailing"}},
		{"a  b   :c d", []string{"a", "b", "c d"}},
		{"a :", []string{"a", ""}},
		{":", []string{""}},
		{"", nil},
		{"single", []string{"single"}},
		// perl quirk carried over: a middle param containing ':' turns
		// the whole rest into the trailing param
		{"foo:bar baz", []string{"foo:bar baz"}},
		{"#chan foo:bar :tail", []string{"#chan", "foo:bar :tail"}},
	} {
		if got := SplitParams(tc.in); !slices.Equal(got, tc.want) {
			t.Errorf("SplitParams(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLineBuffer(t *testing.T) {
	t.Run("split across chunks", func(t *testing.T) {
		var b LineBuffer
		if got := b.Feed([]byte(":srv PRIV")); len(got) != 0 {
			t.Fatalf("partial line yielded %q", got)
		}
		got := b.Feed([]byte("MSG #a :hi\r\nPING :x\r\n:next "))
		want := []string{":srv PRIVMSG #a :hi", "PING :x"}
		if !slices.Equal(got, want) {
			t.Fatalf("Feed = %q, want %q", got, want)
		}
		if got := b.Feed([]byte("QUIT\n")); !slices.Equal(got, []string{":next QUIT"}) {
			t.Fatalf("bare-lf line = %q", got)
		}
	})

	t.Run("empty lines dropped", func(t *testing.T) {
		var b LineBuffer
		got := b.Feed([]byte("\r\n\nPING :x\r\n\r\n"))
		if !slices.Equal(got, []string{"PING :x"}) {
			t.Fatalf("Feed = %q", got)
		}
	})

	t.Run("utf8 intact across chunk boundary", func(t *testing.T) {
		var b LineBuffer
		full := []byte("PRIVMSG #a :bier \xf0\x9f\x8d\xba proost\r\n")
		b.Feed(full[:20]) // cuts the beer emoji in half
		got := b.Feed(full[20:])
		if !slices.Equal(got, []string{"PRIVMSG #a :bier 🍺 proost"}) {
			t.Fatalf("Feed = %q", got)
		}
	})

	t.Run("invalid utf8 dropped with line intact", func(t *testing.T) {
		var b LineBuffer
		got := b.Feed([]byte("PRIVMSG #a :bad \xff\xfe bytes\r\n"))
		if !slices.Equal(got, []string{"PRIVMSG #a :bad  bytes"}) {
			t.Fatalf("Feed = %q", got)
		}
	})
}
