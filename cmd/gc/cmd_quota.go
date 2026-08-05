package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/gastownhall/gascity/internal/quota"
	"github.com/spf13/cobra"
)

// bindingMarker flags the window that constrains a provider first. Windows gate
// conjunctively, so the marked one is the hottest — never the slackest.
const bindingMarker = "<-"

func newQuotaCmd(stdout, stderr io.Writer) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "quota",
		Short: "Show how much provider allowance is left and the pace that spends it exactly",
		Long: `Read each provider's own report of its remaining allowance and derive the
rate the city may spend it at.

For every window a provider states, gc quota prints how much is used, how much
of the window has elapsed, and the ratio between them:

  ratio = burn_rate / allowed_rate = (used% / elapsed) / (remaining% / time_left)

  ratio < 1   under pace, headroom to spend
  ratio ~ 1   on pace to finish the window at ~100%
  ratio > 1   will exhaust before reset

The ratio is dimensionless, so it compares directly across providers with
different windows and plan types. Under-use is a failure mode too: allowance
left unspent in a window is forfeited, not banked.

Windows within a provider gate conjunctively — a request needs headroom in
every window that covers it — so the marked window is the one that binds.
Choosing a different model cannot dodge a provider's shared window; only
shifting work to a different provider can.

Every reading carries its age. The Codex signal is written asynchronously and
is routinely tens of minutes stale; acting on a ratio without its age is acting
on a number that may already have moved.

This is a read. It changes nothing and holds no pacing policy: what to do about
a hot ratio is a judgment call for whoever is routing work.`,
		Example: "  gc quota\n  gc quota --json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if doQuota(cmd.Context(), stdout, stderr, asJSON) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the reading as JSON")
	return cmd
}

