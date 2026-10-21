import { Badge } from "@/components/ui/badge";
import type { Finding } from "@/lib/types";

export function EvidenceAssessment({ finding }: { finding: Finding }) {
  const state = finding.evidence_assessment ?? (finding.evidence?.trim() ? "needs_review" : "needs_evidence");
  const labels = {
    needs_evidence: "线索 · 证据待补",
    needs_review: "证据待复核",
    supported_for_goal: "目标证据已复核",
    unsupported_for_goal: "证据不支持目标",
  };
  return (
    <Badge variant="outline" title="证据复核与漏洞处置状态分别记录；有流量或已确认状态均不能替代机制证据。">
      {labels[state]}
    </Badge>
  );
}
