package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// workerProvenance is the executor-lineage triple a claim stamps onto routed
// work beads (worker_model / worker_genus / worker_effort, ga-vkcp). The
// cross-genus review policy needs the genus on EVERY worked bead: a reviewer
// lane cannot verify its genus differs from the implementer's if the
// implementer genus was never recorded. Values are provider option choice
// values (the opt_model / opt_effort vocabulary); empty fields are unknown
// and stay unstamped rather than fabricated.
type workerProvenance struct {
	Model  string
	Genus  string
	Effort string
}

// builtinFamilyGenus maps single-lineage builtin provider families to the
// model training lineage ("genus") their CLI fronts. This is vendor fact
// data — the same class of fact as the builtin catalog's upstream env
// bindings (ANTHROPIC_BASE_URL on claude, OPENAI_BASE_URL on codex) — not a
// judgment call. Families absent here (cursor, copilot, amp, opencode,
// mimocode, groq, cerebras, auggie, pi, omp, kiro) are multi-model routers or
// hardware inference clouds with no single lineage; providerGenus falls back
// to the family name itself for those, which is still a stable label that
// differs from every single-lineage genus, so cross-genus comparisons stay
// sound.
var builtinFamilyGenus = map[string]string{
	"claude":      "anthropic",
	"codex":       "openai",
	"gemini":      "google",
	"antigravity": "google",
	"grok":        "xai",
	"kimi":        "moonshot",
}

// providerGenus derives the model-lineage genus for a provider name: the
// builtin family's lineage when the family is single-lineage, else the family
// name, else the provider name itself (a fully custom standalone provider
// still gets a stable, distinct genus label). Empty in ⇒ empty out.
func providerGenus(providerName string, cityProviders map[string]config.ProviderSpec) string {
	providerName = strings.TrimSpace(providerName)
	if providerName == "" {
		return ""
	}
	family := config.BuiltinFamily(providerName, cityProviders)
	if genus, ok := builtinFamilyGenus[family]; ok {
		return genus
	}
	if family != "" {
		return family
	}
	return providerName
}

// hookWorkerProvenance resolves the claiming session's execution provenance
// from what the hook already holds — no store reads. Provider name precedence:
// the session bead's stored provider (info, loaded by the claim fence), then
// the runtime's GC_PROVIDER from the claim env (the same env the claim path
// reads its sibling identity vars from, see hookClaimSessionID), then the
// agent template's configured provider. Model/effort come from the provider's
// resolved EffectiveDefaults (schema defaults + provider/agent option_defaults
// + choices inferred from pinned provider args), overridden by any schema
// choice explicitly present in the session's stored launch command — the
// launch ground truth, which carries per-dispatch overrides. Best-effort: an
// unresolvable provider still yields a genus from the name so the cross-genus
// record survives.
func hookWorkerProvenance(cfg *config.City, agent *config.Agent, info *session.Info, env []string) workerProvenance {
	if cfg == nil || agent == nil {
		return workerProvenance{}
	}
	providerName := ""
	if info != nil {
		providerName = strings.TrimSpace(info.Provider)
	}
	if providerName == "" {
		providerName = hookClaimEnvValue(env, "GC_PROVIDER")
	}
	if providerName == "" {
		providerName = strings.TrimSpace(agent.Provider)
	}
	if providerName == "" {
		providerName = strings.TrimSpace(cfg.Workspace.Provider)
	}
	if providerName == "" {
		return workerProvenance{}
	}

	prov := workerProvenance{Genus: providerGenus(providerName, cfg.Providers)}

	// Resolve the provider the session actually runs, not necessarily the
	// template's configured one, keeping agent-level option_defaults in the
	// merge. The permissive lookPath skips PATH verification: this resolution
	// feeds metadata only, and a claim must never lose its provenance stamp to
	// a PATH quirk in the hook's environment.
	resolveAgent := agent.Clone()
	resolveAgent.Provider = providerName
	resolved, err := config.ResolveProvider(&resolveAgent, &cfg.Workspace, cfg.Providers, func(string) (string, error) { return "", nil })
	if err != nil || resolved == nil {
		return prov
	}
	effective := map[string]string{}
	for key, value := range resolved.EffectiveDefaults {
		effective[key] = value
	}
	if info != nil {
		for key, value := range config.InferOptionDefaultsFromCommand(resolved.OptionsSchema, info.Command) {
			effective[key] = value
		}
	}
	prov.Model = strings.TrimSpace(effective["model"])
	prov.Effort = strings.TrimSpace(effective["effort"])
	return prov
}
