package server

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// cleanStrs trims each string and drops empties.
func cleanStrs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// rewriteSkillFrontmatter parses the SKILL.md YAML frontmatter and replaces the
// fields given as non-nil pointers. The instruction body (after the closing ---) is
// preserved verbatim. Returns an error if the file has no recognisable frontmatter.
func rewriteSkillFrontmatter(content []byte, mcps *[]string, description, license, compatibility *string) ([]byte, error) {
	lines := strings.Split(string(content), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[0]) != "---" {
		return nil, fmt.Errorf("SKILL.md has no YAML frontmatter")
	}
	fmEnd := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			fmEnd = i
			break
		}
	}
	if fmEnd < 0 {
		return nil, fmt.Errorf("SKILL.md frontmatter is not closed")
	}
	// Use the same YAML semantics as the runtime loader. Line-based rewriting can
	// corrupt block values and lists, then report success for unreadable metadata.
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:fmEnd], "\n")), &document); err != nil {
		return nil, fmt.Errorf("SKILL.md has invalid YAML frontmatter: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("SKILL.md YAML frontmatter must be a mapping")
	}
	head := document.Content[0]
	var metadata struct {
		Name          string `yaml:"name"`
		Description   string `yaml:"description"`
		License       string `yaml:"license"`
		Compatibility string `yaml:"compatibility"`
		WhenToUse     string `yaml:"whenToUse"`
		WhenToUseUS   string `yaml:"when_to_use"`
	}
	if err := head.Decode(&metadata); err != nil {
		return nil, fmt.Errorf("SKILL.md has invalid YAML frontmatter: %w", err)
	}
	setField := func(key string, value any) error {
		var node yaml.Node
		if err := node.Encode(value); err != nil {
			return fmt.Errorf("encode skill field %s: %w", key, err)
		}
		for i := 0; i < len(head.Content); i += 2 {
			if head.Content[i].Value == key {
				head.Content[i+1] = &node
				return nil
			}
		}
		head.Content = append(head.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, &node)
		return nil
	}
	for _, update := range []struct {
		key   string
		value *string
	}{
		{key: "description", value: description},
		{key: "license", value: license},
		{key: "compatibility", value: compatibility},
	} {
		if update.value == nil {
			continue
		}
		if err := setField(update.key, strings.TrimSpace(*update.value)); err != nil {
			return nil, err
		}
	}
	if mcps != nil {
		// Remove the legacy alias too, so clearing mcps cannot revive an old mcp.
		fields := head.Content[:0]
		for i := 0; i < len(head.Content); i += 2 {
			key := head.Content[i].Value
			if key != "mcps" && key != "mcp" {
				fields = append(fields, head.Content[i], head.Content[i+1])
			}
		}
		head.Content = fields
		if cleaned := cleanStrs(*mcps); len(cleaned) > 0 {
			if err := setField("mcps", cleaned); err != nil {
				return nil, err
			}
		}
	}
	var frontmatter bytes.Buffer
	encoder := yaml.NewEncoder(&frontmatter)
	encoder.SetIndent(2)
	if err := encoder.Encode(head); err != nil {
		return nil, fmt.Errorf("encode skill frontmatter: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("close skill frontmatter encoder: %w", err)
	}
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.Write(frontmatter.Bytes())
	sb.WriteString("---\n")
	// Body (lines after closing ---)
	if fmEnd+1 < len(lines) {
		sb.WriteString(strings.Join(lines[fmEnd+1:], "\n"))
	}
	return []byte(sb.String()), nil
}

// skillNameFromFrontmatter extracts the `name:` value from a SKILL.md's YAML
// frontmatter (the block between the first two `---` lines). "" if absent.
func skillNameFromFrontmatter(md []byte) string {
	lines := strings.Split(string(md), "\n")
	inFM := false
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "---" {
			if !inFM {
				inFM = true
				continue
			}
			break // end of frontmatter
		}
		if inFM && strings.HasPrefix(t, "name:") {
			v := strings.TrimSpace(strings.TrimPrefix(t, "name:"))
			return strings.Trim(v, `"'`) // name: "中文技能" 也认
		}
	}
	return ""
}
