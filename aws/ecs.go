package aws

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// ECS: a cluster (Fargate and Fargate Spot, and container instances on EC2), task definitions and
// services. A one-off task (a load test) is not declared: it is the explicit operation run-task
// of a service, which runs one task the way the service runs its own (a service may keep zero).

// Kinds and operations of ECS.
const (
	KindCluster        protocol.ResourceKind = "AwsEcsCluster"
	KindTaskDefinition protocol.ResourceKind = "AwsEcsTaskDefinition"
	KindService        protocol.ResourceKind = "AwsEcsService"

	OpRevise  protocol.Op = "revise"   // a new revision of a task definition
	OpUpdate  protocol.Op = "update"   // a service's count or task definition
	OpRunTask protocol.Op = "run-task" // explicit: one task, as the service runs them
)

const (
	// familyPrefix starts the family of every task definition this provider registers (ECS
	// lists task definitions by family, and the provider must find its own).
	familyPrefix = "mamela-"
	// runStartedBy is the startedBy of the tasks of run-task.
	runStartedBy = "mamela-run"
	// TagDigest is the digest of what a task definition revision was registered with.
	TagDigest = "mamela:digest"
	// TagRun is the token of a run-task: the task it started carries it.
	TagRun = "mamela:run"
)

// Launches: how a service runs its tasks.
const (
	LaunchEC2         = "EC2"
	LaunchFargate     = "FARGATE"
	LaunchFargateSpot = "FARGATE_SPOT"
)

var (
	listClusters      = jsonCall(APIECS, "ListClusters", "nextToken")
	describeClusters  = jsonCall(APIECS, "DescribeClusters", "clusters", "include")
	createCluster     = jsonCall(APIECS, "CreateCluster", "clusterName", "capacityProviders", "defaultCapacityProviderStrategy", "tags", "settings")
	deleteCluster     = jsonCall(APIECS, "DeleteCluster", "cluster")
	listTaskDefs      = jsonCall(APIECS, "ListTaskDefinitions", "status", "nextToken")
	describeTaskDef   = jsonCall(APIECS, "DescribeTaskDefinition", "taskDefinition", "include")
	registerTaskDef   = jsonCall(APIECS, "RegisterTaskDefinition", "family", "containerDefinitions", "cpu", "memory", "networkMode", "requiresCompatibilities", "executionRoleArn", "tags")
	deregisterTaskDef = jsonCall(APIECS, "DeregisterTaskDefinition", "taskDefinition")
	listServices      = jsonCall(APIECS, "ListServices", "cluster", "nextToken")
	describeServices  = jsonCall(APIECS, "DescribeServices", "cluster", "services", "include")
	createService     = jsonCall(APIECS, "CreateService", "cluster", "serviceName", "taskDefinition", "desiredCount", "launchType", "capacityProviderStrategy",
		"networkConfiguration", "deploymentConfiguration", "tags")
	updateService = jsonCall(APIECS, "UpdateService", "cluster", "service", "desiredCount", "taskDefinition")
	deleteService = jsonCall(APIECS, "DeleteService", "cluster", "service", "force")
	listTasks     = jsonCall(APIECS, "ListTasks", "cluster", "startedBy", "desiredStatus", "nextToken")
	describeTasks = jsonCall(APIECS, "DescribeTasks", "cluster", "tasks", "include")
	runTask       = jsonCall(APIECS, "RunTask", "cluster", "taskDefinition", "count", "launchType", "capacityProviderStrategy", "networkConfiguration",
		"startedBy", "clientToken", "tags")
)

// ecsTagsWith are ECS tags with more.
func ecsTagsWith(boundary, name string, more ...[2]string) []map[string]string {
	out := ecsTags(boundary, name)
	for _, kv := range more {
		out = append(out, map[string]string{"key": kv[0], "value": kv[1]})
	}
	return out
}

// lastPart is what follows the last "/" of an ARN ("cluster/x" -> "x").
func lastPart(arn string) string { return arn[strings.LastIndex(arn, "/")+1:] }

// --- cluster ----------------------------------------------------------------------------------

type clusterKind struct{}

type clusterSpec struct {
	Boundary string `json:"boundary"`
}

type clusterState struct {
	Instances int64 `json:"instances"` // registered container instances
	Running   int64 `json:"running"`   // running tasks
}

// ecsCluster is a cluster as DescribeClusters answers.
type ecsCluster struct {
	Name      string   `json:"clusterName"`
	Status    string   `json:"status"`
	Instances int64    `json:"registeredContainerInstancesCount"`
	Running   int64    `json:"runningTasksCount"`
	Pending   int64    `json:"pendingTasksCount"`
	Tags      []ecsTag `json:"tags"`
}

