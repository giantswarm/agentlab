package lab

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeJWT is a JWT-shaped value whose header names typ: three segments, no
// real signature.
func fakeJWT(typ string) string {
	seg := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return seg(`{"alg":"ES256","typ":"`+typ+`","x5c":["MIIB"]}`) + "." + seg(`{"sub":"lab","aud":"lab","iat":1,"exp":2}`) + "." + seg(strings.Repeat("s", 64))
}

// TestDataTokensCommand runs the data-tokens search under sh on fixtures: only
// Claude Code's signed cache, at its one path and with nothing else
// token-shaped in it, is admitted.
func TestDataTokensCommand(t *testing.T) {
	for _, tool := range strings.Fields("sh timeout base64 grep") {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("no %s on this machine", tool)
		}
	}
	// cacheFile names the signed cache's own path among a case's files.
	const cacheFile = "cache"
	signed := `{"jws":"` + fakeJWT(claudeSignedCacheType) + `","receivedAt":1}`
	ghToken := "gh" + "p_" + strings.Repeat("A", 36)
	for _, c := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"nothing", map[string]string{"data/claude/settings.json": "{}"}, "0"},
		{"the signed cache", map[string]string{cacheFile: signed}, "0"},
		{"the signed cache elsewhere", map[string]string{"data/claude/other.json": signed}, "1"},
		{"another JWT at the cache's path", map[string]string{cacheFile: `{"jws":"` + fakeJWT("JWT") + `"}`}, "1"},
		{"a token beside the signed cache", map[string]string{cacheFile: signed + "\n" + ghToken}, "1"},
		{"a JWT beside the signed cache", map[string]string{cacheFile: signed + "\n" + fakeJWT("JWT")}, "1"},
		{"a JWT without typ at the cache's path", map[string]string{cacheFile: `{"jws":"` + fakeJWT("") + `"}`}, "1"},
		{"the signed cache and a token in the workspace", map[string]string{cacheFile: signed, "workspace/repo/.git/config": "url = https://x:" + ghToken + "@github.com/"}, "1"},
		{"a file name with a space", map[string]string{"workspace/a b.txt": ghToken, "workspace/c.txt": fakeJWT("JWT")}, "2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			cache := filepath.Join(dir, "data/claude/policy-limits.json.signature.json")
			for name, content := range c.files {
				path := filepath.Join(dir, name)
				if name == cacheFile {
					path = cache
				}
				if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			roots := filepath.Join(dir, "data") + " " + filepath.Join(dir, "workspace")
			script := "P=" + probeMarker + "\n" + dataTokensCommand(roots, cache)
			out, err := exec.Command("sh", "-c", script).CombinedOutput() //nolint:gosec // the probe fragment under test, on this test's fixture paths
			if err != nil {
				t.Fatalf("sh: %v: %s", err, out)
			}
			if got := parseCodingProbe(string(out))["data-tokens"]; got != c.want {
				t.Errorf("data-tokens=%q, want %q (output %q)", got, c.want, out)
			}
		})
	}
}
