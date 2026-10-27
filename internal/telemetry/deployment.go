package telemetry

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/google/uuid"
)

// Deployment is one lab cluster, from the `agentlab up` that creates it to
// its deletion.
// ID goes out with every signal of the lab, the CLI's (EnterLab) and its
// Backstage's, so the two can be read together. Salt hashes the lab's
// Backstage users, scoping them to the deployment: every lab has the same Dex
// users, and an unscoped admin@lab.local would be one person across all labs.
// The salt never leaves the lab — sent next to the hashes, it would let a
// guessed GitHub login be checked against them.
//
// TestMode is AGENTLAB_TELEMETRY_TESTMODE as the deployment started: the
// lab's portal keeps it for the deployment's lifetime, so a later command run
// from another shell does not change the portal's config and restart it.
type Deployment struct {
	ID       string `json:"id"`
	Salt     string `json:"salt"`
	TestMode bool   `json:"testMode,omitempty"`
}

// NewDeployment draws a deployment's ID and salt, in the environment's test
// mode.
func NewDeployment() (Deployment, error) {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return Deployment{}, err
	}
	return Deployment{ID: uuid.NewString(), Salt: hex.EncodeToString(salt), TestMode: TestMode()}, nil
}
