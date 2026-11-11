package aws

import (
	"strings"
	"testing"

	"github.com/ieyoukan/mamela/provider/protocol"
)

var settings = GuardSettings{Boundary: "locust", InstanceTypes: []string{"t4g.micro"}, MaxInstances: 1, MaxTasks: 3}

// ours is a world with the boundary's VPC, subnet, group, role and cluster.
func ours() world {
	w := world{byKind: map[protocol.ResourceKind][]object{}, ours: map[string]bool{}}
	for _, id := range []string{"vpc-1", "subnet-1", "sg-1", "locust-instance", "locust-main", "mamela-locust-web:1"} {
		w.ours[id] = true
	}
	return w
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
}
