package llm

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"go-botje/internal/bus"
	"go-botje/internal/conf"
	"go-botje/internal/fetch"
	"go-botje/internal/irc/cmd"
	"go-botje/internal/module"
	"go-botje/internal/storage"
)

type call struct {
	url     string
	body    string
	headers map[string]string
	signed  bool
}

type fixture struct {
	m     *Module
	b     *bus.Bus
	cmds  *cmd.Registry
	cf    *conf.Conf
	sent  []string
	calls []call
	cbs   []func(fetch.Result)
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{}
	f.b = bus.New()
	f.b.RegisterEvent("IRC_PRIVMSG")
	f.cmds = cmd.New()
	f.cf = conf.New()
	f.m = New()
	f.m.Now = func() time.Time { return time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC) }
	f.m.fetch = func(url string, opts fetch.Options, cb func(fetch.Result)) bool {
		body := ""
		if opts.Body != nil {
			body = string(opts.Body)
		}
		f.calls = append(f.calls, call{url: url, body: body, headers: opts.Headers, signed: opts.Sign != nil})
		f.cbs = append(f.cbs, cb)
		return true
	}
	err := f.m.Load(&module.Context{
		Bus: f.b, Cmd: f.cmds, Conf: f.cf, Store: storage.NewMemory(),
		Privmsg: func(ch, msg string) { f.sent = append(f.sent, ch+"|"+msg) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) say(nick, msg string) {
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: "#testing",
		Msg: msg}
	ev.Sender.Nick = nick
	f.b.Submit(ev)
	f.cmds.Handle(ev)
}

func (f *fixture) reply(t *testing.T, n int, body string) {
	t.Helper()
	if n >= len(f.cbs) {
		t.Fatalf("no fetch #%d in flight (have %d)", n, len(f.cbs))
	}
	f.cbs[n](fetch.Result{Status: 200, Body: []byte(body)})
}

func (f *fixture) take() []string {
	s := f.sent
	f.sent = nil
	return s
}

func openaiResp(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
	})
	return string(b)
}

func TestGPTNeedsKey(t *testing.T) {
	f := newFixture(t)
	f.say("BenV", "!gpt hallo")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "no openai API key configured") {
		t.Fatalf("keyless = %q", got)
	}
	if len(f.calls) != 0 {
		t.Fatalf("called without key: %v", f.calls)
	}
}

func TestGPTRoundTrip(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("llm_openai_key", "sk-test")
	f.say("BenV", "!gpt wat is ethereum")
	if len(f.calls) != 1 {
		t.Fatalf("calls = %d", len(f.calls))
	}
	c := f.calls[0]
	if c.url != "https://api.openai.com/v1/chat/completions" {
		t.Fatalf("url = %q", c.url)
	}
	if c.headers["Authorization"] != "Bearer sk-test" {
		t.Fatalf("auth = %q", c.headers["Authorization"])
	}
	if c.signed {
		t.Fatal("openai request must not be sigv4-signed")
	}
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(c.body), &req); err != nil {
		t.Fatalf("body not json: %v", err)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" {
		t.Fatalf("messages = %+v", req.Messages)
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "user" || last.Content != "BenV: wat is ethereum" {
		t.Fatalf("user message = %+v", last)
	}

	f.reply(t, 0, openaiResp("Ethereum is een blockchain."))
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|Ethereum is een blockchain." {
		t.Fatalf("reply = %q", got)
	}
}

func TestHistoryAccumulates(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("llm_openai_key", "k")
	f.say("BenV", "!gpt eerste")
	f.reply(t, 0, openaiResp("antwoord een"))
	f.take()
	f.say("BenV", "!gpt tweede")

	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	json.Unmarshal([]byte(f.calls[1].body), &req)
	// system + (user1, assistant1) + user2
	if len(req.Messages) != 4 {
		t.Fatalf("messages = %d, want 4 with history", len(req.Messages))
	}
	if req.Messages[1].Content != "BenV: eerste" || req.Messages[2].Content != "antwoord een" {
		t.Fatalf("history = %+v", req.Messages)
	}
}

func TestHistoryCappedAt16(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("llm_openai_key", "k")
	for i := range 12 { // 12 exchanges = 24 turns, must clamp to 16
		f.say("BenV", "!gpt vraag")
		f.reply(t, i, openaiResp("antwoord"))
	}
	f.say("BenV", "!gpt laatste")
	var req struct {
		Messages []any `json:"messages"`
	}
	json.Unmarshal([]byte(f.calls[12].body), &req)
	// system + at most 16 history + the new user turn
	if len(req.Messages) != 18 {
		t.Fatalf("messages = %d, want 18 (system + 16 capped history + user)", len(req.Messages))
	}
}

func TestHistoryPerChannel(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("llm_openai_key", "k")
	f.say("BenV", "!gpt kanaal een")
	f.reply(t, 0, openaiResp("ok"))
	f.take()
	// a different channel starts fresh
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: "#other",
		Msg: "!gpt kanaal twee"}
	ev.Sender.Nick = "BenV"
	f.b.Submit(ev)
	f.cmds.Handle(ev)
	var req struct {
		Messages []any `json:"messages"`
	}
	json.Unmarshal([]byte(f.calls[1].body), &req)
	if len(req.Messages) != 2 {
		t.Fatalf("other channel messages = %d, want 2 (fresh)", len(req.Messages))
	}
}

func TestGPTGarbage(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("llm_openai_key", "k")
	f.say("BenV", "!gpt iets")
	f.reply(t, 0, "not json at all")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "returned some garbage data for [{W}iets{/}]") {
		t.Fatalf("garbage = %q", got)
	}
}

