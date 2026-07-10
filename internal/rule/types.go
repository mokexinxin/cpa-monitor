// Package rule converts trustworthy collector and CLIProxyAPI facts into
// stable alert conditions. An empty, complete batch means healthy; an
// incomplete batch means unknown and must not be used to infer recoveries.
package rule

import (
	"errors"
	"fmt"
)

const (
	ScopeHealth  = "health"
	ScopeMemory  = "memory"
	ScopeDisk    = "disk"
	ScopeNetwork = "network"
	ScopeAuth    = "auth"
)

var (
	ErrMissingAuthIndex   = errors.New("missing auth_index")
	ErrDuplicateAuthIndex = errors.New("duplicate auth_index")
)

// Condition is the transport-neutral description persisted by the state
// layer and rendered by the mailer.
type Condition struct {
	Key       string
	Scope     string
	Summary   string
	Current   string
	Threshold string
	Details   map[string]string
}

// Batch contains all currently unhealthy conditions for one recovery scope.
// Errors are entry-level problems; when any are present Complete is false.
type Batch struct {
	Scope      string
	Complete   bool
	Conditions []Condition
	Errors     []error
}

// Err joins the batch's entry-level errors. A health-check failure is a down
// condition, not an entry error, and therefore is deliberately absent here.
func (b Batch) Err() error {
	return errors.Join(b.Errors...)
}

// AuthEntryError identifies an invalid auth-files array entry. Position is
// one-based so logs correspond naturally to the management response.
type AuthEntryError struct {
	Position  int
	AuthIndex string
	Err       error
}

func (e AuthEntryError) Error() string {
	if errors.Is(e.Err, ErrDuplicateAuthIndex) {
		return fmt.Sprintf("auth entry %d: duplicate auth_index %q", e.Position, e.AuthIndex)
	}
	return fmt.Sprintf("auth entry %d: %v", e.Position, e.Err)
}

func (e AuthEntryError) Unwrap() error { return e.Err }