// allClusters are the account's clusters in the region (the guard's too).
func allClusters(c *client) ([]ecsCluster, error) {
	var arns []string
	next := ""
	for {
		var resp struct {
			Arns []string `json:"clusterArns"`
			Next string   `json:"nextToken"`
		}
		m := map[string]any{}
		if next != "" {
			m["nextToken"] = next
		}
		if err := c.call(APIECS, "ListClusters", m, &resp); err != nil {
			return nil, err
		}
		arns = append(arns, resp.Arns...)
		if resp.Next == "" {
			break
		}
		next = resp.Next
	}
	slices.Sort(arns)
	var out []ecsCluster
	for chunk := range slices.Chunk(arns, 100) {
		var resp struct {
			Clusters []ecsCluster `json:"clusters"`
		}
		if err := c.call(APIECS, "DescribeClusters", map[string]any{"clusters": chunk, "include": []string{"TAGS"}}, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Clusters...)
	}
	return out, nil
}

func (clusterKind) kind() protocol.ResourceKind { return KindCluster }
func (clusterKind) reads() []protocol.Call      { return []protocol.Call{listClusters, describeClusters} }
func (clusterKind) operations() []protocol.Operation {
	return []protocol.Operation{op(OpCreate, createCluster), deleteOp(deleteCluster)}
}

func (clusterKind) list(c *client) ([]object, error) {
	all, err := allClusters(c)
	if err != nil {
		return nil, err
	}
	var out []object
	for _, x := range all {
		if ecsTagOf(x.Tags, TagName) == "" || x.Status == "INACTIVE" {
			continue
		}
		out = append(out, object{Name: ecsTagOf(x.Tags, TagName), Boundary: ecsTagOf(x.Tags, TagBoundary), IDs: map[string]string{"cluster": x.Name},
			State: clusterState{Instances: x.Instances, Running: x.Running + x.Pending}})
	}
	return out, nil
}

func (clusterKind) missing(*client, protocol.Resource) (any, error) { return nil, nil }

func (clusterKind) diff(r protocol.Resource, o protocol.Observed, _ map[string]protocol.Observed) ([]protocol.Change, error) {
	s, err := spec[clusterSpec](r)
	if err != nil {
		return nil, err
	}
	name := awsName(s.Boundary, r.Name)
	if err := checkName(name); err != nil {
		return nil, err
	}
	if o.Exists {
		return nil, nil
	}
	return []protocol.Change{{Resource: r.ID, Kind: KindCluster, Op: OpCreate, IDs: map[string]string{"mamela-name": r.Name, "cluster": name},
		Reasons: []string{"ECS cluster " + name + " (Fargate Spot by default; Container Insights off)"},
		Requests: []protocol.Approved{jsonReq(APIECS, "CreateCluster", map[string]any{
			"clusterName":                     name,
			"capacityProviders":               []string{LaunchFargate, LaunchFargateSpot},
			"defaultCapacityProviderStrategy": []map[string]any{{"capacityProvider": LaunchFargateSpot, "weight": 1}},
			"settings":                        []map[string]string{{"name": "containerInsights", "value": "disabled"}},
			"tags":                            ecsTags(s.Boundary, r.Name),
		})}}}, nil
}

func (clusterKind) remove(o protocol.Observed) (protocol.Change, error) {
	name := o.IDs["cluster"]
	st := state[clusterState](o)
	return protocol.Change{Resource: o.ID, Kind: KindCluster, Op: OpDelete, IDs: map[string]string{"cluster": name},
		Reasons: []string{fmt.Sprintf("delete ECS cluster %s (now %d container instances, %d tasks: its services must be deleted and its instances terminated first)",
			name, st.Instances, st.Running)},
		Requests: []protocol.Approved{jsonReq(APIECS, "DeleteCluster", map[string]any{"cluster": name})}}, nil
}

func (k clusterKind) done(c *client, _ protocol.Change, a protocol.Approved) (bool, error) {
	objs, err := k.list(c)
	if err != nil {
		return false, err
	}
	name := param(a, "clusterName") + param(a, "cluster")
	found := slices.ContainsFunc(objs, func(x object) bool { return x.IDs["cluster"] == name })
	switch action(a) {
	case "CreateCluster":
		return found, nil
	case "DeleteCluster":
		return !found, nil
	}
	return false, nil
}

// --- task definition --------------------------------------------------------------------------

type taskDefinitionKind struct{}

