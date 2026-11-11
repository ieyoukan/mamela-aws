package aws_test

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ieyoukan/mamela-aws/aws"
	"github.com/ieyoukan/mamela-aws/fakeaws"
	"github.com/ieyoukan/mamela/provider/catalogs"
	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/providertest"
)

var guardSettings = []byte(`{"boundary":"locust","instance_types":["t4g.micro"],"max_instances":1,"max_tasks":3}`)

func res(kind protocol.ResourceKind, name, spec string, after ...string) protocol.Resource {
	return protocol.Resource{ID: string(kind) + " " + name, Kind: kind, Name: name, Spec: []byte(`{"boundary":"locust"` + spec + `}`), After: after}
}

// network is a VPC of its own with one public subnet, and the security groups of the load
// generator (locust) and of its target.
func network() []protocol.Resource {
	return []protocol.Resource{
		res(aws.KindVpc, "net", `,"cidr":"10.42.0.0/16"`),
		res(aws.KindInternetGateway, "igw", `,"vpc":"net"`, "AwsVpc net"),
		res(aws.KindSubnet, "public", `,"vpc":"net","cidr":"10.42.1.0/24","zone":"ap-northeast-1a"`, "AwsVpc net"),
		res(aws.KindRouteTable, "public", `,"vpc":"net","internetGateway":"igw","subnet":"public"`, "AwsVpc net", "AwsInternetGateway igw", "AwsSubnet public"),
		res(aws.KindSecurityGroup, "locust", `,"vpc":"net","description":"the load generator"`, "AwsVpc net"),
		res(aws.KindSecurityGroup, "target", `,"vpc":"net","description":"the target","ingress":[{"port":80,"fromGroup":"locust"}]`,
			"AwsVpc net", "AwsSecurityGroup locust"),
	}
}

// locust is the load test of churaverse_infra's terraform/locust, on a target of its own: nginx on
// one EC2 instance (no IAM role), and locust on Fargate Spot, run by run-task.
func locust() []protocol.Resource {
	return append(network(),
		res(aws.KindLogGroup, "locust", `,"retentionDays":7`),
		res(aws.KindIAMRole, "task-execution", `,"trust":"ecs-tasks","policy":"ecs-task-execution"`),
		res(aws.KindCluster, "main", ``),
		res(aws.KindInstance, "target", `,"subnet":"public","securityGroup":"target","instanceType":"t4g.micro",`+
			`"container":{"image":"public.ecr.aws/nginx/nginx:stable","port":80}`,
			"AwsSubnet public", "AwsSecurityGroup target"),
		res(aws.KindTaskDefinition, "locust", `,"launch":"FARGATE","cpu":256,"memoryMiB":512,"executionRole":"task-execution","logGroup":"locust",`+
			`"containers":[{"name":"locust","image":"locustio/locust:2.32.0","command":["-f","/mnt/locustfile.py","--headless","-u","5","-r","1","-t","1m"],`+
			`"environment":[{"name":"LOCUST_HOST","instanceIp":"target","prefix":"http://"}]}]`,
			"AwsIamRole task-execution", "AwsLogGroup locust", "AwsInstance target"),
		res(aws.KindService, "locust", `,"cluster":"main","taskDefinition":"locust","count":0,"launch":"FARGATE_SPOT","subnet":"public",`+
			`"securityGroup":"locust","publicIp":true`,
			"AwsEcsCluster main", "AwsEcsTaskDefinition locust", "AwsSubnet public", "AwsSecurityGroup locust"),
	)
}

// ecsOnEC2 is nginx as an ECS service on a container instance: the role's instance profile lets
// the instance join the cluster.
func ecsOnEC2() []protocol.Resource {
	return append(network(),
		res(aws.KindLogGroup, "web", `,"retentionDays":1`),
		res(aws.KindIAMRole, "task-execution", `,"trust":"ecs-tasks","policy":"ecs-task-execution"`),
		res(aws.KindIAMRole, "instance", `,"trust":"ec2","policy":"ecs-for-ec2","instanceProfile":true`),
		res(aws.KindCluster, "main", ``),
		res(aws.KindInstance, "host", `,"cluster":"main","subnet":"public","securityGroup":"target","role":"instance","instanceType":"t4g.micro"`,
			"AwsEcsCluster main", "AwsSubnet public", "AwsSecurityGroup target", "AwsIamRole instance"),
		res(aws.KindTaskDefinition, "nginx", `,"launch":"EC2","cpu":128,"memoryMiB":128,"executionRole":"task-execution","logGroup":"web",`+
			`"containers":[{"name":"nginx","image":"public.ecr.aws/nginx/nginx:stable","ports":[{"container":80,"host":80}]}]`,
			"AwsIamRole task-execution", "AwsLogGroup web"),
		res(aws.KindService, "web", `,"cluster":"main","taskDefinition":"nginx","count":1,"launch":"EC2"`,
			"AwsEcsCluster main", "AwsEcsTaskDefinition nginx"),
	)
}

