package fakeaws

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"strings"
)

// Terraform's view of what it made: `terraform show -json` of a state, as the AWS provider (v5)
// writes its resources' attributes. The objects are in the fake, without any mark of Mamela's.

// tfResource is one managed resource of a state.
type tfResource struct {
	Address string         `json:"address"`
	Mode    string         `json:"mode"`
	Type    string         `json:"type"`
	Name    string         `json:"name"`
	Values  map[string]any `json:"values"`
}

func resource(typ, name string, values map[string]any) tfResource {
	return tfResource{Address: typ + "." + name, Mode: "managed", Type: typ, Name: name, Values: values}
}

// showJSON is the output of terraform show -json for resources.
func showJSON(rs []tfResource) []byte {
	b, err := json.Marshal(map[string]any{"format_version": "1.0", "terraform_version": "1.9.8",
		"values": map[string]any{"root_module": map[string]any{"resources": rs}}}, json.Deterministic(true))
	if err != nil {
		panic(err)
	}
	var v jsontext.Value = b
	if err := v.Indent(); err != nil {
		panic(err)
	}
	return v
}

// TerraformNetwork is a network made by Terraform (no Mamela tags): its IDs, and the JSON of
// `terraform show -json` of the state that made it.
type TerraformNetwork struct {
	Vpc, Gateway, Subnet, RouteTable, Association, Web, Admin string
	State                                                     []byte
	resources                                                 []tfResource
}

// AddTerraformNetwork makes a VPC with an attached internet gateway, a subnet, a route table that
// routes 0.0.0.0/0 to the gateway and serves the subnet, and two security groups: web (TCP 80 from
// anywhere, 443 from admin, and a port range Mamela does not express) and admin (TCP 22 from one
// address, written as an aws_security_group_rule). None carries a tag of Mamela's.
func (a *API) AddTerraformNetwork() TerraformNetwork {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.addTerraformNetwork()
}

func (a *API) addTerraformNetwork() TerraformNetwork {
	n := TerraformNetwork{Vpc: a.id("vpc"), Gateway: a.id("igw"), Subnet: a.id("subnet"), RouteTable: a.id("rtb"),
		Association: a.id("rtbassoc"), Web: a.id("sg"), Admin: a.id("sg")}
	tf := tags{"Name": "legacy"}
	a.vpcs = append(a.vpcs, &vpc{id: n.Vpc, cidr: "10.9.0.0/16", tags: clone(tf)})
	a.gateways = append(a.gateways, &gateway{id: n.Gateway, vpc: n.Vpc, tags: clone(tf)})
	a.subnets = append(a.subnets, &subnet{id: n.Subnet, vpc: n.Vpc, cidr: "10.9.1.0/24", zone: Region + "a", tags: clone(tf)})
	a.routes = append(a.routes, &routeTable{id: n.RouteTable, vpc: n.Vpc, routes: map[string]string{"0.0.0.0/0": n.Gateway},
		assocID: n.Association, assocWith: n.Subnet, tags: clone(tf)})
	a.groups = append(a.groups,
		&group{id: n.Web, vpc: n.Vpc, name: "legacy-web", tags: clone(tf), rules: []rule{
			{port: 80, ip: "0.0.0.0/0"}, {port: 443, group: n.Admin}}},
		&group{id: n.Admin, vpc: n.Vpc, name: "legacy-admin", tags: clone(tf), rules: []rule{{port: 22, ip: "203.0.113.7/32"}}})
	arn := func(kind, id string) string { return "arn:aws:ec2:" + Region + ":" + Account + ":" + kind + "/" + id }
	rule := func(from, to int, cidrs []string, groups []string) map[string]any {
		return map[string]any{"from_port": from, "to_port": to, "protocol": "tcp", "cidr_blocks": cidrs, "security_groups": groups, "self": false,
			"ipv6_cidr_blocks": []string{}, "prefix_list_ids": []string{}, "description": ""}
	}
	egress := []any{map[string]any{"from_port": 0, "to_port": 0, "protocol": "-1", "cidr_blocks": []string{"0.0.0.0/0"}, "security_groups": []string{},
		"self": false, "ipv6_cidr_blocks": []string{}, "prefix_list_ids": []string{}, "description": ""}}
	n.resources = []tfResource{
		resource("aws_vpc", "main", map[string]any{"id": n.Vpc, "arn": arn("vpc", n.Vpc), "cidr_block": "10.9.0.0/16", "enable_dns_hostnames": true,
			"enable_dns_support": true, "instance_tenancy": "default", "tags": map[string]string{"Name": "legacy"}, "tags_all": map[string]string{"Name": "legacy"}}),
		resource("aws_internet_gateway", "gw", map[string]any{"id": n.Gateway, "arn": arn("internet-gateway", n.Gateway), "vpc_id": n.Vpc, "owner_id": Account}),
		resource("aws_subnet", "public", map[string]any{"id": n.Subnet, "arn": arn("subnet", n.Subnet), "vpc_id": n.Vpc, "cidr_block": "10.9.1.0/24",
			"availability_zone": "ap-northeast-1a", "map_public_ip_on_launch": true}),
		resource("aws_route_table", "public", map[string]any{"id": n.RouteTable, "arn": arn("route-table", n.RouteTable), "vpc_id": n.Vpc,
			"route": []any{map[string]any{"cidr_block": "0.0.0.0/0", "gateway_id": n.Gateway, "nat_gateway_id": "", "ipv6_cidr_block": ""}}, "propagating_vgws": []string{}}),
		resource("aws_route_table_association", "public", map[string]any{"id": n.Association, "route_table_id": n.RouteTable, "subnet_id": n.Subnet, "gateway_id": ""}),
		resource("aws_security_group", "web", map[string]any{"id": n.Web, "arn": arn("security-group", n.Web), "name": "legacy-web", "vpc_id": n.Vpc,
			"description": "web servers", "egress": egress, "ingress": []any{
				rule(80, 80, []string{"0.0.0.0/0"}, []string{}),
				rule(443, 443, []string{}, []string{n.Admin}),
				rule(8000, 8100, []string{"10.9.0.0/16"}, []string{})}}),
		resource("aws_security_group", "admin", map[string]any{"id": n.Admin, "arn": arn("security-group", n.Admin), "name": "legacy-admin", "vpc_id": n.Vpc,
			"description": "admin", "egress": egress, "ingress": []any{}}),
		resource("aws_security_group_rule", "ssh", map[string]any{"id": "sgrule-1", "type": "ingress", "security_group_id": n.Admin, "from_port": 22, "to_port": 22,
			"protocol": "tcp", "cidr_blocks": []string{"203.0.113.7/32"}}),
		resource("aws_s3_bucket", "logs", map[string]any{"id": "legacy-logs", "bucket": "legacy-logs"}),
	}
	n.State = showJSON(n.resources)
	return n
}

