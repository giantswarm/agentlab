package lab

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// The report of `agentlab workspaces-measure`: per workspace, every figure
// with what it measured from and to, its samples and their median, p90 and
// max, or why the cluster could not report it; the standing storage with
// each mirror's measured size against the provider's, the headroom and the
// online expansion. Written as JSON and as a Markdown table.

// The sizing defaults the measurements check (workspaces design W8): a
// repository not yet mirrored counts measureSizingFactor × the size its
// provider reports; the headroom is max(measureHeadroomFloor,
// measureHeadroomShare × the need).
const (
	measureSizingFactor  = 1.5
	measureHeadroomShare = 0.5
	measureGiB           = int64(1) << 30
	measureHeadroomFloor = 4 * measureGiB
)

// The units of a figure.
const (
	unitSeconds = "s"
	unitBytes   = "B"
)

// measureFigureDef is what a figure measures, from and to.
type measureFigureDef struct {
	key, name, from, to, unit string
}

// The figures' keys.
const (
	figFirstSync      = "firstSync"
	figNoOpCycle      = "noOpCycle"
	figChangedCycle   = "changedCycle"
	figAuthorize      = "authorize"
	figSessionStart   = "sessionStart"
	figSessionSetup   = "sessionSetup"
	figOnDemandFetch  = "onDemandFetch"
	figCheckoutVolume = "checkoutVolume"
	figCheckoutLocal  = "checkoutLocal"
	figStatusVolume   = "statusVolume"
	figStatusLocal    = "statusLocal"
	figBuildVolume    = "buildVolume"
	figBuildLocal     = "buildLocal"
	figSessionBytes   = "sessionDirectoryBytes"
)

// measureFigureDefs are the figures in the order the report lists them.
var measureFigureDefs = []measureFigureDef{
	{figFirstSync, "First sync", "create_workspace called", "get_workspace reports the first cycle's result, every mirror written", unitSeconds},
	{figNoOpCycle, "No-op cycle", "sync_workspace called, nothing pushed and the manager's listing cache expired", "get_workspace reports the cycle's NoChange", unitSeconds},
	{figChangedCycle, "Changed cycle", "sync_workspace called after a push to one repository", "get_workspace reports the cycle's result, that mirror at the pushed head", unitSeconds},
	{figAuthorize, "authorize_workspace", "the call through muster", "its granted answer", unitSeconds},
	{figSessionStart, "Session directory setup", "start_session called", "the setup's record in the session directory: the actor Running, every clone in place, the changed repository fetched", unitSeconds},
	{figSessionSetup, "Setup in the actor", "the first turn sent", "the setup's record in the session directory", unitSeconds},
	{figOnDemandFetch, "On-demand fetch", "`git fetch --quiet --prune origin` started in the Session's clone after a push (the setup's own command, in the Session's sandbox)", "the fetch ended with the pushed head", unitSeconds},
	{figCheckoutVolume, "First checkout, volume", "`git switch <default branch>` in the Session's clone, no working tree yet", "the working tree checked out, on the workspace volume", unitSeconds},
	{figCheckoutLocal, "First checkout, local disk", "the same in a clone of a copy of the mirror", "the working tree checked out, on the node's local disk", unitSeconds},
	{figStatusVolume, "git status, volume", "`git status --porcelain` after the checkout", "its answer, on the workspace volume", unitSeconds},
	{figStatusLocal, "git status, local disk", "the same", "its answer, on the node's local disk", unitSeconds},
	{figBuildVolume, "Build, volume", "`go build ./...` with a cold build cache beside the clone", "the binary written, on the workspace volume", unitSeconds},
	{figBuildLocal, "Build, local disk", "the same", "the binary written, on the node's local disk", unitSeconds},
	{figSessionBytes, "Session directory size", "the Session's directory", "after the measured turns and the checkout (`du -sb`)", unitBytes},
}

// measureFigure is one figure of a workspace: what it measured, the samples
// of every run and their statistics, or why it is not available.
type measureFigure struct {
	Key          string    `json:"key"`
	Name         string    `json:"name"`
	From         string    `json:"from"`
	To           string    `json:"to"`
	Unit         string    `json:"unit"`
	Samples      []float64 `json:"samples,omitempty"`
	Median       *float64  `json:"median,omitempty"`
	P90          *float64  `json:"p90,omitempty"`
	Max          *float64  `json:"max,omitempty"`
	NotAvailable string    `json:"notAvailable,omitempty"`
}

