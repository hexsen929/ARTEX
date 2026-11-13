package kb

import (
	"strings"
	"unicode/utf8"
)

// Term retains the original distinction between strong anchors and softer
// Chinese bigrams. Losing this bit changes both candidate selection and rank.
type Term struct {
	Text string
	Hard bool
}

var asciiStopwords = wordSet("http https www the and for with from this that test target check scan com net org")
var chineseStopwords = wordSet("的 了 和 与 或 及 对 在 是 有 无 测试 检测 检查 尝试 确认 目标 任务 进行 通过 使用 利用 这个 那个 一个 我们 可能 是否 存在 扫描 探测 哪些 如何 怎么 什么 需要 可以 不能 没有")

const chineseStopchars = "的了是和与或在有对及"

func wordSet(s string) map[string]bool {
	m := map[string]bool{}
	for _, w := range strings.Fields(s) {
		m[w] = true
	}
	return m
}

func ExtractTerms(q string) []Term {
	var out []Term
	seen := map[string]bool{}
	add := func(s string, hard bool) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, Term{s, hard})
		}
	}
	var ascii, chinese strings.Builder
	var segments []string
	flushASCII := func() {
		s := strings.Trim(ascii.String(), ".-_")
		ascii.Reset()
		if len(s) >= 2 && !asciiStopwords[s] {
			add(s, len(s) >= 3)
		}
	}
	flushChinese := func() {
		if chinese.Len() > 0 {
			segments = append(segments, chinese.String())
			chinese.Reset()
		}
	}
	for _, r := range strings.ToLower(strings.TrimSpace(q)) {
		switch {
		case r < 128 && (r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_'):
			flushChinese()
			ascii.WriteRune(r)
		case r >= 0x4e00 && r <= 0x9fff || r >= 0x3400 && r <= 0x4dbf || r >= 0xf900 && r <= 0xfaff:
			flushASCII()
			chinese.WriteRune(r)
		default:
			flushASCII()
			flushChinese()
		}
	}
	flushASCII()
	flushChinese()
	for _, segment := range segments {
		runes := []rune(segment)
		if len(runes) < 2 {
			continue
		}
		if len(runes) <= 8 && !chineseStopwords[segment] {
			add(segment, true)
		}
		for i := 0; i+1 < len(runes); i++ {
			pair := string(runes[i : i+2])
			if !chineseStopwords[pair] && !strings.ContainsRune(chineseStopchars, runes[i]) && !strings.ContainsRune(chineseStopchars, runes[i+1]) {
				add(pair, false)
			}
		}
	}
	if len(out) > 24 {
		out = out[:24]
	}
	return out
}

func Terms(q string) []string {
	var out []string
	for _, t := range ExtractTerms(q) {
		out = append(out, t.Text)
	}
	return out
}

func MinTermMatches(terms []Term) int {
	if len(terms) >= 3 {
		return 2
	}
	return 1
}

// Each field group is counted only once per term, even when several of its
// fields match. A term can contribute to several groups, but just one match.
func ScoreTerms(e Entry, terms []Term, includeContent bool) (score, matches int) {
	groups := []struct {
		text   string
		weight int
	}{
		{strings.ToLower(e.Title + "\x00" + e.Code), 3},
		{strings.ToLower(e.Component + "\x00" + e.Tags + "\x00" + e.CVE + "\x00" + e.AssetTarget), 2},
		{strings.ToLower(e.Versions + "\x00" + e.Description + "\x00" + e.Usage), 1},
	}
	if includeContent {
		groups = append(groups, struct {
			text   string
			weight int
		}{strings.ToLower(e.Content), 1})
	}
	for _, term := range terms {
		weight, hit := 1, false
		if term.Hard {
			weight = 2
		}
		for _, group := range groups {
			if strings.Contains(group.text, term.Text) {
				score += group.weight * weight
				hit = true
			}
		}
		if hit {
			matches++
		}
	}
	return score, matches
}

func Score(e Entry, terms []string) int {
	if len(terms) == 0 {
		return 1
	}
	var ts []Term
	for _, t := range terms {
		ts = append(ts, Term{t, utf8.RuneCountInString(t) >= 3})
	}
	n, _ := ScoreTerms(e, ts, false)
	return n
}
