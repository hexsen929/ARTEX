// Package report renders a deliverable Markdown report from the exploration
// graph: confirmed findings (with severity/evidence) (docs §15 P5).
package report

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// Input bundles what the report needs.
type Input struct {
	Title       string
	Goal        string
	GeneratedAt time.Time
	AssetCounts map[string]int
	Findings    []*db.Node
}

type findingView struct {
	VulnClass          string
	Name               string
	Severity           string
	Summary            string
	VerificationStatus string
	VerificationNote   string
	PoC                string
}

func parseFinding(n *db.Node) findingView {
	var p struct {
		VerificationStatus string `json:"verification_status"`
		VerificationNote   string `json:"verification_note"`
		VulnClass          string `json:"vulnclass"`
		Name               string `json:"name"`
		Severity           string `json:"severity"`
		Summary            string `json:"summary"`
		Evidence           struct {
			PoC string `json:"poc"`
		} `json:"evidence"`
	}
	_ = json.Unmarshal(n.Payload, &p)
	return findingView{VulnClass: p.VulnClass, Name: p.Name, Severity: p.Severity, Summary: p.Summary, PoC: p.Evidence.PoC, VerificationStatus: p.VerificationStatus, VerificationNote: p.VerificationNote}
}

var sevRank = map[string]int{"critical": 0, "high": 1, "medium": 2, "low": 3, "": 4}

// Markdown renders the report.
func Markdown(in Input) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# 渗透测试报告 — %s\n\n", nz(in.Title, "未命名任务"))
	fmt.Fprintf(&b, "- **任务目标**：%s\n", nz(in.Goal, "（未指定）"))
	fmt.Fprintf(&b, "- **生成时间**：%s\n\n", in.GeneratedAt.Format("2006-01-02 15:04:05"))

	// summary
	fmt.Fprintf(&b, "## 摘要\n\n")
	counts := map[string]int{}
	for _, n := range in.Findings {
		counts[db.VerificationLabel(parseFinding(n).VerificationStatus)]++
	}
	fmt.Fprintf(&b, "- 发现总数：**%d** 个（包含待验证线索）\n", len(in.Findings))
	fmt.Fprintf(&b, "- 已确认：%d；待验证：%d；验证受阻：%d；已排除：%d；历史未复核：%d\n", counts["已确认"], counts["待验证"], counts["验证受阻"], counts["已排除"], counts["历史未复核"])

	fmt.Fprintf(&b, "- 资产：")
	var types []string
	for t := range in.AssetCounts {
		types = append(types, t)
	}
	sort.Strings(types)
	for i, t := range types {
		if i > 0 {
			b.WriteString("、")
		}
		fmt.Fprintf(&b, "%s %d", t, in.AssetCounts[t])
	}
	b.WriteString("\n\n")

	// findings
	fmt.Fprintf(&b, "## 发现\n\n")
	if len(in.Findings) == 0 {
		b.WriteString("_本次未确认漏洞。_\n\n")
	} else {
		fs := make([]findingView, 0, len(in.Findings))
		for _, n := range in.Findings {
			fs = append(fs, parseFinding(n))
		}
		sort.SliceStable(fs, func(i, j int) bool { return sevRank[fs[i].Severity] < sevRank[fs[j].Severity] })
		for i, f := range fs {
			fmt.Fprintf(&b, "### %d. [%s] %s\n\n", i+1, strings.ToUpper(nz(f.Severity, "info")), nz(f.Name, nz(f.VulnClass, "未分类")))
			fmt.Fprintf(&b, "- **验证状态**：%s\n- **验证说明**：%s\n\n", db.VerificationLabel(f.VerificationStatus), nz(f.VerificationNote, "未提供；需进一步复核"))
			fmt.Fprintf(&b, "%s\n\n", nz(f.Summary, ""))
			if f.PoC != "" {
				fmt.Fprintf(&b, "**PoC / 证据：**\n\n```\n%s\n```\n\n", f.PoC)
			}
		}
	}

	return b.String()
}

func nz(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}