// Container is one container of a task definition.
type Container struct {
	Name        string   `json:"name"`
	Image       string   `json:"image"`
	Command     []string `json:"command,omitempty"`
	Environment []Env    `json:"environment,omitempty"`
	Ports       []Port   `json:"ports,omitempty"`
	MemoryMiB   int64    `json:"memoryMiB,omitempty"`
}

// Env is an environment variable: Value, or Prefix and the private IP address of the
// AwsInstance InstanceIP ("http://10.0.1.5").
type Env struct {
	Name       string `json:"name"`
	Value      string `json:"value,omitempty"`
	InstanceIP string `json:"instanceIp,omitempty"`
	Prefix     string `json:"prefix,omitempty"`
}

// Port is a container's port, and on EC2 the instance's port it is reached on.
type Port struct {
	Container int64 `json:"container"`
	Host      int64 `json:"host,omitempty"`
}

type taskDefinitionSpec struct {
	Boundary      string      `json:"boundary"`
	Launch        string      `json:"launch"`    // FARGATE (Fargate and Fargate Spot) or EC2
	CPU           int64       `json:"cpu"`       // CPU units, 1024 to a vCPU
	MemoryMiB     int64       `json:"memoryMiB"` // MiB
	ExecutionRole string      `json:"executionRole,omitempty"`
	LogGroup      string      `json:"logGroup,omitempty"`
	Containers    []Container `json:"containers"`
}

type taskDefinitionState struct {
	Revision int64  `json:"revision"`
	Digest   string `json:"digest"`
}

// Family is a task definition's family.
func Family(boundary, name string) string { return familyPrefix + awsName(boundary, name) }

func (taskDefinitionKind) kind() protocol.ResourceKind { return KindTaskDefinition }
func (taskDefinitionKind) reads() []protocol.Call {
	return []protocol.Call{listTaskDefs, describeTaskDef}
}
func (taskDefinitionKind) operations() []protocol.Operation {
	return []protocol.Operation{op(OpCreate, registerTaskDef), op(OpRevise, registerTaskDef, deregisterTaskDef), deleteOp(deregisterTaskDef)}
}

// latestTaskDefinitions are the latest active revision of each family of this provider's.
func latestTaskDefinitions(c *client) (map[string]int64, error) {
	latest := map[string]int64{}
	next := ""
	for {
		var resp struct {
			Arns []string `json:"taskDefinitionArns"`
			Next string   `json:"nextToken"`
		}
		m := map[string]any{"status": "ACTIVE"}
		if next != "" {
			m["nextToken"] = next
		}
		if err := c.call(APIECS, "ListTaskDefinitions", m, &resp); err != nil {
			return nil, err
		}
		for _, arn := range resp.Arns {
			family, rev, ok := strings.Cut(lastPart(arn), ":")
			n, err := strconv.ParseInt(rev, 10, 64)
			if !ok || err != nil || !strings.HasPrefix(family, familyPrefix) {
				continue
			}
			latest[family] = max(latest[family], n)
		}
		if resp.Next == "" {
			return latest, nil
		}
		next = resp.Next
	}
}

type ecsTaskDefinition struct {
	Definition struct {
		Family   string `json:"family"`
		Revision int64  `json:"revision"`
		Status   string `json:"status"`
	} `json:"taskDefinition"`
	Tags []ecsTag `json:"tags"`
}

func describeTaskDefinition(c *client, td string) (ecsTaskDefinition, error) {
	var resp ecsTaskDefinition
	err := c.call(APIECS, "DescribeTaskDefinition", map[string]any{"taskDefinition": td, "include": []string{"TAGS"}}, &resp)
	return resp, err
}

func (taskDefinitionKind) list(c *client) ([]object, error) {
	latest, err := latestTaskDefinitions(c)
	if err != nil {
		return nil, err
	}
	var out []object
	for _, family := range slices.Sorted(maps.Keys(latest)) {
		td := fmt.Sprintf("%s:%d", family, latest[family])
		d, err := describeTaskDefinition(c, td)
		if err != nil {
			return nil, err
		}
		if ecsTagOf(d.Tags, TagName) == "" {
			continue
		}
		out = append(out, object{Name: ecsTagOf(d.Tags, TagName), Boundary: ecsTagOf(d.Tags, TagBoundary),
			IDs:   map[string]string{"family": family, "taskDefinition": td},
			State: taskDefinitionState{Revision: latest[family], Digest: ecsTagOf(d.Tags, TagDigest)}})
	}
	return out, nil
}

