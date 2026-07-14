package main

import (
	"flag"
	"testing"
)

// the shared collectors: env fills the gaps, flags win over env.
func TestCoreFlags(t *testing.T) {
	t.Setenv("BOTJE_NETWORK", "")
	t.Setenv("BOTJE_NICK", "EnvNick")
	t.Setenv("BOTJE_CHANNELS", "")
	t.Setenv("BOTJE_ADMIN", "")
	t.Setenv("BOTJE_METRICS", "")
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	get := coreFlags(fs)
	if err := fs.Parse([]string{"-channels", "#a,#b"}); err != nil {
		t.Fatal(err)
	}
	o := get()
	if o.network != "junerules" {
		t.Errorf("network = %q, want the default", o.network)
	}
	if o.nick != "EnvNick" {
		t.Errorf("nick = %q, want the env value", o.nick)
	}
	if o.channels != "#a,#b" {
		t.Errorf("channels = %q, want the flag value", o.channels)
	}
	if o.admin != "127.0.0.1:1924" {
		t.Errorf("admin = %q, want the default", o.admin)
	}
	if o.metrics != "" {
		t.Errorf("metrics = %q, want off by default", o.metrics)
	}
}

func TestIRCFlags(t *testing.T) {
	t.Setenv("BOTJE_IRC_ADDR", "irc.example.org:6697")
	t.Setenv("BOTJE_IRC_TLS", "")
	t.Setenv("BOTJE_TLS_CERT", "")
	t.Setenv("BOTJE_TLS_KEY", "")
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	get := ircFlags(fs)
	if err := fs.Parse([]string{"-tls=false", "-tls-cert", "c.pem", "-tls-key", "k.pem"}); err != nil {
		t.Fatal(err)
	}
	c := get()
	if c.addr != "irc.example.org:6697" {
		t.Errorf("addr = %q, want the env value", c.addr)
	}
	if c.tls {
		t.Error("tls = true, the flag should have switched it off")
	}
	if c.cert != "c.pem" || c.key != "k.pem" {
		t.Errorf("cert/key = %q/%q", c.cert, c.key)
	}
}

// there is deliberately no in-code IRC server default (public repo):
// an empty addr must refuse, not fall back.
func TestRequireAddr(t *testing.T) {
	if requireAddr("") {
		t.Error("empty addr accepted")
	}
	if !requireAddr("irc.example.org:6697") {
		t.Error("real addr refused")
	}
}
