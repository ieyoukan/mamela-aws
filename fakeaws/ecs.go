package fakeaws

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// ECS of the fake: clusters, task definitions, services and tasks. A service's tasks run at once
// (its running count is its desired count); a run task is RUNNING until StopTask.

func ecsArn(kind, name string) string {
	return "arn:aws:ecs:" + Region + ":" + Account + ":" + kind + "/" + name
}

func lastPart(s string) string { return s[strings.LastIndex(s, "/")+1:] }

func strs(in map[string]any, k string) []string {
	var out []string
	if l, ok := in[k].([]any); ok {
		for _, v := range l {
			s, _ := v.(string)
			out = append(out, s)
		}
	}
	return out
}

func ecsTags(in map[string]any) ([]map[string]string, error) {
	var out []map[string]string
	l, _ := in["tags"].([]any)
	for _, v := range l {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fail(400, "InvalidParameterException", "tags are {key, value}")
		}
		k, _ := m["key"].(string)
		val, _ := m["value"].(string)
		if k == "" || len(m) != 2 {
			return nil, fail(400, "InvalidParameterException", "tags are {key, value}")
		}
		out = append(out, map[string]string{"key": k, "value": val})
	}
	return out, nil
}

// page answers a list a page at a time.
func page(items []string, key string, in map[string]any) map[string]any {
	start, _ := strconv.Atoi(str(in, "nextToken"))
	out := map[string]any{}
	if end := start + pageSize; end < len(items) {
		out[key], out["nextToken"] = items[start:end], strconv.Itoa(end)
	} else {
		out[key] = append([]string{}, items[min(start, len(items)):]...)
	}
	return out
}

func (a *API) findCluster(nameOrArn string) *cluster {
	n := lastPart(nameOrArn)
	for _, c := range a.clusters {
		if c.name == n && c.status == "ACTIVE" {
			return c
		}
	}
	return nil
}

// findTaskDef finds a task definition by family:revision (or its ARN), or a family's latest
// active revision.
func (a *API) findTaskDef(ref string) *taskDef {
	family, rev, hasRev := strings.Cut(lastPart(ref), ":")
	var latest *taskDef
	for _, t := range a.taskDefs {
		if t.family != family {
			continue
		}
		if hasRev && strconv.FormatInt(t.revision, 10) == rev {
			return t
		}
		if !hasRev && t.status == "ACTIVE" && (latest == nil || t.revision > latest.revision) {
			latest = t
		}
	}
	return latest
}

func (a *API) findService(cl, name string) *service {
	for _, s := range a.services {
		if s.cluster == cl && s.name == lastPart(name) && s.status != "INACTIVE" {
			return s
		}
	}
	return nil
}

// checkNetwork checks a Fargate task's network: subnets and security groups that exist.
func (a *API) checkNetwork(in map[string]any) (map[string]any, error) {
	nc, _ := in["networkConfiguration"].(map[string]any)
	vpc, _ := nc["awsvpcConfiguration"].(map[string]any)
	if vpc == nil {
		return nil, fail(400, "InvalidParameterException", "network configuration must be provided when networkMode 'awsvpc' is specified")
	}
	for _, s := range strs(vpc, "subnets") {
		if a.findSubnet(s) == nil {
			return nil, fail(400, "InvalidParameterException", "error retrieving subnet information for [%s]", s)
		}
	}
	for _, g := range strs(vpc, "securityGroups") {
		if a.findGroup(g) == nil {
			return nil, fail(400, "InvalidParameterException", "error retrieving security group information for [%s]", g)
		}
	}
	return nc, nil
}

func (a *API) launchOf(in map[string]any) (string, []map[string]any, error) {
	if lt := str(in, "launchType"); lt != "" {
		return lt, nil, nil
	}
	l, _ := in["capacityProviderStrategy"].([]any)
	if len(l) != 1 {
		return "", nil, fail(400, "InvalidParameterException", "the fake takes a launch type or one capacity provider")
	}
	m, _ := l[0].(map[string]any)
	return "", []map[string]any{{"capacityProvider": m["capacityProvider"], "weight": m["weight"], "base": 0}}, nil
}

