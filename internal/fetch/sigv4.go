package fetch

// Minimal AWS Signature Version 4 signer for the Bedrock backend. A
// self-contained implementation (verified against the official AWS
// SigV4 test-suite vectors) instead of pulling the full aws-sdk, in
// the same spirit as the rest of botje: small focused code, no big
// frameworks. Wire it into Options.Sign.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// AWSCredentials are the keys used to sign; Session is optional (STS).
type AWSCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
	Session         string
}

// SignV4 returns an Options.Sign hook that signs the request for the
// given service and region at signing time t (use time.Now in prod).
func SignV4(creds AWSCredentials, service, region string, t func() time.Time) func(*http.Request) error {
	if t == nil {
		t = time.Now
	}
	return func(req *http.Request) error {
		return signV4(req, creds, service, region, t().UTC())
	}
}

func signV4(req *http.Request, creds AWSCredentials, service, region string, now time.Time) error {
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")

	// hash the body (and restore it for the transport)
	var bodyBytes []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return err
		}
		req.Body.Close()
		bodyBytes = b
		req.Body = io.NopCloser(strings.NewReader(string(b)))
		req.ContentLength = int64(len(b))
	}
	payloadHash := sha256Hex(bodyBytes)

	req.Header.Set("X-Amz-Date", amzDate)
	if creds.Session != "" {
		req.Header.Set("X-Amz-Security-Token", creds.Session)
	}
	if req.Host == "" {
		req.Host = req.URL.Host
	}

	canonHeaders, signedHeaders := canonicalHeaders(req)
	canonicalRequest := strings.Join([]string{
		req.Method,
		canonicalURI(req.URL.EscapedPath()),
		canonicalQuery(req.URL.Query()),
		canonHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	signingKey := hmacSHA256(
		hmacSHA256(
			hmacSHA256(
				hmacSHA256([]byte("AWS4"+creds.SecretAccessKey), []byte(dateStamp)),
				[]byte(region)),
			[]byte(service)),
		[]byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		creds.AccessKeyID, scope, signedHeaders, signature))
	return nil
}

func canonicalHeaders(req *http.Request) (headers, signed string) {
	names := []string{"host"}
	values := map[string]string{"host": req.Host}
	for name, vs := range req.Header {
		l := strings.ToLower(name)
		if l == "authorization" {
			continue
		}
		names = append(names, l)
		values[l] = strings.Join(trimAll(vs), ",")
	}
	sort.Strings(names)
	var b strings.Builder
	for _, n := range names {
		b.WriteString(n)
		b.WriteString(":")
		b.WriteString(values[n])
		b.WriteString("\n")
	}
	return b.String(), strings.Join(names, ";")
}

func canonicalURI(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

func canonicalQuery(q map[string][]string) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, awsEscape(k)+"="+awsEscape(v))
		}
	}
	return strings.Join(parts, "&")
}

func trimAll(vs []string) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = strings.TrimSpace(v)
	}
	return out
}

// awsEscape is RFC3986 unreserved-only escaping (AWS canonical form).
func awsEscape(s string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.~"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}
