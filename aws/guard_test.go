package aws

import (
	"net/url"
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/provider/protocol"
)

var settings = GuardSettings{Boundary: "locust", InstanceTypes: []string{"t4g.micro"}, MaxInstances: 1, MaxTasks: 3}

const arn = "arn:aws:ecs:ap-northeast-1:123456789012:"

// ours is a world with the boundary's VPC, subnet, group, role and cluster, another boundary's
// VPC, cluster and log group, and what the guard read of objects to adopt.
func ours() world {
	w := world{byKind: map[protocol.ResourceKind][]object{}, ours: map[string]bool{}, others: map[string]bool{}, adopting: map[string]adoptFact{
		"i-adopt": {InstanceType: "t4g.micro"}, "i-big": {InstanceType: "m7i.large"},
		arn + "service/locust-main/web": {Tasks: 1, Cluster: "locust-main"}, arn + "service/legacy/web": {Tasks: 1, Cluster: "legacy"},
		"legacy-execution": {Path: "/mamela/"}, "admin": {Path: "/"},
	}}
	for _, id := range []string{"vpc-1", "subnet-1", "sg-1", "locust-instance", "locust-main", "mamela-locust-web:1"} {
		w.ours[id] = true
	}
	for _, id := range []string{"vpc-prod", arn + "cluster/prod-main", "arn:aws:logs:ap-northeast-1:123456789012:log-group:/mamela/locust/prod-logs"} {
		w.others[id] = true
	}
	return w
}

// adoptTags are the tags of an adopt (EC2's CreateTags).
func adoptTags(id, boundary, name string) map[string]string {
	return map[string]string{"ResourceId.1": id, "Tag.1.Key": TagName, "Tag.1.Value": name, "Tag.2.Key": TagBoundary, "Tag.2.Value": boundary}
}