func clone(t tags) tags {
	out := tags{}
	for k, v := range t {
		out[k] = v
	}
	return out
}

// Trust policies as Terraform's aws_iam_policy_document writes them (with an empty Sid).
const (
	tfTrustECSTasks = `{"Version":"2012-10-17","Statement":[{"Sid":"","Effect":"Allow","Principal":{"Service":"ecs-tasks.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	tfTrustEC2      = `{"Version":"2012-10-17","Statement":[{"Sid":"","Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	policyExecution = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
	policyForEC2    = "arn:aws:iam::aws:policy/service-role/AmazonEC2ContainerServiceforEC2Role"
)

// TerraformStack is what Terraform made for an ECS service on Fargate Spot and two EC2 instances,
// in the boundary's names (Mamela adopts what is named so; the rest stays Terraform's):
//
//   - the network of AddTerraformNetwork;
//   - IAM roles under /mamela/: <boundary>-task-execution (ECS tasks) and <boundary>-instance (EC2,
//     with its instance profile), each with its policy attachment; and legacy-admin under / with
//     AdministratorAccess (not imported);
//   - log groups /mamela/<boundary>/app (14 days) and /ecs/old (not imported);
//   - cluster legacy-cluster with its capacity providers;
//   - task definitions mamela-<boundary>-app (Fargate, nginx, logs to app) and batch (not
//     imported: another family), and their services app (Fargate Spot, 1 task) and batch;
//   - instances bastion (no role) and worker (the instance role), both t4g.micro.
type TerraformStack struct {
	TerraformNetwork
	Cluster, Service, Bastion, Worker string
	State                             []byte
}

// AddTerraformStack makes the stack of TerraformStack in the boundary's names.
func (a *API) AddTerraformStack(boundary string) TerraformStack {
	a.mu.Lock()
	defer a.mu.Unlock()
	st := TerraformStack{TerraformNetwork: a.addTerraformNetwork(), Cluster: "legacy-cluster", Service: "app"}
	n := st.TerraformNetwork
	rs := n.resources

	// IAM.
	addRole := func(tfName, name, path, trust, policy, description string) {
		r := &role{name: name, path: path, trust: trust, description: description, policies: []string{policy}}
		a.roles = append(a.roles, r)
		rs = append(rs, resource("aws_iam_role", tfName, map[string]any{"id": name, "name": name, "path": path, "arn": roleArn(r),
			"assume_role_policy": trust, "description": description, "managed_policy_arns": []string{policy}, "max_session_duration": 3600,
			"inline_policy": []any{}, "permissions_boundary": "", "force_detach_policies": false, "unique_id": "AROA" + strings.ToUpper(name[:4]) + "EXAMPLE",
			"tags": map[string]string{}, "tags_all": map[string]string{}}),
			resource("aws_iam_role_policy_attachment", tfName, map[string]any{"id": name + "-20261101000000000000000001", "role": name, "policy_arn": policy}))
	}
	execRole, instRole := boundary+"-task-execution", boundary+"-instance"
	addRole("execution", execRole, "/mamela/", tfTrustECSTasks, policyExecution, "ECS task execution")
	addRole("instance", instRole, "/mamela/", tfTrustEC2, policyForEC2, "")
	addRole("admin", "legacy-admin", "/", tfTrustEC2, "arn:aws:iam::aws:policy/AdministratorAccess", "")
	a.profiles = append(a.profiles, &profile{name: instRole, path: "/mamela/", roles: []string{instRole}})
	rs = append(rs, resource("aws_iam_instance_profile", "instance", map[string]any{"id": instRole, "name": instRole, "path": "/mamela/", "role": instRole,
		"arn": "arn:aws:iam::" + Account + ":instance-profile/mamela/" + instRole}))

	// CloudWatch Logs.
	addLogGroup := func(tfName, name string, days int64) {
		g := &logGroup{name: name, retention: days, tags: map[string]string{}, stored: 1 << 20}
		a.logs = append(a.logs, g)
		rs = append(rs, resource("aws_cloudwatch_log_group", tfName, map[string]any{"id": name, "name": name, "arn": g.arn(), "retention_in_days": days,
			"kms_key_id": "", "log_group_class": "STANDARD", "skip_destroy": false, "tags": map[string]string{}}))
	}
	appLogs := "/mamela/" + boundary + "/app"
	addLogGroup("app", appLogs, 14)
	addLogGroup("old", "/ecs/old", 7)

	// ECS: the cluster, two task definitions and their services.
	a.clusters = append(a.clusters, &cluster{name: st.Cluster, status: "ACTIVE"})
	clusterArn := ecsArn("cluster", st.Cluster)
	rs = append(rs, resource("aws_ecs_cluster", "main", map[string]any{"id": clusterArn, "arn": clusterArn, "name": st.Cluster,
		"setting": []any{map[string]any{"name": "containerInsights", "value": "disabled"}}, "configuration": []any{}, "service_connect_defaults": []any{}}),
		resource("aws_ecs_cluster_capacity_providers", "main", map[string]any{"id": st.Cluster, "cluster_name": st.Cluster,
			"capacity_providers": []string{"FARGATE", "FARGATE_SPOT"}, "default_capacity_provider_strategy": []any{
				map[string]any{"capacity_provider": "FARGATE_SPOT", "weight": 1, "base": 0}}}))
	addTaskDefinition := func(tfName, family string, containers []any) string {
		content := map[string]any{"containerDefinitions": containers, "cpu": "256", "memory": "512", "networkMode": "awsvpc",
			"requiresCompatibilities": []any{"FARGATE"}, "executionRoleArn": "arn:aws:iam::" + Account + ":role/mamela/" + execRole}
		var asJSON map[string]any // as the fake keeps a registration: decoded JSON
		b, _ := json.Marshal(content)
		json.Unmarshal(b, &asJSON)
		td := &taskDef{family: family, revision: 1, status: "ACTIVE", compatible: []string{"FARGATE"}, content: asJSON}
		a.taskDefs = append(a.taskDefs, td)
		arn := ecsArn("task-definition", family+":1")
		defs, _ := json.Marshal(containers, json.Deterministic(true))
		rs = append(rs, resource("aws_ecs_task_definition", tfName, map[string]any{"id": family, "arn": arn, "arn_without_revision": ecsArn("task-definition", family),
			"family": family, "revision": 1, "container_definitions": string(defs), "cpu": "256", "memory": "512", "network_mode": "awsvpc",
			"requires_compatibilities": []string{"FARGATE"}, "execution_role_arn": content["executionRoleArn"], "task_role_arn": "", "pid_mode": "", "ipc_mode": "",
			"volume": []any{}, "placement_constraints": []any{}, "proxy_configuration": []any{}, "inference_accelerator": []any{}, "ephemeral_storage": []any{},
			"runtime_platform": []any{}, "skip_destroy": false, "track_latest": false, "tags": map[string]string{}}))
		return arn
	}
	appTD := addTaskDefinition("app", "mamela-"+boundary+"-app", []any{map[string]any{
		"name": "app", "image": "public.ecr.aws/nginx/nginx:stable", "essential": true,
		"environment":  []any{map[string]any{"name": "MODE", "value": "web"}},
		"portMappings": []any{map[string]any{"containerPort": 80, "hostPort": 80, "protocol": "tcp"}},
		"logConfiguration": map[string]any{"logDriver": "awslogs", "options": map[string]any{
			"awslogs-group": appLogs, "awslogs-region": Region, "awslogs-stream-prefix": "app"}}}})
	batchTD := addTaskDefinition("batch", "batch", []any{map[string]any{"name": "batch", "image": "public.ecr.aws/docker/library/busybox:stable", "essential": true}})
	addService := func(tfName, name, td string, desired int64) {
		network := map[string]any{"awsvpcConfiguration": map[string]any{"subnets": []any{n.Subnet}, "securityGroups": []any{n.Web}, "assignPublicIp": "ENABLED"}}
		a.services = append(a.services, &service{cluster: st.Cluster, name: name, status: "ACTIVE", taskDef: lastPart(td), desired: desired,
			strategy: []map[string]any{{"capacityProvider": "FARGATE_SPOT", "weight": 1, "base": 0}}, network: network})
		rs = append(rs, resource("aws_ecs_service", tfName, map[string]any{"id": ecsArn("service/"+st.Cluster, name), "name": name, "cluster": clusterArn,
			"task_definition": td, "desired_count": desired, "launch_type": "", "scheduling_strategy": "REPLICA",
			"capacity_provider_strategy": []any{map[string]any{"capacity_provider": "FARGATE_SPOT", "weight": 1, "base": 0}},
			"network_configuration":      []any{map[string]any{"subnets": []string{n.Subnet}, "security_groups": []string{n.Web}, "assign_public_ip": true}},
			"deployment_controller":      []any{map[string]any{"type": "ECS"}}, "load_balancer": []any{}, "service_registries": []any{},
			"deployment_maximum_percent": 200, "deployment_minimum_healthy_percent": 100, "enable_execute_command": false, "propagate_tags": "NONE",
			"platform_version": "LATEST", "wait_for_steady_state": false, "tags": map[string]string{}}))
	}
	addService("app", st.Service, appTD, 1)
	addService("batch", "batch", batchTD, 0)

	// EC2 instances.
	addInstance := func(tfName, name, group, profile string) string {
		s := a.findSubnet(n.Subnet)
		s.next++
		ip := strings.TrimSuffix(strings.SplitN(s.cidr, "/", 2)[0], "0") + fmt.Sprint(10+s.next)
		i := &instance{id: a.id("i"), typ: "t4g.micro", image: "ami-0legacyfake", ip: ip, state: "running", subnet: n.Subnet, groups: []string{group},
			profile: profile, tags: tags{"Name": "legacy-" + name}}
		a.insts = append(a.insts, i)
		rs = append(rs, resource("aws_instance", tfName, map[string]any{"id": i.id, "arn": "arn:aws:ec2:" + Region + ":" + Account + ":instance/" + i.id,
			"ami": i.image, "instance_type": i.typ, "subnet_id": n.Subnet, "vpc_security_group_ids": []string{group}, "security_groups": []string{},
			"iam_instance_profile": profile, "associate_public_ip_address": true, "private_ip": ip, "instance_state": "running", "key_name": "",
			"user_data": "4c2c0b47e30a9e2e8b5b8f5d6c3e2a1b0c9d8e7f", "user_data_replace_on_change": false, "monitoring": false, "ebs_optimized": false,
			"metadata_options":  []any{map[string]any{"http_tokens": "required", "http_endpoint": "enabled", "http_put_response_hop_limit": 2}},
			"root_block_device": []any{map[string]any{"volume_size": 30, "volume_type": "gp3", "encrypted": false, "delete_on_termination": true}},
			"tags":              map[string]string{"Name": "legacy-" + name}, "tags_all": map[string]string{"Name": "legacy-" + name}}))
		return i.id
	}
	st.Bastion = addInstance("bastion", "bastion", n.Admin, "")
	st.Worker = addInstance("worker", "worker", n.Web, instRole)

	st.TerraformNetwork.resources = rs
	st.State = showJSON(rs)
	return st
}
