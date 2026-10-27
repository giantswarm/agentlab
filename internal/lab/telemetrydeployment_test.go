package lab

import (
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/telemetry"
)

// telemetryOn turns usage data on for one test whatever the environment says,
// in a lab directory of its own, with a lab cluster that exists while
// *cluster says so.
func telemetryOn(t *testing.T) (cluster *bool) {
	t.Helper()
	t.Setenv(telemetry.OptOutEnv, "")
	t.Setenv("DO_NOT_TRACK", "")
	t.Setenv(telemetry.TestModeEnv, "")
	t.Chdir(t.TempDir())
	exists := false
	prev := labClusterExists
	labClusterExists = func(string) bool { return exists }
	t.Cleanup(func() { labClusterExists = prev })
	return &exists
}

// startDeployment is `up`'s start of the deployment, read back.
func startDeployment(t *testing.T) telemetry.Deployment {
	t.Helper()
	StartTelemetryDeployment(config.Default())
	d, ok := TelemetryDeployment()
	if !ok {
		t.Fatal("up recorded no deployment")
	}
	return d
}

// TestTelemetryDeploymentLivesWithTheCluster: the `up` that creates the
// cluster records a deployment, every `up` while the cluster exists keeps it,
// and a new cluster — after `down`, or after the cluster went missing without
// one — is a new deployment.
func TestTelemetryDeploymentLivesWithTheCluster(t *testing.T) {
	cluster := telemetryOn(t)

	if _, ok := TelemetryDeployment(); ok {
		t.Fatal("a lab that never booted has a deployment")
	}
	first := startDeployment(t)
	*cluster = true
	if again := startDeployment(t); again != first {
		t.Errorf("an up on the running cluster changed the deployment: %+v, then %+v", first, again)
	}

	*cluster = false // kind delete, a reset docker: no `down`
	second := startDeployment(t)
	if second.ID == first.ID || second.Salt == first.Salt {
		t.Errorf("a new cluster kept the deployment of the one that went missing: %+v", second)
	}

	*cluster = true
	endTelemetryDeployment()
	if _, ok := TelemetryDeployment(); ok {
		t.Fatal("down left the deployment behind")
	}
	if third := startDeployment(t); third.ID == second.ID {
		t.Errorf("the up after down reused the deployment: %+v", third)
	}
}

func TestTelemetryDeploymentIsNotRecordedWhenOptedOut(t *testing.T) {
	telemetryOn(t)
	t.Setenv(telemetry.OptOutEnv, "1")

	StartTelemetryDeployment(config.Default())
	if _, ok := TelemetryDeployment(); ok {
		t.Error("a deployment was written with usage data off")
	}
}

// TestBackstageOverlayTelemetry: the lab's portal reports into agentlab's
// app under the lab's deployment and salt, and sends nothing before the
// first `up` or with usage data off: an empty app ID, which every portal
// version honours, where a null one would make an older portal send test
// signals.
func TestBackstageOverlayTelemetry(t *testing.T) {
	telemetryOn(t)
	cfg := config.Default()
	telemetryOff := func(when string) {
		t.Helper()
		td := dig(backstageOverlayAppConfig(t, cfg), "app", "telemetrydeck")
		if got, _ := td.(map[string]any); len(got) != 1 || got["appID"] != "" {
			t.Errorf("%s, app.telemetrydeck = %#v, want only an empty appID", when, td)
		}
	}

	telemetryOff("before the first up")

	d := startDeployment(t)
	td := dig(backstageOverlayAppConfig(t, cfg), "app", "telemetrydeck")
	want := map[string]any{"appID": telemetry.AppID(), "salt": d.Salt, "deploymentID": d.ID}
	got, _ := td.(map[string]any)
	if len(got) != len(want) {
		t.Errorf("app.telemetrydeck = %v, want %v", td, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("app.telemetrydeck.%s = %v, want %v", k, got[k], v)
		}
	}

	// A salt YAML would read as a number stays a string.
	withDeployment := func(dep telemetry.Deployment) {
		t.Helper()
		if err := writeTelemetryDeployment(dep); err != nil {
			t.Fatal(err)
		}
	}
	withDeployment(telemetry.Deployment{ID: d.ID, Salt: "123e45"})
	if got := dig(backstageOverlayAppConfig(t, cfg), "app", "telemetrydeck", "salt"); got != "123e45" {
		t.Errorf("app.telemetrydeck.salt = %#v, want the string \"123e45\"", got)
	}
	withDeployment(d)

	t.Setenv(telemetry.OptOutEnv, "1")
	telemetryOff("opted out")
}

// TestBackstageOverlayTestModeIsTheDeployments: the portal's test mode is the
// one the deployment started in, so a command run from a shell with another
// AGENTLAB_TELEMETRY_TESTMODE renders the same overlay and restarts nothing.
func TestBackstageOverlayTestModeIsTheDeployments(t *testing.T) {
	telemetryOn(t)
	cfg := config.Default()
	testMode := func() any {
		return dig(backstageOverlayAppConfig(t, cfg), "app", "telemetrydeck", "testMode")
	}

	t.Setenv(telemetry.TestModeEnv, "1")
	startDeployment(t)
	t.Setenv(telemetry.TestModeEnv, "")
	if got := testMode(); got != true {
		t.Errorf("a deployment started in test mode renders testMode = %v outside it, want true", got)
	}

	endTelemetryDeployment()
	startDeployment(t)
	t.Setenv(telemetry.TestModeEnv, "1")
	if got := testMode(); got != nil {
		t.Errorf("a deployment started outside test mode renders testMode = %v in it, want none", got)
	}
}