func TestGuardKeepsTheBoundary(t *testing.T) {
	tagged := func(p map[string]string) map[string]string { return ec2Tags("subnet", "locust", "public", p) }
	for _, c := range []struct {
		name string
		a    protocol.Approved
		want string // "" when allowed
	}{
		{"a subnet in its VPC", queryReq(APIEC2, "CreateSubnet", tagged(map[string]string{"VpcId": "vpc-1", "CidrBlock": "10.0.1.0/24", "AvailabilityZone": "z"})), ""},
		{"a subnet in someone else's VPC", queryReq(APIEC2, "CreateSubnet", tagged(map[string]string{"VpcId": "vpc-other", "CidrBlock": "10.0.1.0/24", "AvailabilityZone": "z"})), "VpcId vpc-other"},
		{"a create of another boundary", queryReq(APIEC2, "CreateSubnet", ec2Tags("subnet", "prod", "public", map[string]string{"VpcId": "vpc-1"})), `mamela:boundary="prod"`},
		{"a rule on someone else's group", queryReq(APIEC2, "AuthorizeSecurityGroupIngress", map[string]string{"GroupId": "sg-other"}), "GroupId sg-other"},
		{"a rule from someone else's group", queryReq(APIEC2, "AuthorizeSecurityGroupIngress", map[string]string{"GroupId": "sg-1", "IpPermissions.1.Groups.1.GroupId": "sg-other"}), "sg-other"},
		{"terminating someone else's instance", queryReq(APIEC2, "TerminateInstances", map[string]string{"InstanceId.1": "i-other"}), "i-other"},
		{"an instance of a type not allowed", queryReq(APIEC2, "RunInstances", ec2Tags("instance", "locust", "target", map[string]string{"InstanceType": "m7i.large",
			"NetworkInterface.1.SubnetId": "subnet-1", "NetworkInterface.1.SecurityGroupId.1": "sg-1", "IamInstanceProfile.Name": "locust-instance"})), "m7i.large"},
		{"a role not named after the boundary", queryReq(APIIAM, "CreateRole", map[string]string{"RoleName": "admin", "Description": RoleMarker("locust", "admin")}), `"admin"`},
		{"a role marked for another boundary", queryReq(APIIAM, "CreateRole", map[string]string{"RoleName": "locust-x", "Description": RoleMarker("prod", "x")}), `mamela:boundary="prod"`},
		{"a role with no marker", queryReq(APIIAM, "CreateRole", map[string]string{"RoleName": "locust-x"}), `mamela:boundary=""`},
		{"a role and its policy", queryReq(APIIAM, "AttachRolePolicy", map[string]string{"RoleName": "locust-new", "PolicyArn": "arn"}), ""}, // created by the change
		{"a policy on someone else's role", queryReq(APIIAM, "AttachRolePolicy", map[string]string{"RoleName": "OrganizationAccountAccessRole", "PolicyArn": "arn"}), "OrganizationAccountAccessRole"},
		{"a service in someone else's cluster", jsonReq(APIECS, "UpdateService", map[string]any{"cluster": "prod", "service": "web", "desiredCount": 0}), "cluster prod"},
		{"a task in someone else's subnet", jsonReq(APIECS, "RunTask", map[string]any{"cluster": "locust-main", "taskDefinition": "mamela-locust-web:1", "count": 1,
			"networkConfiguration": map[string]any{"awsvpcConfiguration": map[string]any{"subnets": []string{"subnet-other"}, "securityGroups": []string{"sg-1"}}},
			"tags":                 ecsTags("locust", "web")}), "subnet-other"},
		{"a task definition of another family", jsonReq(APIECS, "RegisterTaskDefinition", map[string]any{"family": "web", "tags": ecsTags("locust", "web")}), `"web"`},
		{"someone else's log group", jsonReq(APILogs, "DeleteLogGroup", map[string]any{"logGroupName": "/ecs/locust"}), "/ecs/locust"},

		// Resizing and security groups.
		{"a resize to an allowed type", queryReq(APIEC2, "ModifyInstanceAttribute", map[string]string{"InstanceId": "locust-instance", "InstanceType.Value": "t4g.micro"}), ""},
		{"a resize to a type not allowed", queryReq(APIEC2, "ModifyInstanceAttribute", map[string]string{"InstanceId": "locust-instance", "InstanceType.Value": "m7i.large"}), "m7i.large"},
		{"a resize of someone else's instance", queryReq(APIEC2, "ModifyInstanceAttribute", map[string]string{"InstanceId": "i-other", "InstanceType.Value": "t4g.micro"}), "i-other"},
		{"someone else's group on an instance", queryReq(APIEC2, "ModifyInstanceAttribute", map[string]string{"InstanceId": "locust-instance", "GroupId.1": "sg-other"}), "sg-other"},
		{"a stop of someone else's instance", queryReq(APIEC2, "StopInstances", map[string]string{"InstanceId.1": "i-other"}), "i-other"},
		{"a service moved to someone else's subnet", jsonReq(APIECS, "UpdateService", map[string]any{"cluster": "locust-main", "service": "locust-web", "desiredCount": 1,
			"networkConfiguration": map[string]any{"awsvpcConfiguration": map[string]any{"subnets": []string{"subnet-other"}, "securityGroups": []string{"sg-1"}}}}), "subnet-other"},

		// Adopting (ADR 0009 of Mamela): into this boundary, never another boundary's object.
		{"adopting a VPC", queryReq(APIEC2, "CreateTags", adoptTags("vpc-tf", "locust", "net")), ""},
		{"adopting into another boundary", queryReq(APIEC2, "CreateTags", adoptTags("vpc-tf", "prod", "net")), `boundary "prod"`},
		{"adopting without a name", queryReq(APIEC2, "CreateTags", adoptTags("vpc-tf", "locust", "")), "a declaration's name"},
		{"adopting another boundary's VPC", queryReq(APIEC2, "CreateTags", adoptTags("vpc-prod", "locust", "net")), "another boundary's"},
		{"adopting an instance", queryReq(APIEC2, "CreateTags", adoptTags("i-adopt", "locust", "host")), ""},
		{"adopting an instance of a type not allowed", queryReq(APIEC2, "CreateTags", adoptTags("i-big", "locust", "host")), "m7i.large"},
		{"adopting a cluster", jsonReq(APIECS, "TagResource", map[string]any{"resourceArn": arn + "cluster/legacy", "tags": ecsTags("locust", "main")}), ""},
		{"adopting another boundary's cluster", jsonReq(APIECS, "TagResource", map[string]any{"resourceArn": arn + "cluster/prod-main", "tags": ecsTags("locust", "main")}), "another boundary's"},
		{"adopting a cluster into another boundary", jsonReq(APIECS, "TagResource", map[string]any{"resourceArn": arn + "cluster/legacy", "tags": ecsTags("prod", "main")}), `boundary "prod"`},
		{"adopting a service of the boundary's cluster", jsonReq(APIECS, "TagResource", map[string]any{"resourceArn": arn + "service/locust-main/web", "tags": ecsTags("locust", "web")}), ""},
		{"adopting a service of a cluster not adopted", jsonReq(APIECS, "TagResource", map[string]any{"resourceArn": arn + "service/legacy/web", "tags": ecsTags("locust", "web")}), "adopt the cluster first"},
		{"adopting a task definition", jsonReq(APIECS, "TagResource", map[string]any{"resourceArn": arn + "task-definition/mamela-locust-app:3", "tags": ecsTags("locust", "app")}), ""},
		{"adopting a task definition of another family", jsonReq(APIECS, "TagResource", map[string]any{"resourceArn": arn + "task-definition/web:3", "tags": ecsTags("locust", "app")}), "not of the family"},
		{"tagging a task", jsonReq(APIECS, "TagResource", map[string]any{"resourceArn": arn + "task/locust-main/abc", "tags": ecsTags("locust", "x")}), "no cluster, service or task definition"},
		{"adopting a log group", jsonReq(APILogs, "TagResource", map[string]any{"resourceArn": "arn:aws:logs:ap-northeast-1:123456789012:log-group:/mamela/locust/app",
			"tags": map[string]string{TagName: "app", TagBoundary: "locust"}}), ""},
		{"adopting a log group outside /mamela/<boundary>/", jsonReq(APILogs, "TagResource", map[string]any{"resourceArn": "arn:aws:logs:ap-northeast-1:123456789012:log-group:/ecs/app",
			"tags": map[string]string{TagName: "app", TagBoundary: "locust"}}), "not of /mamela/locust/"},
		{"adopting another boundary's log group", jsonReq(APILogs, "TagResource", map[string]any{"resourceArn": "arn:aws:logs:ap-northeast-1:123456789012:log-group:/mamela/locust/prod-logs",
			"tags": map[string]string{TagName: "app", TagBoundary: "locust"}}), "another boundary's"},
		{"adopting a role", queryReq(APIIAM, "UpdateRole", map[string]string{"RoleName": "legacy-execution", "Description": RoleMarker("locust", "execution")}), ""},
		{"adopting a role into another boundary", queryReq(APIIAM, "UpdateRole", map[string]string{"RoleName": "legacy-execution", "Description": RoleMarker("prod", "execution")}), `boundary "prod"`},
		{"rewriting a role's description", queryReq(APIIAM, "UpdateRole", map[string]string{"RoleName": "legacy-execution", "Description": "anything"}), "a declaration's name"},
		{"adopting a role outside /mamela/", queryReq(APIIAM, "UpdateRole", map[string]string{"RoleName": "admin", "Description": RoleMarker("locust", "admin")}), "not under /mamela/"},
	} {
		t.Run(c.name, func(t *testing.T) {
			created := map[string]bool{}
			if c.name == "a role and its policy" {
				checkRequest(settings, ours(), created, queryReq(APIIAM, "CreateRole", map[string]string{"RoleName": "locust-new", "Description": RoleMarker("locust", "new")}))
			}
			got := strings.Join(checkRequest(settings, ours(), created, c.a), "; ")
			switch {
			case c.want == "" && got != "":
				t.Errorf("refused: %s", got)
			case c.want != "" && !strings.Contains(got, c.want):
				t.Errorf("got %q, want a refusal with %q", got, c.want)
			}
		})
	}
}

