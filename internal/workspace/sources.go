package workspace

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The web sources of a finished run (ADR-0001 D29). idp-host (its D49) records what a run's outward tools
// returned and sends it in the run's event stream; the workspace re-checks every field before it stores or
// shows it, because a title and an address come from a web page and the stream is the host's word, not the
// page's.
const (
	// MaxRunSources is how many sources an item keeps (the host's own limit).
	MaxRunSources = 100
	// MaxSourceNumber bounds n: idp-web numbers by caller and never wraps, so the bound is large (idp-host D49-2).
	MaxSourceNumber = 1_000_000
	// MaxUnverified bounds the numbers an answer may cite that are no source.
	MaxUnverified = 100
	// MaxCitationNumber is the largest unverified number: the host reports only one to three digits, since longer
	// numbers read as years and other figures (idp-host D49).
	MaxCitationNumber = 999
	// MaxSourceURL is the longest address kept, in bytes.
	MaxSourceURL = 2048
	// MaxSourceTitle is the longest title kept, in characters; a longer one is cut and ends with an ellipsis.
	MaxSourceTitle = 300
	// maxRawSources bounds the work done on one event, however large the stream's entry is.
	maxRawSources = 1000
)

// RunSource is one web page the run read or found (contract idp.telos.workspace 1.3.0). The title is untrusted
// text from the page: it is kept as data and shown as plain text only.
type RunSource struct {
	N         int        `json:"n"`
	Title     string     `json:"title"`
	URL       string     `json:"url"`
	FetchedAt *time.Time `json:"fetchedAt,omitempty"`
	SHA256    string     `json:"sha256,omitempty"`
	Cited     bool       `json:"cited"`
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// CleanSourceTitle makes a page's title safe to keep and to show as text: invalid UTF-8 is replaced, line breaks,
// tabs and the other space characters become one space, control and format characters (which include the
// bidirectional overrides and isolates, zero-width characters and the byte order mark) are removed, runs of
// spaces collapse, and a title longer than MaxSourceTitle characters is cut so that it ends with an ellipsis and
// is MaxSourceTitle characters long.
func CleanSourceTitle(s string) string {
	var b strings.Builder
	runes, space := 0, false
	more := false
	for _, r := range strings.ToValidUTF8(s, "�") {
		switch {
		case unicode.IsSpace(r) || r == ' ' || r == ' ':
			space = true
			continue
		case unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			continue
		}
		if runes >= MaxSourceTitle+1 { // enough to know it is too long
			more = true
			break
		}
		if space && runes > 0 {
			b.WriteByte(' ')
			runes++
		}
		space = false
		b.WriteRune(r)
		runes++
	}
	out := b.String()
	if runes > MaxSourceTitle || more {
		out = string([]rune(out)[:MaxSourceTitle-1]) + "…"
	}
	return out
}

// ValidSourceURL reports why an address is not one the workspace keeps: it must be an absolute http or https
// address of at most MaxSourceURL bytes, with a host, without user information, and without whitespace, control
// or format characters. The scheme is the whole defence against javascript: and data: links, so it is checked on
// the parsed address, never by a prefix of the text.
func ValidSourceURL(raw string) error {
	if raw == "" || len(raw) > MaxSourceURL {
		return fmt.Errorf("the address is empty or longer than %d bytes", MaxSourceURL)
	}
	if !utf8.ValidString(raw) {
		return fmt.Errorf("the address is not UTF-8")
	}
	for _, r := range raw {
		if unicode.IsSpace(r) || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == ' ' || r == ' ' {
			return fmt.Errorf("the address has whitespace or a control character")
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("the address does not parse")
	}
	if s := strings.ToLower(u.Scheme); s != "http" && s != "https" {
		return fmt.Errorf("the scheme is not http or https")
	}
	if u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return fmt.Errorf("the address has no host, or carries user information")
	}
	return nil
}

// Valid is what a source must be before it is stored: ParseRunSources produces only such sources, and the store
// checks them again, so no other producer's path can put an unchecked one into an item.
func (s RunSource) Valid() error {
	switch {
	case s.N < 1 || s.N > MaxSourceNumber:
		return fmt.Errorf("source number %d is out of range", s.N)
	case len(s.Title) > 4*MaxSourceTitle || utf8.RuneCountInString(s.Title) > MaxSourceTitle || CleanSourceTitle(s.Title) != s.Title:
		return fmt.Errorf("source %d: the title is not clean text of at most %d characters", s.N, MaxSourceTitle)
	case s.SHA256 != "" && !sha256Hex.MatchString(s.SHA256):
		return fmt.Errorf("source %d: sha256 is not 64 lowercase hex digits", s.N)
	}
	if err := ValidSourceURL(s.URL); err != nil {
		return fmt.Errorf("source %d: %v", s.N, err)
	}
	return nil
}

// ValidRunSources checks the sources and the unverified numbers of an item: numbers unique and ascending, at most
// MaxRunSources sources and MaxUnverified numbers.
func ValidRunSources(sources []RunSource, unverified []int) error {
	if len(sources) > MaxRunSources {
		return fmt.Errorf("at most %d sources", MaxRunSources)
	}
	prev := 0
	for _, s := range sources {
		if err := s.Valid(); err != nil {
			return err
		}
		if s.N <= prev {
			return fmt.Errorf("source numbers must be unique and ascending")
		}
		prev = s.N
	}
	if len(unverified) > MaxUnverified {
		return fmt.Errorf("at most %d unverified citations", MaxUnverified)
	}
	prev = 0
	for _, n := range unverified {
		if n < 1 || n > MaxCitationNumber || n <= prev {
			return fmt.Errorf("unverified citations must be unique, ascending numbers from 1 to %d", MaxCitationNumber)
		}
		prev = n
	}
	return nil
}

// ParseRunSources reads the value of the host's idp.host/run.sources event and keeps what is valid: an entry
// that is not wholly valid is dropped (never repaired, except that the title is cleaned and cut), a number given
// twice keeps its first entry, the rest are ordered by n and cut to MaxRunSources, and the unverified numbers
// are kept when they are integers from 1 to MaxCitationNumber (unique, ascending, at most MaxUnverified).
// dropped counts the entries left out. A value that is not an object yields nothing.
func ParseRunSources(raw json.RawMessage) (sources []RunSource, unverified []int, dropped int) {
	var v struct {
		Sources []json.RawMessage `json:"sources"`
		Unverif []json.RawMessage `json:"unverifiedCitations"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return nil, nil, 0
	}
	seen := map[int]bool{}
	for i, entry := range v.Sources {
		if i >= maxRawSources {
			dropped += len(v.Sources) - i
			break
		}
		s, ok := parseSource(entry)
		if !ok || seen[s.N] {
			dropped++
			continue
		}
		seen[s.N] = true
		sources = append(sources, s)
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].N < sources[j].N })
	if len(sources) > MaxRunSources {
		dropped += len(sources) - MaxRunSources
		sources = sources[:MaxRunSources]
	}
	have := map[int]bool{}
	for _, e := range v.Unverif {
		n, ok := wholeNumber(e)
		if !ok || n < 1 || n > MaxCitationNumber || have[n] || len(have) >= MaxUnverified {
			continue
		}
		have[n] = true
		unverified = append(unverified, n)
	}
	sort.Ints(unverified)
	return sources, unverified, dropped
}

// wholeNumber reads a JSON integer; 1.5, 1e2, "3" and null are not whole numbers.
func wholeNumber(raw json.RawMessage) (int, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > 9 {
		return 0, false
	}
	n := 0
	for _, c := range raw {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	if len(raw) > 1 && raw[0] == '0' {
		return 0, false
	}
	return n, true
}

func parseSource(raw json.RawMessage) (RunSource, bool) {
	var e struct {
		N         json.RawMessage `json:"n"`
		Title     json.RawMessage `json:"title"`
		URL       json.RawMessage `json:"url"`
		FetchedAt json.RawMessage `json:"fetchedAt"`
		SHA256    json.RawMessage `json:"sha256"`
		Cited     json.RawMessage `json:"cited"`
	}
	if json.Unmarshal(raw, &e) != nil {
		return RunSource{}, false
	}
	var s RunSource
	var ok bool
	if s.N, ok = wholeNumber(e.N); !ok || s.N < 1 || s.N > MaxSourceNumber {
		return RunSource{}, false
	}
	var title string
	if json.Unmarshal(e.Title, &title) != nil || e.Title == nil || string(e.Title) == "null" {
		return RunSource{}, false
	}
	s.Title = CleanSourceTitle(title)
	if json.Unmarshal(e.URL, &s.URL) != nil || ValidSourceURL(s.URL) != nil {
		return RunSource{}, false
	}
	if optional(e.FetchedAt) {
		var at time.Time
		if json.Unmarshal(e.FetchedAt, &at) != nil { // RFC 3339, as time.Time requires
			return RunSource{}, false
		}
		at = at.UTC()
		s.FetchedAt = &at
	}
	if optional(e.SHA256) {
		if json.Unmarshal(e.SHA256, &s.SHA256) != nil || !sha256Hex.MatchString(s.SHA256) {
			return RunSource{}, false
		}
	}
	if optional(e.Cited) && json.Unmarshal(e.Cited, &s.Cited) != nil {
		return RunSource{}, false
	}
	return s, true
}

// optional is true for a field that is present with a value: absent, null and "" all mean "not given".
func optional(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t != "" && t != "null" && t != `""`
}
