// Package blob keeps members' file contents in an S3-compatible object store (ADR-0001 D24): RustFS in the platform's
// compose and kind setups, AWS S3, Cloudflare R2, OCI ... in production. A small client of its own (path-style requests
// signed with AWS Signature Version 4) so that the workspace takes no SDK dependency for five calls: put, get, delete,
// list, and a check that the bucket answers.
package blob

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// ErrNotFound: no object under the key.
var ErrNotFound = errors.New("object not found")

// ErrTooLarge: the object is larger than the caller allows.
var ErrTooLarge = errors.New("object too large")

// S3 is a bucket of an S3-compatible store.
type S3 struct {
	Endpoint  *url.URL // scheme://host[:port], no path
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	// SSE asks the store to encrypt every object it keeps (SSE-S3, x-amz-server-side-encryption: AES256).
	SSE  bool
	HTTP *http.Client
	Now  func() time.Time
}

// Config is how the operator names the store; the keys come from files, never from the environment.
type Config struct {
	Endpoint, Bucket, Region     string
	AccessKeyFile, SecretKeyFile string
	CAFile                       string // extra trusted CA (PEM) for an https endpoint
	SSE                          bool
	Timeout                      time.Duration
}

// New checks the configuration and reads the keys.
func New(c Config) (*S3, error) {
	u, err := url.Parse(strings.TrimRight(c.Endpoint, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
		return nil, errors.New("the S3 endpoint must be http(s)://host[:port]")
	}
	u.Path = ""
	if !validBucket(c.Bucket) {
		return nil, errors.New("the S3 bucket name must be 3-63 lower-case letters, digits, '-' or '.'")
	}
	read := func(path, what string) (string, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("the S3 %s: %w", what, err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", fmt.Errorf("the S3 %s file is empty", what)
		}
		return v, nil
	}
	ak, err := read(c.AccessKeyFile, "access key")
	if err != nil {
		return nil, err
	}
	sk, err := read(c.SecretKeyFile, "secret key")
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	if c.CAFile != "" {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, err
		}
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("the S3 CA file holds no certificate")
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	if c.Timeout == 0 {
		c.Timeout = 2 * time.Minute
	}
	region := c.Region
	if region == "" {
		region = "us-east-1"
	}
	return &S3{Endpoint: u, Bucket: c.Bucket, Region: region, AccessKey: ak, SecretKey: sk, SSE: c.SSE,
		HTTP: &http.Client{Timeout: c.Timeout, Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func validBucket(b string) bool {
	if len(b) < 3 || len(b) > 63 || b[0] == '.' || b[0] == '-' || b[len(b)-1] == '.' || b[len(b)-1] == '-' {
		return false
	}
	for _, r := range b {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

// Put stores data under key, encrypted by the store when SSE is on.
func (s *S3) Put(ctx context.Context, key string, data []byte) error {
	h := http.Header{"Content-Type": {"application/octet-stream"}}
	if s.SSE {
		h.Set("X-Amz-Server-Side-Encryption", "AES256")
	}
	resp, err := s.do(ctx, http.MethodPut, key, nil, h, data)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return s.failure("put", resp)
	}
	return nil
}

// Get returns the object under key; ErrTooLarge when it is larger than max bytes.
func (s *S3) Get(ctx context.Context, key string, max int64) ([]byte, error) {
	resp, err := s.do(ctx, http.MethodGet, key, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, s.failure("get", resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, ErrTooLarge
	}
	return data, nil
}

// Head reports whether the object exists and whether the store says it keeps it encrypted.
func (s *S3) Head(ctx context.Context, key string) (exists, encrypted bool, err error) {
	resp, err := s.do(ctx, http.MethodHead, key, nil, nil, nil)
	if err != nil {
		return false, false, err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		return true, resp.Header.Get("X-Amz-Server-Side-Encryption") != "", nil
	case http.StatusNotFound:
		return false, false, nil
	}
	return false, false, s.failure("head", resp)
}

// Delete removes the object under key (an absent object is not an error).
func (s *S3) Delete(ctx context.Context, key string) error {
	resp, err := s.do(ctx, http.MethodDelete, key, nil, nil, nil)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return s.failure("delete", resp)
	}
	return nil
}

// Object is one entry of a listing.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// List returns the objects under prefix after the continuation token ("" = from the start), up to 1000, and the token
// of the next page ("" = no more).
func (s *S3) List(ctx context.Context, prefix, token string) ([]Object, string, error) {
	q := url.Values{"list-type": {"2"}, "prefix": {prefix}, "max-keys": {"1000"}}
	if token != "" {
		q.Set("continuation-token", token)
	}
	resp, err := s.do(ctx, http.MethodGet, "", q, nil, nil)
	if err != nil {
		return nil, "", err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, "", s.failure("list", resp)
	}
	var out struct {
		Contents []struct {
			Key          string `xml:"Key"`
			Size         int64  `xml:"Size"`
			LastModified string `xml:"LastModified"`
		} `xml:"Contents"`
		IsTruncated bool   `xml:"IsTruncated"`
		Next        string `xml:"NextContinuationToken"`
	}
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, "", fmt.Errorf("the object store's listing: %w", err)
	}
	objs := make([]Object, 0, len(out.Contents))
	for _, c := range out.Contents {
		t, _ := time.Parse(time.RFC3339Nano, c.LastModified)
		objs = append(objs, Object{Key: c.Key, Size: c.Size, LastModified: t})
	}
	next := ""
	if out.IsTruncated {
		next = out.Next
	}
	return objs, next, nil
}

// Check answers whether the bucket exists and the keys may list it.
func (s *S3) Check(ctx context.Context) error {
	resp, err := s.do(ctx, http.MethodGet, "", url.Values{"list-type": {"2"}, "max-keys": {"1"}}, nil, nil)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return s.failure("check", resp)
	}
	return nil
}

func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
}

// failure names the store's error code, never its whole answer.
func (s *S3) failure(op string, resp *http.Response) error {
	var e struct {
		Code string `xml:"Code"`
	}
	_ = xml.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&e)
	if e.Code == "" {
		e.Code = http.StatusText(resp.StatusCode)
	}
	return fmt.Errorf("object store %s: HTTP %d %s", op, resp.StatusCode, e.Code)
}

func (s *S3) do(ctx context.Context, method, key string, q url.Values, h http.Header, body []byte) (*http.Response, error) {
	u := *s.Endpoint
	u.Path = "/" + s.Bucket
	if key != "" {
		u.Path += "/" + key
	}
	u.RawPath = escapePath(u.Path)
	u.RawQuery = canonicalQuery(q)
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body == nil {
		req.Body, req.ContentLength = http.NoBody, 0
	}
	for k, v := range h {
		req.Header[k] = v
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	sum := sha256.Sum256(body)
	sign(req, hex.EncodeToString(sum[:]), s.AccessKey, s.SecretKey, s.Region, now().UTC())
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("object store %s: %w", strings.ToLower(method), err)
	}
	return resp, nil
}

// sign adds an AWS Signature Version 4 Authorization header (service s3) over the host, the x-amz-* headers and the
// content headers the request carries.
func sign(req *http.Request, payloadSHA, accessKey, secretKey, region string, t time.Time) {
	amzDate := t.Format("20060102T150405Z")
	day := amzDate[:8]
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadSHA)
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	signed := map[string]string{"host": host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-") || lk == "content-type" || lk == "content-md5" || lk == "range" {
			signed[lk] = strings.Join(trimAll(v), ",")
		}
	}
	names := make([]string, 0, len(signed))
	for k := range signed {
		names = append(names, k)
	}
	sort.Strings(names)
	var ch strings.Builder
	for _, k := range names {
		ch.WriteString(k + ":" + signed[k] + "\n")
	}
	signedHeaders := strings.Join(names, ";")
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonical := strings.Join([]string{req.Method, path, req.URL.RawQuery, ch.String(), signedHeaders, payloadSHA}, "\n")
	scope := day + "/" + region + "/s3/aws4_request"
	cr := sha256.Sum256([]byte(canonical))
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(cr[:])
	k := hmacSHA256([]byte("AWS4"+secretKey), day)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, "s3")
	k = hmacSHA256(k, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(k, toSign))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+accessKey+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+sig)
}

func trimAll(v []string) []string {
	out := make([]string, len(v))
	for i, s := range v {
		out[i] = strings.Join(strings.Fields(s), " ")
	}
	return out
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// escapePath URI-encodes each segment of a path the way SigV4 wants it (unreserved characters kept, '/' kept).
func escapePath(p string) string {
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s)
	}
	return strings.Join(segs, "/")
}

func uriEncode(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// canonicalQuery sorts and encodes the query as SigV4 does (and as the request sends it).
func canonicalQuery(q url.Values) string {
	if len(q) == 0 {
		return ""
	}
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
			parts = append(parts, uriEncode(k)+"="+uriEncode(v))
		}
	}
	return strings.Join(parts, "&")
}
