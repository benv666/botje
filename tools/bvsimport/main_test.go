package main

import "testing"

func TestParseLine(t *testing.T) {
	cases := []struct {
		line       string
		nick, text string
		ok         bool
	}{
		// irssi chat, with and without mode char
		{"00:29 < Bram> tjongejonge", "Bram", "tjongejonge", true},
		{"00:29 <@hoer> Karma for windows", "hoer", "Karma for windows", true},
		{"13:44 <+Willow> hoi", "Willow", "hoi", true},
		// irssi meta
		{"--- Log opened Wed Jan 01 00:00:12 2014", "", "", false},
		{"13:45 -!- willow1 [Willow@lotjuh.xenbro.nl] has joined #bvs", "", "", false},
		{"21:21 -!- Irssi: #bvs: Total of 10 nicks", "", "", false},
		{"13:40  * Bram doet iets", "", "", false},
		// weechat chat, with and without mode char
		{"2020-01-01 01:05:24\tBenV\tNou, dan zal ik maar", "BenV", "Nou, dan zal ik maar", true},
		{"2020-01-01 01:05:26\t@hoer\tKarma for whisky", "hoer", "Karma for whisky", true},
		// weechat meta
		{"2016-08-20 13:59:09\t<--\tWillow (willow@x) has quit", "", "", false},
		{"2016-08-20 13:59:55\t--\twillow1 is now known as Willow", "", "", false},
		{"2020-05-01 12:00:00\t-->\tBram (b@x) has joined #bvs", "", "", false},
		{"2020-05-01 12:00:00\t*\tBram doet iets", "", "", false},
		{"", "", "", false},
	}
	for _, c := range cases {
		nick, text, ok := parseLine(c.line)
		if nick != c.nick || text != c.text || ok != c.ok {
			t.Errorf("parseLine(%q) = %q %q %v, want %q %q %v",
				c.line, nick, text, ok, c.nick, c.text, c.ok)
		}
	}
}

func TestCanonical(t *testing.T) {
	alias := map[string]string{"wi11ow": "willow"}
	cases := map[string]string{
		"ventiel_":  "ventiel", // reconnect ghost
		"ventiel__": "ventiel",
		"wi11ow":    "willow",  // explicit alias
		"willow1":   "willow1", // digit variants stay separate
		"benv":      "benv",
	}
	for in, want := range cases {
		if got := canonical(in, alias); got != want {
			t.Errorf("canonical(%q) = %q, want %q", in, got, want)
		}
	}
}
