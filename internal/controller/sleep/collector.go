package sleep

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gatewayv1 "github.com/Naveen-1-1/agent-sandbox-infra/gen/go/sbx/gateway/v1"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/policy"
)

// Collector polls every gateway replica for the traffic signals it holds
// (turns, held requests, open streams) and keeps the merged view. Replicas
// are found through the EndpointSlices of the gateway's headless signals
// Service, so scaling the gateway needs no configuration.
type Collector struct {
	Reader    client.Reader // lists EndpointSlices; uncached is fine at this rate
	Namespace string
	Service   string
	PortName  string        // default "signals"
	Interval  time.Duration // default 5s
	Timeout   time.Duration // per replica, default 2s
	// Changed, if set, is told which workspaces' turn or held-request
	// signals changed since the last poll, so they can be re-decided now.
	Changed func(keys []string)
	Log     logr.Logger
	Now     func() time.Time

	mu    sync.Mutex
	view  view
	conns map[string]*grpc.ClientConn
}

type view struct {
	at       time.Time
	complete bool // every ready replica answered
	signals  map[string]policy.Signals
}

// NeedLeaderElection: only the leader decides, so only it polls.
func (c *Collector) NeedLeaderElection() bool { return true }

// Start polls until ctx ends.
func (c *Collector) Start(ctx context.Context) error {
	if c.Interval <= 0 {
		c.Interval = 5 * time.Second
	}
	defer c.Close()
	t := time.NewTicker(c.Interval)
	defer t.Stop()
	for {
		c.Collect(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// Signals returns what the gateways know about key. ok is false until a
// complete, recent poll exists: a policy must then treat the gateways as
// unknown rather than as silent.
func (c *Collector) Signals(key string) (policy.Signals, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.view.complete || c.now().Sub(c.view.at) > 3*c.interval() {
		return policy.Signals{}, false
	}
	return c.view.signals[key], true
}

// Collect polls every replica once.
func (c *Collector) Collect(ctx context.Context) {
	addrs, err := c.replicas(ctx)
	if err != nil {
		c.Log.Info("listing gateway replicas failed", "err", err.Error())
		c.mu.Lock()
		c.view.complete = false
		c.mu.Unlock()
		return
	}
	type answer struct {
		resp *gatewayv1.GetWorkspaceSignalsResponse
		err  error
	}
	answers := make([]answer, len(addrs))
	var wg sync.WaitGroup
	for i, addr := range addrs {
		conn, err := c.conn(addr)
		if err != nil {
			answers[i].err = err
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			timeout := c.Timeout
			if timeout <= 0 {
				timeout = 2 * time.Second
			}
			cctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			answers[i].resp, answers[i].err = gatewayv1.NewGatewaySignalsServiceClient(conn).
				GetWorkspaceSignals(cctx, &gatewayv1.GetWorkspaceSignalsRequest{})
		}()
	}
	wg.Wait()

	next := view{at: c.now(), complete: true, signals: map[string]policy.Signals{}}
	for i, a := range answers {
		if a.err != nil {
			next.complete = false
			c.Log.Info("gateway replica didn't answer", "addr", addrs[i], "err", a.err.Error())
			continue
		}
		for key, s := range a.resp.GetSignals() {
			merged := next.signals[key]
			merged.Merge(policy.FromProto(s))
			next.signals[key] = merged
		}
	}

	c.mu.Lock()
	prev := c.view
	c.view = next
	c.prune(addrs)
	c.mu.Unlock()
	if c.Changed != nil && next.complete {
		if keys := changed(prev.signals, next.signals); len(keys) > 0 {
			c.Changed(keys)
		}
	}
}

// replicas lists the ready signal endpoints of the gateway Service.
func (c *Collector) replicas(ctx context.Context) ([]string, error) {
	slices := &discoveryv1.EndpointSliceList{}
	if err := c.Reader.List(ctx, slices, client.InNamespace(c.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: c.Service}); err != nil {
		return nil, err
	}
	name := c.PortName
	if name == "" {
		name = "signals"
	}
	var addrs []string
	for _, sl := range slices.Items {
		var port int32
		for _, p := range sl.Ports {
			if p.Name != nil && *p.Name == name && p.Port != nil {
				port = *p.Port
			}
		}
		if port == 0 {
			continue
		}
		for _, ep := range sl.Endpoints {
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue // starting or terminating: not serving traffic
			}
			for _, a := range ep.Addresses {
				addrs = append(addrs, net.JoinHostPort(a, strconv.Itoa(int(port))))
			}
		}
	}
	return addrs, nil
}

func (c *Collector) conn(addr string) (*grpc.ClientConn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cc, ok := c.conns[addr]; ok {
		return cc, nil
	}
	cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return nil, err
	}
	if c.conns == nil {
		c.conns = map[string]*grpc.ClientConn{}
	}
	c.conns[addr] = cc
	return cc, nil
}

// prune closes connections to replicas that are gone. Callers hold mu.
func (c *Collector) prune(live []string) {
	keep := map[string]bool{}
	for _, a := range live {
		keep[a] = true
	}
	for a, cc := range c.conns {
		if !keep[a] {
			_ = cc.Close()
			delete(c.conns, a)
		}
	}
}

// Close drops the connections to every replica.
func (c *Collector) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(nil)
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Collector) interval() time.Duration {
	if c.Interval <= 0 {
		return 5 * time.Second
	}
	return c.Interval
}

// changed lists workspaces whose turn or held-request signals moved: the
// ones a policy may now decide differently about straight away.
func changed(prev, next map[string]policy.Signals) []string {
	var keys []string
	for k, n := range next {
		p := prev[k]
		if !n.LastTurnEnded.Equal(p.LastTurnEnded) || !n.LastTurnStarted.Equal(p.LastTurnStarted) || n.HeldRequests != p.HeldRequests {
			keys = append(keys, k)
		}
	}
	return keys
}
