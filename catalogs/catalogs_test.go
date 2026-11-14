package catalogs

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// shipped are the catalogs as the Pome ships them (pome/catalogs/*.json), read as Mamela reads them.
func shipped(t *testing.T) map[string]protocol.Catalog {
	t.Helper()
	out := map[string]protocol.Catalog{}
	for _, name := range Names() {
		data, err := os.ReadFile("../pome/catalogs/" + name + ".json")
		if err != nil {
			t.Fatal(err)
		}
		c, err := protocol.DecodeCatalog(data)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out[name] = c
	}
	return out
}

// models are the pinned models from the cache; the test is skipped when they are not there
// (go run ./cmd/aws-catalogs fetch).
func models(t *testing.T) map[string]*Model {
	t.Helper()
	data, err := os.ReadFile("models.lock.json")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := ParseLock(data)
	if err != nil {
		t.Fatal(err)
	}
	dir := os.Getenv("MAMELA_AWS_MODELS")
	if dir == "" {
		dir = "../.cache/aws-models"
	}
	out := map[string]*Model{}
	for _, name := range lock.Names() {
		pin := lock.Models[name]
		data, err := os.ReadFile(filepath.Join(dir, pin.CacheName()))
		if errors.Is(err, fs.ErrNotExist) {
			t.Skipf("model %s is not in %s: go run ./cmd/aws-catalogs fetch", name, dir)
		}
		if err != nil || !pin.Pinned(data) {
			t.Fatalf("model %s in the cache is not the pinned one (%v)", name, err)
		}
		if out[name], err = LoadModel(data); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// The shipped catalogs are what the overlay and the rules say (this needs no model): the same
// operations, parameters, bind, values, measures, and the effects of the rules.
func TestShippedCatalogsFollowTheOverlay(t *testing.T) {
	cats := shipped(t)
	for _, a := range APIs {
		if err := Consistent(a, cats[a.Name]); err != nil {
			t.Errorf("%v\n  regenerate: go run ./cmd/aws-catalogs fetch && go run ./cmd/aws-catalogs generate pome/catalogs", err)
		}
	}
}

// The shipped catalogs are exactly what the generator makes of the pinned models and the overlay.
func TestCatalogsFollowTheModels(t *testing.T) {
	ms := models(t)
	cats, err := Generate(ms)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range Names() {
		want, err := Encode(cats[name])
		if err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile("../pome/catalogs/" + name + ".json")
		if !bytes.Equal(got, want) {
			t.Errorf("pome/catalogs/%s.json is not what the models and the overlay give: go run ./cmd/aws-catalogs generate pome/catalogs", name)
		}
	}
}

// The generator fails closed: an operation or a parameter the model does not have, a verb no rule
// classifies, a read-only operation classified as a change, a deprecated operation, an override
// that overrides nothing, and a query list written the wrong way are refused, with what is wrong.
func TestGeneratorFailsClosed(t *testing.T) {
	ms := models(t)
	with := func(api string, ops ...Op) []API {
		var out []API
		for _, a := range APIs {
			if a.Name == api {
				a.Ops = append(slices.Clone(a.Ops), ops...)
			}
			out = append(out, a)
		}
		return out
	}
	for _, c := range []struct {
		name      string
		apis      []API
		overrides map[string]map[string]Override
		want      string
	}{
		{"an operation the model does not have", with("ec2", Op{Name: "LaunchRocket"}), Overrides, "has no operation LaunchRocket"},
		{"a misspelt filter", with("ec2", Op{Name: "DescribeKeyPairs", Params: []string{"Filter.1.Valu.1"}}), Overrides, `no parameter "Valu"`},
		{"a list without an index", with("ec2", Op{Name: "DescribeKeyPairs", Params: []string{"KeyName"}}), Overrides, "index from 1"},
		{"an IAM list written flat", with("iam", Op{Name: "TagRole", Params: []string{"RoleName", "Tags.1.Key"}}), Overrides, "Tags.member.N"},
		{"a structure, not a value", with("ec2", Op{Name: "RunScheduledInstances", Params: []string{"LaunchSpecification"}}), Overrides, "a structure, not a value"},
		{"a JSON member the model does not have", with("ecs", Op{Name: "ListAttributes", Params: []string{"clusterName"}}), Overrides, `no member "clusterName"`},
		{"a verb no rule knows", with("ec2", Op{Name: "AcceptVpcPeeringConnection"}), Overrides, "no rule classifies it"},
		{"a read-only operation called a change", APIs, map[string]map[string]Override{"ecs": {"DescribeClusters": {protocol.EffectChange, "wrong"}}},
			"marks it read-only"},
		{"an override of nothing", APIs, map[string]map[string]Override{"ec2": {"CreateTags": {protocol.EffectCreate, "the rules say so"}}},
			"the rules already classify it"},
		{"an override of an operation not in the catalog", APIs, map[string]map[string]Override{"ec2": {"AcceptVpcPeeringConnection": {protocol.EffectChange, "x"}}},
			"not an operation of the overlay"},
		{"a deprecated operation", with("logs", Op{Name: "TagLogGroup", Params: []string{"logGroupName", "tags"}}), Overrides, "deprecated"},
		{"a fixed member of a JSON API", with("ecs", Op{Name: "ListAttributes", Params: []string{"cluster"}, Fixed: map[string]string{"cluster": "x"}}), Overrides,
			"fixes no member"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := generate(ms, c.apis, c.overrides)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("got %v, want an error with %q", err, c.want)
			}
		})
	}
	// The calls a provider declares are checked too.
	byAPI := map[string]*Model{}
	for _, a := range APIs {
		byAPI[a.Name] = ms[a.Model]
	}
	bad := []protocol.Call{
		{API: "ec2", Method: "POST", Path: "/", Form: []string{"Action", "Version", "InstanceIds.1"}, Fixed: map[string]string{"Action": "StopInstances", "Version": "2016-11-15"}},
		{API: "ecs", Method: "POST", Path: "/", Header: []string{"X-Amz-Target"}, JSON: []string{"cluster"}, Fixed: map[string]string{"X-Amz-Target": "AmazonECS.ListServices"}},
		{API: "iam", Method: "POST", Path: "/", Form: []string{"Action", "Version", "RoleName"}, Fixed: map[string]string{"Action": "GetRole", "Version": "2009-01-01"}},
	}
	for i, want := range []string{`no parameter "InstanceIds"`, "is not of service", "version"} {
		if err := CheckCalls(byAPI, bad[i:i+1]); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("call %d: got %v, want %q", i, err, want)
		}
	}
}

