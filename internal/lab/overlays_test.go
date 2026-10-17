package lab

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	releasecommon "helm.sh/helm/v4/pkg/release/common"

	"github.com/giantswarm/agentlab/internal/config"
)

// A proof overlay in the shape a lab was handed on with: component pins the
// chart's own ranges do not admit.
const (
	proofOverlay     = "proof/released-overlay.yaml"
	proofOverlayYAML = `# a proof's pins
components:
  kagent:
    versionRange: "1.15.0-rc.4"
  kagent-crds:
    versionRange: "1.15.0-rc.4"
  agent-manager:
    versionRange: "2.5.0-rc.2"
`
	kagentPin       = "components.kagent.versionRange=1.15.0-rc.4"
	agentManagerPin = "components.agent-manager.versionRange=2.5.0-rc.2"
	goneOverlay     = "gone/overlay.yaml"
	earlierOverlay  = "earlier.yaml"
	droppedWord     = "--released` drops every overlay"
)

// TestOverlaySets: an overlay reads as its leaves in key order — a list as
// one, an empty map as one, a null as the key's deletion.
func TestOverlaySets(t *testing.T) {
	sets, err := overlaySets([]byte(proofOverlayYAML + "kagent:\n  harness: {}\n  tools: [a, b]\n  image: null\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		agentManagerPin,
		"components.kagent-crds.versionRange=1.15.0-rc.4",
		kagentPin,
		"kagent.harness={}",
		"kagent.image=null",
		"kagent.tools=[2 items]",
	}
	if !slices.Equal(sets, want) {
		t.Errorf("sets = %q, want %q", sets, want)
	}
	if _, err := overlaySets([]byte("components: [")); err == nil {
		t.Error("a malformed overlay: want an error")
	}
}

// TestInstalledOverlaysRoundTrip: a platform run records its overlays in the
// revision's description, and the chart in place reads them back; a run
// without overlays, and a revision of an agentlab from before the record,
// read as none.
func TestInstalledOverlaysRoundTrip(t *testing.T) {
	cfg := &config.Config{}
	cfg.Platform.ChartVersion = releaseInPlace
	cfg.Platform.ValuesFiles = []string{proofOverlay, "/abs/second.yaml"}
	desc := installDescription(cfg)
	if !strings.Contains(desc, "the pinned release; overlays: "+proofOverlay+", /abs/second.yaml") {
		t.Errorf("description = %q, want the source and both overlays", desc)
	}
	if got := (installedChart{Description: desc}).Overlays(); !slices.Equal(got, cfg.Platform.ValuesFiles) {
		t.Errorf("Overlays() = %q, want %q", got, cfg.Platform.ValuesFiles)
	}
	cfg.Platform.ValuesFiles = nil
	if got := (installedChart{Description: installDescription(cfg)}).Overlays(); got != nil {
		t.Errorf("a run without overlays: Overlays() = %q", got)
	}
	if got := (installedChart{Description: "Install complete"}).Overlays(); got != nil {
		t.Errorf("a foreign revision: Overlays() = %q", got)
	}
}

// TestReadOverlays: status reads every listed overlay from the lab directory
// with what it sets, names one it cannot read, marks the ones the release in
// place carries, and names an overlay the release carries that agentlab.yaml
// dropped.
func TestReadOverlays(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(proofOverlay)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, proofOverlay), []byte(proofOverlayYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Platform.ValuesFiles = []string{proofOverlay, goneOverlay}
	installed := &installedChart{Description: "agentlab v1.0.0: the pinned release" + overlaysNote([]string{proofOverlay, earlierOverlay})}

	overlays := readOverlays(cfg, dir, installed)
	if len(overlays) != 2 {
		t.Fatalf("overlays = %+v, want two", overlays)
	}
	if o := overlays[0]; o.File != proofOverlay || !o.Installed || o.Error != "" || !slices.Contains(o.Sets, kagentPin) || !slices.Contains(o.Sets, agentManagerPin) {
		t.Errorf("the proof overlay = %+v", o)
	}
	if o := overlays[1]; o.File != goneOverlay || o.Installed || !strings.Contains(o.Error, "no such file") {
		t.Errorf("the missing overlay = %+v", o)
	}
	if stale := staleOverlays(cfg, installed); !slices.Equal(stale, []string{earlierOverlay}) {
		t.Errorf("stale = %q, want the overlay agentlab.yaml dropped", stale)
	}
	if len(readOverlays(&config.Config{}, dir, installed)) != 0 {
		t.Error("a config without overlays reads none")
	}
	if staleOverlays(cfg, nil) != nil {
		t.Error("no release in place: nothing stale")
	}
}

// TestPrintStatusNamesOverlays: a lab running a proof overlay says so next to
// the chart it reports — the chart line's provenance names the overlays the
// revision carries, an overlay line names each file with what it pins, one
// the release does not carry yet and one it carries that agentlab.yaml
// dropped — and points at the restore step; JSON carries the same. A lab
// without overlays prints no overlay line.
func TestPrintStatusNamesOverlays(t *testing.T) {
	deployed := string(releasecommon.StatusDeployed)
	s := &Status{
		Name: "handed-on", Dir: "/labs/handed-on",
		Chart: &installedChart{
			Version: "4.145.0-rc.5", Channel: config.ChartChannelStable, Status: deployed, Revision: 9,
			Description: "agentlab v1.0.0: the pinned release" + overlaysNote([]string{proofOverlay, earlierOverlay}),
		},
		Configured: "agent-platform 4.145.0-rc.5",
		Overlays: []Overlay{
			{File: proofOverlay, Sets: []string{agentManagerPin, kagentPin}, Installed: true},
			{File: goneOverlay, Error: "open " + goneOverlay + ": no such file or directory"},
		},
		StaleOverlays: []string{earlierOverlay},
	}
	var out bytes.Buffer
	if err := PrintStatus(&out, s, false); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{
		"chart       agent-platform 4.145.0-rc.5 — Helm revision 9 " + deployed + ", agentlab v1.0.0: the pinned release; overlays: " + proofOverlay + ", earlier.yaml",
		"overlay     " + proofOverlay + " sets " + agentManagerPin + ", " + kagentPin + "\n",
		"overlay     " + goneOverlay + " — unreadable: open " + goneOverlay + ": no such file or directory (not in the release in place: `agentlab platform` applies it)",
		"overlay     " + earlierOverlay + " is in the release in place but no longer in " + config.File + ": `agentlab platform` drops it",
		"the component versions differ from the chart's own",
		droppedWord,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("status lacks %q:\n%s", want, text)
		}
	}

	out.Reset()
	if err := PrintStatus(&out, s, true); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Overlays      []Overlay
		StaleOverlays []string
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("json: %v\n%s", err, out.String())
	}
	if len(decoded.Overlays) != 2 || decoded.Overlays[0].File != proofOverlay || !slices.Contains(decoded.Overlays[0].Sets, kagentPin) || !decoded.Overlays[0].Installed || !slices.Equal(decoded.StaleOverlays, []string{earlierOverlay}) {
		t.Errorf("json = %+v", decoded)
	}

	many := make([]string, overlaySetsShown+3)
	for i := range many {
		many[i] = "k" + string(rune('a'+i)) + "=v"
	}
	out.Reset()
	if err := PrintStatus(&out, &Status{Name: "many", Overlays: []Overlay{{File: "big.yaml", Sets: many}}}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "and 3 more") || strings.Contains(out.String(), many[overlaySetsShown]) {
		t.Errorf("a long overlay is not cut at %d keys:\n%s", overlaySetsShown, out.String())
	}

	out.Reset()
	if err := PrintStatus(&out, &Status{Name: "plain", Dir: "/labs/plain", Configured: "agent-platform " + releaseInPlace}, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "overlay") {
		t.Errorf("a lab without overlays prints an overlay line:\n%s", out.String())
	}
}