func (taskDefinitionKind) missing(*client, protocol.Resource) (any, error) { return nil, nil }

// register is the RegisterTaskDefinition of a declaration, with the digest it is tagged with.
func register(r protocol.Resource, s taskDefinitionSpec, deps map[string]protocol.Observed) (protocol.Approved, string, error) {
	var mode string
	switch s.Launch {
	case LaunchFargate:
		mode = "awsvpc"
	case LaunchEC2:
		mode = "bridge"
	default:
		return protocol.Approved{}, "", fmt.Errorf("launch %q is FARGATE (Fargate and Fargate Spot) or EC2", s.Launch)
	}
	if len(s.Containers) == 0 {
		return protocol.Approved{}, "", fmt.Errorf("a task definition has containers")
	}
	if s.LogGroup != "" && s.ExecutionRole == "" {
		return protocol.Approved{}, "", fmt.Errorf("logs to %s need an executionRole (ecs-tasks, ecs-task-execution) to write them", s.LogGroup)
	}
	var defs []map[string]any
	for _, ct := range s.Containers {
		d := map[string]any{"name": ct.Name, "image": ct.Image, "essential": true}
		if len(ct.Command) > 0 {
			d["command"] = ct.Command
		}
		var env []map[string]string
		for _, e := range ct.Environment {
			v := e.Value
			if e.InstanceIP != "" {
				inst, err := dep(deps, KindInstance, e.InstanceIP)
				if err != nil {
					return protocol.Approved{}, "", err
				}
				ip := state[instanceState](inst).PrivateIP
				if ip == "" {
					return protocol.Approved{}, "", fmt.Errorf("%s %s has no private IP address yet (it is %s)", KindInstance, e.InstanceIP, state[instanceState](inst).State)
				}
				v = e.Prefix + ip
			}
			env = append(env, map[string]string{"name": e.Name, "value": v})
		}
		if len(env) > 0 {
			d["environment"] = env
		}
		var ports []map[string]any
		for _, p := range ct.Ports {
			m := map[string]any{"containerPort": p.Container, "protocol": "tcp"}
			if p.Host != 0 {
				m["hostPort"] = p.Host
			}
			ports = append(ports, m)
		}
		if len(ports) > 0 {
			d["portMappings"] = ports
		}
		if ct.MemoryMiB > 0 {
			d["memory"] = ct.MemoryMiB
		}
		if s.LogGroup != "" {
			lg, err := dep(deps, KindLogGroup, s.LogGroup)
			if err != nil {
				return protocol.Approved{}, "", err
			}
			d["logConfiguration"] = map[string]any{"logDriver": "awslogs", "options": map[string]string{
				"awslogs-group": lg.IDs["logGroupName"], "awslogs-region": state[logGroupState](lg).Region, "awslogs-stream-prefix": r.Name}}
		}
		defs = append(defs, d)
	}
	members := map[string]any{"family": Family(s.Boundary, r.Name), "containerDefinitions": defs, "cpu": strconv.FormatInt(s.CPU, 10),
		"memory": strconv.FormatInt(s.MemoryMiB, 10), "networkMode": mode, "requiresCompatibilities": []string{s.Launch}}
	if s.ExecutionRole != "" {
		role, err := dep(deps, KindIAMRole, s.ExecutionRole)
		if err != nil {
			return protocol.Approved{}, "", err
		}
		members["executionRoleArn"] = state[roleState](role).Arn
	}
	text, err := json.Marshal(members, json.Deterministic(true))
	if err != nil {
		return protocol.Approved{}, "", err
	}
	sum := sha256.Sum256(text)
	digest := hex.EncodeToString(sum[:12])
	members["tags"] = ecsTagsWith(s.Boundary, r.Name, [2]string{TagDigest, digest})
	return jsonReq(APIECS, "RegisterTaskDefinition", members), digest, nil
}

