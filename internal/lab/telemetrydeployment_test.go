package lab

import (
	"testing"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/telemetry"
)

// telemetryOn turns usage data on for one test whatever the environment says,
// in a lab directory of its own.
func telemetryOn(t *testing.T) {
	t.Helper()
	t.Setenv(telemetry.OptOutEnv, "")
	t.Setenv("DO_NOT_TRACK", "")
	t.Setenv(telemetry.TestModeEnv, "")
	t.Chdir(t.TempDir())
}

// TestTelemetryDeploymentLivesFromUpToDown: the first `up` records a
// deployment, every later one keeps it, and after `down` the next `up` starts
// a new one.
func TestTelemetryDeploymentLivesFromUpToDown(t *testing.T) {
	telemetryOn(t)

	if _, ok := TelemetryDeployment(); ok {
		t.Fatal("a lab that never booted has a deployment")
	}
	first, ok := StartTelemetryDeployment()
	if !ok {
		t.Fatal("up recorded no deployment")
	}
	if again, _ := StartTelemetryDeployment(); again != first {
		t.Errorf("a second up changed the deployment: %+v, then %+v", first, again)
	}
	if read, _ := TelemetryDeployment(); read != first {
		t.Errorf("read back %+v, want %+v", read, first)
	}

	endTelemetryDeployment()
	if _, ok := TelemetryDeployment(); ok {
		t.Fatal("down left the deployment behind")
	}
	if next, _ := StartTelemetryDeployment(); next.ID == first.ID || next.Salt == first.Salt {
		t.Errorf("the up after down reused the deployment: %+v", next)
	}
}

func TestTelemetryDeploymentIsNotRecordedWhenOptedOut(t *testing.T) {
	telemetryOn(t)
	t.Setenv(telemetry.OptOutEnv, "1")

	if _, ok := StartTelemetryDeployment(); ok {
		t.Error("up recorded a deployment with usage data off")
	}
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

	d, _ := StartTelemetryDeployment()
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
	withSalt := func(salt string) {
		t.Helper()
		if err := writeTelemetryDeployment(telemetry.Deployment{ID: d.ID, Salt: salt}); err != nil {
			t.Fatal(err)
		}
	}
	withSalt("123e45")
	if got := dig(backstageOverlayAppConfig(t, cfg), "app", "telemetrydeck", "salt"); got != "123e45" {
		t.Errorf("app.telemetrydeck.salt = %#v, want the string \"123e45\"", got)
	}
	withSalt(d.Salt)

	t.Setenv(telemetry.TestModeEnv, "1")
	if got := dig(backstageOverlayAppConfig(t, cfg), "app", "telemetrydeck", "testMode"); got != true {
		t.Errorf("app.telemetrydeck.testMode = %v under %s, want true", got, telemetry.TestModeEnv)
	}

	t.Setenv(telemetry.OptOutEnv, "1")
	telemetryOff("opted out")
}
