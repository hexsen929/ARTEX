package server

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

var errSkillFrontmatter = errors.New("Skill YAML 元数据格式无效，请检查字段与分隔符")

// splitSkillFrontmatter changes no bytes. In particular, body includes every
// byte after the closing delimiter's line ending, including leading blank lines.
func splitSkillFrontmatter(content []byte) (head, body []byte, err error) {
	if len(content) > maxSkillIndexBytes || !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 {
		return nil, nil, errSkillFrontmatter
	}
	line := func(offset int) (value []byte, next int) {
		end := bytes.IndexByte(content[offset:], '\n')
		if end < 0 {
			return bytes.TrimSuffix(content[offset:], []byte{'\r'}), len(content)
		}
		end += offset
		return bytes.TrimSuffix(content[offset:end], []byte{'\r'}), end + 1
	}
	first, next := line(0)
	if !bytes.Equal(bytes.TrimRight(first, " \t"), []byte("---")) || next == len(content) {
		return nil, nil, errSkillFrontmatter
	}
	start := next
	for next < len(content) {
		offset := next
		value, following := line(offset)
		if bytes.Equal(bytes.TrimRight(value, " \t"), []byte("---")) {
			return content[start:offset], content[following:], nil
		}
		next = following
	}
	return nil, nil, errSkillFrontmatter
}

func parseSkillFrontmatter(head []byte) (*yaml.Node, error) {
	var document yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(head))
	if err := decoder.Decode(&document); errors.Is(err, io.EOF) {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil
	} else if err != nil {
		return nil, errSkillFrontmatter
	}
	if err := decoder.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
		return nil, errSkillFrontmatter
	}
	if document.Kind == 0 {
		return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}, nil
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		if document.Kind == yaml.DocumentNode && len(document.Content) == 1 && document.Content[0].Tag == "!!null" && document.Content[0].Value == "" {
			return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", HeadComment: document.Content[0].HeadComment}, nil
		}
		return nil, errSkillFrontmatter
	}
	mapping := document.Content[0]
	seen := map[string]bool{}
	for i := 0; i < len(mapping.Content); i += 2 {
		key := mapping.Content[i]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" || seen[key.Value] {
			return nil, errSkillFrontmatter
		}
		seen[key.Value] = true
	}
	return mapping, nil
}

func skillMetadataString(value string) (*yaml.Node, error) {
	if !utf8.ValidString(value) || strings.ContainsRune(value, 0) || len(value) > maxSkillIndexBytes {
		return nil, errSkillFrontmatter
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}, nil
}

func skillMetadataKey(key string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
}

func cleanSkillMCPNames(names []string) ([]string, error) {
	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !utf8.ValidString(name) || strings.IndexFunc(name, unicode.IsControl) >= 0 || len(name) > maxSkillIndexBytes {
			return nil, errSkillFrontmatter
		}
		if !seen[name] {
			out, seen[name] = append(out, name), true
		}
	}
	return out, nil
}

func setSkillMetadata(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != key {
			continue
		}
		if value == nil {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
		} else {
			old := mapping.Content[i+1]
			value.HeadComment, value.LineComment, value.FootComment = old.HeadComment, old.LineComment, old.FootComment
			mapping.Content[i+1] = value
		}
		return
	}
	if value != nil {
		mapping.Content = append(mapping.Content, skillMetadataKey(key), value)
	}
}

func writeSkillMetadata(mapping *yaml.Node) ([]byte, error) {
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(mapping); err != nil {
		_ = encoder.Close()
		return nil, errSkillFrontmatter
	}
	if err := encoder.Close(); err != nil || out.Len() > maxSkillIndexBytes {
		return nil, errSkillFrontmatter
	}
	result := make([]byte, 0, out.Len()+8)
	result = append(result, "---\n"...)
	result = append(result, out.Bytes()...)
	result = append(result, "---\n"...)
	return result, nil
}