func doQuota(ctx context.Context, stdout, stderr io.Writer, asJSON bool) int {
	if ctx == nil {
		ctx = context.Background()
	}
	reading := quota.Read(ctx, time.Now(), quota.DefaultProbes()...)

	// Surface what each probe had to skip so a partially unreadable source
	// never passes as a clean one.
	for _, p := range reading.Providers {
		for _, w := range p.Warnings {
			fmt.Fprintf(stderr, "gc quota: %s: %s\n", p.Provider, w) //nolint:errcheck // best-effort stderr
		}
	}

	if asJSON {
		if err := renderQuotaJSON(stdout, reading); err != nil {
			fmt.Fprintf(stderr, "gc quota: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return quotaExitCode(reading)
	}
	renderQuotaTable(stdout, reading)
	return quotaExitCode(reading)
}

// quotaExitCode fails only when no provider could be read at all. One
// unreadable provider is a reported condition, not a broken meter.
func quotaExitCode(reading quota.Reading) int {
	readable := 0
	for _, p := range reading.Providers {
		if p.Err == nil {
			readable++
		}
	}
	if readable == 0 {
		return 1
	}
	return 0
}

func renderQuotaTable(w io.Writer, reading quota.Reading) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tWINDOW\tUSED%\tELAPSED%\tRATIO\tPACE\tEXHAUSTS\tRESETS\tAGE") //nolint:errcheck
	for _, p := range reading.Providers {
		if p.Err != nil {
			fmt.Fprintf(tw, "%s\tunreadable\t-\t-\t-\t-\t-\t-\t-\n", p.Provider) //nolint:errcheck
			continue
		}
		binding, hasBinding := p.Binding("")
		for _, pace := range p.Windows {
			mark := ""
			if hasBinding && sameWindow(pace, binding) {
				mark = " " + bindingMarker
			}
			fmt.Fprintf(tw, "%s\t%s%s\t%.1f\t%s\t%s\t%s\t%s\t%s\t%s\n", //nolint:errcheck
				p.Provider,
				windowLabel(pace),
				mark,
				pace.UsedPercent,
				formatElapsed(pace),
				formatRatio(pace),
				paceVerdict(pace),
				formatInstant(pace.ExhaustsAt),
				formatInstant(pace.ResetsAt),
				formatAge(pace.Age),
			)
		}
	}
	tw.Flush() //nolint:errcheck

	for _, p := range reading.Providers {
		if p.Err != nil {
			fmt.Fprintf(w, "\n%s: unreadable — %v\n", p.Provider, p.Err) //nolint:errcheck
			continue
		}
		fmt.Fprintf(w, "\n%s", quotaProviderNote(p)) //nolint:errcheck
	}
	fmt.Fprintf(w, "\n%s marks the window that binds. Ratio ~1 finishes the window at 100%%;\n"+ //nolint:errcheck
		"under 1 leaves allowance unspent, which is forfeited rather than banked.\n", bindingMarker)
}

// quotaProviderNote is the one-paragraph read of a provider: what binds, how
// hot it is, and what the projection is worth.
func quotaProviderNote(p quota.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s", p.Provider)
	if p.Plan != "" {
		fmt.Fprintf(&b, " (%s)", p.Plan)
	}
	binding, ok := p.Binding("")
	if !ok {
		b.WriteString(": no windows reported\n")
		return b.String()
	}
	fmt.Fprintf(&b, ": %s binds", windowLabel(binding))
	switch binding.State {
	case quota.RatioOK:
		fmt.Fprintf(&b, " at ratio %.2f (%s)", binding.Ratio, paceVerdict(binding))
		if dead := binding.DeadTime(); dead > 0 {
			fmt.Fprintf(&b, ", projected to exhaust %s before reset and idle until then",
				formatSpan(dead))
		}
		fmt.Fprintf(&b, "; %.0f%% of the window elapsed", binding.ElapsedPercent)
	default:
		fmt.Fprintf(&b, " (%s)", binding.State)
	}
	for _, sub := range hotterSubCaps(p, binding) {
		fmt.Fprintf(&b, "\n  sub-cap %s is hotter (%s) and blocks that model before %s does",
			windowLabel(sub), subCapPressure(sub), windowLabel(binding))
	}
	if p.Source != "" {
		fmt.Fprintf(&b, "\n  source: %s", p.Source)
	}
	if inferred := collectInferred(p); len(inferred) > 0 {
		fmt.Fprintf(&b, "\n  inferred by gc (not provider-stated): %s", strings.Join(inferred, ", "))
	}
	b.WriteString("\n")
	return b.String()
}

// hotterSubCaps returns the model-scoped windows that constrain their model
// harder than the provider's shared binding window does.
//
// A sub-cap is never spare capacity — spending it draws the shared budget down
// just the same — but it can still be the first thing to block a model, so it
// is reported as a possible blocker rather than left to look like slack.
func hotterSubCaps(p quota.Report, binding quota.Pace) []quota.Pace {
	var out []quota.Pace
	for _, w := range p.Windows {
		if w.ModelScope == "" {
			continue
		}
		if w.State == quota.RatioExhausted || (w.State == quota.RatioOK && binding.State == quota.RatioOK && w.Ratio > binding.Ratio) {
			out = append(out, w)
		}
	}
	return out
}

// subCapPressure describes a sub-cap in the terms that matter for it: exhausted
// blocks outright, otherwise its ratio says how much harder it is running.
func subCapPressure(p quota.Pace) string {
	if p.State == quota.RatioExhausted {
		return "exhausted"
	}
	return fmt.Sprintf("ratio %.2f", p.Ratio)
}

// collectInferred lists, once each, the attributes gascity had to derive across
// a provider's windows.
func collectInferred(p quota.Report) []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range p.Windows {
		for _, item := range w.Inferred {
			if seen[item] {
				continue
			}
			seen[item] = true
			out = append(out, item)
		}
	}
	return out
}

func sameWindow(a, b quota.Pace) bool {
	return a.Name == b.Name && a.ModelScope == b.ModelScope
}

func windowLabel(p quota.Pace) string {
	if p.ModelScope == "" {
		return p.Name
	}
	return fmt.Sprintf("%s (%s)", p.Name, p.ModelScope)
}

func formatElapsed(p quota.Pace) string {
	if p.Duration <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", p.ElapsedPercent)
}

func formatRatio(p quota.Pace) string {
	if p.State != quota.RatioOK {
		return "-"
	}
	return fmt.Sprintf("%.2f", p.Ratio)
}

// paceVerdict names which side of the target rate a window is on. The band
// around 1.0 is a display convention only — nothing branches on it.
func paceVerdict(p quota.Pace) string {
	switch p.State {
	case quota.RatioOK:
		switch {
		case p.Ratio > 1.05:
			return "over"
		case p.Ratio < 0.95:
			return "under"
		default:
			return "on pace"
		}
	default:
		return string(p.State)
	}
}

func formatInstant(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("Jan 2 15:04")
}

func formatAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return formatDuration(d)
}

// formatSpan renders a projected span with one decimal place. Unlike
// formatDuration, which floors to whole units for at-a-glance display, a span
// here is the quantity being reasoned about — "5.6h of dead time" is the
// finding, and truncating it to "5h" throws away the part that argues.
func formatSpan(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 48*time.Hour {
		return fmt.Sprintf("%.1fh", d.Hours())
	}
	return fmt.Sprintf("%.1fd", d.Hours()/24)
}