// TestDropProofState: the restore step leaves agentlab.yaml on the released
// platform — no overlay, dev image, chart directory, dev channel or local
// guest image, a dev build in chartVersion back to the default release — and
// names everything it dropped; a release candidate pin is a release and
// stays, and a config without proof state drops nothing.
func TestDropProofState(t *testing.T) {
	cfg := &config.Config{}
	p := &cfg.Platform
	p.ChartVersion = devBuildInPlace
	p.ChartBranch = devBranch
	p.ChartPinned = true
	p.ChartPath = srcChartDir
	p.ValuesFiles = []string{proofOverlay}
	p.DevImages = map[string]string{componentMuster: "muster:dev", componentBackstage: "backstage:dev"}
	p.VMManager.ImageDir = "/vm/images/build"

	dropped := DropProofState(cfg)
	if p.ValuesFiles != nil || p.DevImages != nil || p.ChartPath != "" || p.ChartBranch != "" || p.ChartPinned || p.VMManager.ImageDir != "" || p.ChartVersion != config.DefaultChartVersion {
		t.Errorf("proof state left: %+v", p)
	}
	for _, want := range []string{
		"overlay " + proofOverlay,
		"dev image backstage -> backstage:dev",
		"dev image muster -> muster:dev",
		"chart directory " + srcChartDir,
		"dev channel of branch " + devBranch,
		"dev build " + devBuildInPlace + " in chartVersion (now " + config.DefaultChartVersion + ")",
		"guest image build /vm/images/build",
	} {
		if !slices.Contains(dropped, want) {
			t.Errorf("dropped = %q, lacks %q", dropped, want)
		}
	}

	rc := &config.Config{}
	rc.Platform.ChartVersion = rcInPlace
	if dropped := DropProofState(rc); len(dropped) != 0 || rc.Platform.ChartVersion != rcInPlace {
		t.Errorf("a release candidate pin without proof state: dropped %q, chartVersion %s", dropped, rc.Platform.ChartVersion)
	}
}
