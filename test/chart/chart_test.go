// Package chart renders the sbx chart with helm and checks what the
// controller is given for object storage in each supported setup.
package chart

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/Naveen-1-1/agent-sandbox-infra/internal/testutil/leaktest"
)

func TestMain(m *testing.M) { leaktest.VerifyTestMain(m) }

// render runs helm template with args, returning the controller's
// Deployment and ServiceAccount, or helm's error output.
func render(t *testing.T, args ...string) (deployment, sa string, err error) {
	t.Helper()
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm isn't installed")
	}
	if _, err := os.Stat("../../charts/sbx/charts/.deps"); err != nil {
		t.Skip("the chart's dependencies aren't built (make charts/sbx/charts/.deps)")
	}
	var out, stderr bytes.Buffer
	cmd := exec.Command("helm", append([]string{"template", "sbx", "../../charts/sbx", "--set", "images.tag=t"}, args...)...)
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		return "", "", &helmError{stderr.String()}
	}
	for _, doc := range strings.Split(out.String(), "\n---") {
		switch {
		case strings.Contains(doc, "\nkind: Deployment\n") && strings.Contains(doc, "\n  name: sbx-controller\n"):
			deployment = doc
		case strings.Contains(doc, "\nkind: ServiceAccount\n") && strings.Contains(doc, "\n  name: sbx-controller\n"):
			sa = doc
		}
	}
	if deployment == "" || sa == "" {
		t.Fatalf("no controller Deployment or ServiceAccount in the output")
	}
	return deployment, sa, nil
}

type helmError struct{ msg string }

func (e *helmError) Error() string { return e.msg }

func TestStaticS3Credentials(t *testing.T) {
	dep, _, err := render(t, "-f", "../../deploy/values/kind.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name: SBX_S3_ACCESS_KEY", "name: SBX_S3_SECRET_KEY", "name: sbx-s3-credentials",
		`{name: SBX_S3_ENDPOINT, value: "http://sbx-s3.default.svc:8333"}`} {
		if !strings.Contains(dep, want) {
			t.Errorf("kind values: the controller lacks %q", want)
		}
	}
	dep, _, err = render(t, "--set", "s3.existingSecret=my-keys", "--set", "s3.endpoint=https://minio.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(dep, "name: my-keys") {
		t.Error("an existing Secret isn't used")
	}
}

func TestAmbientS3Credentials(t *testing.T) {
	dep, sa, err := render(t, "--set", "s3.credentials=ambient", "--set", "s3.pathStyle=false", "--set", "s3.region=eu-west-1",
		"--set", "controller.serviceAccount.annotations.eks\\.amazonaws\\.com/role-arn=arn:aws:iam::1:role/sbx")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dep, "SBX_S3_ACCESS_KEY") || strings.Contains(dep, "secretKeyRef: {name: sbx-s3") {
		t.Error("ambient credentials still read keys from a Secret")
	}
	for _, want := range []string{`{name: SBX_S3_ENDPOINT, value: ""}`, `{name: SBX_S3_REGION, value: "eu-west-1"}`, `{name: SBX_S3_PATH_STYLE, value: "false"}`} {
		if !strings.Contains(dep, want) {
			t.Errorf("ambient: the controller lacks %q", want)
		}
	}
	if !strings.Contains(sa, "eks.amazonaws.com/role-arn: arn:aws:iam::1:role/sbx") {
		t.Errorf("the ServiceAccount annotation is missing:\n%s", sa)
	}
}

func TestS3CredentialMistakesFailToRender(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-f", "../../deploy/values/kind.yaml", "--set", "s3.credentials=ambient"}, "needs s3.credentials: secret"},
		{[]string{"--set", "s3.credentials=keys"}, "must be secret or ambient"},
		{nil, "set s3.existingSecret"},
	} {
		_, _, err := render(t, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want an error saying %q", c.args, err, c.want)
		}
	}
}