func TestGPTAPIError(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("llm_openai_key", "k")
	f.say("BenV", "!gpt iets")
	f.reply(t, 0, `{"error":{"code":"invalid_api_key"}}`)
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "failure => invalid_api_key") {
		t.Fatalf("api error = %q", got)
	}
}

func TestContextLengthReducesHistory(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("llm_openai_key", "k")
	// build up 2 pairs of history
	f.say("BenV", "!gpt een")
	f.reply(t, 0, openaiResp("a1"))
	f.say("BenV", "!gpt twee")
	f.reply(t, 1, openaiResp("a2"))
	f.take()
	f.say("BenV", "!gpt drie")
	f.reply(t, 2, `{"error":{"code":"context_length_exceeded"}}`)
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "Context history was too large") {
		t.Fatalf("context error = %q", got)
	}
	// next call has trimmed history (was 4 msgs + system, now 2 + system)
	f.say("BenV", "!gpt vier")
	var req struct {
		Messages []any `json:"messages"`
	}
	json.Unmarshal([]byte(f.calls[3].body), &req)
	if len(req.Messages) != 4 {
		t.Fatalf("trimmed messages = %d, want 4 (system + 1 pair + new user)", len(req.Messages))
	}
}

func TestBedrockSignedAndModel(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("llm_aws_key", "AKID")
	f.cf.Set("llm_aws_secret", "secret")
	f.say("BenV", "!claude leg blockchains uit")
	if len(f.calls) != 1 {
		t.Fatalf("calls = %d", len(f.calls))
	}
	c := f.calls[0]
	if !strings.HasPrefix(c.url, "https://bedrock-runtime.us-east-1.amazonaws.com/model/") ||
		!strings.HasSuffix(c.url, "/invoke") {
		t.Fatalf("url = %q", c.url)
	}
	if !c.signed {
		t.Fatal("bedrock request must be sigv4-signed")
	}
	var req struct {
		AnthropicVersion string `json:"anthropic_version"`
		System           string `json:"system"`
		Messages         []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(c.body), &req); err != nil {
		t.Fatalf("body: %v", err)
	}
	if req.AnthropicVersion != "bedrock-2023-05-31" || req.System == "" {
		t.Fatalf("req = %+v", req)
	}
	// bedrock keeps system separate; messages has just the user turn
	if len(req.Messages) != 1 || req.Messages[0].Content != "BenV: leg blockchains uit" {
		t.Fatalf("messages = %+v", req.Messages)
	}

	f.reply(t, 0, `{"content":[{"type":"text","text":"Een blockchain is..."}],"usage":{"input_tokens":10,"output_tokens":8},"model":"claude","stop_reason":"end_turn","id":"x"}`)
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|Een blockchain is..." {
		t.Fatalf("reply = %q", got)
	}
}

func TestBedrockNeedsCreds(t *testing.T) {
	f := newFixture(t)
	f.say("BenV", "!claude iets")
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "no AWS credentials configured") {
		t.Fatalf("keyless = %q", got)
	}
}

func TestBedrockModelOverride(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("llm_aws_key", "K")
	f.cf.Set("llm_aws_secret", "S")
	f.cf.Set("llm_bedrock_models", "anthropic.claude-x anthropic.claude-y")
	f.say("BenV", "!claude anthropic.claude-y wat dan ook")
	if !strings.Contains(f.calls[0].url, "anthropic.claude-y") {
		t.Fatalf("url = %q, want overridden model", f.calls[0].url)
	}
	// a disallowed model is refused
	f.say("BenV", "!claude anthropic.claude-evil ")
	got := f.take()
	if len(got) == 0 || !strings.Contains(got[len(got)-1], "denied") {
		t.Fatalf("disallowed model = %q", got)
	}
}

func TestOllamaLocal(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("llm_ollama_url", "http://ollama.example:11434")
	f.say("BenV", "!oi hallo daar")
	if len(f.calls) != 1 {
		t.Fatalf("calls = %d", len(f.calls))
	}
	c := f.calls[0]
	if c.url != "http://ollama.example:11434/api/chat" || c.signed {
		t.Fatalf("call = %+v", c)
	}
	var req struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	json.Unmarshal([]byte(c.body), &req)
	if req.Model != "llama3" || req.Stream {
		t.Fatalf("req = %+v", req)
	}
	f.reply(t, 0, `{"model":"llama3:latest","message":{"role":"assistant","content":"Hoi!"}}`)
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|BenV: [llama3] Hoi!" {
		t.Fatalf("reply = %q", got)
	}
}

func TestOllamaDefaultURL(t *testing.T) {
	f := newFixture(t)
	f.say("BenV", "!oi iets")
	// no url configured: refuse politely
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0], "no ollama url configured") {
		t.Fatalf("no url = %q", got)
	}
}

func TestEmptyQuery(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("llm_openai_key", "k")
	f.say("BenV", "!gpt")
	got := f.take()
	if len(got) != 1 || got[0] != "#testing|~ If I only had a brain..." {
		t.Fatalf("empty = %q", got)
	}
}

func TestOwnMessageIgnored(t *testing.T) {
	f := newFixture(t)
	f.cf.Set("llm_openai_key", "k")
	ev := &bus.Event{Name: "IRC_PRIVMSG", Server: "junerules", Channel: "#testing",
		Msg: "!gpt iets", SenderMe: true}
	f.b.Submit(ev)
	f.cmds.Handle(ev)
	if len(f.calls) != 0 {
		t.Fatal("acted on own message")
	}
}
