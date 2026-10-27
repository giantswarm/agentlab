package telemetry

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/google/uuid"
)

// Deployment is one lab from its first `agentlab up` to its `agentlab down`.
// ID goes out with every signal of the lab, the CLI's (EnterLab) and its
// Backstage's, so the two can be read together. Salt hashes the lab's
// Backstage users, scoping them to the deployment: every lab has the same Dex
// users, and an unscoped admin@lab.local would be one person across all labs.
// The salt never leaves the lab — sent next to the hashes, it would let a
// guessed GitHub login be checked against them.
type Deployment struct {
	ID   string `json:"id"`
	Salt string `json:"salt"`
}

// NewDeployment draws a deployment's ID and salt.
func NewDeployment() (Deployment, error) {
	salt := make([]byte, 32)
	if _, err := rand.Read(salt); err != nil {
		return Deployment{}, err
	}
	return Deployment{ID: uuid.NewString(), Salt: hex.EncodeToString(salt)}, nil
}
