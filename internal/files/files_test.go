package files

import (
	"archive/zip"
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"idp-workspace/internal/apptoken"
)

func TestTicketsSayWhatTheyAllow(t *testing.T) {
	s, _ := apptoken.Generate("https://ws.example")
	now := time.Now()
	tk := &Ticketer{Signer: s, TTL: time.Minute, Now: func() time.Time { return now }}
	r, err := tk.Read("t1", "9f0c-bob-uuid", "bob", "f1", "ws_run")
	if err != nil {
		t.Fatal(err)
	}
	got, err := tk.Open(r)
	if err != nil || got.ID == "" || !got.Expires.Equal(now.Add(time.Minute).Truncate(time.Second)) {
		t.Fatalf("a ticket has an id and an expiry of its own (D27): %+v %v", got, err)
	}
	id := got.ID
	got.ID, got.Expires = "", time.Time{}
	if got != (Ticket{Tenant: "t1", Caller: "9f0c-bob-uuid", Owner: "bob", Kind: KindRead, File: "f1", Run: "ws_run"}) {
		t.Fatalf("%+v", got)
	}
	if again, _ := tk.Read("t1", "9f0c-bob-uuid", "bob", "f1", "ws_run"); again == r {
		t.Fatal("two tickets for the same file and run are the same ticket")
	} else if o, _ := tk.Open(again); o.ID == id {
		t.Fatal("two tickets share an id")
	}
	w, _ := tk.Write("t1", "9f0c-bob-uuid", "bob", "w1", "ws_run", 5, 1024, "")
	if got, err := tk.Open(w); err != nil || got.Kind != KindWrite || got.Workspace != "w1" || got.MaxFiles != 5 || got.MaxBytes != 1024 || got.Sensitivity != "" {
		t.Fatalf("%+v %v", got, err)
	}
	// D28: a write ticket says how what it delivers is classified, when that is above internal
	for level, want := range map[string]string{"internal": "", "public": "public", "confidential": "confidential", "restricted": "restricted"} {
		w, err := tk.Write("t1", "bob", "bob", "w1", "ws_run", 5, 1024, level)
		if got, oerr := tk.Open(w); err != nil || oerr != nil || got.Sensitivity != want {
			t.Fatalf("%s: %+v %v %v", level, got, err, oerr)
		}
	}
	if _, err := tk.Write("t1", "bob", "bob", "w1", "ws_run", 5, 1024, "secret"); err == nil {
		t.Fatal("a ticket with an unknown level was signed")
	}
	for _, c := range [][3]string{{"", "", "internal"}, {"", "public", "internal"}, {"public", "public", "public"}, {"public", "internal", "internal"}, {"internal", "confidential", "confidential"},
		{"restricted", "confidential", "restricted"}, {"confidential", "", "confidential"}, {"bogus", "public", "internal"}} {
		if got := HigherSensitivity(c[0], c[1]); got != c[2] {
			t.Errorf("the higher of %q and %q is %q", c[0], c[1], got)
		}
	}
	// another workspace's key, another audience, tampering, expiry
	other, _ := apptoken.Generate("https://ws.example")
	forged, _ := (&Ticketer{Signer: other}).Read("t1", "bob", "bob", "f1", "ws_run")
	appToken, _, _ := s.Issue(apptoken.Claims{Subject: "bob", Tenant: "t1", App: "orders"})
	noRun, _ := s.Sign(jwt.MapClaims{"iss": s.Issuer, "aud": Audience, "exp": now.Add(time.Minute).Unix(), "tenant_id": "t1", "sub": "bob", "owner": "bob", "kind": KindRead, "file": "f1"})
	noOwner, _ := s.Sign(jwt.MapClaims{"iss": s.Issuer, "aud": Audience, "exp": now.Add(time.Minute).Unix(), "tenant_id": "t1", "sub": "bob", "kind": KindRead, "file": "f1", "run": "r"})
	noID, _ := s.Sign(jwt.MapClaims{"iss": s.Issuer, "aud": Audience, "exp": now.Add(time.Minute).Unix(), "tenant_id": "t1", "sub": "bob", "owner": "bob", "kind": KindRead, "file": "f1", "run": "r"})
	parts := strings.Split(r, ".")
	tampered := parts[0] + "." + strings.TrimRight(parts[1], "A") + "B." + parts[2]
	for name, tok := range map[string]string{"forged": forged, "app token": appToken, "no run": noRun, "no owner": noOwner, "no id": noID, "tampered": tampered, "garbage": "a.b.c"} {
		if _, err := tk.Open(tok); err == nil {
			t.Errorf("%s ticket opened", name)
		}
	}
	tk.Now = func() time.Time { return now.Add(2 * time.Minute) }
	s.Now = tk.Now
	if _, err := tk.Open(r); err == nil {
		t.Error("an expired ticket opened")
	}
}

func docx(t *testing.T, parts map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for n, c := range parts {
		w, _ := z.Create(n)
		_, _ = w.Write([]byte(c))
	}
	_ = z.Close()
	return b.Bytes()
}

func TestCheckAndNames(t *testing.T) {
	if err := Check("报价单.docx", docx(t, map[string]string{"word/document.xml": "<w/>"})); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{"a.docm": docx(t, nil), "a.docx": []byte("text"), "b.docx": docx(t, map[string]string{"word/vbaProject.bin": "x"}),
		"a.exe": []byte("MZ"), "a.txt": []byte("a\x00b"), "a.pdf": []byte("PK"), ".docx": docx(t, nil)} {
		if Check(name, data) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if MediaType("x.XLSX") != "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" || MediaType("x.exe") != "" {
		t.Fatal("media types")
	}
	for in, want := range map[string]string{"../../etc/passwd": "passwd", `C:\docs\报价单.docx`: "报价单.docx", "a\nb.txt": "ab.txt", "..": "", " .env ": "env", "a\u2028b.md": "ab.md"} {
		if got := SafeName(in); got != want {
			t.Errorf("SafeName(%q) = %q, want %q", in, got, want)
		}
	}
	long := SafeName(strings.Repeat("长", 100) + ".docx")
	if len(long) > 200 || !strings.HasSuffix(long, ".docx") || !strings.HasPrefix(long, "长") {
		t.Fatalf("%q", long)
	}
	for _, r := range long[:len(long)-5] {
		if r == 0xfffd {
			t.Fatal("a character was cut in half")
		}
	}
}