func (taskDefinitionKind) diff(r protocol.Resource, o protocol.Observed, deps map[string]protocol.Observed) ([]protocol.Change, error) {
	s, err := spec[taskDefinitionSpec](r)
	if err != nil {
		return nil, err
	}
	a, digest, err := register(r, s, deps)
	if err != nil {
		return nil, err
	}
	family := Family(s.Boundary, r.Name)
	what := fmt.Sprintf("%s, %d CPU units, %d MiB, %d containers (digest %s)", s.Launch, s.CPU, s.MemoryMiB, len(s.Containers), digest)
	if !o.Exists {
		return []protocol.Change{{Resource: r.ID, Kind: KindTaskDefinition, Op: OpCreate, IDs: map[string]string{"mamela-name": r.Name, "family": family},
			Reasons: []string{"task definition " + family + ": " + what}, Requests: []protocol.Approved{a}}}, nil
	}
	st := state[taskDefinitionState](o)
	if st.Digest == digest {
		return nil, nil
	}
	old := o.IDs["taskDefinition"]
	return []protocol.Change{{Resource: r.ID, Kind: KindTaskDefinition, Op: OpRevise, IDs: map[string]string{"family": family, "taskDefinition": old},
		Reasons:  []string{fmt.Sprintf("revision %d was registered with digest %s: register a new one (%s) and deregister it", st.Revision, st.Digest, what)},
		Requests: []protocol.Approved{a, jsonReq(APIECS, "DeregisterTaskDefinition", map[string]any{"taskDefinition": old})}}}, nil
}

func (taskDefinitionKind) remove(o protocol.Observed) (protocol.Change, error) {
	td := o.IDs["taskDefinition"]
	return protocol.Change{Resource: o.ID, Kind: KindTaskDefinition, Op: OpDelete, IDs: map[string]string{"taskDefinition": td},
		Reasons:  []string{"deregister " + td + " (ECS keeps it as INACTIVE; a service that uses it keeps running)"},
		Requests: []protocol.Approved{jsonReq(APIECS, "DeregisterTaskDefinition", map[string]any{"taskDefinition": td})}}, nil
}

// digestOf is the digest an approved RegisterTaskDefinition is tagged with.
func digestOf(a protocol.Approved) string {
	var tags []ecsTag
	if v := a.Params["tags"]; len(v) == 1 {
		json.Unmarshal([]byte(v[0]), &tags)
	}
	return ecsTagOf(tags, TagDigest)
}

func (k taskDefinitionKind) done(c *client, _ protocol.Change, a protocol.Approved) (bool, error) {
	switch action(a) {
	case "RegisterTaskDefinition":
		objs, err := k.list(c)
		if err != nil {
			return false, err
		}
		return slices.ContainsFunc(objs, func(x object) bool {
			return x.IDs["family"] == param(a, "family") && x.State.(taskDefinitionState).Digest == digestOf(a)
		}), nil
	case "DeregisterTaskDefinition":
		d, err := describeTaskDefinition(c, param(a, "taskDefinition"))
		if notFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return d.Definition.Status != "ACTIVE", nil
	}
	return false, nil
}

// --- service ----------------------------------------------------------------------------------

type serviceKind struct{}

type serviceSpec struct {
	Boundary       string `json:"boundary"`
	Cluster        string `json:"cluster"`
	TaskDefinition string `json:"taskDefinition"`
	Count          int64  `json:"count"`
	Launch         string `json:"launch"`                  // EC2, FARGATE or FARGATE_SPOT
	Subnet         string `json:"subnet,omitempty"`        // Fargate: the subnet of its tasks
	SecurityGroup  string `json:"securityGroup,omitempty"` // Fargate: their security group
	PublicIP       bool   `json:"publicIp,omitempty"`      // Fargate: a public IP address (to pull images without a NAT)
}

type serviceState struct {
	Boundary       string    `json:"boundary"`
	TaskDefinition string    `json:"taskDefinition"` // family:revision
	Count          int64     `json:"count"`
	Running        int64     `json:"running"`
	Status         string    `json:"status"`
	Launch         string    `json:"launch"`
	Subnets        []string  `json:"subnets,omitempty"`
	Groups         []string  `json:"groups,omitempty"`
	PublicIP       bool      `json:"publicIp,omitempty"`
	Runs           []taskRun `json:"runs,omitempty"` // run-task's tasks ECS still knows
}

type taskRun struct {
	Token  string `json:"token"`
	Task   string `json:"task"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"` // why it stopped
}

type ecsService struct {
	Name           string `json:"serviceName"`
	Status         string `json:"status"`
	Desired        int64  `json:"desiredCount"`
	Running        int64  `json:"runningCount"`
	TaskDefinition string `json:"taskDefinition"`
	LaunchType     string `json:"launchType"`
	Strategy       []struct {
		Provider string `json:"capacityProvider"`
	} `json:"capacityProviderStrategy"`
	Network struct {
		Awsvpc struct {
			Subnets  []string `json:"subnets"`
			Groups   []string `json:"securityGroups"`
			PublicIP string   `json:"assignPublicIp"`
		} `json:"awsvpcConfiguration"`
	} `json:"networkConfiguration"`
	Tags []ecsTag `json:"tags"`
}

