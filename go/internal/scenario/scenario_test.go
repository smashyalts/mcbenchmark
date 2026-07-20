package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "scenario.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const base = `
name: t
target: {host: 127.0.0.1}
traces: {manifest: traces/manifest.json}
`

// A prefix that leaves no room for the account index used to be truncated,
// which does not rename the bots so much as merge them: every session logs in
// as the same player and the server kicks all but one as duplicate logins.
func TestLongUsernamePrefixIsRejected(t *testing.T) {
	_, err := Load(write(t, base+"identity: {username_prefix: BENCHMARK_LOAD_}\n"))
	if err == nil {
		t.Fatal("want an error for a prefix with no room for the index")
	}
	if !strings.Contains(err.Error(), "username_prefix") {
		t.Fatalf("error should name the offending field, got %v", err)
	}
}

// The longest prefix that still fits must keep working, or the check is just
// moving the breakage.
func TestElevenCharacterPrefixIsAccepted(t *testing.T) {
	s, err := Load(write(t, base+"identity: {username_prefix: ABCDEFGHIJK}\n"))
	if err != nil {
		t.Fatalf("11 characters plus 5 digits is exactly 16: %v", err)
	}
	if s.Identity.UsernamePrefix != "ABCDEFGHIJK" {
		t.Fatalf("prefix mangled: %q", s.Identity.UsernamePrefix)
	}
}

func TestNegativeLoadIsRejected(t *testing.T) {
	_, err := Load(write(t, base+"load: {target_players: -5}\n"))
	if err == nil {
		t.Fatal("want an error for a negative player count")
	}
}

func TestDefaultsStillLoad(t *testing.T) {
	s, err := Load(write(t, base))
	if err != nil {
		t.Fatal(err)
	}
	if s.Identity.UsernamePrefix != "BENCH_" || s.Target.Port != 25565 {
		t.Fatalf("defaults not applied: %+v", s)
	}
}