func (a *API) ecs(act string, in map[string]any) (any, error) {
	clusterOf := func() (*cluster, error) {
		c := a.findCluster(str(in, "cluster"))
		if c == nil {
			return nil, fail(400, "ClusterNotFoundException", "cluster not found")
		}
		return c, nil
	}
	switch act {
	case "ListClusters":
		var arns []string
		for _, c := range a.clusters {
			if c.status == "ACTIVE" {
				arns = append(arns, ecsArn("cluster", c.name))
			}
		}
		slices.Sort(arns)
		return page(arns, "clusterArns", in), nil
	case "DescribeClusters":
		var out []map[string]any
		for _, ref := range strs(in, "clusters") {
			i := slices.IndexFunc(a.clusters, func(c *cluster) bool { return c.name == lastPart(ref) })
			if i < 0 {
				continue
			}
			c := a.clusters[i]
			instances, running := 0, 0
			for _, x := range a.insts {
				if x.cluster == c.name && x.state == "running" {
					instances++
				}
			}
			for _, s := range a.services {
				if s.cluster == c.name && s.status == "ACTIVE" {
					running += int(s.desired)
				}
			}
			for _, t := range a.tasks {
				if t.cluster == c.name && t.last == "RUNNING" {
					running++
				}
			}
			m := map[string]any{"clusterArn": ecsArn("cluster", c.name), "clusterName": c.name, "status": c.status,
				"registeredContainerInstancesCount": instances, "runningTasksCount": running, "pendingTasksCount": 0}
			if slices.Contains(strs(in, "include"), "TAGS") && c.tags != nil {
				m["tags"] = c.tags
			}
			out = append(out, m)
		}
		return map[string]any{"clusters": out, "failures": []any{}}, nil
	case "CreateCluster":
		name := str(in, "clusterName")
		t, err := ecsTags(in)
		if err != nil {
			return nil, err
		}
		if c := a.findCluster(name); c != nil {
			return map[string]any{"cluster": map[string]any{"clusterName": c.name}}, nil // idempotent
		}
		if !slices.Equal(strs(in, "capacityProviders"), []string{"FARGATE", "FARGATE_SPOT"}) {
			return nil, fail(400, "InvalidParameterException", "the fake wants capacity providers FARGATE and FARGATE_SPOT")
		}
		a.clusters = append(a.clusters, &cluster{name: name, status: "ACTIVE", tags: t})
		return map[string]any{"cluster": map[string]any{"clusterName": name}}, nil
	case "DeleteCluster":
		c, err := clusterOf()
		if err != nil {
			return nil, err
		}
		for _, x := range a.insts {
			if x.cluster == c.name && x.state != "terminated" {
				return nil, fail(400, "ClusterContainsContainerInstancesException", "the cluster cannot be deleted while container instances are active or draining")
			}
		}
		for _, s := range a.services {
			if s.cluster == c.name && s.status == "ACTIVE" {
				return nil, fail(400, "ClusterContainsServicesException", "the cluster cannot be deleted while services are active")
			}
		}
		for _, t := range a.tasks {
			if t.cluster == c.name && t.last == "RUNNING" {
				return nil, fail(400, "ClusterContainsTasksException", "the cluster cannot be deleted while tasks are active")
			}
		}
		c.status = "INACTIVE"
		return map[string]any{"cluster": map[string]any{"clusterName": c.name, "status": c.status}}, nil

	case "RegisterTaskDefinition":
		family := str(in, "family")
		t, err := ecsTags(in)
		if err != nil {
			return nil, err
		}
		compat := strs(in, "requiresCompatibilities")
		defs, _ := in["containerDefinitions"].([]any)
		if family == "" || len(defs) == 0 || len(compat) != 1 {
			return nil, fail(400, "ClientException", "family, containerDefinitions and requiresCompatibilities are required")
		}
		if compat[0] == "FARGATE" && (str(in, "networkMode") != "awsvpc" || str(in, "cpu") == "" || str(in, "memory") == "") {
			return nil, fail(400, "ClientException", "Fargate requires network mode awsvpc and task-level cpu and memory")
		}
		if arn := str(in, "executionRoleArn"); arn != "" && a.findRole(lastPart(arn)) == nil {
			return nil, fail(400, "ClientException", "role %s does not exist", arn)
		}
		for _, d := range defs {
			m, _ := d.(map[string]any)
			lc, _ := m["logConfiguration"].(map[string]any)
			opts, _ := lc["options"].(map[string]any)
			if g := str(opts, "awslogs-group"); g != "" {
				if !slices.ContainsFunc(a.logs, func(l *logGroup) bool { return l.name == g }) || str(opts, "awslogs-region") != Region {
					return nil, fail(400, "ClientException", "log group %s in %s does not exist", g, str(opts, "awslogs-region"))
				}
				if str(in, "executionRoleArn") == "" {
					return nil, fail(400, "ClientException", "awslogs needs an execution role")
				}
			}
		}
		var rev int64 = 1
		for _, x := range a.taskDefs {
			if x.family == family {
				rev = max(rev, x.revision+1)
			}
		}
		content := map[string]any{}
		for k, v := range in {
			if k != "family" && k != "tags" {
				content[k] = v
			}
		}
		a.taskDefs = append(a.taskDefs, &taskDef{family: family, revision: rev, status: "ACTIVE", tags: t, compatible: compat, content: content})
		return map[string]any{"taskDefinition": map[string]any{"family": family, "revision": rev}}, nil
	case "ListTaskDefinitions":
		var arns []string
		for _, t := range a.taskDefs {
			if t.status == str(in, "status") || str(in, "status") == "" && t.status == "ACTIVE" {
				arns = append(arns, ecsArn("task-definition", fmt.Sprintf("%s:%d", t.family, t.revision)))
			}
		}
		return page(arns, "taskDefinitionArns", in), nil
	case "DescribeTaskDefinition":
		t := a.findTaskDef(str(in, "taskDefinition"))
		if t == nil {
			return nil, fail(400, "ClientException", "Unable to describe task definition.")
		}
		out := map[string]any{"taskDefinition": t.describe()}
		if slices.Contains(strs(in, "include"), "TAGS") {
			out["tags"] = t.tags
		}
		return out, nil
	case "DeregisterTaskDefinition":
		t := a.findTaskDef(str(in, "taskDefinition"))
		if t == nil {
			return nil, fail(400, "ClientException", "the specified task definition does not exist")
		}
		t.status = "INACTIVE"
		return map[string]any{"taskDefinition": map[string]any{"family": t.family, "revision": t.revision, "status": t.status}}, nil
	case "DeleteTaskDefinitions":
		var out []map[string]any
		for _, td := range strs(in, "taskDefinitions") {
			t := a.findTaskDef(td)
			if t == nil || t.status == "ACTIVE" {
				return nil, fail(400, "ClientException", "The specified task definition must be INACTIVE to be deleted.")
			}
			t.status = "DELETE_IN_PROGRESS"
			out = append(out, map[string]any{"family": t.family, "revision": t.revision, "status": t.status})
		}
		return map[string]any{"taskDefinitions": out, "failures": []any{}}, nil

	case "CreateService", "RunTask":
		c, err := clusterOf()
		if err != nil {
			return nil, err
		}
		td := a.findTaskDef(str(in, "taskDefinition"))
		if td == nil || td.status != "ACTIVE" {
			return nil, fail(400, "ClientException", "TaskDefinition not found or not active")
		}
		t, err := ecsTags(in)
		if err != nil {
			return nil, err
		}
		launch, strategy, err := a.launchOf(in)
		if err != nil {
			return nil, err
		}
		var network map[string]any
		if launch != "EC2" {
			if network, err = a.checkNetwork(in); err != nil {
				return nil, err
			}
		} else if !slices.Contains(td.compatible, "EC2") {
			return nil, fail(400, "InvalidParameterException", "task definition does not support launch_type EC2")
		}
		if act == "RunTask" {
			if num(in, "count") != 1 {
				return nil, fail(400, "InvalidParameterException", "the fake runs one task at a time")
			}
			token := str(in, "clientToken")
			for _, x := range a.tasks {
				if token != "" && x.token == token {
					return map[string]any{"tasks": []any{map[string]any{"taskArn": x.arn}}, "failures": []any{}}, nil
				}
			}
			x := &task{arn: ecsArn("task/"+c.name, strings.TrimPrefix(a.id("t"), "t-")), cluster: c.name, taskDef: str(in, "taskDefinition"),
				startedBy: str(in, "startedBy"), token: token, desired: "RUNNING", last: "RUNNING", tags: t}
			a.tasks = append(a.tasks, x)
			return map[string]any{"tasks": []any{map[string]any{"taskArn": x.arn, "lastStatus": "PROVISIONING"}}, "failures": []any{}}, nil
		}
		name := str(in, "serviceName")
		if a.findService(c.name, name) != nil {
			return nil, fail(400, "InvalidParameterException", "Creation of service was not idempotent.")
		}
		s := &service{cluster: c.name, name: name, status: "ACTIVE", taskDef: fmt.Sprintf("%s:%d", td.family, td.revision), launch: launch,
			desired: num(in, "desiredCount"), strategy: strategy, network: network, tags: t}
		a.services = append(a.services, s)
		return map[string]any{"service": map[string]any{"serviceName": name}}, nil
	case "ListServices":
		c, err := clusterOf()
		if err != nil {
			return nil, err
		}
		var arns []string
		for _, s := range a.services {
			if s.cluster == c.name && s.status != "INACTIVE" {
				arns = append(arns, ecsArn("service/"+c.name, s.name))
			}
		}
		return page(arns, "serviceArns", in), nil
	case "DescribeServices":
		c, err := clusterOf()
		if err != nil {
			return nil, err
		}
		if len(strs(in, "services")) > 10 {
			return nil, fail(400, "InvalidParameterException", "at most 10 services")
		}
		var out []map[string]any
		for _, ref := range strs(in, "services") {
			i := slices.IndexFunc(a.services, func(s *service) bool { return s.cluster == c.name && s.name == lastPart(ref) })
			if i < 0 {
				continue
			}
			s := a.services[i]
			m := map[string]any{"serviceName": s.name, "serviceArn": ecsArn("service/"+c.name, s.name), "status": s.status, "desiredCount": s.desired,
				"runningCount": s.desired, "taskDefinition": ecsArn("task-definition", s.taskDef)}
			if s.launch != "" {
				m["launchType"] = s.launch
			} else {
				m["capacityProviderStrategy"] = s.strategy
			}
			if s.network != nil {
				m["networkConfiguration"] = s.network
			}
			if slices.Contains(strs(in, "include"), "TAGS") {
				m["tags"] = s.tags
			}
			out = append(out, m)
		}
		return map[string]any{"services": out, "failures": []any{}}, nil
	case "TagResource":
		arn := str(in, "resourceArn")
		t, err := ecsTags(in)
		if err != nil || len(t) == 0 {
			return nil, fail(400, "InvalidParameterException", "tags are {key, value}")
		}
		var target *[]map[string]string
		for _, c := range a.clusters {
			if c.status == "ACTIVE" && ecsArn("cluster", c.name) == arn {
				target = &c.tags
			}
		}
		for _, x := range a.services {
			if x.status == "ACTIVE" && ecsArn("service/"+x.cluster, x.name) == arn {
				target = &x.tags
			}
		}
		for _, x := range a.taskDefs {
			if ecsArn("task-definition", fmt.Sprintf("%s:%d", x.family, x.revision)) == arn {
				target = &x.tags
			}
		}
		if target == nil {
			return nil, fail(400, "InvalidParameterException", "the specified resource %s does not exist", arn)
		}
		for _, kv := range t {
			i := slices.IndexFunc(*target, func(m map[string]string) bool { return m["key"] == kv["key"] })
			if i >= 0 {
				(*target)[i] = kv
			} else {
				*target = append(*target, kv)
			}
		}
		return map[string]any{}, nil
	case "UpdateService", "DeleteService":
		c, err := clusterOf()
		if err != nil {
			return nil, err
		}
		s := a.findService(c.name, str(in, "service"))
		if s == nil || s.status != "ACTIVE" {
			return nil, fail(400, "ServiceNotFoundException", "service not found")
		}
		if act == "DeleteService" {
			if f, _ := in["force"].(bool); !f && s.desired > 0 {
				return nil, fail(400, "InvalidParameterException", "the service cannot be stopped while it is scaled above 0")
			}
			s.status = "INACTIVE"
			return map[string]any{"service": map[string]any{"serviceName": s.name, "status": "DRAINING"}}, nil
		}
		if _, ok := in["desiredCount"]; ok {
			s.desired = num(in, "desiredCount")
		}
		if ref := str(in, "taskDefinition"); ref != "" {
			td := a.findTaskDef(ref)
			if td == nil || td.status != "ACTIVE" {
				return nil, fail(400, "ClientException", "TaskDefinition not found or not active")
			}
			s.taskDef = fmt.Sprintf("%s:%d", td.family, td.revision)
		}
		if _, ok := in["capacityProviderStrategy"]; ok {
			_, strategy, err := a.launchOf(map[string]any{"capacityProviderStrategy": in["capacityProviderStrategy"]})
			if err != nil {
				return nil, err
			}
			if s.launch == "EC2" {
				return nil, fail(400, "InvalidParameterException", "a service on EC2 does not move to a capacity provider in the fake")
			}
			if b, _ := in["forceNewDeployment"].(bool); !b {
				return nil, fail(400, "InvalidParameterException", "changing the capacity provider strategy needs forceNewDeployment")
			}
			s.launch, s.strategy = "", strategy
		}
		if _, ok := in["networkConfiguration"]; ok {
			network, err := a.checkNetwork(in)
			if err != nil {
				return nil, err
			}
			s.network = network
		}
		return map[string]any{"service": map[string]any{"serviceName": s.name}}, nil

	case "ListTasks":
		c, err := clusterOf()
		if err != nil {
			return nil, err
		}
		want := str(in, "desiredStatus")
		if want == "" {
			want = "RUNNING"
		}
		var arns []string
		for _, t := range a.tasks {
			if t.cluster == c.name && t.desired == want && (str(in, "startedBy") == "" || t.startedBy == str(in, "startedBy")) {
				arns = append(arns, t.arn)
			}
		}
		return page(arns, "taskArns", in), nil
	case "DescribeTasks":
		c, err := clusterOf()
		if err != nil {
			return nil, err
		}
		var out []map[string]any
		for _, ref := range strs(in, "tasks") {
			for _, t := range a.tasks {
				if t.cluster == c.name && (t.arn == ref || lastPart(t.arn) == ref) {
					m := map[string]any{"taskArn": t.arn, "lastStatus": t.last, "desiredStatus": t.desired}
					if t.reason != "" {
						m["stoppedReason"] = t.reason
					}
					if slices.Contains(strs(in, "include"), "TAGS") {
						m["tags"] = t.tags
					}
					out = append(out, m)
				}
			}
		}
		return map[string]any{"tasks": out, "failures": []any{}}, nil
	case "StopTask":
		for _, t := range a.tasks {
			if t.arn == str(in, "task") || lastPart(t.arn) == str(in, "task") {
				t.desired, t.last = "STOPPED", "STOPPED"
				return map[string]any{"task": map[string]any{"taskArn": t.arn}}, nil
			}
		}
		return nil, fail(400, "InvalidParameterException", "the referenced task was not found")
	}
	return nil, fail(400, "InvalidAction", "the fake does not know %s", act)
}

