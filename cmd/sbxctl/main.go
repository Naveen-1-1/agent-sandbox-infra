// Command sbxctl talks to the sbx gateway.
//
//	sbxctl create --repo URL [--ref REF] [--image IMG] [--name N] [--namespace NS]
//	              [--policy fixed --param idleTimeout=5m ...]
//	sbxctl list
//	sbxctl get NS/NAME
//	sbxctl exec [--wait-for deps|none] NS/NAME -- COMMAND [ARGS...]
//	sbxctl service list|start|stop NS/NAME [SVC -- COMMAND ...]
//	sbxctl cat NS/NAME PATH            sbxctl put NS/NAME PATH < file
//	sbxctl wake | wake-hint | turn-ended | suspend | delete | token NS/NAME
//	sbxctl wait NS/NAME PHASE [--timeout 3m]
//	sbxctl demo [--image IMG] [--thinking 10s] [--after-turn 5s] [--keep]   (admin token)
//
// The gateway URL and token come from --gateway/--token or SBX_GATEWAY and
// SBX_TOKEN (the admin token, or a workspace's own token).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/demo"
	"github.com/Naveen-1-1/agent-sandbox-infra/internal/sbxapi"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sbxctl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	global := flag.NewFlagSet("sbxctl", flag.ContinueOnError)
	gw := global.String("gateway", envOr("SBX_GATEWAY", "http://127.0.0.1:8080"), "gateway URL")
	tok := global.String("token", os.Getenv("SBX_TOKEN"), "admin or workspace token")
	asJSON := global.Bool("json", false, "print JSON")
	if err := global.Parse(args); err != nil {
		return err
	}
	rest := global.Args()
	if len(rest) == 0 {
		global.Usage()
		return errors.New("missing command")
	}
	c := &sbxapi.Client{BaseURL: *gw, Token: *tok}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	cmd, rest := rest[0], rest[1:]

	switch cmd {
	case "create":
		fs := flag.NewFlagSet("create", flag.ContinueOnError)
		var r sbxapi.CreateRequest
		fs.StringVar(&r.RepoURL, "repo", "", "git repository URL (http or https)")
		fs.StringVar(&r.Ref, "ref", "", "branch, tag or commit")
		fs.StringVar(&r.Image, "image", "", "workload image (default: from devcontainer.json)")
		fs.StringVar(&r.Name, "name", "", "workspace name (default: generated)")
		fs.StringVar(&r.Namespace, "namespace", "", "namespace (default: the gateway's)")
		fs.StringVar(&r.Mode, "mode", "", "Auto, AlwaysOn or Off")
		pol := fs.String("policy", "", "sleep policy for this workspace (default: the cluster's)")
		params := map[string]string{}
		fs.Func("param", "sleep policy parameter as key=value (repeatable)", func(v string) error {
			k, val, ok := strings.Cut(v, "=")
			if !ok {
				return errors.New("want key=value")
			}
			params[k] = val
			return nil
		})
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *pol != "" {
			r.Policy = &sbxapi.PolicyRef{Name: *pol, Params: params}
		} else if len(params) > 0 {
			return errors.New("--param needs --policy")
		}
		resp, err := c.Create(ctx, r)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(resp)
		}
		fmt.Printf("%s/%s\ntoken: %s\n", resp.Workspace.Namespace, resp.Workspace.Name, resp.Token)
		return nil

	case "list":
		list, err := c.List(ctx)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(list)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "WORKSPACE\tPHASE\tEPOCH\tNODE\tSLEEP\tREPO")
		for _, w := range list {
			fmt.Fprintf(tw, "%s/%s\t%s\t%d\t%s\t%s\t%s\n", w.Namespace, w.Name, w.Phase, w.Epoch, w.Node, sleepSummary(w.Sleep), w.RepoURL)
		}
		return tw.Flush()

	case "exec":
		fs := flag.NewFlagSet("exec", flag.ContinueOnError)
		wait := fs.String("wait-for", "", `"deps" to always wait for dependencies, "none" to never (default: inferred)`)
		if err := fs.Parse(rest); err != nil {
			return err
		}
		rest = fs.Args()
		ns, name, err := target(rest)
		if err != nil {
			return err
		}
		argv := rest[1:]
		if len(argv) > 0 && argv[0] == "--" {
			argv = argv[1:]
		}
		if len(argv) == 0 {
			return errors.New("usage: sbxctl exec [--wait-for deps|none] NS/NAME -- COMMAND [ARGS...]")
		}
		req := sbxapi.ExecRequest{Command: argv}
		if *wait != "" {
			req.WaitFor = []string{*wait}
		}
		out, err := c.Exec(ctx, ns, name, req)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(out)
		}
		fmt.Print(out.Stdout)
		fmt.Fprint(os.Stderr, out.Stderr)
		if out.WaitedMillis > 0 {
			fmt.Fprintf(os.Stderr, "(waited %dms for the workspace to wake)\n", out.WaitedMillis)
		}
		if out.DepsWaitedMillis > 0 {
			fmt.Fprintf(os.Stderr, "(waited %dms for dependencies)\n", out.DepsWaitedMillis)
		}
		if out.ExitCode != 0 {
			os.Exit(int(out.ExitCode))
		}
		return nil

	case "cat", "put":
		ns, name, err := target(rest)
		if err != nil || len(rest) != 2 {
			return fmt.Errorf("usage: sbxctl %s NS/NAME PATH", cmd)
		}
		if cmd == "cat" {
			data, err := c.ReadFile(ctx, ns, name, rest[1])
			if err == nil {
				_, err = os.Stdout.Write(data)
			}
			return err
		}
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		return c.WriteFile(ctx, ns, name, rest[1], data)

	case "service":
		return service(ctx, c, rest, *asJSON)

	case "demo":
		fs := flag.NewFlagSet("demo", flag.ContinueOnError)
		o := demo.Options{Out: os.Stdout}
		fs.StringVar(&o.RepoURL, "repo", "http://sbx-fixtures.sbx-system.svc/hello-node.git", "repository to start from")
		fs.StringVar(&o.Image, "image", "", "workload image (needs git; default: the platform's)")
		fs.StringVar(&o.Name, "name", "", "workspace name (default: generated)")
		fs.StringVar(&o.Namespace, "namespace", "", "namespace (default: the gateway's)")
		fs.DurationVar(&o.Thinking, "thinking", 10*time.Second, "how long the simulated LLM thinks before its first command")
		fs.DurationVar(&o.AfterTurn, "after-turn", 5*time.Second, "how soon the workspace sleeps after a turn ends")
		fs.BoolVar(&o.Keep, "keep", false, "keep the workspace afterwards")
		if err := fs.Parse(rest); err != nil {
			return err
		}
		if *asJSON {
			o.Out = io.Discard
		}
		res, err := demo.Run(ctx, c, o)
		if *asJSON && res != nil {
			_ = printJSON(res)
		}
		return err

	case "wait":
		ns, name, err := target(rest)
		if err != nil || len(rest) < 2 {
			return errors.New("usage: sbxctl wait NS/NAME PHASE [--timeout 3m]")
		}
		fs := flag.NewFlagSet("wait", flag.ContinueOnError)
		timeout := fs.Duration("timeout", 3*time.Minute, "give up after")
		if err := fs.Parse(rest[2:]); err != nil {
			return err
		}
		deadline := time.Now().Add(*timeout)
		for {
			w, err := c.Get(ctx, ns, name)
			if err != nil {
				return err
			}
			if strings.EqualFold(w.Phase, rest[1]) {
				fmt.Printf("%s/%s is %s (epoch %d)\n", ns, name, w.Phase, w.Epoch)
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%s/%s is still %s", ns, name, w.Phase)
			}
			time.Sleep(500 * time.Millisecond)
		}
	}

	ns, name, err := target(rest)
	if err != nil {
		return err
	}
	switch cmd {
	case "get":
		w, err := c.Get(ctx, ns, name)
		if err != nil {
			return err
		}
		return printJSON(w)
	case "wake":
		w, err := c.Wake(ctx, ns, name)
		if err != nil {
			return err
		}
		fmt.Printf("%s/%s is %s (epoch %d)\n", ns, name, w.Phase, w.Epoch)
		return nil
	case "wake-hint":
		return c.WakeHint(ctx, ns, name)
	case "turn-ended":
		return c.TurnEnded(ctx, ns, name)
	case "suspend":
		return c.Suspend(ctx, ns, name)
	case "delete":
		return c.Delete(ctx, ns, name)
	case "token":
		t, err := c.WorkspaceToken(ctx, ns, name)
		if err == nil {
			fmt.Println(t)
		}
		return err
	}
	return fmt.Errorf("unknown command %q", cmd)
}

