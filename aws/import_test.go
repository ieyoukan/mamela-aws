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

// after are the resources a declaration refers to, as the generated Pome adds them: every
// reference of its kind's spec (the manifest's), in lists and objects too.
func after(kind protocol.ResourceKind, spec []byte) []string {
	m := aws.Provider.Manifest()
	k := m.Kinds[slices.IndexFunc(m.Kinds, func(x protocol.Kind) bool { return x.Kind == kind })]
	var v any
	json.Unmarshal(spec, &v)
	var out []string
	var walk func(fields []protocol.Field, v any)
	var value func(f protocol.Field, v any)
	value = func(f protocol.Field, v any) {
		switch f.Type {
		case protocol.FieldRef:
			if n, _ := v.(string); n != "" && !slices.Contains(out, string(f.Ref)+" "+n) {
				out = append(out, string(f.Ref)+" "+n)
			}
		case protocol.FieldList:
			l, _ := v.([]any)
			for _, x := range l {
				value(*f.Elem, x)
			}
		case protocol.FieldObject:
			walk(f.Fields, v)
		}
	}
	walk = func(fields []protocol.Field, v any) {
		o, _ := v.(map[string]any)
		for _, f := range fields {
			if x, ok := o[f.Name]; ok {
				value(f, x)
			}
		}
	}
	walk(k.Spec, v)
	return out
}

// declare makes the declarations of what an import gives, and the adoptions Mamela records.
func declare(res protocol.ImportResult) ([]protocol.Resource, []protocol.Adopting) {
	var desired []protocol.Resource
	var adopting []protocol.Adopting
	for _, r := range res.Resources {
		id := string(r.Kind) + " " + r.Name
		desired = append(desired, protocol.Resource{ID: id, Kind: r.Kind, Name: r.Name, Spec: r.Spec, After: after(r.Kind, r.Spec)})
		adopting = append(adopting, protocol.Adopting{Resource: id, IDs: r.IDs})
	}
	return desired, adopting
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

// stackGuard are the guard's settings for the stack Terraform made: two instances.
var stackGuard = []byte(`{"boundary":"locust","instance_types":["t4g.micro","t4g.small"],"max_instances":2,"max_tasks":3}`)

// stackOrder is the order a person adopts the stack in: what is referred to first.
var stackOrder = []string{"AwsVpc main", "AwsInternetGateway gw", "AwsSubnet public", "AwsSecurityGroup admin", "AwsSecurityGroup web",
	"AwsRouteTable public", "AwsIamRole execution", "AwsIamRole instance", "AwsLogGroup app", "AwsEcsCluster main",
	"AwsEcsTaskDefinition app", "AwsEcsService app", "AwsInstance bastion", "AwsInstance worker"}

// An ECS stack Terraform made moves to Mamela: its network, the IAM roles under /mamela/ (with
// their policy attachments and instance profile), the log group under /mamela/<boundary>/, the
// cluster (with its capacity providers), the task definition of the family mamela-<boundary>-*
// and its service, and two instances (one with the instance role) are declared as they are;
// what Mamela cannot adopt safely is left to Terraform with why: a role under / with
// AdministratorAccess (and its attachment), a log group under /ecs/, a task definition of another
// family and the service that runs it.
func TestImportStackFromTerraform(t *testing.T) {
	st := fakeaws.New().AddTerraformStack("locust")
	res, err := aws.Provider.Import(context.Background(), protocol.ImportParams{Objects: importState(t, st.State), Settings: stackGuard})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]protocol.Imported{}
	for _, r := range res.Resources {
		got[r.Address] = r
	}
	for addr, want := range map[string]struct {
		spec string
		also []string
	}{
		"aws_iam_role.execution": {`{"boundary":"locust","policy":"ecs-task-execution","trust":"ecs-tasks"}`, []string{"aws_iam_role_policy_attachment.execution"}},
		"aws_iam_role.instance": {`{"boundary":"locust","instanceProfile":true,"policy":"ecs-for-ec2","trust":"ec2"}`,
			[]string{"aws_iam_instance_profile.instance", "aws_iam_role_policy_attachment.instance"}},
		"aws_cloudwatch_log_group.app": {`{"boundary":"locust","retentionDays":14}`, nil},
		"aws_ecs_cluster.main":         {`{"boundary":"locust"}`, []string{"aws_ecs_cluster_capacity_providers.main"}},
		"aws_ecs_task_definition.app": {`{"boundary":"locust","containers":[{"containerName":"app","environment":[{"value":"web","variable":"MODE"}],` +
			`"image":"public.ecr.aws/nginx/nginx:stable","ports":[{"container":80}]}],"cpu":256,"executionRole":"execution","launch":"FARGATE","logGroup":"app","memoryMiB":512}`, nil},
		"aws_ecs_service.app":  {`{"boundary":"locust","cluster":"main","count":1,"launch":"FARGATE_SPOT","publicIp":true,"securityGroup":"web","subnet":"public","taskDefinition":"app"}`, nil},
		"aws_instance.bastion": {`{"boundary":"locust","instanceType":"t4g.micro","securityGroup":"admin","subnet":"public"}`, nil},
		"aws_instance.worker":  {`{"boundary":"locust","instanceType":"t4g.micro","role":"instance","securityGroup":"web","subnet":"public"}`, nil},
	} {
		if canonical(got[addr].Spec) != canonical([]byte(want.spec)) || !slices.Equal(got[addr].Also, want.also) {
			t.Errorf("%s: %s with %v, want %s with %v", addr, got[addr].Spec, got[addr].Also, want.spec, want.also)
		}
	}
	skipped := map[string]string{}
	for _, sk := range res.Skipped {
		skipped[sk.Address] = sk.Why
	}
	for addr, why := range map[string]string{
		"aws_s3_bucket.logs":                   "imports aws_s3_bucket",
		"aws_iam_role.admin":                   "is under /: Mamela's IAM catalog lets it change roles under /mamela/ only",
		"aws_iam_role_policy_attachment.admin": "moves with its role",
		"aws_cloudwatch_log_group.old":         "log group /ecs/old is not under /mamela/locust/",
		"aws_ecs_task_definition.batch":        "its family batch is not mamela-locust-<name>",
		"aws_ecs_service.batch":                "its task definition batch:1",
	} {
		if !strings.Contains(skipped[addr], why) {
			t.Errorf("%s: skipped %q, want why with %q", addr, skipped[addr], why)
		}
	}
	if len(res.Resources) != len(stackOrder) || len(res.Skipped) != 6 {
		t.Fatalf("imported %d, skipped %d: %+v", len(res.Resources), len(res.Skipped), res.Skipped)
	}
	desired, adopting := declare(res)
	var adopt []providertest.Explicit
	for _, id := range stackOrder {
		adopt = append(adopt, providertest.Explicit{Op: aws.OpAdopt, Resource: id})
	}
	// The declaration as a person edits it after the import: two tasks on Fargate (not Spot)
	// without a public IP, nginx of another tag, logs kept 30 days (longer: a plan does it).
	edited := slices.Clone(desired)
	for i, r := range edited {
		spec := string(r.Spec)
		switch r.ID {
		case "AwsEcsService app":
			spec = strings.NewReplacer(`"count":1`, `"count":2`, `"FARGATE_SPOT"`, `"FARGATE"`, `"publicIp":true,`, ``).Replace(spec)
		case "AwsEcsTaskDefinition app":
			spec = strings.Replace(spec, "nginx:stable", "nginx:stable-alpine", 1)
		case "AwsLogGroup app":
			spec = strings.Replace(spec, `"retentionDays":14`, `"retentionDays":30`, 1)
		}
		edited[i].Spec = []byte(spec)
	}
	world := func(api http.Handler) {
		if s := api.(*fakeaws.API).AddTerraformStack("locust"); s.Vpc != st.Vpc || s.Worker != st.Worker {
			t.Fatal("the fake's IDs differ")
		}
	}
	providertest.Run(t, providertest.Suite{
		Provider:      providertest.BuildWASI(t, "../cmd/aws-provider"),
		Catalogs:      shipped(t),
		NewAPI:        func() http.Handler { return fakeaws.New() },
		GuardSettings: stackGuard,
		Cases: []providertest.Case{
			{Name: "terraform-stack", Desired: desired, Adopting: adopting, Explicit: adopt, World: world},
			{Name: "terraform-stack-edited", Desired: edited, Adopting: adopting, Explicit: adopt, World: world},
		},
	})
}

