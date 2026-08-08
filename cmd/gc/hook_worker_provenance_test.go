package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// provenanceTestConfig builds a city config whose "fable" provider wraps
// builtin:claude with a pinned model/effort in args (the canonical
// city.toml shape: args_append on a builtin base) and whose "worker" agent
// uses it. The claude builtin schema declares "claude-fable-5" and the
// effort tiers as choices, so the pinned args are inferred into
// EffectiveDefaults at resolve time.
func provenanceTestConfig() *config.City {
	return &config.City{
		Providers: map[string]config.ProviderSpec{
			"fable": {
				Base:       strPtr("builtin:claude"),
				ArgsAppend: []string{"--model", "claude-fable-5", "--effort", "low"},
			},
			"luna": {
				Base: strPtr("builtin:codex"),
				OptionDefaults: map[string]string{
					"model":  "gpt-5.6-luna",
					"effort": "max",
				},
			},
			"local-gemma": {
				Base:       strPtr("builtin:opencode"),
				ArgsAppend: []string{"--model", "local/gemma4-26b-a4b"},
			},
			"bespoke": {
				Base:    strPtr(""),
				Command: "my-llm",
			},
		},
	}
}

// TestProviderGenus pins the provider-name → genus derivation: single-lineage
// builtin families map to their model lineage, wrapped custom providers
// resolve through their base chain, multi-model router families fall back to
// the family name, and a fully custom standalone provider falls back to its
// own name (still a stable, distinct genus label for cross-genus comparison).
func TestProviderGenus(t *testing.T) {
	cfg := provenanceTestConfig()
	tests := []struct {
		provider string
		want     string
	}{
		{"claude", "anthropic"},
		{"codex", "openai"},
		{"gemini", "google"},
		{"antigravity", "google"},
		{"grok", "xai"},
		{"kimi", "moonshot"},
		{"fable", "anthropic"},      // wrapped claude
		{"luna", "openai"},          // wrapped codex
		{"local-gemma", "opencode"}, // router family: falls back to family name
		{"bespoke", "bespoke"},      // standalone custom: falls back to provider name
		{"", ""},
	}
	for _, tc := range tests {
		if got := providerGenus(tc.provider, cfg.Providers); got != tc.want {
			t.Errorf("providerGenus(%q) = %q, want %q", tc.provider, got, tc.want)
		}
	}
}

// TestHookWorkerProvenanceFromConfigDefaults covers the no-session-info path:
// the provider's resolved EffectiveDefaults (here inferred from the pinned
// args on the builtin:claude base) supply model and effort, and the genus
// derives from the builtin family.
func TestHookWorkerProvenanceFromConfigDefaults(t *testing.T) {
	cfg := provenanceTestConfig()
	agent := &config.Agent{Name: "worker", Provider: "fable"}

	got := hookWorkerProvenance(cfg, agent, nil, nil)

	// "fable-5" is the schema choice value the pinned --model claude-fable-5
	// args resolve to — the same choice-value vocabulary opt_model uses.
	want := workerProvenance{Model: "fable-5", Genus: "anthropic", Effort: "low"}
	if got != want {
		t.Fatalf("hookWorkerProvenance = %+v, want %+v", got, want)
	}
}

// TestHookWorkerProvenanceFromOptionDefaults covers the option_defaults shape
// (the codex-luna city.toml pattern): model/effort come from the provider's
// option_defaults even when no flag in Args declares them.
func TestHookWorkerProvenanceFromOptionDefaults(t *testing.T) {
	cfg := provenanceTestConfig()
	agent := &config.Agent{Name: "worker", Provider: "luna"}

	got := hookWorkerProvenance(cfg, agent, nil, nil)

	want := workerProvenance{Model: "gpt-5.6-luna", Genus: "openai", Effort: "max"}
	if got != want {
		t.Fatalf("hookWorkerProvenance = %+v, want %+v", got, want)
	}
}