// The rules classify by the verb a name starts with, as a word.
func TestRules(t *testing.T) {
	for op, want := range map[string]protocol.Effect{
		"DescribeInstances": protocol.EffectRead, "GetRole": protocol.EffectRead, "ListTasks": protocol.EffectRead,
		"RunTask": protocol.EffectCreate, "RegisterTaskDefinition": protocol.EffectCreate, "CreateVpc": protocol.EffectCreate,
		"TerminateInstances": protocol.EffectLoseData, "DeregisterTaskDefinition": protocol.EffectLoseData, "DeleteRole": protocol.EffectLoseData,
		"StopTask": protocol.EffectRestart, "RebootInstances": protocol.EffectRestart,
		"TagResource": protocol.EffectChange, "PutRetentionPolicy": protocol.EffectChange, "AddRoleToInstanceProfile": protocol.EffectChange,
	} {
		if got, ok := byRule(op); !ok || got != want {
			t.Errorf("%s: %s, want %s", op, got, want)
		}
	}
	for _, op := range []string{"Tagged", "Getaway", "AcceptHandshake", "Runway", "Listen"} {
		if got, ok := byRule(op); ok {
			t.Errorf("%s is classified %s", op, got)
		}
	}
}

// An EC2 instance's size is read from its type, and RunInstances is one instance a request.
func TestCatalogs(t *testing.T) {
	cats := shipped(t)
	ec2 := cats["ec2"]
	version := ec2.Entries[0].Fixed["Version"]
	run, ok := ec2.LookupApproved(protocol.Approved{Method: "POST", Path: "/", Params: map[string][]string{
		"Action": {"RunInstances"}, "Version": {version}, "MinCount": {"1"}, "MaxCount": {"1"}}})
	if !ok || run.Effect != protocol.EffectCreate {
		t.Fatal("RunInstances")
	}
	limits := map[string]int64{protocol.MeasureCPU: 2, protocol.MeasureMemoryMiB: 4096, protocol.MeasureDiskGiB: 20}
	for typ, ok := range map[string]bool{"t3.medium": true, "t3.large": false, "m6i.xlarge": false, "x9.huge": false} {
		if err := run.CheckLimits(map[string][]string{"InstanceType": {typ}}, limits); (err == nil) != ok {
			t.Errorf("%s: %v", typ, err)
		}
	}
	if _, ok := ec2.LookupApproved(protocol.Approved{Method: "POST", Path: "/", Params: map[string][]string{
		"Action": {"RunInstances"}, "Version": {version}, "MinCount": {"1"}, "MaxCount": {"10"}}}); ok {
		t.Fatal("a RunInstances of ten instances is a known call")
	}
	if term, _ := ec2.LookupApproved(protocol.Approved{Method: "POST", Path: "/", Params: map[string][]string{
		"Action": {"TerminateInstances"}, "Version": {version}}}); term.Effect != protocol.EffectLoseData {
		t.Fatal("TerminateInstances loses data")
	}
	// A resize is held to the site's limits like a launch.
	modify, _ := ec2.LookupApproved(protocol.Approved{Method: "POST", Path: "/", Params: map[string][]string{
		"Action": {"ModifyInstanceAttribute"}, "Version": {version}}})
	if err := modify.CheckLimits(map[string][]string{"InstanceType.Value": {"m6i.xlarge"}}, limits); err == nil {
		t.Error("a resize over the limits")
	}
}

