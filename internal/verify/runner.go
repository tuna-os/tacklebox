package verify

import (
	"github.com/tuna-os/tacklebox/internal/runner"
)

// CommandRunner abstracts external subprocess execution for verification operations.
type CommandRunner interface {
	Run(name string, args ...string) error
	Output(name string, args ...string) ([]byte, error)
}

type defaultRunner struct{}

func (defaultRunner) Run(name string, args ...string) error {
	return runner.Run(name, args...)
}

func (defaultRunner) Output(name string, args ...string) ([]byte, error) {
	return runner.Output(name, args...)
}

// DefaultRunner uses internal/runner to execute subprocesses.
var DefaultRunner CommandRunner = defaultRunner{}
