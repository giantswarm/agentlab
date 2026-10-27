package lab

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/giantswarm/agentlab/internal/telemetry"
)

// telemetryDeploymentPath records the lab's usage-data deployment
// (telemetry.Deployment): written by the first `up` after a `down`, removed by
// `down`, read by every command that enters the lab.
const telemetryDeploymentPath = StateDir + "/telemetry-deployment.json"

// TelemetryDeployment is the deployment recorded for this lab, false when
// there is none or the record is unreadable.
func TelemetryDeployment() (telemetry.Deployment, bool) {
	raw, err := os.ReadFile(telemetryDeploymentPath)
	if err != nil {
		return telemetry.Deployment{}, false
	}
	var d telemetry.Deployment
	if err := json.Unmarshal(raw, &d); err != nil || d.ID == "" || d.Salt == "" {
		return telemetry.Deployment{}, false
	}
	return d, true
}

// StartTelemetryDeployment is TelemetryDeployment for `up`: the recorded
// deployment, or a new one recorded now. False when usage data is off, which
// records nothing, or when the record cannot be written, which only costs the
// correlation and never the boot.
func StartTelemetryDeployment() (telemetry.Deployment, bool) {
	if !telemetry.Enabled() {
		return telemetry.Deployment{}, false
	}
	if d, ok := TelemetryDeployment(); ok {
		return d, true
	}
	d, err := telemetry.NewDeployment()
	if err == nil {
		err = writeTelemetryDeployment(d)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: not recording the lab's usage-data deployment: %v\n", err)
		return telemetry.Deployment{}, false
	}
	return d, true
}

func writeTelemetryDeployment(d telemetry.Deployment) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(StateDir, 0o750); err != nil {
		return err
	}
	return os.WriteFile(telemetryDeploymentPath, raw, 0o600)
}

// endTelemetryDeployment removes the record: the next `up` is a new
// deployment.
func endTelemetryDeployment() {
	if err := os.Remove(telemetryDeploymentPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		note("removing %s: %v", telemetryDeploymentPath, err)
	}
}

// backstageTelemetry is the overlay's app.telemetrydeck: the lab's Backstage
// reports into agentlab's TelemetryDeck app under the lab's deployment.
type backstageTelemetry struct {
	AppID, Salt, DeploymentID string
	TestMode                  bool
}

// backstageTelemetryFor is the overlay's app.telemetrydeck, nil — an empty
// app ID, telemetry off in the portal — when usage data is off or the lab
// records no deployment (a render before the first `up`).
func backstageTelemetryFor() *backstageTelemetry {
	if !telemetry.Enabled() {
		return nil
	}
	d, ok := TelemetryDeployment()
	if !ok {
		return nil
	}
	return &backstageTelemetry{
		AppID:        telemetry.AppID(),
		Salt:         d.Salt,
		DeploymentID: d.ID,
		TestMode:     telemetry.TestMode(),
	}
}