// preserveAliasesToChangedFields materializes only aliases into fields that
// are about to be replaced/deleted. Otherwise an unchanged unknown field could
// lose its value or become a dangling alias when a metadata anchor disappears.
func preserveAliasesToChangedFields(mapping *yaml.Node, changed map[string]bool) error {
	targets := map[*yaml.Node]bool{}
	var mark func(*yaml.Node)
	mark = func(node *yaml.Node) {
		if targets[node] {
			return
		}
		targets[node] = true
		for _, child := range node.Content {
			mark(child)
		}
	}
	for i := 0; i < len(mapping.Content); i += 2 {
		if changed[mapping.Content[i].Value] {
			mark(mapping.Content[i+1])
		}
	}
	budget := 10000
	var clone func(*yaml.Node, map[*yaml.Node]bool, int) (*yaml.Node, error)
	clone = func(node *yaml.Node, stack map[*yaml.Node]bool, depth int) (*yaml.Node, error) {
		budget--
		if node == nil || budget < 0 || depth > 100 || stack[node] {
			return nil, errSkillFrontmatter
		}
		stack[node] = true
		defer delete(stack, node)
		if node.Kind == yaml.AliasNode {
			return clone(node.Alias, stack, depth+1)
		}
		copy := *node
		copy.Anchor, copy.Alias, copy.Content = "", nil, nil
		for _, child := range node.Content {
			value, err := clone(child, stack, depth+1)
			if err != nil {
				return nil, err
			}
			copy.Content = append(copy.Content, value)
		}
		return &copy, nil
	}
	var visit func(*yaml.Node) error
	visit = func(node *yaml.Node) error {
		if node.Kind == yaml.AliasNode && targets[node.Alias] {
			copy, err := clone(node.Alias, map[*yaml.Node]bool{}, 0)
			if err != nil {
				return err
			}
			copy.HeadComment, copy.LineComment, copy.FootComment = node.HeadComment, node.LineComment, node.FootComment
			*node = *copy
			return nil
		}
		for _, child := range node.Content {
			if err := visit(child); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(mapping)
}

// rewriteSkillFrontmatter updates only non-nil metadata fields. The Markdown
// body remains byte-for-byte identical, including its CRLF and blank lines.
func rewriteSkillFrontmatter(content []byte, mcps *[]string, description, license, compatibility *string) ([]byte, error) {
	head, body, err := splitSkillFrontmatter(content)
	if err != nil {
		return nil, err
	}
	mapping, err := parseSkillFrontmatter(head)
	if err != nil {
		return nil, err
	}
	changed := map[string]bool{}
	for key, value := range map[string]*string{"description": description, "license": license, "compatibility": compatibility} {
		if value != nil {
			changed[key] = true
		}
	}
	if mcps != nil {
		changed["mcp"], changed["mcps"] = true, true
	}
	if len(changed) == 0 {
		return bytes.Clone(content), nil
	}
	if err := preserveAliasesToChangedFields(mapping, changed); err != nil {
		return nil, err
	}
	for _, item := range []struct {
		key   string
		value *string
	}{{"description", description}, {"license", license}, {"compatibility", compatibility}} {
		if item.value == nil {
			continue
		}
		var node *yaml.Node
		if strings.TrimSpace(*item.value) != "" {
			node, err = skillMetadataString(*item.value)
			if err != nil {
				return nil, err
			}
		}
		setSkillMetadata(mapping, item.key, node)
	}
	if mcps != nil {
		clean, err := cleanSkillMCPNames(*mcps)
		if err != nil {
			return nil, err
		}
		setSkillMetadata(mapping, "mcp", nil)
		setSkillMetadata(mapping, "mcps", nil)
		if len(clean) > 0 {
			node := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			for _, name := range clean {
				value, err := skillMetadataString(name)
				if err != nil {
					return nil, err
				}
				node.Content = append(node.Content, value)
			}
			setSkillMetadata(mapping, "mcps", node)
		}
	}
	encoded, err := writeSkillMetadata(mapping)
	if err != nil || len(encoded)+len(body) > maxSkillIndexBytes {
		return nil, errSkillFrontmatter
	}
	return append(encoded, body...), nil
}

// encodeSkillFrontmatter returns the header only. Callers append the original
// or scaffolded Markdown body separately; strings are YAML scalars, not syntax.
func encodeSkillFrontmatter(name, description, license, compatibility string, mcps []string) ([]byte, error) {
	if !validSkillName(name) || strings.TrimSpace(description) == "" {
		return nil, errSkillFrontmatter
	}
	mapping := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	for _, item := range []struct{ key, value string }{{"name", name}, {"description", description}, {"license", license}, {"compatibility", compatibility}} {
		if item.key != "name" && item.key != "description" && strings.TrimSpace(item.value) == "" {
			continue
		}
		value, err := skillMetadataString(item.value)
		if err != nil {
			return nil, err
		}
		setSkillMetadata(mapping, item.key, value)
	}
	clean, err := cleanSkillMCPNames(mcps)
	if err != nil {
		return nil, err
	}
	if len(clean) > 0 {
		sequence := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, name := range clean {
			value, err := skillMetadataString(name)
			if err != nil {
				return nil, err
			}
			sequence.Content = append(sequence.Content, value)
		}
		setSkillMetadata(mapping, "mcps", sequence)
	}
	return writeSkillMetadata(mapping)
}
