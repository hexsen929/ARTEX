import type { LLMProfile } from "./types";

// Only searchable profile metadata belongs here. Never include API keys or
// their hints; the form value remains the stable profile ID.
export function llmProfileSearchLabel(profile: Pick<LLMProfile, "id" | "name" | "model" | "base_url">): string {
  return [profile.name, profile.model, profile.base_url, `#${profile.id}`]
    .filter((part) => typeof part === "string" && part.trim() !== "")
    .join(" ");
}