// quotaJSONSchemaVersion is the wire version of the --json result. Bump it
// when a field changes meaning; the contract is checked in at
// schemas/quota/result.schema.json.
const quotaJSONSchemaVersion = "1"

// quotaReadingJSON is the machine-readable form of a reading. It is a flat,
// fully-typed projection so a consumer never re-parses the table.
type quotaReadingJSON struct {
	SchemaVersion string              `json:"schema_version"`
	TakenAt       time.Time           `json:"taken_at"`
	Providers     []quotaProviderJSON `json:"providers"`
}

type quotaProviderJSON struct {
	Provider string            `json:"provider"`
	Plan     string            `json:"plan,omitempty"`
	Source   string            `json:"source,omitempty"`
	Windows  []quotaWindowJSON `json:"windows"`
	// Binding is the window that constrains every request to this provider.
	Binding  *quotaWindowJSON `json:"binding,omitempty"`
	Warnings []string         `json:"warnings,omitempty"`
	Error    string           `json:"error,omitempty"`
}

type quotaWindowJSON struct {
	Window                string   `json:"window"`
	ModelScope            string   `json:"model_scope,omitempty"`
	UsedPercent           float64  `json:"used_percent"`
	ElapsedPercent        float64  `json:"elapsed_percent"`
	WindowSeconds         float64  `json:"window_seconds"`
	RemainingSeconds      float64  `json:"remaining_seconds"`
	BurnPercentPerHour    float64  `json:"burn_percent_per_hour"`
	AllowedPercentPerHour float64  `json:"allowed_percent_per_hour"`
	Ratio                 float64  `json:"ratio"`
	State                 string   `json:"state"`
	ResetsAt              string   `json:"resets_at,omitempty"`
	ExhaustsAt            string   `json:"exhausts_at,omitempty"`
	DeadSeconds           float64  `json:"dead_seconds"`
	AgeSeconds            float64  `json:"age_seconds"`
	Confidence            float64  `json:"confidence"`
	Source                string   `json:"source,omitempty"`
	Inferred              []string `json:"inferred,omitempty"`
}

func renderQuotaJSON(w io.Writer, reading quota.Reading) error {
	out := quotaReadingJSON{
		SchemaVersion: quotaJSONSchemaVersion,
		TakenAt:       reading.TakenAt,
		Providers:     make([]quotaProviderJSON, 0, len(reading.Providers)),
	}
	for _, p := range reading.Providers {
		pj := quotaProviderJSON{
			Provider: p.Provider,
			Plan:     p.Plan,
			Source:   p.Source,
			Warnings: p.Warnings,
			Windows:  make([]quotaWindowJSON, 0, len(p.Windows)),
		}
		if p.Err != nil {
			pj.Error = p.Err.Error()
		}
		for _, pace := range p.Windows {
			pj.Windows = append(pj.Windows, quotaWindowToJSON(pace, p.Source))
		}
		if binding, ok := p.Binding(""); ok {
			bj := quotaWindowToJSON(binding, p.Source)
			pj.Binding = &bj
		}
		out.Providers = append(out.Providers, pj)
	}
	if err := writeCLIJSONLine(w, out); err != nil {
		return fmt.Errorf("encoding quota reading: %w", err)
	}
	return nil
}

func quotaWindowToJSON(p quota.Pace, source string) quotaWindowJSON {
	out := quotaWindowJSON{
		Window:                p.Name,
		ModelScope:            p.ModelScope,
		UsedPercent:           p.UsedPercent,
		ElapsedPercent:        p.ElapsedPercent,
		WindowSeconds:         p.Duration.Seconds(),
		RemainingSeconds:      p.Remaining.Seconds(),
		BurnPercentPerHour:    p.BurnPercentPerHour,
		AllowedPercentPerHour: p.AllowedPercentPerHour,
		Ratio:                 p.Ratio,
		State:                 string(p.State),
		DeadSeconds:           p.DeadTime().Seconds(),
		AgeSeconds:            p.Age.Seconds(),
		Confidence:            p.Confidence,
		Source:                source,
		Inferred:              p.Inferred,
	}
	if !p.ResetsAt.IsZero() {
		out.ResetsAt = p.ResetsAt.UTC().Format(time.RFC3339)
	}
	if !p.ExhaustsAt.IsZero() {
		out.ExhaustsAt = p.ExhaustsAt.UTC().Format(time.RFC3339)
	}
	return out
}
