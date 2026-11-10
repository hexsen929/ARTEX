package report

import (
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
)

func reproductionMarkdown(r *db.Reproduction) string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n## 保存的结构化复现\n\n")
	fmt.Fprintf(&b, "入口：%s\n\n", r.EntryPoint)
	for _, pre := range r.Prerequisites {
		fmt.Fprintf(&b, "- %s\n", pre)
	}
	for i, s := range r.Steps {
		fmt.Fprintf(&b, "\n### 步骤 %d：%s\n\n", i+1, s.Title)
		for _, text := range []string{s.Request, s.Response} {
			if text != "" {
				fence := "```"
				for strings.Contains(text, fence) {
					fence += "`"
				}
				fmt.Fprintf(&b, "%stext\n%s\n%s\n\n", fence, text, fence)
			}
		}
		fmt.Fprintf(&b, "预期：%s\n\n证明与局限：%s\n\n", s.Expected, s.Proves)
		if len(s.TrafficIDs) > 0 {
			fmt.Fprintf(&b, "已绑定流量 ID：%s\n\n", strings.Join(s.TrafficIDs, ", "))
		}
	}
	fmt.Fprintf(&b, "来源：%s\n\n", r.Provenance)
	return b.String()
}
