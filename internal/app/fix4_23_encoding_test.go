package app

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A14: a hand-edited audit tail carrying a BOM, CRLF/lone CR endings and an
// invalid UTF-8 byte must render the same rows as a clean LF file. Kept in
// go test ./... so the normalization cannot silently rot.
func TestFix4_23AuditTailEncodingNormalization(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, routerDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, routerAuditName)

	clean := []byte(`{"event":"old","status":"ok"}` + "\n" +
		`{"event":"mid","status":"ok","method":"caf\uFFFD"}` + "\n" +
		`{"event":"new","status":"error_-32001"}` + "\n")

	dirty := []byte{0xEF, 0xBB, 0xBF}
	dirty = append(dirty, []byte(`{"event":"old","status":"ok"}`)...)
	dirty = append(dirty, []byte("\r\n")...)
	dirty = append(dirty, 0xEF, 0xBB, 0xBF)
	dirty = append(dirty, []byte(`{"event":"mid","status":"ok","method":"caf`)...)
	dirty = append(dirty, 0xE9)
	dirty = append(dirty, []byte(`"}`)...)
	dirty = append(dirty, []byte("\r")...)
	dirty = append(dirty, []byte(`{"event":"new","status":"error_-32001"}`)...)
	dirty = append(dirty, []byte("\r\n")...)

	if err := os.WriteFile(path, clean, 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := auditLogRowsTail(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 3 {
		t.Fatalf("clean fixture rows = %d, want 3: %+v", len(want), want)
	}

	if err := os.WriteFile(path, dirty, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := auditLogRowsTail(10)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("encoding-normalized tail differs\ngot  = %+v\nwant = %+v", got, want)
	}
}
