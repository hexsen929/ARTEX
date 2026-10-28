package server

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func decodeSkillMetadataFixture(t *testing.T, content []byte) (map[string]any, []byte) {
	t.Helper()
	head, body, err := splitSkillFrontmatter(content)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]any
	if err := yaml.Unmarshal(head, &metadata); err != nil {
		t.Fatal("rewritten header is not valid YAML", err)
	}
	return metadata, body
}

func TestRewriteSkillFrontmatterPreservesBodyAndUnknownNestedMetadata(t *testing.T) {
	body := []byte("\r\n# 原始正文\r\n```text\r\n---\r\nmcps: not-metadata\r\n```\r\n  trailing spaces  \r\n")
	head := []byte("---\r\nname: fixture-skill\r\ndescription: |\r\n  original: colon\r\n  original second line\r\nlicense: MIT\r\ncompatibility: fixture\r\nmcp: fixture-original\r\nunknown:\r\n  nested:\r\n    - first\r\n    - label: 'keep: value'\r\n      flags: [one, two]\r\n---\r\n")
	content := append(head, body...)
	before, originalBody := decodeSkillMetadataFixture(t, content)
	description := "updated: exact literal\nmcps: [must-not-become-a-dependency]\n## not an instruction field"
	updated, err := rewriteSkillFrontmatter(content, nil, &description, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	after, updatedBody := decodeSkillMetadataFixture(t, updated)
	if !bytes.Equal(originalBody, body) || !bytes.Equal(updatedBody, body) {
		t.Fatal("metadata update changed CRLF, blank lines, delimiters, or trailing spaces in the body")
	}
	if after["name"] != before["name"] || after["description"] != description || !reflect.DeepEqual(after["unknown"], before["unknown"]) || after["mcp"] != "fixture-original" {
		t.Fatal("editing a string damaged untouched nested metadata, name, or original dependency")
	}
	if _, exists := after["mcps"]; exists {
		t.Fatal("description newline was interpreted as an MCP field injection")
	}
	unchanged, err := rewriteSkillFrontmatter(content, nil, nil, nil, nil)
	if err != nil || !bytes.Equal(unchanged, content) {
		t.Fatal("empty metadata patch was not byte-preserving", err)
	}
}

func TestRewriteSkillFrontmatterReplacesBothDependencyAliasesOnlyWhenExplicit(t *testing.T) {
	content := []byte("---\nname: fixture-skill\ndescription: fixture\nmcp: old-one\nmcps: [old-two, old-three]\nlicense: MIT\ncompatibility: old\n---\n# Body\n")
	selected := []string{" new-one ", "", "new-two", "new-one"}
	updated, err := rewriteSkillFrontmatter(content, &selected, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	metadata, body := decodeSkillMetadataFixture(t, updated)
	if _, exists := metadata["mcp"]; exists || !reflect.DeepEqual(metadata["mcps"], []any{"new-one", "new-two"}) || !bytes.Equal(body, []byte("# Body\n")) {
		t.Fatal("explicit dependency patch retained an old alias, added an implicit dependency, or changed the body")
	}
	empty, clear := []string{}, ""
	updated, err = rewriteSkillFrontmatter(updated, &empty, nil, &clear, &clear)
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ = decodeSkillMetadataFixture(t, updated)
	for _, key := range []string{"mcp", "mcps", "license", "compatibility"} {
		if _, exists := metadata[key]; exists {
			t.Fatalf("explicit clear retained metadata field %s", key)
		}
	}
	if metadata["name"] != "fixture-skill" || metadata["description"] != "fixture" {
		t.Fatal("clearing optional fields changed identity or description")
	}
}

func TestRewriteSkillFrontmatterPreservesAliasesIntoChangedFields(t *testing.T) {
	content := []byte("---\nname: fixture-skill\ndescription: &old-description original\nmcps: &old-dependencies [original-mcp]\nunknown:\n  saved-description: *old-description\n  saved-dependencies: *old-dependencies\n  preserved-anchor: &nested {item: unchanged}\n  preserved-alias: *nested\n---\n# Exact body\n")
	before, body := decodeSkillMetadataFixture(t, content)
	description, deps := "replacement", []string{}
	updated, err := rewriteSkillFrontmatter(content, &deps, &description, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	after, newBody := decodeSkillMetadataFixture(t, updated)
	if after["description"] != description || !reflect.DeepEqual(before["unknown"], after["unknown"]) || !bytes.Equal(body, newBody) {
		t.Fatal("editing/deleting metadata anchors changed an untouched unknown field or body")
	}
}

func TestEncodeSkillFrontmatterUsesTypedScalarsAndProducesLoadableIndex(t *testing.T) {
	description := "API: fixture\nmcps: [not-an-actual-dependency]\n说明第二行"
	header, err := encodeSkillFrontmatter("fixture-skill", description, "license: literal", "line one\nline two", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(header, []byte("---\n")) || !bytes.HasSuffix(header, []byte("---\n")) {
		t.Fatal("created header does not have the expected standalone delimiters")
	}
	metadata, body := decodeSkillMetadataFixture(t, header)
	if len(body) != 0 || metadata["description"] != description || metadata["license"] != "license: literal" || metadata["compatibility"] != "line one\nline two" {
		t.Fatal("metadata values were not encoded as exact YAML strings or a body was added")
	}
	if _, exists := metadata["mcps"]; exists {
		t.Fatal("description injection silently created an MCP dependency")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "fixture-skill")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), append(header, []byte("# Explicit fixture body\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := inspectSkillEntry(root, "fixture-skill"); !got.Loadable || got.DeclaredName != "fixture-skill" {
		t.Fatalf("newly encoded Skill is rejected by actual catalog inspection: %+v", got)
	}
	selected := []string{"fixture:mcp", "another-mcp"}
	header, err = encodeSkillFrontmatter("fixture-skill", "fixture", "", "", selected)
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ = decodeSkillMetadataFixture(t, header)
	if !reflect.DeepEqual(metadata["mcps"], []any{"fixture:mcp", "another-mcp"}) {
		t.Fatal("explicit dependency strings were changed or supplemented")
	}
}

func TestSkillFrontmatterErrorsDoNotEchoPrivateInput(t *testing.T) {
	const privateText = "fixture-secret-never-echo-parser-input"
	description := "replacement"
	for _, content := range [][]byte{
		[]byte("# " + privateText),
		[]byte("---\nname: [broken " + privateText + "\n---\nBody\n"),
		[]byte("---\nname: fixture\nname: " + privateText + "\n---\nBody\n"),
		[]byte("---\n- " + privateText + "\n---\nBody\n"),
		[]byte("---\nname: fixture\n" + privateText),
	} {
		if _, err := rewriteSkillFrontmatter(content, nil, &description, nil, nil); err == nil || strings.Contains(err.Error(), privateText) {
			t.Fatal("invalid metadata was accepted or private parser input leaked")
		}
	}
	if _, err := encodeSkillFrontmatter("fixture-skill", privateText+"\x00", "", "", nil); err == nil || strings.Contains(err.Error(), privateText) {
		t.Fatal("invalid string was accepted or echoed")
	}
}

func TestDecodeSkillFrontmatterNameResolvesStringsQuotingAndAliases(t *testing.T) {
	for _, tc := range []struct {
		header string
		want   string
	}{
		{"name: on\n", "on"},
		{"name: \"on\"\n", "on"},
		{"name: 'true'\n", "true"},
		{"name: \"false\"\n", "false"},
		{"name: \"null\"\n", "null"},
		{"name: !!str 007\n", "007"},
		{"name: \"中文-技能\"\n", "中文-技能"},
		{"name: \"\\u4e2d\\u6587-\\u6280\\u80fd\"\n", "中文-技能"},
		{"original: &skill-alias 'aliased-skill'\nname: *skill-alias\n", "aliased-skill"},
		{"name: 'fixture-skill' # not part of the name\n", "fixture-skill"},
	} {
		content := []byte("---\r\n" + strings.ReplaceAll(tc.header, "\n", "\r\n") + "---\r\n# Untouched body\r\n")
		before := bytes.Clone(content)
		if got := decodeSkillFrontmatterName(content); got != tc.want {
			t.Errorf("YAML name decode = %q, want %q", got, tc.want)
		}
		if !bytes.Equal(before, content) {
			t.Fatal("name inspection changed the input file bytes")
		}
	}
	// This is a decoder, not an authorization or filesystem-name validator.
	if got := decodeSkillFrontmatterName([]byte("---\nname: '../not-a-safe-path'\n---\nBody\n")); got != "../not-a-safe-path" || validSkillName(got) {
		t.Fatal("upload name safety validation was silently changed or bypassed")
	}
}

func TestEncodedSkillNameRoundTripsIntoUploadDecoder(t *testing.T) {
	for _, name := range []string{"on", "off", "true", "false", "null", "yes", "no", "中文-技能", "fixture-skill"} {
		header, err := encodeSkillFrontmatter(name, "metadata: literal\nsecond line", "", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		content := append(header, []byte("# Exact Markdown body\n")...)
		if got := decodeSkillFrontmatterName(content); got != name || !validSkillName(got) {
			t.Fatalf("encoder/upload name roundtrip = %q, want %q", got, name)
		}
	}
}

func TestDecodeSkillFrontmatterNameReturnsEmptyForMalformedOrAbsentName(t *testing.T) {
	for _, content := range []string{
		"# No frontmatter\n",
		"---\nname: 'unterminated\n---\nBody\n",
		"---\nname: [not, a, scalar]\n---\nBody\n",
		"---\nname: *missing-alias\n---\nBody\n",
		"---\nname: one\nname: two\n---\nBody\n",
		"---\ndescription: no-name\n---\nBody\n",
		"---\nname: fixture\n",
		"---\nname: \"\\0\"\n---\nBody\n",
	} {
		if got := decodeSkillFrontmatterName([]byte(content)); got != "" {
			t.Fatalf("invalid/absent YAML name produced %q", got)
		}
	}
}
