package aws_test

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela-aws/aws"
	"github.com/ieyoukan/mamela-aws/fakeaws"
	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/providertest"
)

// importState is what `mamela import terraform` gives the provider: the managed resources of a
// state, named by their names.
func importState(t *testing.T, state []byte) []protocol.ImportObject {
	t.Helper()
	var s struct {
		Values struct {
			Root struct {
				Resources []struct {
					Address string         `json:"address"`
					Type    string         `json:"type"`
					Name    string         `json:"name"`
					Values  jsontext.Value `json:"values"`
				} `json:"resources"`
			} `json:"root_module"`
		} `json:"values"`
	}
	if err := json.Unmarshal(state, &s); err != nil {
		t.Fatal(err)
	}
	var out []protocol.ImportObject
	for _, r := range s.Values.Root.Resources {
		out = append(out, protocol.ImportObject{Address: r.Address, Type: r.Type, Name: r.Name, Values: r.Values})
	}
	return out
}

// after are the resources a declaration refers to, as the generated Pome adds them.
func after(kind protocol.ResourceKind, spec []byte) []string {
	var s struct {
		Vpc             string `json:"vpc"`
		InternetGateway string `json:"internetGateway"`
		Subnet          string `json:"subnet"`
		Ingress         []struct {
			FromGroup string `json:"fromGroup"`
		} `json:"ingress"`
	}
	json.Unmarshal(spec, &s)
	var out []string
	add := func(k, n string) {
		if n != "" && !slices.Contains(out, k+" "+n) {
			out = append(out, k+" "+n)
		}
	}
	add("AwsVpc", s.Vpc)
	add("AwsInternetGateway", s.InternetGateway)
	add("AwsSubnet", s.Subnet)
	for _, in := range s.Ingress {
		add("AwsSecurityGroup", in.FromGroup)
	}
	return out
}

// A network Terraform made moves to Mamela: each resource is declared as it is (the rules Mamela
// does not express are left out, and stay), the route table takes its association with it, what
// the provider does not make is not imported, and nothing of it is planned until it is adopted;
// adopting tags it, and then it is in sync.
func TestImportFromTerraform(t *testing.T) {
	probe := fakeaws.New()
	n := probe.AddTerraformNetwork()
	res, err := aws.Provider.Import(context.Background(), protocol.ImportParams{Objects: importState(t, n.State), Settings: guardSettings})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]protocol.Imported{}
	for _, r := range res.Resources {
		got[r.Address] = r
	}
	for addr, want := range map[string]string{
		"aws_vpc.main":             `{"boundary":"locust","cidr":"10.9.0.0/16"}`,
		"aws_internet_gateway.gw":  `{"boundary":"locust","vpc":"main"}`,
		"aws_subnet.public":        `{"boundary":"locust","cidr":"10.9.1.0/24","vpc":"main","zone":"ap-northeast-1a"}`,
		"aws_route_table.public":   `{"boundary":"locust","internetGateway":"gw","subnet":"public","vpc":"main"}`,
		"aws_security_group.web":   `{"boundary":"locust","description":"web servers","ingress":[{"cidr":"0.0.0.0/0","port":80},{"fromGroup":"admin","port":443}],"vpc":"main"}`,
		"aws_security_group.admin": `{"boundary":"locust","description":"admin","ingress":[{"cidr":"203.0.113.7/32","port":22}],"vpc":"main"}`,
	} {
		if canonical(got[addr].Spec) != canonical([]byte(want)) {
			t.Errorf("%s: %s, want %s", addr, got[addr].Spec, want)
		}
	}
	if a := got["aws_route_table.public"].Also; !slices.Equal(a, []string{"aws_route_table_association.public"}) {
		t.Errorf("the route table includes %v", a)
	}
	if a := got["aws_security_group.admin"].Also; !slices.Equal(a, []string{"aws_security_group_rule.ssh"}) {
		t.Errorf("the admin group includes %v", a)
	}
	if len(res.Resources) != 6 || len(res.Skipped) != 1 || res.Skipped[0].Address != "aws_s3_bucket.logs" {
		t.Fatalf("imported %d, skipped %+v", len(res.Resources), res.Skipped)
	}
	// Without a boundary there is nothing to import into.
	none, err := aws.Provider.Import(context.Background(), protocol.ImportParams{Objects: importState(t, n.State)})
	if err != nil || len(none.Resources) != 0 || !strings.Contains(none.Skipped[0].Why, "boundary") {
		t.Fatalf("without a boundary: %+v %v", none, err)
	}

	var desired []protocol.Resource
	var adopting []protocol.Adopting
	var adopt []providertest.Explicit
	for _, addr := range []string{"aws_vpc.main", "aws_internet_gateway.gw", "aws_subnet.public", "aws_security_group.admin",
		"aws_security_group.web", "aws_route_table.public"} {
		r := got[addr]
		id := string(r.Kind) + " " + r.Name
		desired = append(desired, protocol.Resource{ID: id, Kind: r.Kind, Name: r.Name, Spec: r.Spec, After: after(r.Kind, r.Spec)})
		adopting = append(adopting, protocol.Adopting{Resource: id, IDs: r.IDs})
		adopt = append(adopt, providertest.Explicit{Op: aws.OpAdopt, Resource: id})
	}
	providertest.Run(t, providertest.Suite{
		Provider:      providertest.BuildWASI(t, "../cmd/aws-provider"),
		Catalogs:      shipped(t),
		NewAPI:        func() http.Handler { return fakeaws.New() },
		GuardSettings: guardSettings,
		Cases: []providertest.Case{{
			Name: "terraform-network", Desired: desired, Adopting: adopting, Explicit: adopt,
			// The same objects, with the same IDs, as the state names.
			World: func(api http.Handler) {
				if m := api.(*fakeaws.API).AddTerraformNetwork(); m.Vpc != n.Vpc || m.Web != n.Web {
					t.Fatal("the fake's IDs differ")
				}
			},
		}},
	})
}