// measureMirror is a repository's mirror against the size its provider
// reports.
type measureMirror struct {
	Repository string `json:"repository"`
	// ProviderKiB is the provider's size (GitHub's `size`, its packed
	// objects), as preview_workspace reports it.
	ProviderKiB int64 `json:"providerKiB"`
	// MirrorBytes is the mirror's size the sync measured (the manifest's).
	MirrorBytes int64 `json:"mirrorBytes"`
	// Factor is MirrorBytes over the provider's size, against W8's 1.5.
	Factor *float64 `json:"factor,omitempty"`
	// WithinEstimate says whether 1.5 × the provider's size covered the
	// mirror.
	WithinEstimate *bool `json:"withinEstimate,omitempty"`
}

// measureHeadroom is the check of W8's headroom on the volume after the
// measured turn.
type measureHeadroom struct {
	// NeededBytes is what the volume stores: the mirrors and the session
	// directories, measured.
	NeededBytes int64 `json:"neededBytes"`
	// WantBytes is max(4 GiB, 50 % of the need).
	WantBytes int64 `json:"wantBytes"`
	// FreeBytes is the capacity less the need.
	FreeBytes *int64 `json:"freeBytes,omitempty"`
	// Held says whether FreeBytes is at least WantBytes.
	Held         *bool  `json:"held,omitempty"`
	NotAvailable string `json:"notAvailable,omitempty"`
}

// measureExpansion is one online expansion of the workspace's volume.
type measureExpansion struct {
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// RequestedSeconds is from update_workspace raising the minimum to the
	// manager raising the claim's request; ResizedSeconds from then to the
	// claim's capacity at the new size.
	RequestedSeconds *float64 `json:"requestedSeconds,omitempty"`
	ResizedSeconds   *float64 `json:"resizedSeconds,omitempty"`
	NotAvailable     string   `json:"notAvailable,omitempty"`
}

// measureStorage is the workspace's standing storage after the last run's
// measured turn.
type measureStorage struct {
	StorageClass string `json:"storageClass"`
	Driver       string `json:"driver"`
	// ProvisionsSize is the class allowing volume expansion: the manager
	// sizes and grows the volume (W8); elastic otherwise.
	ProvisionsSize bool `json:"provisionsSize"`
	// CapacityBytes is the claim's capacity, as the cluster reports it.
	CapacityBytes        *int64 `json:"capacityBytes,omitempty"`
	CapacityNotAvailable string `json:"capacityNotAvailable,omitempty"`
	// MirrorsBytes and SessionsBytes are the volume's use, measured on it.
	MirrorsBytes  int64 `json:"mirrorsBytes"`
	SessionsBytes int64 `json:"sessionsBytes"`
	// FilesystemBytes and FilesystemAvailBytes are what the filesystem the
	// volume lives on reports (df): on a shared export or an elastic one,
	// not the volume's own.
	FilesystemBytes      int64 `json:"filesystemBytes"`
	FilesystemAvailBytes int64 `json:"filesystemAvailBytes"`
	// NeededSize and VolumeSize are the manager's (get_workspace), where the
	// class provisions a size.
	NeededSize string            `json:"neededSize,omitempty"`
	VolumeSize string            `json:"volumeSize,omitempty"`
	Mirrors    []measureMirror   `json:"mirrors"`
	Headroom   measureHeadroom   `json:"headroom"`
	Expansion  *measureExpansion `json:"expansion,omitempty"`
}

// measureWorkspaceReport is one workspace's measurements.
type measureWorkspaceReport struct {
	Name         string          `json:"name"`
	Kind         string          `json:"kind"`
	Repositories []string        `json:"repositories"`
	Pushed       string          `json:"pushed,omitempty"`
	Worked       string          `json:"worked"`
	Figures      []measureFigure `json:"figures"`
	Storage      measureStorage  `json:"storage"`
}

// measureReport is the whole report.
type measureReport struct {
	Started          time.Time                `json:"started"`
	Ended            time.Time                `json:"ended"`
	Target           string                   `json:"target"`
	WorkspaceManager string                   `json:"workspaceManager,omitempty"`
	Runs             int                      `json:"runs"`
	Harness          string                   `json:"harness,omitempty"`
	Workspaces       []measureWorkspaceReport `json:"workspaces"`
	Leftovers        []measureLeftover        `json:"leftovers,omitempty"`
	Error            string                   `json:"error,omitempty"`
}

// measureLeftover is an interrupted run's workspace that was still deleting:
// cleared, it goes at its manager's next deletion cycle, and the run
// measured under another name.
type measureLeftover struct {
	Name       string `json:"name"`
	MeasuredAs string `json:"measuredAs"`
}

// measureSamples collects a workspace's samples per figure, and the reason
// a figure is not available.
type measureSamples struct {
	samples map[string][]float64
	missing map[string]string
}

func newMeasureSamples() *measureSamples {
	return &measureSamples{samples: map[string][]float64{}, missing: map[string]string{}}
}

func (s *measureSamples) add(key string, v float64) { s.samples[key] = append(s.samples[key], v) }

