//go:build e2e

package e2e

import (
	"fmt"
	"testing"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/objstore/objstoretest"
	"github.com/Naveen-1-1/agent-sandbox-infra/test/e2e/framework"
)

// TestLocalS3MeetsContract runs the objstore contract against the deployed
// SeaweedFS endpoint: create-once and compare-and-swap must have exactly one
// winner under contention, and presigned writes must enforce their checksum.
// This is what lets the snapshot manifest live in S3.
func TestLocalS3MeetsContract(t *testing.T) {
	ns := framework.Namespace()
	addr := framework.PortForward(t, ns, "svc/sbx-s3", 8333)
	secret := "sbx-s3-credentials"
	store, err := objstore.NewS3(objstore.S3Config{
		Endpoint:  "http://" + addr,
		Bucket:    "sbx",
		AccessKey: framework.SecretValue(t, ns, secret, "accessKey"),
		SecretKey: framework.SecretValue(t, ns, secret, "secretKey"),
		PathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	objstoretest.Run(t, store, fmt.Sprintf("e2e/contract-%d/", time.Now().UnixNano()), 32)
}