func canonical(b []byte) string {
	var v any
	json.Unmarshal(b, &v)
	out, _ := json.Marshal(v, json.Deterministic(true))
	return string(out)
}

// The whole move, as a person makes it: `mamela import terraform` with the state's JSON writes
// the declarations (with the Pome's generated Aws.Kinds) and records them to adopt; the declaration
// that uses them evaluates into a graph where each refers to what it needs.
func TestImportTerraformCommand(t *testing.T) {
	if os.Getenv("MAMELA_TOOLCHAIN_DIR") == "" {
		t.Skip("MAMELA_TOOLCHAIN_DIR: the toolchain of Mamela")
	}
	n := fakeaws.New().AddTerraformNetwork()
	dir := t.TempDir()
	decl := filepath.Join(dir, "decl")
	repo, _ := filepath.Abs("..")
	lock, _ := os.ReadFile("../examples/locust/Pome.lock")
	// The operator's providers file (the guard's boundary is where the imported resources go).
	example, _ := os.ReadFile("../examples/locust/mamela.providers.example.toml")
	files := map[string]string{
		"decl/Pome.toml": `[pome]
name = "example.com/decl"
version = "0.1.0"
emela = "0.27.0"

[build]
target = "wasm"

[dependencies]
"github.com/ieyoukan/mamela/pome/mamela" = "v0.3.0"
"github.com/ieyoukan/mamela-aws/pome" = "v0.1.0"

[replace]
"github.com/ieyoukan/mamela/pome/mamela" = { path = "` + filepath.Join(repo, "../mamela/pome/mamela") + `" }
"github.com/ieyoukan/mamela-aws/pome" = { path = "` + filepath.Join(repo, "pome") + `" }
`,
		"decl/Pome.lock": string(lock),
		"decl/src/main.emel": `import Std.Console.{SystemConsole}
import Mamela

fn main() {
  with SystemConsole {
    Mamela.empty() |> Mamela.print
  }
}
`,
		"state.json":            string(n.State),
		"mamela.providers.toml": strings.Replace(string(example), `boundary = "locust"`, `boundary = "legacy"`, 1),
	}
	for name, data := range files {
		p := filepath.Join(dir, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mamela := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("go", append([]string{"tool", "mamela"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("mamela %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}
	out := mamela("import", "terraform", "--state", filepath.Join(dir, "state.json"), "--db", filepath.Join(dir, "state.db"), "--by", "tester",
		"--providers", filepath.Join(dir, "mamela.providers.toml"), decl)
	for _, want := range []string{
		"8 of 9 resources import, as 6 declarations",
		"with aws_route_table_association.public", "with aws_security_group_rule.ssh",
		"aws_s3_bucket.logs", `mamela act --confirm main adopt "AwsVpc main"`,
		"terraform state rm 'aws_internet_gateway.gw'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("import: no %q in\n%s", want, out)
		}
	}
	src, _ := os.ReadFile(filepath.Join(decl, "src/imported.emel"))
	for _, want := range []string{"import Aws.Kinds.{", `AwsVpc(name: "main", boundary: "legacy", cidr: "10.9.0.0/16")`, "|> Kinds.aws_route_table("} {
		if !strings.Contains(string(src), want) {
			t.Errorf("imported.emel: no %q in\n%s", want, src)
		}
	}
	os.WriteFile(filepath.Join(decl, "src/main.emel"), []byte(`import Std.Console.{SystemConsole}
import Mamela
import Imported

fn main() {
  with SystemConsole {
    Mamela.empty() |> Imported.imported |> Mamela.print
  }
}
`), 0o644)
	graph := mamela("eval", decl)
	for _, want := range []string{"6 resources", "AwsRouteTable       public", `"internetGateway":"gw","subnet":"public"`, `{"fromGroup":"admin","port":443}`} {
		if !strings.Contains(graph, want) {
			t.Errorf("the graph has no %s:\n%s", want, graph)
		}
	}
}
