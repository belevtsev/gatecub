package cli

import (
	"errors"
	"testing"

	"github.com/belevtsev/kitsu/v3/internal/workflowrunner"
)

// The record of a task names its runner, and the record is a file in the
// checkout. Where a runner service is registered, or may be, a task must not
// become a local one.
func TestLocalRunnerIsRefusedUnlessNoRunnerServiceIsRegistered(t *testing.T) {
	saved := registeredRunner
	t.Cleanup(func() { registeredRunner = saved })
	for name, c := range map[string]struct {
		err     error
		allowed bool
	}{
		"nothing is registered":       {err: workflowrunner.ErrUnregistered, allowed: true},
		"a service is registered":     {},
		"the registration is damaged": {err: errors.New("invalid machine runner registration")},
	} {
		registeredRunner = func(string) (*workflowrunner.Client, error) { return nil, c.err }
		if err := localRunnerAllowed(t.TempDir()); (err == nil) != c.allowed {
			t.Errorf("%s: local runner allowed = %t, want %t", name, err == nil, c.allowed)
		}
	}
}
