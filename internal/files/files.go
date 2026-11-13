// Package files is what the workspace knows about a member's files (ADR-0001 D23; platform ADR-0002 F1-F4): which
// types it keeps and how it checks them, and the tickets it signs so that a run can read a file the member attached or
// deliver one to them. A ticket is the only credential the sandbox presents; it names the member, the run and the file
// (read) or the run and its limits (write), and expires.
package files

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"

	"idp-workspace/internal/apptoken"
)

// Audience of the tickets.
const Audience = "idp-workspace-files"

// Ticket kinds.
const (
	KindRead  = "read"
	KindWrite = "write"
)

// Ticketer signs and opens tickets with the workspace's key (the app tokens' key, D15).
type Ticketer struct {
	Signer *apptoken.Signer
	TTL    time.Duration
	Now    func() time.Time
}

// Ticket is an opened ticket. Caller is the token subject the platform forwards for the member (the ticket's sub, which
// idp-sandbox compares with the caller of the tool call); Owner is the member as the workspace names them (the files' owner).
type Ticket struct {
	Tenant    string
	Caller    string
	Owner     string
	Kind      string
	File      string // read: the file
	Run       string
	Workspace string // write: where a delivered file belongs
	MaxFiles  int    // write
	MaxBytes  int64  // write: per file
}

func (t *Ticketer) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *Ticketer) sign(c jwt.MapClaims) (string, error) {
	now := t.now()
	ttl := t.TTL
	if ttl == 0 {
		ttl = time.Hour
	}
	jti := make([]byte, 12)
	_, _ = rand.Read(jti)
	c["iss"], c["aud"], c["iat"], c["nbf"], c["exp"], c["jti"] = t.Signer.Issuer, Audience, now.Unix(), now.Unix(), now.Add(ttl).Unix(), base64.RawURLEncoding.EncodeToString(jti)
	return t.Signer.Sign(c)
}

// Read signs a ticket that lets run (acting for caller) read one of owner's files.
func (t *Ticketer) Read(tenant, caller, owner, file, run string) (string, error) {
	return t.sign(jwt.MapClaims{"tenant_id": tenant, "sub": caller, "owner": owner, "kind": KindRead, "file": file, "run": run})
}

// Write signs a ticket that lets run (acting for caller) deliver up to maxFiles files of up to maxBytes each to owner in
// workspace.
func (t *Ticketer) Write(tenant, caller, owner, workspace, run string, maxFiles int, maxBytes int64) (string, error) {
	return t.sign(jwt.MapClaims{"tenant_id": tenant, "sub": caller, "owner": owner, "kind": KindWrite, "run": run, "workspace": workspace, "max_files": maxFiles, "max_bytes": maxBytes})
}

// ErrTicket: the ticket is not one this workspace signed, is expired, or is incomplete.
var ErrTicket = errors.New("invalid or expired file ticket")

// Open verifies a ticket and returns what it allows.
func (t *Ticketer) Open(token string) (Ticket, error) {
	c, err := t.Signer.Verify(token, Audience)
	if err != nil {
		return Ticket{}, ErrTicket
	}
	s := func(k string) string { v, _ := c[k].(string); return v }
	n := func(k string) int64 { v, _ := c[k].(float64); return int64(v) }
	out := Ticket{Tenant: s("tenant_id"), Caller: s("sub"), Owner: s("owner"), Kind: s("kind"), File: s("file"), Run: s("run"), Workspace: s("workspace"), MaxFiles: int(n("max_files")), MaxBytes: n("max_bytes")}
	switch {
	case out.Tenant == "" || out.Caller == "" || out.Owner == "" || out.Run == "":
		return Ticket{}, ErrTicket
	case out.Kind == KindRead && out.File != "":
	case out.Kind == KindWrite && out.Workspace != "" && out.MaxFiles > 0 && out.MaxBytes > 0:
	default:
		return Ticket{}, ErrTicket
	}
	return out, nil
}

