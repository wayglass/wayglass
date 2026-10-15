// Model families, for policy rules with `reviewer: { different_family: true }`:
// the approver's family must differ from the author's, so a model never signs
// off on its own family's blind spots. Families are self-reported (declare
// and approve send them) until per-agent keys carry one; see docs/POLICY.md.

export const FAMILIES = ["claude", "openai", "gemini", "meta", "other", "human"] as const;
export type Family = (typeof FAMILIES)[number];

const ALIASES: Record<string, Family> = {
  anthropic: "claude",
  codex: "openai",
  gpt: "openai",
  chatgpt: "openai",
  google: "gemini",
  llama: "meta",
  person: "human",
};

/** The family for a declared value: "" when none was given, null when it is not one we know. */
export function normalizeFamily(raw: string): Family | "" | null {
  const v = raw.trim().toLowerCase();
  if (v === "") return "";
  if ((FAMILIES as readonly string[]).includes(v)) return v as Family;
  return ALIASES[v] ?? null;
}

/**
 * Why an approver may not approve under a different-family rule, or null if
 * they may. "other" is not a family we can tell apart, so two "other"s are
 * not shown to differ.
 */
export function familyRefusal(author: string | null, approver: string | null): { code: "family_required" | "same_family"; message: string } | null {
  if (!author) {
    return {
      code: "family_required",
      message: "policy needs a reviewer from a different model family, but the author declared none; the author must re-declare with --family (or WAYGLASS_AGENT_FAMILY)",
    };
  }
  if (!approver) {
    return {
      code: "family_required",
      message: `policy needs a reviewer from a model family other than the author's (${author}); declare yours with --family or WAYGLASS_AGENT_FAMILY (a person uses human)`,
    };
  }
  if (approver === author) {
    return { code: "same_family", message: `policy needs a reviewer from a model family other than the author's (${author}); you declared ${approver}` };
  }
  return null;
}