func TestGuardLimits(t *testing.T) {
	w := ours()
	w.byKind[KindInstance] = []object{{Name: "target"}}
	w.byKind[KindService] = []object{{IDs: map[string]string{"cluster": "locust-main", "service": "locust-web"}, State: serviceState{Count: 2,
		Runs: []taskRun{{Status: "RUNNING"}, {Status: "STOPPED"}}}}}
	findings := func(changes ...protocol.Approved) map[string]string {
		out := map[string]string{}
		limits(settings, w, []protocol.Change{{Requests: changes}}, func(code, severity, format string, args ...any) { out[code] = severity })
		return out
	}
	if f := findings(); f["AWS-0103"] != protocol.SeverityOK || f["AWS-0104"] != protocol.SeverityOK {
		t.Errorf("1 instance and 3 tasks are within the limits: %v", f)
	}
	if f := findings(queryReq(APIEC2, "RunInstances", map[string]string{})); f["AWS-0103"] != protocol.SeverityBlock {
		t.Errorf("a second instance: %v", f)
	}
	if f := findings(jsonReq(APIECS, "RunTask", map[string]any{"count": 1})); f["AWS-0104"] != protocol.SeverityBlock {
		t.Errorf("a fourth task: %v", f)
	}
	if f := findings(jsonReq(APIECS, "UpdateService", map[string]any{"cluster": "locust-main", "service": "locust-web", "desiredCount": 1}),
		jsonReq(APIECS, "RunTask", map[string]any{"count": 1})); f["AWS-0104"] != protocol.SeverityOK {
		t.Errorf("one task fewer, then one more: %v", f)
	}
	// What is adopted counts: an instance, and a service's tasks.
	w.adopting = ours().adopting
	if f := findings(queryReq(APIEC2, "CreateTags", adoptTags("i-adopt", "locust", "host"))); f["AWS-0103"] != protocol.SeverityBlock {
		t.Errorf("adopting a second instance: %v", f)
	}
	if f := findings(jsonReq(APIECS, "TagResource", map[string]any{"resourceArn": arn + "service/locust-main/web", "tags": ecsTags("locust", "web")})); f["AWS-0104"] != protocol.SeverityBlock {
		t.Errorf("adopting a service of 1 task with 3 running: %v", f)
	}
}

// A trust policy is read for what it allows: one service assumes the role, with no condition; a
// Sid and the way it is written do not matter, anything more makes it none Mamela makes.
func TestTrustOf(t *testing.T) {
	for doc, want := range map[string]string{
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ecs-tasks.amazonaws.com"},"Action":"sts:AssumeRole"}]}`:                    "ecs-tasks",
		`{"Version":"2012-10-17","Statement":[{"Sid":"","Effect":"Allow","Principal":{"Service":["ec2.amazonaws.com"]},"Action":["sts:AssumeRole"]}]}`:             "ec2",
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com","AWS":"*"},"Action":"sts:AssumeRole"}]}`:                "",
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole","Condition":{"x":{}}}]}`:     "",
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":["ec2.amazonaws.com","lambda.amazonaws.com"]},"Action":"sts:AssumeRole"}]}`: "",
		`{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`:                           "",
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:*"}]}`:                                   "",
		`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"lambda.amazonaws.com"},"Action":"sts:AssumeRole"}]}`:                       "",
	} {
		if got := trustOf(url.QueryEscape(doc)); got != want {
			t.Errorf("%s: %q, want %q", doc, got, want)
		}
	}
}