// Types the platform keeps, by extension, with what their content must begin with (the same list as idp-sandbox's).
var types = map[string]struct {
	mediaType string
	magic     []string // one of these prefixes; empty = text
	ooxml     bool
}{
	".docx": {"application/vnd.openxmlformats-officedocument.wordprocessingml.document", []string{"PK\x03\x04"}, true},
	".xlsx": {"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", []string{"PK\x03\x04"}, true},
	".pptx": {"application/vnd.openxmlformats-officedocument.presentationml.presentation", []string{"PK\x03\x04"}, true},
	".pdf":  {"application/pdf", []string{"%PDF-"}, false},
	".png":  {"image/png", []string{"\x89PNG\r\n\x1a\n"}, false},
	".jpg":  {"image/jpeg", []string{"\xff\xd8\xff"}, false},
	".jpeg": {"image/jpeg", []string{"\xff\xd8\xff"}, false},
	".csv":  {"text/csv", nil, false},
	".txt":  {"text/plain", nil, false},
	".md":   {"text/markdown", nil, false},
}

// Accepted lists the extensions, for messages and the shell's file picker.
const Accepted = ".docx .xlsx .pptx .pdf .png .jpg .jpeg .csv .txt .md"

const macroExtensions = ".docm .xlsm .pptm .dotm .xltm .potm .ppsm .xlsb .xlam .ppam"

// MediaType returns the media type of a name the platform accepts ("" when it does not).
func MediaType(name string) string { return types[strings.ToLower(path.Ext(name))].mediaType }

// Check refuses what the platform does not keep: a type outside the list, a macro-enabled format, content that is not
// what its name says, a zip bomb, a part whose name climbs out, or a macro project inside a .docx/.xlsx/.pptx.
func Check(name string, data []byte) error {
	ext := strings.ToLower(path.Ext(name))
	if ext != "" && strings.Contains(" "+macroExtensions+" ", " "+ext+" ") {
		return fmt.Errorf("%s files can carry macros and are not accepted", ext)
	}
	t, ok := types[ext]
	if !ok {
		return fmt.Errorf("only these file types are accepted: %s", Accepted)
	}
	if t.magic == nil {
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return fmt.Errorf("%s is not a text file", name)
		}
		return nil
	}
	okMagic := false
	for _, m := range t.magic {
		okMagic = okMagic || bytes.HasPrefix(data, []byte(m))
	}
	if !okMagic {
		return fmt.Errorf("%s is not a %s file", name, ext)
	}
	if !t.ooxml {
		return nil
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("%s is not a valid %s package", name, ext)
	}
	if len(zr.File) > 10000 {
		return fmt.Errorf("%s has too many parts", name)
	}
	var unpacked uint64
	for _, f := range zr.File {
		n := strings.ToLower(f.Name)
		if len(n) > 512 || strings.HasPrefix(n, "/") || strings.Contains(n, "\\") || strings.Contains("/"+n+"/", "/../") {
			return fmt.Errorf("%s has a part with an unsafe name", name)
		}
		if strings.Contains(n, "vbaproject.bin") || strings.HasSuffix(n, ".bin") && strings.Contains(n, "vba") {
			return fmt.Errorf("%s carries a macro project and is not accepted", name)
		}
		unpacked += f.UncompressedSize64
		if f.CompressedSize64 > 0 && f.UncompressedSize64/f.CompressedSize64 > 200 && f.UncompressedSize64 > 1<<20 {
			return fmt.Errorf("%s has a part that unpacks to far more than its size", name)
		}
	}
	if unpacked > 256<<20 {
		return fmt.Errorf("%s unpacks to more than 256 MiB", name)
	}
	return nil
}

// SafeName keeps a file's name to one path element of printable characters (at most 200 bytes); "" when nothing is left.
func SafeName(name string) string {
	name = path.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		if r >= 0x20 && r != 0x7f && !(r >= 0x80 && r < 0xa0) && r != 0x2028 && r != 0x2029 && !strings.ContainsRune(`/:*?"<>|`, r) {
			b.WriteRune(r)
		}
	}
	out := strings.TrimLeft(strings.TrimSpace(b.String()), ".")
	if len(out) > 200 {
		ext := path.Ext(out)
		if len(ext) > 20 {
			ext = ""
		}
		cut := 200 - len(ext)
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut] + ext
	}
	if out == "." || out == ".." {
		return ""
	}
	return out
}