func canonical(b []byte) string {
	var v any
	json.Unmarshal(b, &v)
	out, _ := json.Marshal(v, json.Deterministic(true))
	return string(out)
}

// The whole move, as a person makes it: `mamela import terraform` with the state's JSON of the
// stack (network, IAM, logs, ECS, instances) writes the declarations (with the Pome's generated
// Aws.Kinds) and records them to adopt, says why the rest stays Terraform's; the declaration that
// uses them evaluates into a graph where each refers to what it needs.
func TestImportTerraformCommand(t *testing.T) {
	if os.Getenv("MAMELA_TOOLCHAIN_DIR") == "" {
		t.Skip("MAMELA_TOOLCHAIN_DIR: the toolchain of Mamela")
	}
	n := fakeaws.New().AddTerraformStack("legacy")
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
		"20 of 26 resources import, as 14 declarations",
		"with aws_route_table_association.public", "with aws_security_group_rule.ssh", "with aws_iam_instance_profile.instance",
		"with aws_iam_role_policy_attachment.execution", "with aws_ecs_cluster_capacity_providers.main",
		"aws_s3_bucket.logs", "aws_iam_role.admin", "aws_cloudwatch_log_group.old", "aws_ecs_task_definition.batch", "aws_ecs_service.batch",
		`mamela act --confirm main adopt "AwsVpc main"`, `mamela act --confirm execution adopt "AwsIamRole execution"`,
		`mamela act --confirm app adopt "AwsEcsTaskDefinition app"`, `mamela act --confirm worker adopt "AwsInstance worker"`,
		"terraform state rm 'aws_cloudwatch_log_group.app'", "'aws_internet_gateway.gw'", "'aws_ecs_service.app'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("import: no %q in\n%s", want, out)
		}
	}
	src, _ := os.ReadFile(filepath.Join(decl, "src/imported.emel"))
	for _, want := range []string{"import Aws.Kinds.{", `AwsVpc(name: "main", boundary: "legacy", cidr: "10.9.0.0/16")`, "|> Kinds.aws_route_table(",
		"|> Kinds.aws_ecs_service(", "|> Kinds.aws_instance(", `container_name: "app"`, `AwsLogGroup(name: "app", boundary: "legacy", retention_days: 14)`} {
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
	for _, want := range []string{"14 resources", "AwsRouteTable", `"internetGateway":"gw","subnet":"public"`, `{"fromGroup":"admin","port":443}`,
		`"containerName":"app"`, `"executionRole":"execution"`, `"role":"instance"`, `"taskDefinition":"app"`} {
		if !strings.Contains(graph, want) {
			t.Errorf("the graph has no %s:\n%s", want, graph)
		}
	}
}
