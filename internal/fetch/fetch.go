// Package fetch is the async HTTP client for modules, the Go
// counterpart of the Perl Fetcher: single-flight per exact URL,
// redirect cap (default 8), timeout (whole-request here vs the Perl
// inactivity timer, default 30s), size limit, basic auth, streaming
// callbacks. Built on net/http instead of the hand-rolled Perl HTTP.
//
// Fetch returns immediately; the request runs in a goroutine and the
// callback is handed to Deliver, which the core wires to the
// dispatcher loop so module code stays single-threaded. SigV4 signing
// (Bedrock) plugs in later via Options.Sign.
package fetch

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"sync"
	"time"
)

const (
	defaultTimeout   = 30 * time.Second
	defaultRedirects = 8
)

// Options mirror the Perl fetcher options.
type Options struct {
	Method      string                    // default GET, or POST when Body is set
	Body        []byte                    // request body; implies POST unless Method set
	ContentType string                    // Content-Type for Body
	Headers     map[string]string         // extra request headers
	Redirects   int                       // max redirect hops; 0 means the default 8
	NoRedirect  bool                      // return the redirect response itself
	Timeout     time.Duration             // whole-request timeout; 0 means 30s
	SizeLimit   int64                     // stop reading past this many body bytes
	User, Pass  string                    // basic auth
	Stream      func(chunk []byte)        // body chunks as they arrive; Body stays empty
	Sign        func(*http.Request) error // request signing hook (SigV4 later)
}

// Result is what the callback receives.
type Result struct {
	URL              string
	Status           int
	StatusLine       string
	Headers          http.Header
	Body             []byte
	SizeLimitReached bool
	Err              error
}

// Fetcher runs requests and serializes callback delivery.
type Fetcher struct {
	deliver func(fn func())

	// Observe, when set, is called after every request with the target
	// host, the request duration in seconds, and whether it errored.
	// Called from the fetch goroutine before the callback is delivered:
	// must be goroutine-safe. Set it before the first Fetch. Metrics
	// food.
	Observe func(host string, seconds float64, isErr bool)

	mu       sync.Mutex
	inFlight map[string]bool
}

// New returns a fetcher delivering callbacks through deliver (the core
// passes an enqueue-on-dispatcher function).
func New(deliver func(fn func())) *Fetcher {
	return &Fetcher{deliver: deliver, inFlight: make(map[string]bool)}
}

// Fetch starts an async request. Reports false without doing anything
// when the same URL is already in flight (Perl single-flight).
func (f *Fetcher) Fetch(url string, opts Options, cb func(Result)) bool {
	f.mu.Lock()
	if f.inFlight[url] {
		f.mu.Unlock()
		return false
	}
	f.inFlight[url] = true
	f.mu.Unlock()

	go func() {
		start := time.Now()
		res := f.do(url, opts)
		if f.Observe != nil {
			f.Observe(hostOf(url), time.Since(start).Seconds(), res.Err != nil)
		}
		f.mu.Lock()
		delete(f.inFlight, url)
		f.mu.Unlock()
		f.deliver(func() { cb(res) })
	}()
	return true
}

// hostOf extracts the host:port for the Observe label; bad URLs (which
// fail in do anyway) collapse into one bucket.
func hostOf(rawURL string) string {
	if u, err := neturl.Parse(rawURL); err == nil && u.Host != "" {
		return u.Host
	}
	return "invalid"
}

func (f *Fetcher) do(url string, opts Options) Result {
	res := Result{URL: url}

	method := opts.Method
	if method == "" {
		method = "GET"
		if len(opts.Body) > 0 {
			method = "POST" // body presence implies POST, like the Perl
		}
	}
	var body io.Reader
	if len(opts.Body) > 0 {
		body = bytes.NewReader(opts.Body)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		res.Err = err
		return res
	}
	if opts.ContentType != "" {
		req.Header.Set("Content-Type", opts.ContentType)
	}
	for k, v := range opts.Headers {
		req.Header.Set(k, v)
	}
	if opts.User != "" || opts.Pass != "" {
		req.SetBasicAuth(opts.User, opts.Pass)
	}
	if opts.Sign != nil {
		if err := opts.Sign(req); err != nil {
			res.Err = fmt.Errorf("fetch: sign: %w", err)
			return res
		}
	}

	maxHops := opts.Redirects
	if maxHops == 0 {
		maxHops = defaultRedirects
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if opts.NoRedirect {
				return http.ErrUseLastResponse
			}
			if len(via) >= maxHops {
				return fmt.Errorf("fetch: more than %d redirects", maxHops)
			}
			return nil
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		res.Err = err
		return res
	}
	defer resp.Body.Close()

	res.Status = resp.StatusCode
	res.StatusLine = resp.Status
	res.Headers = resp.Header
	res.readBody(resp.Body, opts)
	return res
}

// readBody drains the response into Body or the Stream callback,
// stopping at the size limit.
func (r *Result) readBody(from io.Reader, opts Options) {
	var read int64
	buf := make([]byte, 32*1024)
	for {
		if opts.SizeLimit > 0 && read >= opts.SizeLimit {
			r.SizeLimitReached = true
			return
		}
		limit := int64(len(buf))
		if opts.SizeLimit > 0 && opts.SizeLimit-read < limit {
			limit = opts.SizeLimit - read
		}
		n, err := from.Read(buf[:limit])
		if n > 0 {
			read += int64(n)
			if opts.Stream != nil {
				opts.Stream(buf[:n])
			} else {
				r.Body = append(r.Body, buf[:n]...)
			}
		}
		if err == io.EOF {
			return
		}
		if err != nil {
			r.Err = err
			return
		}
	}
}