// since adds the seconds from start to end.
func (s *measureSamples) since(key string, start, end time.Time) {
	s.add(key, end.Sub(start).Seconds())
}

// notAvailable records why the cluster cannot report a figure; the first
// reason stays.
func (s *measureSamples) notAvailable(key, why string) {
	if _, ok := s.missing[key]; !ok {
		s.missing[key] = why
	}
}

// figures are the samples as the report's figures: a figure with samples
// has its statistics, one without them its reason (or that no run reached
// it).
func (s *measureSamples) figures() []measureFigure {
	out := make([]measureFigure, 0, len(measureFigureDefs))
	for _, d := range measureFigureDefs {
		f := measureFigure{Key: d.key, Name: d.name, From: d.from, To: d.to, Unit: d.unit, Samples: s.samples[d.key]}
		if median, p90, maxV, ok := measureStats(f.Samples); ok {
			f.Median, f.P90, f.Max = &median, &p90, &maxV
		} else {
			f.NotAvailable = s.missing[d.key]
			if f.NotAvailable == "" {
				f.NotAvailable = "no run reached it"
			}
		}
		out = append(out, f)
	}
	return out
}

// measureStats are the median, the p90 (nearest rank) and the max.
func measureStats(samples []float64) (median, p90, maxV float64, ok bool) {
	n := len(samples)
	if n == 0 {
		return 0, 0, 0, false
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	if n%2 == 1 {
		median = sorted[n/2]
	} else {
		median = (sorted[n/2-1] + sorted[n/2]) / 2
	}
	rank := int(math.Ceil(0.9*float64(n))) - 1
	return median, sorted[max(rank, 0)], sorted[n-1], true
}

// judgeMirror is a mirror against W8's estimate.
func judgeMirror(repo string, providerKiB, mirrorBytes int64) measureMirror {
	m := measureMirror{Repository: repo, ProviderKiB: providerKiB, MirrorBytes: mirrorBytes}
	if providerKiB > 0 {
		factor := float64(mirrorBytes) / float64(providerKiB*1024)
		within := factor <= measureSizingFactor
		m.Factor, m.WithinEstimate = &factor, &within
	}
	return m
}

// judgeHeadroom checks the volume's capacity against the need and W8's
// headroom; without a capacity the check is not available.
func judgeHeadroom(needed int64, capacity *int64, why string) measureHeadroom {
	h := measureHeadroom{NeededBytes: needed, WantBytes: max(measureHeadroomFloor, int64(math.Ceil(measureHeadroomShare*float64(needed))))}
	if capacity == nil {
		h.NotAvailable = why
		return h
	}
	free := *capacity - needed
	held := free >= h.WantBytes
	h.FreeBytes, h.Held = &free, &held
	return h
}

// formatMeasure words a value in its unit.
func formatMeasure(v float64, unit string) string {
	if unit == unitBytes {
		return formatBytes(int64(v))
	}
	switch {
	case v < 1:
		return fmt.Sprintf("%.0f ms", v*1000)
	case v < 10:
		return fmt.Sprintf("%.2f s", v)
	default:
		return fmt.Sprintf("%.1f s", v)
	}
}

// formatBytes words a size in binary units.
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit && exp < 4; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTP"[exp])
}

// markdown is the report as Markdown: a table of figures and one of storage
// per workspace.
func (r *measureReport) markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "## Workspace measurements: %s\n\n", r.Target)
	fmt.Fprintf(&b, "%d runs, %s to %s", r.Runs, r.Started.UTC().Format(time.RFC3339), r.Ended.UTC().Format(time.RFC3339))
	if r.WorkspaceManager != "" {
		fmt.Fprintf(&b, ", workspace-manager %s", r.WorkspaceManager)
	}
	if r.Harness != "" {
		fmt.Fprintf(&b, ", Sessions on the %s Harness", r.Harness)
	}
	b.WriteString(".\n")
	if r.Error != "" {
		fmt.Fprintf(&b, "\n**The run ended early:** %s\n", r.Error)
	}
	for _, l := range r.Leftovers {
		fmt.Fprintf(&b, "\n**Leftover:** `%s` of an interrupted run was still deleting; its session directories were cleared and its manager deletes it at its next deletion cycle. This run measured it as `%s`.\n", l.Name, l.MeasuredAs)
	}
	for _, w := range r.Workspaces {
		fmt.Fprintf(&b, "\n### %s workspace `%s`: %s\n\n", w.Kind, w.Name, strings.Join(w.Repositories, ", "))
		if w.Storage.read() {
			fmt.Fprintf(&b, "StorageClass `%s`, driver `%s`. ", w.Storage.StorageClass, w.Storage.Driver)
		}
		fmt.Fprintf(&b, "Checkout, status and build in `%s`", w.Worked)
		if w.Pushed != "" {
			fmt.Fprintf(&b, "; pushes to `%s`", w.Pushed)
		}
		b.WriteString(".\n\n| Figure | From → to | Median | p90 | Max | n |\n|---|---|---|---|---|---|\n")
		for _, f := range w.Figures {
			span := mdCell(f.From + " → " + f.To)
			if f.NotAvailable != "" {
				fmt.Fprintf(&b, "| %s | %s | not available: %s | | | 0 |\n", f.Name, span, mdCell(f.NotAvailable))
				continue
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %d |\n", f.Name, span, formatMeasure(*f.Median, f.Unit), formatMeasure(*f.P90, f.Unit), formatMeasure(*f.Max, f.Unit), len(f.Samples))
		}
		b.WriteString("\n" + w.Storage.markdown())
	}
	return b.String()
}