// teardown deletes everything, in the order AWS allows.
var teardown = []providertest.Explicit{
	{Op: aws.OpRunTask, Resource: "AwsEcsService locust"},
	{Op: aws.OpDelete, Resource: "AwsEcsService locust"},
	{Op: aws.OpDelete, Resource: "AwsInstance target"},
	{Op: aws.OpDelete, Resource: "AwsEcsCluster main"},
	{Op: aws.OpDelete, Resource: "AwsEcsTaskDefinition locust"},
	{Op: aws.OpDelete, Resource: "AwsLogGroup locust"},
	{Op: aws.OpDelete, Resource: "AwsIamRole task-execution"},
	{Op: aws.OpDelete, Resource: "AwsSecurityGroup target"},
	{Op: aws.OpDelete, Resource: "AwsSecurityGroup locust"},
	{Op: aws.OpDelete, Resource: "AwsRouteTable public"},
	{Op: aws.OpDelete, Resource: "AwsSubnet public"},
	{Op: aws.OpDelete, Resource: "AwsInternetGateway igw"},
	{Op: aws.OpDelete, Resource: "AwsVpc net"},
}

// TestConformance runs Mamela's conformance suite: every request within its permit and the
// catalogs, convergence in rounds as dependencies are created, undeclared objects kept, a lost
// or unsent request at any point recovered, and run-task and the teardown by explicit operations.
func TestConformance(t *testing.T) {
	var mu sync.Mutex
	var apis []*fakeaws.API
	keep := func(api http.Handler) {
		mu.Lock()
		defer mu.Unlock()
		apis = append(apis, api.(*fakeaws.API))
	}
	providertest.Run(t, providertest.Suite{
		Provider: providertest.BuildWASI(t, "../cmd/aws-provider"),
		Catalogs: map[string]protocol.Catalog{"ec2": catalogs.EC2, "ecs": catalogs.ECS, "logs": catalogs.Logs, "iam": catalogs.IAM, "ssm": catalogs.SSM},
		NewAPI: func() http.Handler {
			api := fakeaws.New()
			return &endingTasks{api}
		},
		GuardSettings: guardSettings,
		Cases: []providertest.Case{
			{Name: "locust", Desired: locust()},
			{Name: "ecs-on-ec2", Desired: ecsOnEC2(), Explicit: []providertest.Explicit{
				{Op: aws.OpDelete, Resource: "AwsEcsService web"},
				{Op: aws.OpDelete, Resource: "AwsInstance host"},
				{Op: aws.OpDelete, Resource: "AwsEcsCluster main"},
				{Op: aws.OpDelete, Resource: "AwsIamRole instance"},
			}},
			// The teardown, on an account with someone else's instance, cluster and VPC: none of
			// them is touched, and nothing of Mamela's is left.
			{Name: "teardown", Desired: locust(), Explicit: teardown, World: func(api http.Handler) {
				f := api.(*endingTasks).API
				f.AddForeignInstance("i-0someoneelse", "running")
				f.AddForeignCluster("someone-elses")
				f.AddForeignVpc()
				keep(f)
			}},
		},
	})
	cleaned := false
	for _, api := range apis {
		if api.Instances()["i-0someoneelse"] != "running" {
			t.Errorf("someone else's instance is %s", api.Instances()["i-0someoneelse"])
		}
		if left := api.Empty(); len(left) == 0 {
			cleaned = true
		}
	}
	if len(apis) > 0 && !t.Failed() && !cleaned {
		t.Errorf("no teardown left the account without Mamela's objects; left: %v", apis[len(apis)-1].Empty())
	}
}

// endingTasks is the fake, where each task run-task starts ends at once (a short load test).
type endingTasks struct{ *fakeaws.API }

func (e *endingTasks) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.API.ServeHTTP(w, r)
	if strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".RunTask") {
		e.API.StopTasks("Essential container in task exited")
	}
}

func TestManifest(t *testing.T) {
	m := aws.Provider{}.Manifest()
	if err := m.Check(); err != nil {
		t.Fatal(err)
	}
	for _, k := range m.Kinds {
		del, ok := m.Operation(k.Kind, aws.OpDelete)
		if !ok || !del.Explicit {
			t.Errorf("%s has no explicit delete", k.Kind)
		}
		for _, o := range k.Operations {
			if o.Op == aws.OpDelete || o.Op == aws.OpRunTask {
				continue
			}
			if o.Explicit {
				t.Errorf("%s %s is explicit", k.Kind, o.Op)
			}
		}
	}
	if !slices.ContainsFunc(m.Kinds, func(k protocol.Kind) bool { return k.Kind == aws.KindService }) {
		t.Error("no service kind")
	}
}

// Every call the provider declares is one its API's catalog classifies: an unclassified call
// always needs a person's approval (MAM-POL-0208), even when the request is an ordinary one.
func TestEveryCallIsClassified(t *testing.T) {
	cats := map[string]protocol.Catalog{"ec2": catalogs.EC2, "ecs": catalogs.ECS, "logs": catalogs.Logs, "iam": catalogs.IAM, "ssm": catalogs.SSM}
	m := aws.Provider{}.Manifest()
	all := append(slices.Clone(m.ObserveCalls), m.Guard.Calls...)
	for _, k := range m.Kinds {
		for _, o := range k.Operations {
			all = append(all, o.Calls...)
		}
	}
	for _, c := range all {
		if v := cats[c.API].Verify(protocol.Operation{Calls: []protocol.Call{c}}); len(v.Unclassified) > 0 {
			t.Errorf("%s %s %v: not in the %s catalog", c.Method, c.Path, c.Fixed, c.API)
		}
	}
}