// TestHookWorkerProvenanceSessionCommandWins: the session bead's stored
// resolved command is the launch ground truth, so schema choices explicitly
// present in it (a per-dispatch --model override, an effort flag) override the
// template's EffectiveDefaults, while options absent from the command still
// fall back to those defaults.
func TestHookWorkerProvenanceSessionCommandWins(t *testing.T) {
	cfg := provenanceTestConfig()
	agent := &config.Agent{Name: "worker", Provider: "fable"}
	info := &session.Info{
		Provider: "fable",
		Command:  `claude --dangerously-skip-permissions --model claude-opus-5 --settings "/x/settings.json"`,
	}

	got := hookWorkerProvenance(cfg, agent, info, nil)

	// Model comes from the live command (as its schema choice value); effort
	// keeps the template default.
	want := workerProvenance{Model: "opus-5", Genus: "anthropic", Effort: "low"}
	if got != want {
		t.Fatalf("hookWorkerProvenance = %+v, want %+v", got, want)
	}
}

// TestHookWorkerProvenanceSessionProviderWins: when the session bead names a
// different provider than the agent template (the session is the live truth),
// provenance resolves against the session's provider.
func TestHookWorkerProvenanceSessionProviderWins(t *testing.T) {
	cfg := provenanceTestConfig()
	agent := &config.Agent{Name: "worker", Provider: "fable"}
	info := &session.Info{Provider: "luna"}

	got := hookWorkerProvenance(cfg, agent, info, nil)

	want := workerProvenance{Model: "gpt-5.6-luna", Genus: "openai", Effort: "max"}
	if got != want {
		t.Fatalf("hookWorkerProvenance = %+v, want %+v", got, want)
	}
}

// TestHookWorkerProvenanceEnvProviderFallback: with no session info, the
// runtime's GC_PROVIDER (set on every live session) outranks the agent
// template's configured provider.
func TestHookWorkerProvenanceEnvProviderFallback(t *testing.T) {
	cfg := provenanceTestConfig()
	agent := &config.Agent{Name: "worker", Provider: "fable"}

	got := hookWorkerProvenance(cfg, agent, nil, []string{"GC_PROVIDER=luna"})

	want := workerProvenance{Model: "gpt-5.6-luna", Genus: "openai", Effort: "max"}
	if got != want {
		t.Fatalf("hookWorkerProvenance = %+v, want %+v", got, want)
	}
}

// TestHookWorkerProvenanceUnknownModelOmitted: a provider with no model pinned
// anywhere (the plain claude CLI picking its own default) yields genus and any
// known options but an empty model — gc must not fabricate a model it never
// resolved.
func TestHookWorkerProvenanceUnknownModelOmitted(t *testing.T) {
	cfg := &config.City{
		Providers: map[string]config.ProviderSpec{
			"plain-claude": {Base: strPtr("builtin:claude")},
		},
	}
	agent := &config.Agent{Name: "worker", Provider: "plain-claude"}

	got := hookWorkerProvenance(cfg, agent, nil, nil)

	if got.Model != "" {
		t.Fatalf("Model = %q, want empty (no model pinned anywhere)", got.Model)
	}
	if got.Genus != "anthropic" {
		t.Fatalf("Genus = %q, want anthropic", got.Genus)
	}
	// The claude builtin declares effort=max in its OptionDefaults.
	if got.Effort != "max" {
		t.Fatalf("Effort = %q, want max (builtin claude option default)", got.Effort)
	}
}

// TestHookWorkerProvenanceNoProviderAnywhere: no session info, no GC_PROVIDER,
// no agent/workspace provider — provenance is empty and the caller stamps
// nothing rather than guessing.
func TestHookWorkerProvenanceNoProviderAnywhere(t *testing.T) {
	cfg := &config.City{}
	agent := &config.Agent{Name: "worker"}

	if got := hookWorkerProvenance(cfg, agent, nil, nil); got != (workerProvenance{}) {
		t.Fatalf("hookWorkerProvenance = %+v, want zero", got)
	}
}

// TestHookWorkerProvenanceUnresolvableProviderStillYieldsGenus: a provider
// name that cannot resolve to a spec (an unknown name, say after a config
// edit) still yields a genus label from the name itself so the cross-genus
// record survives, with model/effort empty.
func TestHookWorkerProvenanceUnresolvableProviderStillYieldsGenus(t *testing.T) {
	cfg := provenanceTestConfig()
	agent := &config.Agent{Name: "worker", Provider: "vanished"}

	got := hookWorkerProvenance(cfg, agent, nil, nil)

	want := workerProvenance{Genus: "vanished"}
	if got != want {
		t.Fatalf("hookWorkerProvenance = %+v, want %+v", got, want)
	}
}