type ecsTask struct {
	Arn        string   `json:"taskArn"`
	LastStatus string   `json:"lastStatus"`
	Reason     string   `json:"stoppedReason"`
	Tags       []ecsTag `json:"tags"`
}

func (serviceKind) kind() protocol.ResourceKind { return KindService }
func (serviceKind) reads() []protocol.Call {
	return []protocol.Call{listClusters, describeClusters, listServices, describeServices, listTasks, describeTasks}
}
func (serviceKind) operations() []protocol.Operation {
	return []protocol.Operation{op(OpCreate, createService), op(OpUpdate, updateService),
		unsupportedOp(OpReplace, "a service keeps how it runs its tasks (launch, subnet, security group): declare a new one"),
		explicitOp(OpRunTask, runTask), deleteOp(deleteService)}
}

// pages lists with a JSON API's nextToken.
func pages[T any](c *client, api, act string, members map[string]any, items func(T) ([]string, string)) ([]string, error) {
	var out []string
	next := ""
	for {
		m := maps.Clone(members)
		if next != "" {
			m["nextToken"] = next
		}
		var resp T
		if err := c.call(api, act, m, &resp); err != nil {
			return nil, err
		}
		got, n := items(resp)
		out = append(out, got...)
		if n == "" {
			return out, nil
		}
		next = n
	}
}

type arnPage struct {
	Services []string `json:"serviceArns"`
	Tasks    []string `json:"taskArns"`
	Next     string   `json:"nextToken"`
}

// runsOf are the tasks of run-task in a cluster, by the name of the service they ran as.
func runsOf(c *client, cluster string) (map[[2]string][]taskRun, error) {
	var arns []string
	for _, status := range []string{"RUNNING", "STOPPED"} {
		got, err := pages(c, APIECS, "ListTasks", map[string]any{"cluster": cluster, "startedBy": runStartedBy, "desiredStatus": status},
			func(p arnPage) ([]string, string) { return p.Tasks, p.Next })
		if err != nil {
			return nil, err
		}
		arns = append(arns, got...)
	}
	slices.Sort(arns)
	out := map[[2]string][]taskRun{}
	for chunk := range slices.Chunk(slices.Compact(arns), 100) {
		var resp struct {
			Tasks []ecsTask `json:"tasks"`
		}
		if err := c.call(APIECS, "DescribeTasks", map[string]any{"cluster": cluster, "tasks": chunk, "include": []string{"TAGS"}}, &resp); err != nil {
			return nil, err
		}
		for _, t := range resp.Tasks {
			if tok := ecsTagOf(t.Tags, TagRun); tok != "" {
				key := [2]string{ecsTagOf(t.Tags, TagBoundary), ecsTagOf(t.Tags, TagName)}
				out[key] = append(out[key], taskRun{Token: tok, Task: lastPart(t.Arn), Status: t.LastStatus, Reason: t.Reason})
			}
		}
	}
	return out, nil
}

func (serviceKind) list(c *client) ([]object, error) {
	clusters, err := clusterKind{}.list(c)
	if err != nil {
		return nil, err
	}
	var out []object
	for _, cl := range clusters {
		cluster := cl.IDs["cluster"]
		arns, err := pages(c, APIECS, "ListServices", map[string]any{"cluster": cluster}, func(p arnPage) ([]string, string) { return p.Services, p.Next })
		if err != nil {
			return nil, err
		}
		runs, err := runsOf(c, cluster)
		if err != nil {
			return nil, err
		}
		slices.Sort(arns)
		for chunk := range slices.Chunk(arns, 10) {
			var resp struct {
				Services []ecsService `json:"services"`
			}
			if err := c.call(APIECS, "DescribeServices", map[string]any{"cluster": cluster, "services": chunk, "include": []string{"TAGS"}}, &resp); err != nil {
				return nil, err
			}
			for _, s := range resp.Services {
				name, boundary := ecsTagOf(s.Tags, TagName), ecsTagOf(s.Tags, TagBoundary)
				if name == "" || s.Status == "INACTIVE" {
					continue
				}
				st := serviceState{Boundary: boundary, TaskDefinition: lastPart(s.TaskDefinition), Count: s.Desired, Running: s.Running, Status: s.Status,
					Launch: s.LaunchType, Subnets: s.Network.Awsvpc.Subnets, Groups: s.Network.Awsvpc.Groups, PublicIP: s.Network.Awsvpc.PublicIP == "ENABLED",
					Runs: runs[[2]string{boundary, name}]}
				if st.Launch == "" && len(s.Strategy) > 0 {
					st.Launch = s.Strategy[0].Provider
				}
				out = append(out, object{Name: name, Boundary: boundary, IDs: map[string]string{"cluster": cluster, "service": s.Name}, State: st})
			}
		}
	}
	return out, nil
}

