package workspace_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"idp-workspace/internal/workspace"
)

const goodHash = "5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d5d"

func parse(t *testing.T, raw string) ([]workspace.RunSource, []int, int) {
	t.Helper()
	return workspace.ParseRunSources(json.RawMessage(raw))
}

func TestAValidSourceIsKeptAndTheListIsOrderedByNumber(t *testing.T) {
	got, unverified, dropped := parse(t, `{"sources":[
		{"n":4,"title":"Four","url":"HTTP://b.example/x?q=1#f","cited":true},
		{"n":2,"title":"Two","url":"https://a.example/","fetchedAt":"2026-11-19T10:00:00+08:00","sha256":"`+goodHash+`","cited":false,"excerpt":"never kept"}],
		"unverifiedCitations":[9,7,7],"droppedInvalid":0,"conflicts":0,"checked":true}`)
	if dropped != 0 || len(got) != 2 || got[0].N != 2 || got[1].N != 4 || !got[1].Cited || got[0].SHA256 != goodHash {
		t.Fatalf("%+v %d", got, dropped)
	}
	if got[0].FetchedAt == nil || got[0].FetchedAt.Location().String() != "UTC" || got[0].FetchedAt.Hour() != 2 {
		t.Fatalf("fetchedAt is kept in UTC: %v", got[0].FetchedAt)
	}
	if fmt.Sprint(unverified) != "[7 9]" {
		t.Fatalf("unverified numbers are unique and ascending: %v", unverified)
	}
	if d, _, _ := parse(t, `{"sources":[{"n":1,"title":"a\u202eb\n c","url":"https://a.example/","cited":false}]}`); len(d) != 1 || d[0].Title != "ab c" {
		t.Fatalf("the title is cleaned when it is read: %+v", d)
	}
	if raw, _ := json.Marshal(got); strings.Contains(string(raw), "excerpt") || strings.Contains(string(raw), "never kept") {
		t.Fatalf("a field of the contract's is kept, nothing else: %s", raw)
	}
}

func TestEachKindOfInvalidSourceIsDroppedAndTheOthersStay(t *testing.T) {
	bad := map[string]string{
		"n is zero":                `{"n":0,"title":"t","url":"https://a.example/","cited":false}`,
		"n is negative":            `{"n":-1,"title":"t","url":"https://a.example/","cited":false}`,
		"n is a fraction":          `{"n":1.5,"title":"t","url":"https://a.example/","cited":false}`,
		"n is a string":            `{"n":"3","title":"t","url":"https://a.example/","cited":false}`,
		"n is above the bound":     `{"n":1000001,"title":"t","url":"https://a.example/","cited":false}`,
		"n is missing":             `{"title":"t","url":"https://a.example/","cited":false}`,
		"title is a number":        `{"n":5,"title":7,"url":"https://a.example/","cited":false}`,
		"title is missing":         `{"n":5,"url":"https://a.example/","cited":false}`,
		"url is a javascript link": `{"n":5,"title":"t","url":"javascript:alert(1)","cited":false}`,
		"url is a data link":       `{"n":5,"title":"t","url":"data:text/html,<script>1</script>","cited":false}`,
		"url is ftp":               `{"n":5,"title":"t","url":"ftp://a.example/f","cited":false}`,
		"url has no scheme":        `{"n":5,"title":"t","url":"a.example/x","cited":false}`,
		"url has no host":          `{"n":5,"title":"t","url":"https:///x","cited":false}`,
		"url has user information": `{"n":5,"title":"t","url":"https://user:pw@a.example/","cited":false}`,
		"url has a space":          `{"n":5,"title":"t","url":"https://a.example/a b","cited":false}`,
		"url has a newline":        `{"n":5,"title":"t","url":"https://a.example/\n","cited":false}`,
		"url has a bidi override":  `{"n":5,"title":"t","url":"https://a.example/‮","cited":false}`,
		"url is too long":          `{"n":5,"title":"t","url":"https://a.example/` + strings.Repeat("a", 2048) + `","cited":false}`,
		"url is an object":         `{"n":5,"title":"t","url":{"x":1},"cited":false}`,
		"fetchedAt is no time":     `{"n":5,"title":"t","url":"https://a.example/","fetchedAt":"yesterday","cited":false}`,
		"sha256 is short":          `{"n":5,"title":"t","url":"https://a.example/","sha256":"abcd","cited":false}`,
		"sha256 is uppercase":      `{"n":5,"title":"t","url":"https://a.example/","sha256":"` + strings.ToUpper(goodHash) + `","cited":false}`,
		"cited is a string":        `{"n":5,"title":"t","url":"https://a.example/","cited":"yes"}`,
		"the entry is a string":    `"https://a.example/"`,
		"the entry is null":        `null`,
		"the entry is a list":      `[1,2]`,
	}
	for name, entry := range bad {
		got, _, dropped := parse(t, `{"sources":[{"n":1,"title":"kept","url":"https://ok.example/","cited":true},`+entry+`]}`)
		if len(got) != 1 || got[0].N != 1 || dropped != 1 {
			t.Errorf("%s: %+v dropped %d", name, got, dropped)
		}
	}
	// a number given twice keeps the first; null and empty optional fields mean "not given"
	got, _, dropped := parse(t, `{"sources":[{"n":3,"title":"first","url":"https://a.example/1","cited":true},{"n":3,"title":"second","url":"https://a.example/2","cited":false},
		{"n":4,"title":"","url":"https://a.example/4","fetchedAt":null,"sha256":"","cited":false}]}`)
	if len(got) != 2 || got[0].Title != "first" || got[1].FetchedAt != nil || got[1].SHA256 != "" || dropped != 1 {
		t.Fatalf("%+v %d", got, dropped)
	}
	for _, v := range []string{``, `null`, `[]`, `"x"`, `{"sources":"x"}`, `{"sources":{}}`, `{}`} {
		if s, u, _ := parse(t, v); s != nil || u != nil {
			t.Errorf("%q leaves nothing: %v %v", v, s, u)
		}
	}
}