// read says whether a run read the storage.
func (s measureStorage) read() bool { return s.StorageClass != "" }

// markdown is the storage as a two-column table.
func (s measureStorage) markdown() string {
	var b strings.Builder
	b.WriteString("| Storage | Measured |\n|---|---|\n")
	if !s.read() {
		b.WriteString("| Standing storage | not available: no run reached the last run's reading |\n")
		if e := s.Expansion; e != nil {
			b.WriteString(e.markdown())
		}
		return b.String()
	}
	switch {
	case s.CapacityBytes != nil:
		fmt.Fprintf(&b, "| Capacity (the claim's status) | %s |\n", formatBytes(*s.CapacityBytes))
	default:
		fmt.Fprintf(&b, "| Capacity | not available: %s |\n", mdCell(s.CapacityNotAvailable))
	}
	fmt.Fprintf(&b, "| Use (`du`): mirrors, session directories | %s, %s |\n", formatBytes(s.MirrorsBytes), formatBytes(s.SessionsBytes))
	fmt.Fprintf(&b, "| The filesystem under the volume (`df`): size, available | %s, %s |\n", formatBytes(s.FilesystemBytes), formatBytes(s.FilesystemAvailBytes))
	if s.NeededSize != "" || s.VolumeSize != "" {
		fmt.Fprintf(&b, "| The manager's need and volume size | %s, %s |\n", formatQuantity(s.NeededSize), formatQuantity(s.VolumeSize))
	}
	for _, m := range s.Mirrors {
		if m.Factor == nil {
			fmt.Fprintf(&b, "| Mirror `%s` | %s; the provider reports no size |\n", m.Repository, formatBytes(m.MirrorBytes))
		} else {
			verdict := "within W8's 1.5"
			if !*m.WithinEstimate {
				verdict = "above W8's 1.5"
			}
			fmt.Fprintf(&b, "| Mirror `%s` | %s against the provider's %s: factor %.2f, %s |\n", m.Repository, formatBytes(m.MirrorBytes), formatBytes(m.ProviderKiB*1024), *m.Factor, verdict)
		}
	}
	h := s.Headroom
	if h.Held != nil {
		verdict := "held"
		if !*h.Held {
			verdict = "did not hold"
		}
		fmt.Fprintf(&b, "| Headroom max(4 GiB, 50 %%) | need %s, free %s, wanted %s: %s |\n", formatBytes(h.NeededBytes), formatBytes(*h.FreeBytes), formatBytes(h.WantBytes), verdict)
	} else {
		fmt.Fprintf(&b, "| Headroom max(4 GiB, 50 %%) | need %s, wanted %s; not available: %s |\n", formatBytes(h.NeededBytes), formatBytes(h.WantBytes), mdCell(h.NotAvailable))
	}
	if e := s.Expansion; e != nil {
		b.WriteString(e.markdown())
	}
	return b.String()
}

// markdown is the expansion as a row of the storage table.
func (e measureExpansion) markdown() string {
	switch {
	case e.NotAvailable != "":
		return fmt.Sprintf("| Online expansion | not available: %s |\n", mdCell(e.NotAvailable))
	case e.RequestedSeconds == nil || e.ResizedSeconds == nil:
		return "| Online expansion | not available: the run ended before the volume grew |\n"
	}
	return fmt.Sprintf("| Online expansion %s → %s | the manager raised the claim %s after update_workspace, the driver resized it %s later |\n",
		e.From, e.To, formatMeasure(*e.RequestedSeconds, unitSeconds), formatMeasure(*e.ResizedSeconds, unitSeconds))
}

// formatQuantity words a Kubernetes quantity of bytes the manager reports
// ("67158407", "5Gi"), as it is where it does not parse.
func formatQuantity(q string) string {
	if q == "" {
		return "—"
	}
	v, err := resource.ParseQuantity(q)
	if err != nil {
		return q
	}
	return formatBytes(v.Value())
}

// mdCell keeps a table cell on one line and its pipes literal.
func mdCell(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "|", `\|`)
}

func cmpOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
