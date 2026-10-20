package broker

import (
	"context"
	"strconv"

	"google.golang.org/grpc"
)

// Credentials makes a client send a pod's broker token, workspace and epoch
// with every call.
func Credentials(brokerToken, workspace string, epoch int64) grpc.DialOption {
	return grpc.WithPerRPCCredentials(podCreds{token: brokerToken, workspace: workspace, epoch: epoch})
}

type podCreds struct {
	token, workspace string
	epoch            int64
}

func (c podCreds) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{
		"authorization": "Bearer " + c.token,
		MDWorkspace:     c.workspace,
		MDEpoch:         strconv.FormatInt(c.epoch, 10),
	}, nil
}

// RequireTransportSecurity is false: in-cluster traffic is plaintext in the
// MVP (mTLS is post-MVP); tokens are per-epoch and short-lived in effect.
func (podCreds) RequireTransportSecurity() bool { return false }