func entryOf(t *testing.T, c protocol.Catalog, op string) protocol.Entry {
	t.Helper()
	for _, e := range c.Entries {
		if e.Fixed["Action"] == op || strings.HasSuffix(e.Fixed["X-Amz-Target"], "."+op) {
			return e
		}
	}
	t.Fatalf("%s has no %s", c.API, op)
	return protocol.Entry{}
}

func TestTaskDefinitionMeasures(t *testing.T) {
	reg := entryOf(t, shipped(t)["ecs"], "RegisterTaskDefinition")
	limits := map[string]int64{protocol.MeasureCPU: 1, protocol.MeasureMemoryMiB: 1024}
	if err := reg.CheckLimits(map[string][]string{"cpu": {`"256"`}, "memory": {`"512"`}}, limits); err != nil {
		t.Fatal(err)
	}
	if err := reg.CheckLimits(map[string][]string{"cpu": {`"2048"`}, "memory": {`"512"`}}, limits); err == nil {
		t.Fatal("2 vCPUs over a limit of 1")
	}
}

// IAM is held to what ECS needs: roles under /mamela/, assumed only by ECS tasks or EC2, with only
// the two AWS-managed ECS policies attached. Any other value is refused before it is sent.
func TestIAMIsHeldToECS(t *testing.T) {
	iam := shipped(t)["iam"]
	attach := entryOf(t, iam, "AttachRolePolicy")
	if err := attach.CheckLimits(map[string][]string{"RoleName": {"r"}, "PolicyArn": {PolicyECSTaskExecution}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := attach.CheckLimits(map[string][]string{"PolicyArn": {"arn:aws:iam::aws:policy/AdministratorAccess"}}, nil); !errors.Is(err, protocol.ErrValueNotAllowed) {
		t.Fatalf("an administrator policy: %v", err)
	}
	create := entryOf(t, iam, "CreateRole")
	for name, params := range map[string]map[string][]string{
		"another path":  {"Path": {"/"}, "AssumeRolePolicyDocument": {TrustEC2}},
		"another trust": {"Path": {IAMPath}, "AssumeRolePolicyDocument": {strings.Replace(TrustEC2, "ec2.amazonaws.com", "lambda.amazonaws.com", 1)}},
	} {
		if err := create.CheckLimits(params, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := create.CheckLimits(map[string][]string{"Path": {IAMPath}, "AssumeRolePolicyDocument": {TrustECSTasks}}, nil); err != nil {
		t.Fatal(err)
	}
}

// The IAM catalog is at least as strict as it was: the same values for what makes a role, a
// profile or attaches a policy; every other change is bound to the role (or profile) the change is
// for; and nothing that writes a policy, a trust or a permission boundary is in it at all.
func TestIAMStaysStrict(t *testing.T) {
	iam := shipped(t)["iam"]
	want := map[string]map[string][]string{
		"CreateRole":            {"Path": {IAMPath}, "AssumeRolePolicyDocument": {TrustECSTasks, TrustEC2}},
		"AttachRolePolicy":      {"PolicyArn": {PolicyECSTaskExecution, PolicyECSForEC2}},
		"CreateInstanceProfile": {"Path": {IAMPath}},
	}
	for _, e := range iam.Entries {
		op := e.Fixed["Action"]
		for _, forbidden := range []string{"PutRolePolicy", "UpdateAssumeRolePolicy", "PutRolePermissionsBoundary", "CreatePolicy", "CreatePolicyVersion",
			"AttachUserPolicy", "AttachGroupPolicy", "CreateUser", "CreateAccessKey", "PassRole"} {
			if op == forbidden {
				t.Errorf("the IAM catalog has %s", op)
			}
		}
		if e.Effect == protocol.EffectRead {
			continue
		}
		if v, ok := want[op]; ok {
			for p, vals := range v {
				if !slices.Equal(e.Values[p], vals) {
					t.Errorf("%s %s: values %v, want %v", op, p, e.Values[p], vals)
				}
			}
			continue
		}
		if len(e.Bind) == 0 {
			t.Errorf("%s changes IAM and is bound to nothing", op)
		}
		if e.In("PolicyArn") != "" && op != "DetachRolePolicy" || e.In("AssumeRolePolicyDocument") != "" || e.In("PermissionsBoundary") != "" {
			t.Errorf("%s may be sent a policy, a trust or a boundary", op)
		}
	}
	update := entryOf(t, iam, "UpdateRole")
	if !slices.Equal(update.Form, []string{"Action", "Version", "RoleName", "Description"}) {
		t.Errorf("UpdateRole may be sent %v: only the description (the marker of an adopted role)", update.Form)
	}
}
