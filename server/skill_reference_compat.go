package server

import (
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/Autumn-27/norma/skill"
)

var legacyChainReference = regexp.MustCompile(`refs/(?:[A-Za-z0-9_-]+/)*[A-Za-z0-9_.-]+\.md(?:#[-A-Za-z0-9_%~:/?=&+.\p{L}\p{N}]*)?`)

// normalizeLegacyChainReferences is an in-memory compatibility adapter for
// binary-only upgrades. It changes explicit documentation references, not shell
// commands or the on-disk file. The SDK expands ${SKILL_DIR} using this Skill's
// own loaded directory immediately before injecting its instructions.
func normalizeLegacyChainReferences(sk skill.Skill) skill.Skill {
	switch filepath.Base(filepath.Clean(sk.Dir)) {
	case "chains-meta", "chains-recon", "chains-web", "chains-ad", "chains-pivot", "chains-priv", "chains-app", "chains-rev", "chains-pwn":
	default:
		return sk
	}
	var out strings.Builder
	var fence byte
	fenceLength, inlineLength := 0, 0
	for _, line := range strings.SplitAfter(sk.Instructions, "\n") {
		if inlineLength > 0 {
			out.WriteString(normalizeLegacyChainLine(line, &inlineLength))
			continue
		}
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if indent <= 3 && len(trimmed) > 0 && (trimmed[0] == '`' || trimmed[0] == '~') {
			length := repeatedReferenceDelimiter(trimmed, 0, trimmed[0])
			if length >= 3 {
				if fence == 0 {
					fence, fenceLength = trimmed[0], length
				} else if fence == trimmed[0] && length >= fenceLength && strings.TrimSpace(trimmed[length:]) == "" {
					fence, fenceLength = 0, 0
				}
				out.WriteString(line)
				continue
			}
		}
		if fence != 0 || indent >= 4 || strings.HasPrefix(line, "\t") {
			out.WriteString(line)
			continue
		}
		out.WriteString(normalizeLegacyChainLine(line, &inlineLength))
	}
	sk.Instructions = out.String()
	return sk
}

func repeatedReferenceDelimiter(text string, start int, delimiter byte) int {
	i := start
	for i < len(text) && text[i] == delimiter {
		i++
	}
	return i - start
}

func closingReferenceBackticks(text string, start, length int) int {
	for start < len(text) {
		next := strings.IndexByte(text[start:], '`')
		if next < 0 {
			return -1
		}
		next += start
		run := repeatedReferenceDelimiter(text, next, '`')
		if run == length {
			return next
		}
		start = next + run
	}
	return -1
}

func normalizeLegacyChainLine(line string, inlineLength *int) string {
	var out strings.Builder
	start := 0
	if *inlineLength > 0 {
		end := closingReferenceBackticks(line, 0, *inlineLength)
		if end < 0 {
			return line
		}
		start = end + *inlineLength
		out.WriteString(line[:start])
		*inlineLength = 0
	}
	trimmed := strings.TrimLeft(line, " ")
	// The original bundled indices also use prose citations like
	// "- 判断依据(refs/web-sqli.md)". Restrict that compatibility to prose bullets;
	// bare program expressions and subshell commands are not documentation links.
	proseCitation := (strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") || strings.HasPrefix(trimmed, "+ ")) && strings.IndexFunc(line, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0
	for start < len(line) {
		open := strings.IndexByte(line[start:], '`')
		if open < 0 {
			out.WriteString(normalizeLegacyChainText(line[start:], proseCitation))
			break
		}
		open += start
		out.WriteString(normalizeLegacyChainText(line[start:open], proseCitation))
		length := repeatedReferenceDelimiter(line, open, '`')
		end := closingReferenceBackticks(line, open+length, length)
		if end < 0 {
			out.WriteString(line[open:])
			*inlineLength = length
			break
		}
		code := line[open+length : end]
		match := legacyChainReference.FindStringIndex(code)
		out.WriteString(line[open : open+length])
		if match != nil && match[0] == 0 && match[1] == len(code) {
			out.WriteString("${SKILL_DIR}/")
		}
		out.WriteString(code)
		out.WriteString(line[end : end+length])
		start = end + length
	}
	return out.String()
}

func normalizeLegacyChainText(text string, proseCitation bool) string {
	var out strings.Builder
	last := 0
	for _, match := range legacyChainReference.FindAllStringIndex(text, -1) {
		start, end := match[0], match[1]
		before, after := text[:start], text[end:]
		markdown := strings.HasSuffix(before, "](") && !escapedReferencePrefix(before, len(before)-2) || strings.HasSuffix(before, "](<") && !escapedReferencePrefix(before, len(before)-3)
		if markdown && (after == "" || !strings.ContainsRune(")> \t\r\n", rune(after[0]))) {
			markdown = false
		}
		plain := proseCitation && strings.HasSuffix(before, "(") && strings.HasPrefix(after, ")") && !escapedReferencePrefix(before, len(before)-1)
		if plain {
			prefix := before[:len(before)-1]
			if i := strings.LastIndexFunc(prefix, unicode.IsSpace); i >= 0 {
				prefix = prefix[i+1:]
			}
			if strings.Contains(prefix, "://") || strings.Contains(prefix, "](") || strings.HasPrefix(prefix, "www.") || strings.HasPrefix(prefix, "/") {
				plain = false
			}
		}
		if !markdown && !plain {
			continue
		}
		out.WriteString(text[last:start])
		out.WriteString("${SKILL_DIR}/")
		out.WriteString(text[start:end])
		last = end
	}
	if last == 0 {
		return text
	}
	out.WriteString(text[last:])
	return out.String()
}

func escapedReferencePrefix(text string, index int) bool {
	backslashes := 0
	for index--; index >= 0 && text[index] == '\\'; index-- {
		backslashes++
	}
	return backslashes%2 == 1
}
