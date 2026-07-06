package llm

import (
	"encoding/json"
	"strings"

	"go-botje/internal/fetch"
)

const userAgent = "IRC Bot/5.0 (botjevanirc@gmail.com) Fetcher/1.5"

// --- OpenAI (!gpt)

type openAI struct{}

func (openAI) command() string      { return "gpt" }
func (openAI) name() string         { return "openai" }
func (openAI) senderPrefixed() bool { return false }

func (openAI) build(m *Module, query, userContent string, hist []message) (string, fetch.Options, string, bool) {
	key := m.ctx.Conf.String("llm_openai_key")
	if key == "" {
		return "", fetch.Options{}, "Error - no openai API key configured (conf llm_openai_key).", false
	}
	messages := []message{{Role: "system", Content: m.systemPrompt()}}
	messages = append(messages, hist...)
	messages = append(messages, message{Role: "user", Content: userContent})

	body := marshal(map[string]any{
		"model":             m.ctx.Conf.String("llm_openai_model"),
		"messages":          messages,
		"max_tokens":        1024,
		"temperature":       0.9,
		"top_p":             1,
		"frequency_penalty": 0.3,
		"presence_penalty":  0.6,
	})
	return "https://api.openai.com/v1/chat/completions", fetch.Options{
		Body:        body,
		ContentType: "application/json",
		Headers: map[string]string{
			"Authorization": "Bearer " + key,
			"Accept":        "application/json",
			"User-Agent":    userAgent,
		},
	}, "", true
}

func (openAI) parse(body []byte) (string, message, string, bool) {
	var r struct {
		Choices []struct {
			Message message `json:"message"`
		} `json:"choices"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &r) != nil {
		return "", message{}, "", false
	}
	if len(r.Choices) == 0 || r.Choices[0].Message.Content == "" {
		return "", message{}, r.Error.Code, false
	}
	var parts []string
	for _, c := range r.Choices {
		parts = append(parts, cleanReply(c.Message.Content))
	}
	return strings.Join(parts, " "), r.Choices[0].Message, "", true
}

// --- Bedrock (!claude)

type bedrock struct{}

func (bedrock) command() string      { return "claude" }
func (bedrock) name() string         { return "bedrock" }
func (bedrock) senderPrefixed() bool { return false }

func (bedrock) build(m *Module, query, userContent string, hist []message) (string, fetch.Options, string, bool) {
	key := m.ctx.Conf.String("llm_aws_key")
	secret := m.ctx.Conf.String("llm_aws_secret")
	if key == "" || secret == "" {
		return "", fetch.Options{}, "Error - no AWS credentials configured (conf llm_aws_key / llm_aws_secret).", false
	}
	model, stripped, reason, ok := pickModel(m, "llm_bedrock_models", query)
	if !ok {
		return "", fetch.Options{}, reason, false
	}
	// a model token consumed from the query drops out of the sent
	// message too (rebuild the nick prefix around the stripped text)
	if stripped != query {
		userContent = strings.SplitN(userContent, ": ", 2)[0] + ": " + stripped
	}
	messages := append(append([]message{}, hist...), message{Role: "user", Content: userContent})
	body := marshal(map[string]any{
		"anthropic_version": "bedrock-2023-05-31",
		"max_tokens":        4000,
		"system":            m.systemPrompt(),
		"messages":          messages,
	})
	region := m.ctx.Conf.String("llm_aws_region")
	url := "https://bedrock-runtime." + region + ".amazonaws.com/model/" + model + "/invoke"
	return url, fetch.Options{
		Body:        body,
		ContentType: "application/json",
		Timeout:     120e9, // 120s
		Headers: map[string]string{
			"Accept":     "application/json",
			"User-Agent": userAgent,
		},
		Sign: fetch.SignV4(fetch.AWSCredentials{
			AccessKeyID:     key,
			SecretAccessKey: secret,
			Session:         m.ctx.Conf.String("llm_aws_session"),
		}, "bedrock", region, m.Now),
	}, "", true
}

func (bedrock) parse(body []byte) (string, message, string, bool) {
	var r struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		Usage *struct{} `json:"usage"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &r) != nil {
		return "", message{}, "", false
	}
	if r.Usage == nil || len(r.Content) == 0 {
		return "", message{}, r.Error.Message, false
	}
	var parts []string
	for _, c := range r.Content {
		if c.Text != "" {
			parts = append(parts, cleanReply(c.Text))
		}
	}
	reply := strings.Join(parts, " ")
	return reply, message{Role: "assistant", Content: reply}, "", true
}

// --- Ollama (!oi)

type ollama struct{}

func (ollama) command() string      { return "oi" }
func (ollama) name() string         { return "ollama" }
func (ollama) senderPrefixed() bool { return true }

func (ollama) build(m *Module, query, userContent string, hist []message) (string, fetch.Options, string, bool) {
	base := m.ctx.Conf.String("llm_ollama_url")
	if base == "" {
		return "", fetch.Options{}, "Error - no ollama url configured (conf llm_ollama_url).", false
	}
	model := m.ctx.Conf.String("llm_ollama_model")
	messages := append(append([]message{}, hist...), message{Role: "user", Content: userContent})
	body := marshal(map[string]any{
		"model":    model,
		"stream":   false,
		"messages": messages,
	})
	return strings.TrimRight(base, "/") + "/api/chat", fetch.Options{
		Body:        body,
		ContentType: "application/json",
		Timeout:     120e9,
		Headers: map[string]string{
			"Accept":     "application/json",
			"User-Agent": userAgent,
		},
	}, "", true
}

func (ollama) parse(body []byte) (string, message, string, bool) {
	var r struct {
		Model   string  `json:"model"`
		Message message `json:"message"`
	}
	if json.Unmarshal(body, &r) != nil {
		return "", message{}, "", false
	}
	if r.Message.Content == "" {
		return "", message{}, "", false
	}
	short := strings.TrimSuffix(r.Model, ":latest")
	reply := "[" + short + "] " + cleanReply(r.Message.Content)
	return reply, r.Message, "", true
}

// pickModel parses an optional leading model token from the query,
// checking it against the conf allow-list. Returns the chosen model,
// the query with the model token stripped, and a denial reason if the
// token looks like a model but is not allowed.
func pickModel(m *Module, confKey, query string) (model, stripped, reason string, ok bool) {
	allowed := strings.Fields(m.ctx.Conf.String(confKey))
	def := ""
	if len(allowed) > 0 {
		def = allowed[0]
	}
	fields := strings.Fields(query)
	if len(fields) == 0 {
		return def, query, "", true
	}
	cand := strings.ToLower(fields[0])
	for _, a := range allowed {
		if strings.ToLower(a) == cand {
			return a, strings.TrimSpace(strings.TrimPrefix(query, fields[0])), "", true
		}
	}
	// looks like a model (provider prefix) but not on the allow-list
	if strings.Contains(cand, ".") || strings.Contains(cand, ":") {
		return "", query, "denied, right now we only support these models: " + strings.Join(allowed, ", "), false
	}
	return def, query, "", true
}
