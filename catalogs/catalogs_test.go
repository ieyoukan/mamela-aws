package catalogs

import (
	"errors"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// The catalogs are consistent, and an EC2 instance's size is read from its type.
func TestCatalogs(t *testing.T) {
	for name, c := range All {
		if err := c.Check(); err != nil || c.API != name {
			t.Fatalf("%s: %v (api %q)", name, err, c.API)
		}
	}
	run, ok := EC2.LookupApproved(protocol.Approved{Method: "POST", Path: "/", Params: map[string][]string{
		"Action": {"RunInstances"}, "Version": {EC2Version}, "MinCount": {"1"}, "MaxCount": {"1"}}})
	if !ok || run.Effect != protocol.EffectCreate {
		t.Fatal("RunInstances")
	}
	limits := map[string]int64{protocol.MeasureCPU: 2, protocol.MeasureMemoryMiB: 4096, protocol.MeasureDiskGiB: 20}
	for typ, ok := range map[string]bool{"t3.medium": true, "t3.large": false, "m6i.xlarge": false, "x9.huge": false} {
		if err := run.CheckLimits(map[string][]string{"InstanceType": {typ}}, limits); (err == nil) != ok {
			t.Errorf("%s: %v", typ, err)
		}
	}
	if _, ok := EC2.LookupApproved(protocol.Approved{Method: "POST", Path: "/", Params: map[string][]string{
		"Action": {"RunInstances"}, "Version": {EC2Version}, "MinCount": {"1"}, "MaxCount": {"10"}}}); ok {
		t.Fatal("a RunInstances of ten instances is a known call")
	}
	if term, _ := EC2.LookupApproved(protocol.Approved{Method: "POST", Path: "/", Params: map[string][]string{
		"Action": {"TerminateInstances"}, "Version": {EC2Version}}}); term.Effect != protocol.EffectLoseData {
		t.Fatal("TerminateInstances loses data")
	}
}

func TestTaskDefinitionMeasures(t *testing.T) {
	var reg protocol.Entry
	for _, e := range ECS.Entries {
		if strings.HasSuffix(e.Fixed["X-Amz-Target"], ".RegisterTaskDefinition") {
			reg = e
		}
	}
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
	entry := func(action string) protocol.Entry {
		for _, e := range IAM.Entries {
			if e.Fixed["Action"] == action {
				return e
			}
		}
		t.Fatalf("no %s", action)
		return protocol.Entry{}
	}
	attach := entry("AttachRolePolicy")
	if err := attach.CheckLimits(map[string][]string{"RoleName": {"r"}, "PolicyArn": {PolicyECSTaskExecution}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := attach.CheckLimits(map[string][]string{"PolicyArn": {"arn:aws:iam::aws:policy/AdministratorAccess"}}, nil); !errors.Is(err, protocol.ErrValueNotAllowed) {
		t.Fatalf("an administrator policy: %v", err)
	}
	create := entry("CreateRole")
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
