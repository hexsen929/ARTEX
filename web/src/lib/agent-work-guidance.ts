export interface AgentWorkGuidance {
  version: string;
  text: string;
}

/** Keep runtime guidance separate from the editable/saved prompt body. */
export function readAgentWorkGuidance(
  source: { work_guidance_version?: string; work_guidance?: string } | null | undefined,
): AgentWorkGuidance | undefined {
  if (typeof source?.work_guidance !== "string" || !source.work_guidance.trim()) return undefined;
  return {
    version: typeof source.work_guidance_version === "string" ? source.work_guidance_version.trim() : "",
    text: source.work_guidance,
  };
}