func TestNoMoreThanAHundredSourcesAreKeptWhateverTheHostSends(t *testing.T) {
	var entries []string
	for n := 150; n >= 1; n-- { // out of order: the first hundred by number are the ones kept
		entries = append(entries, fmt.Sprintf(`{"n":%d,"title":"t%d","url":"https://a.example/%d","cited":false}`, n, n, n))
	}
	got, _, dropped := parse(t, `{"sources":[`+strings.Join(entries, ",")+`]}`)
	if len(got) != 100 || got[0].N != 1 || got[99].N != 100 || dropped != 50 {
		t.Fatalf("%d kept, first %d last %d, dropped %d", len(got), got[0].N, got[len(got)-1].N, dropped)
	}
	// the work on one event is bounded too
	entries = entries[:0]
	for n := 1; n <= 5000; n++ {
		entries = append(entries, `{"n":0}`)
	}
	if got, _, dropped := parse(t, `{"sources":[`+strings.Join(entries, ",")+`]}`); len(got) != 0 || dropped != 5000 {
		t.Fatalf("%d %d", len(got), dropped)
	}
}

func TestUnverifiedNumbersAreIntegersFromOneToNineHundredNinetyNine(t *testing.T) {
	_, u, _ := parse(t, `{"unverifiedCitations":[5,0,-2,1000,2.5,"7",null,999,1,5]}`)
	if fmt.Sprint(u) != "[1 5 999]" {
		t.Fatalf("%v", u)
	}
	var many []string
	for n := 1; n <= 300; n++ {
		many = append(many, fmt.Sprint(n))
	}
	if _, u, _ := parse(t, `{"unverifiedCitations":[`+strings.Join(many, ",")+`]}`); len(u) != 100 {
		t.Fatalf("at most 100 numbers: %d", len(u))
	}
}

func TestATitleIsCleanedOfControlAndBidiCharactersAndCut(t *testing.T) {
	for in, want := range map[string]string{
		"plain":                       "plain",
		"  two   words \n and\ttabs ": "two words and tabs",
		"a\x00b\x07c\x1b[31m":         "abc[31m",
		"safe‮txt.exe":                "safetxt.exe",
		"x⁦y⁩z​w\ufeff":               "xyzw",
		"line break here":             "line break here",
		"bad \xff utf8":               "bad � utf8",
		"<script>alert(1)</script>":   "<script>alert(1)</script>", // markup is not removed: it is escaped where it is shown
		"":                            "",
		"\n\t ":                       "",
	} {
		if got := workspace.CleanSourceTitle(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
	exactly := strings.Repeat("字", workspace.MaxSourceTitle)
	if got := workspace.CleanSourceTitle(exactly); got != exactly {
		t.Errorf("300 characters stay whole")
	}
	got := workspace.CleanSourceTitle(exactly + "字")
	if r := []rune(got); len(r) != workspace.MaxSourceTitle || r[len(r)-1] != '…' {
		t.Errorf("a longer title is cut to %d characters and ends with an ellipsis: %d %q", workspace.MaxSourceTitle, len(r), got[len(got)-6:])
	}
	if got := workspace.CleanSourceTitle(strings.Repeat("x ", 1<<20)); len([]rune(got)) != workspace.MaxSourceTitle {
		t.Errorf("a huge title is cut: %d", len(got))
	}
}

func TestTheStoreCheckRefusesWhatTheFollowerWouldNotKeep(t *testing.T) {
	ok := workspace.RunSource{N: 1, Title: "t", URL: "https://a.example/"}
	for name, s := range map[string]workspace.RunSource{
		"javascript":  {N: 1, Title: "t", URL: "javascript:alert(1)"},
		"dirty":       {N: 1, Title: "a‮b", URL: "https://a.example/"},
		"number zero": {N: 0, Title: "t", URL: "https://a.example/"},
	} {
		if workspace.ValidRunSources([]workspace.RunSource{s}, nil) == nil {
			t.Errorf("%s is stored", name)
		}
	}
	if workspace.ValidRunSources([]workspace.RunSource{ok, ok}, nil) == nil {
		t.Error("numbers must be unique")
	}
	if err := workspace.ValidRunSources([]workspace.RunSource{ok}, []int{2}); err != nil {
		t.Error(err)
	}
}