// StopTasks stops every run task (as their command ends), with the reason.
func (a *API) StopTasks(reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range a.tasks {
		t.desired, t.last, t.reason = "STOPPED", "STOPPED", reason
	}
}

// describe is a task definition as DescribeTaskDefinition answers it: what was registered, with
// the defaults ECS fills in.
func (t *taskDef) describe() map[string]any {
	out := map[string]any{"family": t.family, "revision": t.revision, "status": t.status,
		"taskDefinitionArn": ecsArn("task-definition", fmt.Sprintf("%s:%d", t.family, t.revision)),
		"compatibilities":   []string{"EC2", "FARGATE"}, "requiresAttributes": []any{map[string]any{"name": "com.amazonaws.ecs.capability.logging-driver.awslogs"}},
		"registeredAt": "2026-11-12T00:00:00Z", "registeredBy": "arn:aws:sts::" + Account + ":assumed-role/operator/session",
		"volumes": []any{}, "placementConstraints": []any{}}
	for k, v := range t.content {
		out[k] = v
	}
	awsvpc := t.content["networkMode"] == "awsvpc"
	var defs []any
	for _, d := range list(t.content["containerDefinitions"]) {
		m, _ := d.(map[string]any)
		c := map[string]any{"cpu": 0, "mountPoints": []any{}, "volumesFrom": []any{}, "systemControls": []any{}, "environment": []any{}, "portMappings": []any{}, "essential": true}
		for k, v := range m {
			c[k] = v
		}
		var ports []any
		for _, p := range list(c["portMappings"]) {
			pm := map[string]any{}
			for k, v := range p.(map[string]any) {
				pm[k] = v
			}
			if _, ok := pm["hostPort"]; !ok && awsvpc {
				pm["hostPort"] = pm["containerPort"]
			}
			ports = append(ports, pm)
		}
		c["portMappings"] = append([]any{}, ports...)
		defs = append(defs, c)
	}
	out["containerDefinitions"] = defs
	return out
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}
