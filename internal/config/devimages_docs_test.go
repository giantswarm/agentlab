package config

import (
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The devImages targets the prose names: `platform.devImages.<target>`, and
// `platform.devImages` followed by a list of backticked targets
// (`a` / `b` / `c`); and a YAML example's map keys.
var (
	devImagesKeyRe     = regexp.MustCompile(`platform\.devImages\.([a-z0-9-]+)`)
	devImagesListRe    = regexp.MustCompile("platform\\.devImages`((?:,? *(?:/|or|and)? *`[a-z0-9-]+`)+)")
	backtickedTargetRe = regexp.MustCompile("`([a-z0-9-]+)`")
	yamlKeyRe          = regexp.MustCompile(`^(\s*)([a-z0-9-]+):`)
)

// docsFS is the repository's docs/.
var docsFS = os.DirFS("../../docs")

// documentedDevImageTargets are the devImages targets docs/*.md names: the
// keys under a `devImages:` map in its YAML examples and the targets its
// prose names after platform.devImages.
func documentedDevImageTargets(t *testing.T) map[string]string {
	t.Helper()
	files, err := fs.Glob(docsFS, "*.md")
	if err != nil || len(files) == 0 {
		t.Fatalf("no docs found: %v", err)
	}
	targets := map[string]string{}
	for _, f := range files {
		raw, err := fs.ReadFile(docsFS, f)
		if err != nil {
			t.Fatal(err)
		}
		doc := string(raw)
		name := f
		for _, m := range devImagesKeyRe.FindAllStringSubmatch(doc, -1) {
			targets[m[1]] = name
		}
		for _, m := range devImagesListRe.FindAllStringSubmatch(doc, -1) {
			for _, b := range backtickedTargetRe.FindAllStringSubmatch(m[1], -1) {
				targets[b[1]] = name
			}
		}
		inYAML, mapIndent := false, -1
		for _, line := range strings.Split(doc, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				inYAML, mapIndent = strings.TrimSpace(line) == "```yaml", -1
				continue
			}
			if !inYAML {
				continue
			}
			m := yamlKeyRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			indent := len(m[1])
			switch {
			case m[2] == "devImages":
				mapIndent = indent
			case mapIndent >= 0 && indent > mapIndent:
				targets[m[2]] = name
			default:
				mapIndent = -1
			}
		}
	}
	return targets
}

// TestDocumentedDevImageTargetsAreAccepted ties the devImages targets the
// docs name to DevImageComponents both ways: every documented target is
// accepted, and every accepted target is documented in docs/platform.md.
func TestDocumentedDevImageTargetsAreAccepted(t *testing.T) {
	documented := documentedDevImageTargets(t)
	for _, want := range []string{DevImageClaudeHarness, DevImageCodexHarness, DevImageOmpHarness} {
		if _, ok := documented[want]; !ok {
			t.Fatalf("the docs scan found no %s; the scan is broken", want)
		}
	}
	for target, doc := range documented {
		if !slices.Contains(DevImageComponents, target) {
			t.Errorf("docs/%s names platform.devImages target %q, which DevImageComponents refuses", doc, target)
		}
	}
	platform, err := fs.ReadFile(docsFS, "platform.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range DevImageComponents {
		if !strings.Contains(string(platform), "`"+target+"`") {
			t.Errorf("DevImageComponents accepts %q, which docs/platform.md does not document", target)
		}
	}
}

// TestValidationErrorDevImageTargetsAreAccepted: a refusal that sends the
// person to a devImages target names one the validation accepts, and setting
// it clears the refusal.
func TestValidationErrorDevImageTargetsAreAccepted(t *testing.T) {
	cfg := Default()
	cfg.Platform.Coding.Enabled = true
	cfg.Platform.Coding.Harnesses = []string{CodingRuntimeOmp}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "platform.devImages."+DevImageOmpHarness) {
		t.Fatalf("omp without an image must be refused naming platform.devImages.%s, got %v", DevImageOmpHarness, err)
	}
	for _, m := range devImagesKeyRe.FindAllStringSubmatch(err.Error(), -1) {
		if !slices.Contains(DevImageComponents, m[1]) {
			t.Errorf("the refusal names platform.devImages.%s, which DevImageComponents refuses", m[1])
		}
	}

	// Following the refusal's advice is enough: the dev image stands in for
	// platform.coding.images.omp.
	cfg.Platform.DevImages = map[string]string{DevImageOmpHarness: "omp-harness:dev"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("omp with platform.devImages.%s must validate: %v", DevImageOmpHarness, err)
	}
}