// service runs the service subcommands:
//
//	sbxctl service list NS/NAME
//	sbxctl service start [--cwd DIR] [--env K=V] [--no-wait-deps] NS/NAME SVC -- COMMAND [ARGS...]
//	sbxctl service stop NS/NAME SVC
func service(ctx context.Context, c *sbxapi.Client, args []string, asJSON bool) error {
	if len(args) == 0 {
		return errors.New("usage: sbxctl service list|start|stop NS/NAME")
	}
	sub, args := args[0], args[1:]
	fs := flag.NewFlagSet("service "+sub, flag.ContinueOnError)
	cwd := fs.String("cwd", "", "working directory, relative to the workspace")
	noWait := fs.Bool("no-wait-deps", false, "start without waiting for dependencies")
	env := map[string]string{}
	fs.Func("env", "environment variable as K=V (repeatable)", func(v string) error {
		k, val, ok := strings.Cut(v, "=")
		if !ok {
			return errors.New("want K=V")
		}
		env[k] = val
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return err
	}
	args = fs.Args()
	ns, name, err := target(args)
	if err != nil {
		return err
	}
	switch sub {
	case "list":
		list, err := c.Services(ctx, ns, name)
		if err != nil {
			return err
		}
		if asJSON {
			return printJSON(list)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "SERVICE\tRUNNING\tRESTARTS\tCOMMAND")
		for _, s := range list {
			fmt.Fprintf(tw, "%s\t%v\t%d\t%s\n", s.Name, s.Running, s.Restarts, strings.Join(s.Command, " "))
		}
		return tw.Flush()
	case "start":
		if len(args) < 2 {
			return errors.New("usage: sbxctl service start NS/NAME SVC -- COMMAND [ARGS...]")
		}
		argv := args[2:]
		if len(argv) > 0 && argv[0] == "--" {
			argv = argv[1:]
		}
		svc := sbxapi.Service{Name: args[1], Command: argv, Cwd: *cwd, Env: env}
		if *noWait {
			f := false
			svc.WaitForDeps = &f
		}
		return c.PutService(ctx, ns, name, svc)
	case "stop":
		if len(args) != 2 {
			return errors.New("usage: sbxctl service stop NS/NAME SVC")
		}
		return c.DeleteService(ctx, ns, name, args[1])
	}
	return fmt.Errorf("unknown service command %q", sub)
}

// sleepSummary is the sleep verdict in a few words, e.g. "Active (4m10s)".
func sleepSummary(s *sbxapi.Sleep) string {
	if s == nil {
		return "-"
	}
	if s.SuspendAt != nil {
		return fmt.Sprintf("%s (%s)", s.Reason, time.Until(*s.SuspendAt).Round(time.Second))
	}
	return s.Reason
}

func target(args []string) (ns, name string, err error) {
	if len(args) == 0 {
		return "", "", errors.New("missing NS/NAME")
	}
	ns, name, ok := strings.Cut(args[0], "/")
	if !ok || ns == "" || name == "" {
		return "", "", fmt.Errorf("want NS/NAME, got %q", args[0])
	}
	return ns, name, nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