func (serviceKind) missing(*client, protocol.Resource) (any, error) { return nil, nil }

// launchMembers are how a service (and run-task) runs its tasks.
func launchMembers(m map[string]any, launch string, subnets, groups []string, publicIP bool) {
	if launch == LaunchEC2 {
		m["launchType"] = LaunchEC2
	} else {
		m["capacityProviderStrategy"] = []map[string]any{{"capacityProvider": launch, "weight": 1}}
	}
	if len(subnets) > 0 {
		ip := "DISABLED"
		if publicIP {
			ip = "ENABLED"
		}
		m["networkConfiguration"] = map[string]any{"awsvpcConfiguration": map[string]any{"subnets": subnets, "securityGroups": groups, "assignPublicIp": ip}}
	}
}

func (serviceKind) diff(r protocol.Resource, o protocol.Observed, deps map[string]protocol.Observed) ([]protocol.Change, error) {
	s, err := spec[serviceSpec](r)
	if err != nil {
		return nil, err
	}
	name := awsName(s.Boundary, r.Name)
	if err := checkName(name); err != nil {
		return nil, err
	}
	cl, err := dep(deps, KindCluster, s.Cluster)
	if err != nil {
		return nil, err
	}
	td, err := dep(deps, KindTaskDefinition, s.TaskDefinition)
	if err != nil {
		return nil, err
	}
	var subnets, groups []string
	switch s.Launch {
	case LaunchEC2:
		if s.Subnet != "" || s.SecurityGroup != "" || s.PublicIP {
			return nil, fmt.Errorf("on EC2, the tasks use the instance's network (bridge): a subnet, a security group and a public IP are the instance's")
		}
	case LaunchFargate, LaunchFargateSpot:
		subnet, err := dep(deps, KindSubnet, s.Subnet)
		if err != nil {
			return nil, err
		}
		group, err := dep(deps, KindSecurityGroup, s.SecurityGroup)
		if err != nil {
			return nil, err
		}
		subnets, groups = []string{subnet.IDs["SubnetId"]}, []string{group.IDs["GroupId"]}
	default:
		return nil, fmt.Errorf("launch %q is EC2, FARGATE or FARGATE_SPOT", s.Launch)
	}
	if s.Count < 0 {
		return nil, fmt.Errorf("count %d is negative", s.Count)
	}
	cluster, taskDef := cl.IDs["cluster"], td.IDs["taskDefinition"]
	if !o.Exists {
		m := map[string]any{"cluster": cluster, "serviceName": name, "taskDefinition": taskDef, "desiredCount": s.Count,
			// One task at a time: on EC2 a task holds the instance's port until it stops.
			"deploymentConfiguration": map[string]any{"minimumHealthyPercent": 0, "maximumPercent": 100},
			"tags":                    ecsTags(s.Boundary, r.Name)}
		launchMembers(m, s.Launch, subnets, groups, s.PublicIP)
		return []protocol.Change{{Resource: r.ID, Kind: KindService, Op: OpCreate, IDs: map[string]string{"mamela-name": r.Name, "cluster": cluster, "service": name},
			Reasons:  []string{fmt.Sprintf("service %s in cluster %s: %d tasks of %s on %s", name, cluster, s.Count, taskDef, s.Launch)},
			Requests: []protocol.Approved{jsonReq(APIECS, "CreateService", m)}}}, nil
	}
	st := state[serviceState](o)
	ids := map[string]string{"cluster": o.IDs["cluster"], "service": o.IDs["service"]}
	if st.Launch != s.Launch || !slices.Equal(st.Subnets, subnets) || !slices.Equal(st.Groups, groups) || st.PublicIP != s.PublicIP || o.IDs["cluster"] != cluster {
		return []protocol.Change{unsupported(r, KindService, OpReplace, ids,
			fmt.Sprintf("runs on %s in %s (subnets %v, groups %v, public IP %t); declared %s in %s (%v, %v, %t)",
				st.Launch, o.IDs["cluster"], st.Subnets, st.Groups, st.PublicIP, s.Launch, cluster, subnets, groups, s.PublicIP))}, nil
	}
	if st.Count == s.Count && st.TaskDefinition == taskDef {
		return nil, nil
	}
	var why []string
	if st.Count != s.Count {
		why = append(why, fmt.Sprintf("%d tasks, not %d", s.Count, st.Count))
	}
	if st.TaskDefinition != taskDef {
		why = append(why, fmt.Sprintf("task definition %s, not %s", taskDef, st.TaskDefinition))
	}
	return []protocol.Change{{Resource: r.ID, Kind: KindService, Op: OpUpdate, IDs: ids, Reasons: why,
		Requests: []protocol.Approved{jsonReq(APIECS, "UpdateService", map[string]any{"cluster": ids["cluster"], "service": ids["service"],
			"desiredCount": s.Count, "taskDefinition": taskDef})}}}, nil
}

