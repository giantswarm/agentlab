package config

import (
	"bytes"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// The header every write puts above the configuration; writtenByPrefix is its
// second line. mergeInto drops these lines from the file it updates, so the
// header is never repeated.
const (
	headerFirstLine     = "# agentlab lab configuration. Regenerate with `agentlab configure`."
	headerPasswordsLine = "# Passwords are lab-only throwaway credentials; the hash is cached so"
	headerManifestsLine = "# rendered manifests stay byte-identical across runs (no spurious pod rolls)."
)

// defaultIndent is yaml.v3's own, what every agentlab release wrote.
const defaultIndent = 4

// mergeInto renders c as the body of agentlab.yaml (no header). With the
// previous file in old, it updates that document in place: a value that did
// not change keeps its line, its position and its comments; a changed value
// replaces the old one under the same key; a field the file lacks (a newer
// release's) is appended to its mapping with its default; a field c omits
// (an emptied omitempty field) is removed. Without a previous file, or one
// that is no YAML mapping, it is the plain rendering.
func mergeInto(old []byte, c *Config) ([]byte, error) {
	var next yaml.Node
	if err := next.Encode(c); err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(stripHeader(old), &doc); err != nil ||
		doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return encode(&next, defaultIndent)
	}
	doc.Content[0] = mergeNode(doc.Content[0], &next)
	return encode(&doc, indentOf(old))
}

func encode(n *yaml.Node, indent int) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(indent)
	if err := enc.Encode(n); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// mergeNode is next's content laid over old's document: what is equal stays
// old's node, what differs is next's, carrying old's comments.
func mergeNode(old, next *yaml.Node) *yaml.Node {
	switch {
	case old.Kind == yaml.MappingNode && next.Kind == yaml.MappingNode:
		values := make(map[string]*yaml.Node, len(next.Content)/2)
		for i := 0; i+1 < len(next.Content); i += 2 {
			values[next.Content[i].Value] = next.Content[i+1]
		}
		var content []*yaml.Node
		for i := 0; i+1 < len(old.Content); i += 2 {
			if v, ok := values[old.Content[i].Value]; ok {
				content = append(content, old.Content[i], mergeNode(old.Content[i+1], v))
			}
		}
		// A key the file lacks goes right after the key next renders before
		// it, where a fresh file has it.
		at := 0
		for i := 0; i+1 < len(next.Content); i += 2 {
			key := next.Content[i]
			if j := keyIndex(content, key.Value); j >= 0 {
				at = j + 2
				continue
			}
			content = slices.Insert(content, at, key, next.Content[i+1])
			at += 2
		}
		old.Content = content
		return old
	case old.Kind == yaml.SequenceNode && next.Kind == yaml.SequenceNode && len(old.Content) == len(next.Content):
		for i := range old.Content {
			old.Content[i] = mergeNode(old.Content[i], next.Content[i])
		}
		return old
	case old.Kind == yaml.ScalarNode && next.Kind == yaml.ScalarNode &&
		old.ShortTag() == next.ShortTag() && old.Value == next.Value:
		return old
	}
	next.HeadComment, next.LineComment, next.FootComment = old.HeadComment, old.LineComment, old.FootComment
	return next
}

// keyIndex is the index of key's key node in a mapping's content, -1 if
// it has none.
func keyIndex(content []*yaml.Node, key string) int {
	for i := 0; i+1 < len(content); i += 2 {
		if content[i].Value == key {
			return i
		}
	}
	return -1
}

// stripHeader drops the header lines a write puts above the configuration
// from raw's leading comment block; a comment of the lab's own stays.
func stripHeader(raw []byte) []byte {
	var out []byte
	header := true
	for line := range strings.Lines(string(raw)) {
		if header && !strings.HasPrefix(line, "#") {
			header = false
		}
		if header {
			l := strings.TrimRight(line, "\r\n")
			if l == headerFirstLine || l == headerPasswordsLine || l == headerManifestsLine || strings.HasPrefix(l, writtenByPrefix) {
				continue
			}
		}
		out = append(out, line...)
	}
	return out
}

// indentOf is the indentation of the first key nested in a top-level
// mapping of raw, so a file indented by hand keeps its indentation;
// defaultIndent without one.
func indentOf(raw []byte) int {
	topKey := false
	for line := range strings.Lines(string(raw)) {
		trimmed := strings.TrimLeft(line, " ")
		if strings.HasPrefix(trimmed, "#") || strings.TrimSpace(trimmed) == "" {
			continue
		}
		n := len(line) - len(trimmed)
		if n == 0 {
			topKey = strings.HasSuffix(strings.TrimSpace(trimmed), ":")
			continue
		}
		if topKey && !strings.HasPrefix(trimmed, "-") && n >= 2 {
			return n
		}
		topKey = false
	}
	return defaultIndent
}
