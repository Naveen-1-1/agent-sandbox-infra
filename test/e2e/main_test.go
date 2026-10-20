//go:build e2e

// Package e2e runs end-to-end tests against the kind cluster. Run with
// `make test-e2e`; it requires `make kind-up` and a deployed stack.
package e2e

import (
	"testing"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }
