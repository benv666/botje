package fetch

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// newFetcher returns a fetcher whose callbacks run synchronously on a
// channel the test drains, mimicking the dispatcher loop.
func newFetcher() (*Fetcher, chan func()) {
	deliver := make(chan func(), 16)
	f := New(func(fn func()) { deliver <- fn })
	return f, deliver
}

// await runs delivered callbacks until result is set or timeout.
func await(t *testing.T, deliver chan func(), got *[]Result) Result {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case fn := <-deliver:
			fn()
			if len(*got) > 0 {
				return (*got)[0]
			}
		case <-deadline:
			t.Fatal("no result delivered")
		}
	}
}

func collect(got *[]Result) func(Result) {
	return func(r Result) { *got = append(*got, r) }
}

func TestSimpleGet(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "yes")
		fmt.Fprint(w, "hello body")
	}))
	defer srv.Close()

	f, deliver := newFetcher()
	var got []Result
	if !f.Fetch(srv.URL, Options{}, collect(&got)) {
		t.Fatal("Fetch refused")
	}
	r := await(t, deliver, &got)
	if r.Err != nil || string(r.Body) != "hello body" || r.Status != 200 {
		t.Fatalf("result = %+v", r)
	}
	if r.Headers.Get("X-Custom") != "yes" {
		t.Fatalf("headers = %+v", r.Headers)
	}
}

func TestSingleFlightPerURL(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		fmt.Fprint(w, "slow")
	}))
	defer srv.Close()

	f, deliver := newFetcher()
	var got []Result
	if !f.Fetch(srv.URL, Options{}, collect(&got)) {
		t.Fatal("first Fetch refused")
	}
	if f.Fetch(srv.URL, Options{}, collect(&got)) {
		t.Fatal("duplicate in-flight Fetch accepted, want single-flight refusal")
	}
	close(release)
	await(t, deliver, &got)
	// after completion the URL is fetchable again
	if !f.Fetch(srv.URL, Options{}, collect(&got)) {
		t.Fatal("Fetch refused after previous completed")
	}
}

func TestPostBody(t *testing.T) {
	var method, body, ctype atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		method.Store(r.Method)
		body.Store(string(b))
		ctype.Store(r.Header.Get("Content-Type"))
	}))
	defer srv.Close()

	f, deliver := newFetcher()
	var got []Result
	f.Fetch(srv.URL, Options{Body: []byte(`{"a":1}`), ContentType: "application/json"}, collect(&got))
	await(t, deliver, &got)
	if method.Load() != "POST" {
		t.Fatalf("method = %v, want POST implied by body", method.Load())
	}
	if body.Load() != `{"a":1}` || ctype.Load() != "application/json" {
		t.Fatalf("body/ctype = %v %v", body.Load(), ctype.Load())
	}
}

func TestBasicAuth(t *testing.T) {
	var user, pass atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, _ := r.BasicAuth()
		user.Store(u)
		pass.Store(p)
	}))
	defer srv.Close()

	f, deliver := newFetcher()
	var got []Result
	f.Fetch(srv.URL, Options{User: "benv", Pass: "geheim"}, collect(&got))
	await(t, deliver, &got)
	if user.Load() != "benv" || pass.Load() != "geheim" {
		t.Fatalf("auth = %v/%v", user.Load(), pass.Load())
	}
}

func TestRedirectFollowedAndCapped(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops := strings.Count(r.URL.Path, "/r")
		if hops < 3 {
			http.Redirect(w, r, srv.URL+r.URL.Path+"/r", http.StatusFound)
			return
		}
		fmt.Fprint(w, "landed")
	}))
	defer srv.Close()

	f, deliver := newFetcher()
	var got []Result
	f.Fetch(srv.URL, Options{}, collect(&got))
	if r := await(t, deliver, &got); r.Err != nil || string(r.Body) != "landed" {
		t.Fatalf("redirect follow: %+v", r)
	}

	// cap at 2 hops: the chain of 3 must error
	f2, deliver2 := newFetcher()
	var got2 []Result
	f2.Fetch(srv.URL, Options{Redirects: 2}, collect(&got2))
	if r := await(t, deliver2, &got2); r.Err == nil {
		t.Fatalf("redirect cap: expected error, got %+v", r)
	}
}

func TestNoRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer srv.Close()

	f, deliver := newFetcher()
	var got []Result
	f.Fetch(srv.URL, Options{NoRedirect: true}, collect(&got))
	r := await(t, deliver, &got)
	if r.Err != nil || r.Status != 302 {
		t.Fatalf("no-redirect fetch = %+v, want the 302 itself", r)
	}
}

func TestSizeLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(make([]byte, 100_000))
	}))
	defer srv.Close()

	f, deliver := newFetcher()
	var got []Result
	f.Fetch(srv.URL, Options{SizeLimit: 10_000}, collect(&got))
	r := await(t, deliver, &got)
	if !r.SizeLimitReached {
		t.Fatalf("SizeLimitReached not set: %+v", r)
	}
	if len(r.Body) > 10_000 {
		t.Fatalf("body = %d bytes, want <= limit", len(r.Body))
	}
}

func TestTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer srv.Close()

	f, deliver := newFetcher()
	var got []Result
	start := time.Now()
	f.Fetch(srv.URL, Options{Timeout: 200 * time.Millisecond}, collect(&got))
	r := await(t, deliver, &got)
	if r.Err == nil {
		t.Fatalf("expected timeout error, got %+v", r)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("timeout took far too long")
	}
}

func TestStreaming(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		fmt.Fprint(w, "chunk1")
		fl.Flush()
		fmt.Fprint(w, "chunk2")
	}))
	defer srv.Close()

	f, deliver := newFetcher()
	var got []Result
	var streamed atomic.Value
	streamed.Store("")
	f.Fetch(srv.URL, Options{
		Stream: func(chunk []byte) { streamed.Store(streamed.Load().(string) + string(chunk)) },
	}, collect(&got))
	r := await(t, deliver, &got)
	if streamed.Load() != "chunk1chunk2" {
		t.Fatalf("streamed = %q", streamed.Load())
	}
	if len(r.Body) != 0 {
		t.Fatalf("body should stay empty in streaming mode, got %d bytes", len(r.Body))
	}
}

func TestConnectError(t *testing.T) {
	f, deliver := newFetcher()
	var got []Result
	if !f.Fetch("http://127.0.0.1:1/nope", Options{}, collect(&got)) {
		t.Fatal("Fetch refused")
	}
	if r := await(t, deliver, &got); r.Err == nil {
		t.Fatal("expected connect error")
	}
}
