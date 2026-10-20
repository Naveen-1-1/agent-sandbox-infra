//go:build !linux

package agent

import (
	"errors"
	"syscall"
)

// SystemProcs is only implemented on Linux, where the agent runs.
type SystemProcs struct{}

var errUnsupported = errors.New("agent: freezing processes is only supported on Linux")

func (SystemProcs) List() ([]int, error)             { return nil, errUnsupported }
func (SystemProcs) Signal(int, syscall.Signal) error { return errUnsupported }