func (serviceKind) remove(o protocol.Observed) (protocol.Change, error) {
	ids := map[string]string{"cluster": o.IDs["cluster"], "service": o.IDs["service"]}
	st := state[serviceState](o)
	return protocol.Change{Resource: o.ID, Kind: KindService, Op: OpDelete, IDs: ids,
		Reasons:  []string{fmt.Sprintf("delete service %s of cluster %s: its %d running tasks stop", ids["service"], ids["cluster"], st.Running)},
		Requests: []protocol.Approved{jsonReq(APIECS, "DeleteService", map[string]any{"cluster": ids["cluster"], "service": ids["service"], "force": true})}}, nil
}

// runToken is the token of the next run-task of a service: the same until a task carries it (a
// retry finds the task, never starts a second one), another once one does.
func runToken(o protocol.Observed, runs []taskRun) string {
	var tokens []string
	for _, r := range runs {
		tokens = append(tokens, r.Token)
	}
	slices.Sort(tokens)
	sum := sha256.Sum256([]byte(o.ID + "\x00" + o.IDs["cluster"] + "\x00" + strings.Join(tokens, ",")))
	return "mamela-" + hex.EncodeToString(sum[:12])
}

// runTaskChange is run-task: one task of the service's task definition, run the way the service
// runs them, tagged with a token that tells it apart.
func runTaskChange(o protocol.Observed) (protocol.Change, error) {
	if o.Kind != KindService {
		return protocol.Change{}, fmt.Errorf("%s is not an explicit operation of %s", OpRunTask, o.Kind)
	}
	st := state[serviceState](o)
	if st.Status != "ACTIVE" || st.TaskDefinition == "" {
		return protocol.Change{}, fmt.Errorf("%s is %s: no task is run as it", o.ID, strings.ToLower(st.Status))
	}
	ids := map[string]string{"cluster": o.IDs["cluster"], "service": o.IDs["service"]}
	token := runToken(o, st.Runs)
	m := map[string]any{"cluster": ids["cluster"], "taskDefinition": st.TaskDefinition, "count": 1, "startedBy": runStartedBy, "clientToken": token,
		"tags": ecsTagsWith(st.Boundary, o.Name, [2]string{TagRun, token})}
	launchMembers(m, st.Launch, st.Subnets, st.Groups, st.PublicIP)
	return protocol.Change{Resource: o.ID, Kind: KindService, Op: OpRunTask, IDs: ids,
		Reasons: []string{fmt.Sprintf("run one task of %s on %s, as service %s runs them (apart from its %d tasks); token %s",
			st.TaskDefinition, st.Launch, ids["service"], st.Count, token)},
		Requests: []protocol.Approved{jsonReq(APIECS, "RunTask", m)}}, nil
}

func (k serviceKind) done(c *client, _ protocol.Change, a protocol.Approved) (bool, error) {
	objs, err := k.list(c)
	if err != nil {
		return false, err
	}
	cluster := param(a, "cluster")
	find := func(service string) (serviceState, bool) {
		i := slices.IndexFunc(objs, func(x object) bool { return x.IDs["cluster"] == cluster && x.IDs["service"] == service })
		if i < 0 {
			return serviceState{}, false
		}
		return objs[i].State.(serviceState), true
	}
	switch action(a) {
	case "CreateService":
		_, ok := find(param(a, "serviceName"))
		return ok, nil
	case "UpdateService":
		st, ok := find(param(a, "service"))
		return ok && strconv.FormatInt(st.Count, 10) == param(a, "desiredCount") && st.TaskDefinition == param(a, "taskDefinition"), nil
	case "DeleteService":
		st, ok := find(param(a, "service"))
		return !ok || st.Status != "ACTIVE", nil
	case "RunTask":
		token := param(a, "clientToken")
		return slices.ContainsFunc(objs, func(x object) bool {
			return x.IDs["cluster"] == cluster && slices.ContainsFunc(x.State.(serviceState).Runs, func(r taskRun) bool { return r.Token == token })
		}), nil
	}
	return false, nil
}
