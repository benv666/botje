package fetch

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// The canonical AWS SigV4 test-suite "get-vanilla" vector, with the
// documented expected Authorization header.
func TestSignV4GetVanilla(t *testing.T) {
	creds := AWSCredentials{
		AccessKeyID:     "AKIDEXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
	}
	req, _ := http.NewRequest("GET", "https://example.amazonaws.com/", nil)
	req.Host = "example.amazonaws.com"

	signAt := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	if err := signV4(req, creds, "service", "us-east-1", signAt); err != nil {
		t.Fatal(err)
	}

	// the documented get-vanilla answer
	want := "AWS4-HMAC-SHA256 " +
		"Credential=AKIDEXAMPLE/20150830/us-east-1/service/aws4_request, " +
		"SignedHeaders=host;x-amz-date, " +
		"Signature=5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization =\n%s\nwant\n%s", got, want)
	}
}

// A POST with a JSON body signs the payload hash and preserves the body
// for the transport.
func TestSignV4PostBody(t *testing.T) {
	creds := AWSCredentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret"}
	body := `{"hello":"world"}`
	req, _ := http.NewRequest("POST", "https://bedrock-runtime.us-east-1.amazonaws.com/model/x/invoke",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	signAt := time.Date(2024, 4, 12, 11, 34, 38, 0, time.UTC)
	if err := signV4(req, creds, "bedrock", "us-east-1", signAt); err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Authorization") == "" {
		t.Fatal("no Authorization header")
	}
	if req.Header.Get("X-Amz-Date") != "20240412T113438Z" {
		t.Fatalf("X-Amz-Date = %q", req.Header.Get("X-Amz-Date"))
	}
	// body still readable by the transport
	got := make([]byte, len(body))
	req.Body.Read(got)
	if string(got) != body {
		t.Fatalf("body = %q, want preserved", got)
	}
}

func TestSignV4SessionToken(t *testing.T) {
	creds := AWSCredentials{AccessKeyID: "A", SecretAccessKey: "s", Session: "tok123"}
	req, _ := http.NewRequest("GET", "https://x.amazonaws.com/", nil)
	signV4(req, creds, "svc", "eu-west-1", time.Now().UTC())
	if req.Header.Get("X-Amz-Security-Token") != "tok123" {
		t.Fatal("session token not set")
	}
	if !strings.Contains(req.Header.Get("Authorization"), "x-amz-security-token") {
		t.Fatal("session token not in signed headers")
	}
}
