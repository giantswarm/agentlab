package lab

import (
	"encoding/json"
	"errors"
	"os"

	"github.com/giantswarm/agentlab/internal/config"
	"github.com/giantswarm/agentlab/internal/telemetry"
)

// telemetryDeploymentPath records the lab's usage-data deployment
// (telemetry.Deployment): written by the `up` that creates the cluster,
// removed by `down`, read by every command that enters the lab.
const telemetryDeploymentPath = StateDir + "/telemetry-deployment.json"

// labClusterExists says whether the lab's cluster exists; tests stand in for
// kind.
var labClusterExists = kindClusterExists

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

// EnterTelemetryLab hands the command's usage signal the lab's usage-data
// setting, as cfg was read or saved, and its deployment, none for a lab
// without one (telemetry.EnterLab).
func EnterTelemetryLab(cfg *config.Config) {
	d, _ := TelemetryDeployment()
	telemetry.EnterLab(d.ID, cfg.TelemetryOn())
}

// StartTelemetryDeployment is `up`'s: the recorded deployment while its
// cluster exists, a new one recorded now when the cluster is about to be
// created (or the lab records none) — so a cluster deleted past `down` (kind
// delete, a reset docker) does not hand its deployment to the next, also not
// while usage data is off. `up`'s usage signal leaves with it
// (telemetry.StartDeployment). Nothing is recorded with usage data off, in
// the environment or in cfg, and a record that cannot be written only costs
// the correlation, said under AGENTLAB_TELEMETRY_DEBUG.
func StartTelemetryDeployment(cfg *config.Config) {
	d, ok := TelemetryDeployment()
	if ok && !labClusterExists(cfg.ClusterName) {
		endTelemetryDeployment()
		ok = false
	}
	if !cfg.TelemetryOn() || !telemetry.Enabled() {
		return
	}
	if ok {
		telemetry.StartDeployment(d.ID)
		return
	}
	d, err := telemetry.NewDeployment()
	if err == nil {
		err = writeTelemetryDeployment(d)
	}
	if err != nil {
		telemetry.Logf("not recording the lab's deployment: %v", err)
		telemetry.StartDeployment("")
		return
	}
	telemetry.StartDeployment(d.ID)
}

// writeTelemetryDeployment replaces the record at once (a temporary file
// renamed over it), so a command reading it meanwhile sees the old record or
// the new one, never half of one.
func writeTelemetryDeployment(d telemetry.Deployment) error {
	raw, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(StateDir, 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(StateDir, ".telemetry-deployment-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), telemetryDeploymentPath)
}

// endTelemetryDeployment removes the record: the next `up` is a new
// deployment.
func endTelemetryDeployment() {
	if err := os.Remove(telemetryDeploymentPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		telemetry.Logf("removing %s: %v", telemetryDeploymentPath, err)
	}
}

// backstageTelemetry is the overlay's app.telemetrydeck: the lab's Backstage
// reports into agentlab's TelemetryDeck app under the lab's deployment.
type backstageTelemetry struct {
	AppID, Salt, DeploymentID string
	TestMode                  bool
}

// BackstageTelemetry is the overlay's app.telemetrydeck, nil — an empty app
// ID, telemetry off in the portal — when usage data is off (in the
// environment or the lab's agentlab.yaml) or the lab records no deployment (a render before the first `up`). A method, so only the
// templates that use it read the record. Test mode is the deployment's, not
// the environment's: the overlay changes only with the deployment or an
// opt-out, never with the shell a command runs in.
func (d *tmplData) BackstageTelemetry() *backstageTelemetry {
	if !d.TelemetryOn() || !telemetry.Enabled() {
		return nil
	}
	dep, ok := TelemetryDeployment()
	if !ok {
		return nil
	}
	return &backstageTelemetry{
		AppID:        telemetry.AppID(),
		Salt:         dep.Salt,
		DeploymentID: dep.ID,
		TestMode:     dep.TestMode,
	}
}
