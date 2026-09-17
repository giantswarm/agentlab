package lab

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
)

// TestOnlyTheInstallSendsThePlatformSignal pins who reports the meta chart
// line a lab installs: `up` and `platform`, once per run, both through
// platformUp — after the chart is resolved and ahead of the install, so a run
// that fails to install still says which line it was about to. render, the
// proofs and every other command send nothing new. Read off the package's
// sources: every call of telemetry.Platform and the function it sits in, and
// every caller of that function in turn.
func TestOnlyTheInstallSendsThePlatformSignal(t *testing.T) {
	calls := callSites(t)
	if got, want := calls["telemetry.Platform"], map[string]int{"platformUp": 1}; !maps.Equal(got, want) {
		t.Errorf("telemetry.Platform is called from %v, want %v", got, want)
	}
	if got, want := calls["platformUp"], map[string]int{"Up": 1, "PlatformUp": 1}; !maps.Equal(got, want) {
		t.Errorf("platformUp is called from %v, want %v", got, want)
	}
}

// callSites maps a callee — `pkg.Func` for a qualified call, `Func` for one
// of this package's own — to the functions of this package that call it,
// with the number of calls in each. Test files are left out.
func callSites(t *testing.T) map[string]map[string]int {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	sites := map[string]map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				var callee string
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					callee = fun.Name
				case *ast.SelectorExpr:
					if x, ok := fun.X.(*ast.Ident); ok {
						callee = x.Name + "." + fun.Sel.Name
					}
				}
				if callee == "" {
					return true
				}
				if sites[callee] == nil {
					sites[callee] = map[string]int{}
				}
				sites[callee][fn.Name.Name]++
				return true
			})
		}
	}
	return sites
}

// TestInstalledVersion: the version the platform signal reports is the tag of
// a registry chart, and for a chart directory — where platform.chartVersion
// is ignored — what its Chart.yaml carries; nothing when there is no chart to
// read.
func TestInstalledVersion(t *testing.T) {
	if got := (platformChart{ref: config.ChartRepository, version: config.DefaultChartVersion}).installedVersion(); got != config.DefaultChartVersion {
		t.Errorf("registry chart: installedVersion() = %q, want %q", got, config.DefaultChartVersion)
	}
	// writeChart's Chart.yaml.
	const chartFileVersion = "0.1.0"
	dir := writeChart(t, t.TempDir())
	if got := (platformChart{ref: dir}).installedVersion(); got != chartFileVersion {
		t.Errorf("chart directory: installedVersion() = %q, want the Chart.yaml's %s", got, chartFileVersion)
	}
	if got := (platformChart{ref: filepath.Join(t.TempDir(), "missing")}).installedVersion(); got != "" {
		t.Errorf("missing directory: installedVersion() = %q, want none", got)
	}
}
