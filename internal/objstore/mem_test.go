package objstore_test

import (
	"net/http/httptest"
	"testing"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore/objstoretest"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

func TestMemMeetsContract(t *testing.T) {
	mem := objstore.NewMem()
	srv := httptest.NewServer(mem.Handler())
	t.Cleanup(srv.Close)
	mem.SetBaseURL(srv.URL)

	objstoretest.Run(t, mem, "ws/test/", 32)
}
