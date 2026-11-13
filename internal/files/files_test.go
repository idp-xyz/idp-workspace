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
	if err != nil || got != (Ticket{Tenant: "t1", Caller: "9f0c-bob-uuid", Owner: "bob", Kind: KindRead, File: "f1", Run: "ws_run"}) {
		t.Fatalf("%+v %v", got, err)
	}
	w, _ := tk.Write("t1", "9f0c-bob-uuid", "bob", "w1", "ws_run", 5, 1024)
	if got, err := tk.Open(w); err != nil || got.Kind != KindWrite || got.Workspace != "w1" || got.MaxFiles != 5 || got.MaxBytes != 1024 {
		t.Fatalf("%+v %v", got, err)
	}
	// another workspace's key, another audience, tampering, expiry
	other, _ := apptoken.Generate("https://ws.example")
	forged, _ := (&Ticketer{Signer: other}).Read("t1", "bob", "bob", "f1", "ws_run")
	appToken, _, _ := s.Issue(apptoken.Claims{Subject: "bob", Tenant: "t1", App: "orders"})
	noRun, _ := s.Sign(jwt.MapClaims{"iss": s.Issuer, "aud": Audience, "exp": now.Add(time.Minute).Unix(), "tenant_id": "t1", "sub": "bob", "owner": "bob", "kind": KindRead, "file": "f1"})
	noOwner, _ := s.Sign(jwt.MapClaims{"iss": s.Issuer, "aud": Audience, "exp": now.Add(time.Minute).Unix(), "tenant_id": "t1", "sub": "bob", "kind": KindRead, "file": "f1", "run": "r"})
	parts := strings.Split(r, ".")
	tampered := parts[0] + "." + strings.TrimRight(parts[1], "A") + "B." + parts[2]
	for name, tok := range map[string]string{"forged": forged, "app token": appToken, "no run": noRun, "no owner": noOwner, "tampered": tampered, "garbage": "a.b.c"} {
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
