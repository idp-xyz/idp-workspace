package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// AuthExchange trades the caller's token for one issued to the downstream (RFC 8693, ADR-0001 D16).
const AuthExchange = "exchange"

// Exchanger performs token exchange with the workspace as the requesting client, caching each result
// per (subject token, audience) until shortly before it expires.
type Exchanger struct {
	TokenURL     string
	ClientID     string
	ClientSecret func() (string, error)
	Audience     string
	// SessionBound requests a refresh token (bound to the member's session) and keeps only the access token (D25).
	SessionBound bool
	HTTP         *http.Client
	Now          func() time.Time

	mu    sync.Mutex
	cache map[string]cached
}

type cached struct {
	token string
	until time.Time
}

func (e *Exchanger) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Exchange returns a token for the audience on behalf of subjectToken's holder.
func (e *Exchanger) Exchange(ctx context.Context, subjectToken string) (string, error) {
	if subjectToken == "" {
		return "", errors.New("no caller token to exchange")
	}
	sum := sha256.Sum256([]byte(subjectToken))
	key := hex.EncodeToString(sum[:]) + "|" + e.Audience
	e.mu.Lock()
	if c, ok := e.cache[key]; ok && e.now().Before(c.until) {
		e.mu.Unlock()
		return c.token, nil
	}
	e.mu.Unlock()
	secret, err := e.ClientSecret()
	if err != nil {
		return "", fmt.Errorf("client secret: %w", err)
	}
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	form.Set("subject_token", subjectToken)
	form.Set("subject_token_type", "urn:ietf:params:oauth:token-type:access_token")
	if e.SessionBound { // the refresh token is not kept: asking for it binds the exchange to the member's session
		form.Set("requested_token_type", "urn:ietf:params:oauth:token-type:refresh_token")
	} else {
		form.Set("requested_token_type", "urn:ietf:params:oauth:token-type:access_token")
	}
	form.Set("audience", e.Audience)
	form.Set("client_id", e.ClientID)
	form.Set("client_secret", secret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	hc := e.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", errors.New("token endpoint unreachable")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		var er struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &er)
		if er.Error == "" {
			er.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return "", fmt.Errorf("token exchange refused: %s", er.Error)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if json.Unmarshal(body, &out) != nil || out.AccessToken == "" {
		return "", errors.New("token endpoint returned no access token")
	}
	life := time.Duration(out.ExpiresIn) * time.Second
	if life <= 0 {
		life = 5 * time.Minute
	}
	e.mu.Lock()
	if e.cache == nil {
		e.cache = map[string]cached{}
	}
	for k, c := range e.cache { // drop what has expired so the cache does not grow without bound
		if !e.now().Before(c.until) {
			delete(e.cache, k)
		}
	}
	e.cache[key] = cached{token: out.AccessToken, until: e.now().Add(life - 30*time.Second)}
	e.mu.Unlock()
	return out.AccessToken, nil
}
