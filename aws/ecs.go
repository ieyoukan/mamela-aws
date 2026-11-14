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

	"github.com/ieyoukan/mamela/provider/kit"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// ECS: a cluster (Fargate and Fargate Spot, and container instances on EC2), task definitions and
// services. A one-off task (a load test) is not declared: it is the explicit operation run-task
// of a service, which runs one task the way the service runs its own (a service may keep zero).
// Mamela's ECS objects carry the tags mamela:name and mamela:boundary; one a person adopts keeps
// its own name, and gets the tags (TagResource).

// Kinds and operations of ECS.
const (
	KindCluster        protocol.ResourceKind = "AwsEcsCluster"
	KindTaskDefinition protocol.ResourceKind = "AwsEcsTaskDefinition"
	KindService        protocol.ResourceKind = "AwsEcsService"

	OpRevise  protocol.Op = "revise"   // a new revision of a task definition
	OpUpdate  protocol.Op = "update"   // a service's count, task definition, Fargate (Spot) and network
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
	// TagOrigin is the revision a task definition's lineage started with (the one Mamela
	// registered or adopted first): every later revision carries it.
	TagOrigin = "mamela:origin"
)

// Launches: how a service runs its tasks.
const (
	LaunchEC2         = "EC2"
	LaunchFargate     = "FARGATE"
	LaunchFargateSpot = "FARGATE_SPOT"
)

var (
	listClusters     = jsonCall(APIECS, "ListClusters", "nextToken")
	describeClusters = jsonCall(APIECS, "DescribeClusters", "clusters", "include")
	createCluster    = jsonCall(APIECS, "CreateCluster", "clusterName", "capacityProviders", "defaultCapacityProviderStrategy", "tags", "settings")
	deleteCluster    = jsonCall(APIECS, "DeleteCluster", "cluster")
	listServices     = jsonCall(APIECS, "ListServices", "cluster", "nextToken")
	describeServices = jsonCall(APIECS, "DescribeServices", "cluster", "services", "include")
	createService    = jsonCall(APIECS, "CreateService", "cluster", "serviceName", "taskDefinition", "desiredCount", "launchType", "capacityProviderStrategy",
		"networkConfiguration", "deploymentConfiguration", "tags")
	updateService = jsonCall(APIECS, "UpdateService", "cluster", "service", "desiredCount", "taskDefinition", "capacityProviderStrategy",
		"networkConfiguration", "forceNewDeployment")
	deleteService = jsonCall(APIECS, "DeleteService", "cluster", "service", "force")
	listTasks     = jsonCall(APIECS, "ListTasks", "cluster", "startedBy", "desiredStatus", "nextToken")
	describeTasks = jsonCall(APIECS, "DescribeTasks", "cluster", "tasks", "include")
	runTask       = jsonCall(APIECS, "RunTask", "cluster", "taskDefinition", "count", "launchType", "capacityProviderStrategy", "networkConfiguration",
		"startedBy", "clientToken", "tags")
	tagECS = jsonCall(APIECS, "TagResource", "resourceArn", "tags")
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

// tagChangeECS is the adopt of an ECS object: tag it (by its ARN) with the declaration's name and
// boundary, and more (a task definition's digest). Nothing else is sent.
func tagChangeECS(o protocol.Observed, desired protocol.Resource, ids map[string]string, what string, more ...[2]string) (protocol.Change, error) {
	b, err := boundaryOf(desired)
	if err != nil {
		return protocol.Change{}, err
	}
	arn := o.IDs["resourceArn"]
	ids = maps.Clone(ids)
	ids["resourceArn"] = arn
	return adoptChange(o, desired, ids,
		fmt.Sprintf("adopt %s: tag it %s=%s and %s=%s, which makes it Mamela's; nothing else changes", what, TagName, desired.Name, TagBoundary, b),
		jsonReq(APIECS, "TagResource", map[string]any{"resourceArn": arn, "tags": ecsTagsWith(b, desired.Name, more...)}))
}

// importedAs is the name of the declaration an imported object of a kind makes, if its kind
// declares it (and does not skip it): what a reference to it names.
func importedAs(k kind, r kit.Resolver, x protocol.ImportObject, boundary string) (string, error) {
	if _, err := k.importObject(x, r, boundary); err != nil {
		return "", err
	}
	return x.Name, nil
}

// referTo is the declaration of the imported object of a type that matches, or why there is none.
func referTo(k kind, r kit.Resolver, tfType, what string, boundary string, match func(protocol.ImportObject) bool) (string, error) {
	for _, x := range r.Objects(tfType) {
		if !match(x) {
			continue
		}
		n, err := importedAs(k, r, x, boundary)
		if err != nil {
			return "", kit.Skip(fmt.Sprintf("its %s (%s) is not imported: %v", what, x.Address, err))
		}
		return n, nil
	}
	return "", kit.Skip(fmt.Sprintf("its %s is not in the state: import it with this (a %s of the same state)", what, tfType))
}

// --- cluster ----------------------------------------------------------------------------------

type clusterKind struct{}

type clusterSpec struct {
	Boundary string `json:"boundary" doc:"the boundary (the guard's): it tags the cluster"`
}

type clusterState struct {
	Instances int64 `json:"instances"` // registered container instances
	Running   int64 `json:"running"`   // running tasks
}

// ecsCluster is a cluster as DescribeClusters answers.
type ecsCluster struct {
	Arn       string   `json:"clusterArn"`
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
	return describeClustersOf(c, arns)
}

func describeClustersOf(c *client, refs []string) ([]ecsCluster, error) {
	var out []ecsCluster
	for chunk := range slices.Chunk(refs, 100) {
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

func clusterObject(x ecsCluster) object {
	return object{Name: ecsTagOf(x.Tags, TagName), Boundary: ecsTagOf(x.Tags, TagBoundary), IDs: map[string]string{"cluster": x.Name, "resourceArn": x.Arn},
		State: clusterState{Instances: x.Instances, Running: x.Running + x.Pending}}
}

func (clusterKind) kind() protocol.ResourceKind { return KindCluster }
func (clusterKind) reads() []protocol.Call      { return []protocol.Call{listClusters, describeClusters} }
func (clusterKind) operations() []protocol.Operation {
	return []protocol.Operation{op(OpCreate, createCluster), deleteOp(deleteCluster), adoptOp(tagECS)}
}
func (clusterKind) spec() any { return clusterSpec{} }
func (clusterKind) doc() string {
	return "An ECS cluster: Fargate and Fargate Spot (Spot by default), and the container instances that join it. Container Insights is off."
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
		out = append(out, clusterObject(x))
	}
	return out, nil
}

func (clusterKind) missing(*client, protocol.Resource) (any, error) { return nil, nil }

func (clusterKind) diff(r protocol.Resource, o protocol.Observed, _ map[string]protocol.Observed) ([]protocol.Change, error) {
	s, err := spec[clusterSpec](r)
	if err != nil {
		return nil, err
	}
	if o.Exists {
		return nil, nil // its settings and capacity providers stay as they are
	}
	name := awsName(s.Boundary, r.Name)
	if err := checkName(name); err != nil {
		return nil, err
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

func (k clusterKind) done(c *client, ch protocol.Change, a protocol.Approved) (bool, error) {
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

func (clusterKind) imports() []string {
	return []string{"aws_ecs_cluster", "aws_ecs_cluster_capacity_providers"}
}

// importObject declares a cluster; its capacity providers (aws_ecs_cluster_capacity_providers)
// move with it. Mamela does not change an existing cluster's settings or capacity providers.
func (clusterKind) importObject(o protocol.ImportObject, r kit.Resolver, boundary string) (kit.Imported, error) {
	var v struct {
		Name        string `json:"name"`
		ClusterName string `json:"cluster_name"`
	}
	if err := kit.Values(o, &v); err != nil {
		return kit.Imported{}, fmt.Errorf("the attributes of %s: %w", o.Address, err)
	}
	if o.Type == "aws_ecs_cluster_capacity_providers" {
		return kit.Imported{}, kit.Skip("capacity providers move with their cluster (aws_ecs_cluster of the same state)")
	}
	if err := checkName(v.Name); err != nil {
		return kit.Imported{}, kit.Skip("no cluster name: " + err.Error())
	}
	var also []string
	for _, x := range r.Objects("aws_ecs_cluster_capacity_providers") {
		var cv struct {
			ClusterName string `json:"cluster_name"`
		}
		if kit.Values(x, &cv) == nil && cv.ClusterName == v.Name {
			also = append(also, x.Address)
		}
	}
	return kit.Imported{Spec: clusterSpec{Boundary: boundary}, IDs: map[string]string{"cluster": v.Name}, Also: also}, nil
}

func (clusterKind) find(c *client, ids map[string]string) (*object, error) {
	if ids["cluster"] == "" {
		return nil, nil
	}
	got, err := describeClustersOf(c, []string{ids["cluster"]})
	if err != nil || len(got) != 1 || got[0].Status != "ACTIVE" {
		return nil, err
	}
	o := clusterObject(got[0])
	return &o, nil
}

func (clusterKind) adopt(o protocol.Observed, desired protocol.Resource) (protocol.Change, error) {
	return tagChangeECS(o, desired, map[string]string{"cluster": o.IDs["cluster"]}, "ECS cluster "+o.IDs["cluster"])
}

// --- service ----------------------------------------------------------------------------------

type serviceKind struct{}

type serviceSpec struct {
	Boundary       string `json:"boundary" doc:"the boundary (the guard's): it tags the service"`
	Cluster        string `json:"cluster" mamela:"ref=AwsEcsCluster"`
	TaskDefinition string `json:"taskDefinition" mamela:"ref=AwsEcsTaskDefinition" doc:"its tasks' definition (the latest revision)"`
	Count          int64  `json:"count" doc:"how many tasks it keeps running (0: a service to run one-off tasks with run-task)"`
	Launch         string `json:"launch" doc:"where its tasks run: EC2 (the cluster's container instances), FARGATE or FARGATE_SPOT"`
	Subnet         string `json:"subnet,omitempty" mamela:"ref=AwsSubnet" doc:"Fargate: the subnet of its tasks"`
	SecurityGroup  string `json:"securityGroup,omitempty" mamela:"ref=AwsSecurityGroup" doc:"Fargate: their security group"`
	PublicIP       bool   `json:"publicIp,omitzero" doc:"Fargate: a public IP address (to pull images without a NAT gateway)"`
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
	Arn            string `json:"serviceArn"`
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
		unsupportedOp(OpReplace, "a service keeps its cluster, and whether its tasks run on EC2 or on Fargate: declare a new one"),
		explicitOp(OpRunTask, runTask), deleteOp(deleteService), adoptOp(tagECS)}
}
func (serviceKind) spec() any { return serviceSpec{} }
func (serviceKind) doc() string {
	return "An ECS service that keeps count tasks of a task definition running. With 0 tasks, it is a way to run one-off tasks: `mamela act --confirm <name> run-task` runs one, as the service would."
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

// describeServicesOf describes services of a cluster (with their tags).
func describeServicesOf(c *client, cluster string, refs []string) ([]ecsService, error) {
	var out []ecsService
	for chunk := range slices.Chunk(refs, 10) {
		var resp struct {
			Services []ecsService `json:"services"`
		}
		if err := c.call(APIECS, "DescribeServices", map[string]any{"cluster": cluster, "services": chunk, "include": []string{"TAGS"}}, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Services...)
	}
	return out, nil
}

func serviceObject(cluster string, s ecsService, runs []taskRun) object {
	name, boundary := ecsTagOf(s.Tags, TagName), ecsTagOf(s.Tags, TagBoundary)
	st := serviceState{Boundary: boundary, TaskDefinition: lastPart(s.TaskDefinition), Count: s.Desired, Running: s.Running, Status: s.Status,
		Launch: s.LaunchType, Subnets: s.Network.Awsvpc.Subnets, Groups: s.Network.Awsvpc.Groups, PublicIP: s.Network.Awsvpc.PublicIP == "ENABLED", Runs: runs}
	if st.Launch == "" && len(s.Strategy) > 0 {
		st.Launch = s.Strategy[0].Provider
	}
	return object{Name: name, Boundary: boundary, IDs: map[string]string{"cluster": cluster, "service": s.Name, "resourceArn": s.Arn}, State: st}
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
		services, err := describeServicesOf(c, cluster, arns)
		if err != nil {
			return nil, err
		}
		for _, s := range services {
			name, boundary := ecsTagOf(s.Tags, TagName), ecsTagOf(s.Tags, TagBoundary)
			if name == "" || s.Status == "INACTIVE" {
				continue
			}
			out = append(out, serviceObject(cluster, s, runs[[2]string{boundary, name}]))
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
		m["capacityProviderStrategy"] = strategy(launch)
	}
	if len(subnets) > 0 {
		m["networkConfiguration"] = network(subnets, groups, publicIP)
	}
}

func strategy(launch string) []map[string]any {
	return []map[string]any{{"capacityProvider": launch, "weight": 1}}
}

func network(subnets, groups []string, publicIP bool) map[string]any {
	ip := "DISABLED"
	if publicIP {
		ip = "ENABLED"
	}
	return map[string]any{"awsvpcConfiguration": map[string]any{"subnets": subnets, "securityGroups": groups, "assignPublicIp": ip}}
}

func fargate(launch string) bool { return launch == LaunchFargate || launch == LaunchFargateSpot }

func (serviceKind) diff(r protocol.Resource, o protocol.Observed, deps map[string]protocol.Observed) ([]protocol.Change, error) {
	s, err := spec[serviceSpec](r)
	if err != nil {
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
		name := awsName(s.Boundary, r.Name)
		if err := checkName(name); err != nil {
			return nil, err
		}
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
	if o.IDs["cluster"] != cluster || (st.Launch == LaunchEC2) != (s.Launch == LaunchEC2) {
		return []protocol.Change{unsupported(r, KindService, OpReplace, ids,
			fmt.Sprintf("runs on %s in cluster %s; declared %s in cluster %s", st.Launch, o.IDs["cluster"], s.Launch, cluster))}, nil
	}
	m := map[string]any{"cluster": ids["cluster"], "service": ids["service"], "desiredCount": s.Count, "taskDefinition": taskDef}
	var why []string
	if st.Count != s.Count {
		why = append(why, fmt.Sprintf("%d tasks, not %d", s.Count, st.Count))
	}
	if st.TaskDefinition != taskDef {
		why = append(why, fmt.Sprintf("task definition %s, not %s", taskDef, st.TaskDefinition))
	}
	if fargate(s.Launch) {
		if st.Launch != s.Launch {
			m["capacityProviderStrategy"], m["forceNewDeployment"] = strategy(s.Launch), true
			why = append(why, fmt.Sprintf("its tasks on %s, not %s (a new deployment replaces them)", s.Launch, st.Launch))
		}
		if !slices.Equal(st.Subnets, subnets) || !slices.Equal(st.Groups, groups) || st.PublicIP != s.PublicIP {
			m["networkConfiguration"], m["forceNewDeployment"] = network(subnets, groups, s.PublicIP), true
			why = append(why, fmt.Sprintf("its tasks in subnets %v with groups %v and public IP %t, not %v, %v and %t (a new deployment replaces them)",
				subnets, groups, s.PublicIP, st.Subnets, st.Groups, st.PublicIP))
		}
	}
	if len(why) == 0 {
		return nil, nil
	}
	return []protocol.Change{{Resource: r.ID, Kind: KindService, Op: OpUpdate, IDs: ids, Reasons: why,
		Requests: []protocol.Approved{jsonReq(APIECS, "UpdateService", m)}}}, nil
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

// prepare: run-task, one task of the service's task definition, run the way the service runs
// them, tagged with a token that tells it apart.
func (serviceKind) prepare(op protocol.Op, o protocol.Observed, _ *protocol.Resource) (protocol.Change, error) {
	if op != OpRunTask {
		return protocol.Change{}, fmt.Errorf("%s is not an explicit operation of %s", op, o.Kind)
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
		if !ok || strconv.FormatInt(st.Count, 10) != param(a, "desiredCount") || st.TaskDefinition != param(a, "taskDefinition") {
			return false, nil
		}
		if v := a.Params["capacityProviderStrategy"]; len(v) == 1 {
			var s []struct {
				Provider string `json:"capacityProvider"`
			}
			if json.Unmarshal([]byte(v[0]), &s) != nil || len(s) != 1 || s[0].Provider != st.Launch {
				return false, nil
			}
		}
		if v := a.Params["networkConfiguration"]; len(v) == 1 {
			var n ecsService
			if json.Unmarshal([]byte(`{"networkConfiguration":`+v[0]+`}`), &n) != nil {
				return false, nil
			}
			w := n.Network.Awsvpc
			if !slices.Equal(w.Subnets, st.Subnets) || !slices.Equal(w.Groups, st.Groups) || (w.PublicIP == "ENABLED") != st.PublicIP {
				return false, nil
			}
		}
		return true, nil
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

func (serviceKind) imports() []string { return []string{"aws_ecs_service"} }

// tfService are the attributes of a Terraform ECS service.
type tfService struct {
	Name           string `json:"name"`
	Cluster        string `json:"cluster"`
	TaskDefinition string `json:"task_definition"`
	DesiredCount   int64  `json:"desired_count"`
	LaunchType     string `json:"launch_type"`
	Strategy       []struct {
		Provider string `json:"capacity_provider"`
		Weight   int64  `json:"weight"`
	} `json:"capacity_provider_strategy"`
	Network []struct {
		Subnets  []string `json:"subnets"`
		Groups   []string `json:"security_groups"`
		PublicIP bool     `json:"assign_public_ip"`
	} `json:"network_configuration"`
	LoadBalancers []any  `json:"load_balancer"`
	Registries    []any  `json:"service_registries"`
	Scheduling    string `json:"scheduling_strategy"`
	Controller    []struct {
		Type string `json:"type"`
	} `json:"deployment_controller"`
}

// importObject declares a service as AwsEcsService says one: of an imported cluster and task
// definition, on EC2 or on Fargate (Spot) in one imported subnet with one imported security group,
// with neither a load balancer nor service discovery, deployed by ECS. Anything else is left to
// Terraform, with why.
func (serviceKind) importObject(o protocol.ImportObject, r kit.Resolver, boundary string) (kit.Imported, error) {
	var v tfService
	if err := kit.Values(o, &v); err != nil {
		return kit.Imported{}, fmt.Errorf("the attributes of %s: %w", o.Address, err)
	}
	switch {
	case v.Name == "" || v.Cluster == "":
		return kit.Imported{}, kit.Skip("no name or cluster: the state does not say which service it is")
	case len(v.LoadBalancers) > 0 || len(v.Registries) > 0:
		return kit.Imported{}, kit.Skip("it is behind a load balancer or in service discovery, which AwsEcsService does not declare: Terraform keeps managing it")
	case v.Scheduling != "" && v.Scheduling != "REPLICA":
		return kit.Imported{}, kit.Skip(fmt.Sprintf("its scheduling strategy is %s; AwsEcsService keeps a number of tasks (REPLICA)", v.Scheduling))
	case len(v.Controller) > 0 && v.Controller[0].Type != "" && v.Controller[0].Type != "ECS":
		return kit.Imported{}, kit.Skip(fmt.Sprintf("it is deployed by %s; Mamela updates services that ECS deploys", v.Controller[0].Type))
	}
	clusterName := lastPart(v.Cluster)
	cluster, err := referTo(clusterKind{}, r, "aws_ecs_cluster", "cluster "+clusterName, boundary, func(x protocol.ImportObject) bool {
		var cv struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		return kit.Values(x, &cv) == nil && (cv.ID == v.Cluster || cv.Name == clusterName)
	})
	if err != nil {
		return kit.Imported{}, err
	}
	taskDef, err := referTo(taskDefinitionKind{}, r, "aws_ecs_task_definition", "task definition "+lastPart(v.TaskDefinition), boundary, func(x protocol.ImportObject) bool {
		var tv struct {
			Arn      string `json:"arn"`
			Family   string `json:"family"`
			Revision int64  `json:"revision"`
		}
		return kit.Values(x, &tv) == nil && (tv.Arn == v.TaskDefinition || fmt.Sprintf("%s:%d", tv.Family, tv.Revision) == lastPart(v.TaskDefinition) ||
			tv.Family == v.TaskDefinition)
	})
	if err != nil {
		return kit.Imported{}, err
	}
	s := serviceSpec{Boundary: boundary, Cluster: cluster, TaskDefinition: taskDef, Count: v.DesiredCount}
	switch {
	case v.LaunchType == LaunchEC2 || v.LaunchType == LaunchFargate:
		s.Launch = v.LaunchType
	case v.LaunchType == "" && len(v.Strategy) == 1 && fargate(v.Strategy[0].Provider):
		s.Launch = v.Strategy[0].Provider
	default:
		return kit.Imported{}, kit.Skip(fmt.Sprintf("it runs its tasks on %q with capacity providers %v; AwsEcsService runs them on EC2, on Fargate or on Fargate Spot", v.LaunchType, v.Strategy))
	}
	switch {
	case s.Launch == LaunchEC2 && len(v.Network) > 0:
		return kit.Imported{}, kit.Skip("its tasks on EC2 have a network configuration (awsvpc); AwsEcsService's tasks on EC2 use the instance's (bridge)")
	case s.Launch != LaunchEC2:
		if len(v.Network) != 1 || len(v.Network[0].Subnets) != 1 || len(v.Network[0].Groups) != 1 {
			return kit.Imported{}, kit.Skip("its tasks are in several subnets or security groups (or none); AwsEcsService puts them in one subnet with one security group")
		}
		n := v.Network[0]
		if s.Subnet, err = referTo(subnetKind{}, r, "aws_subnet", "subnet "+n.Subnets[0], boundary, idIs(n.Subnets[0])); err != nil {
			return kit.Imported{}, err
		}
		if s.SecurityGroup, err = referTo(groupKind{}, r, "aws_security_group", "security group "+n.Groups[0], boundary, idIs(n.Groups[0])); err != nil {
			return kit.Imported{}, err
		}
		s.PublicIP = n.PublicIP
	}
	return kit.Imported{Spec: s, IDs: map[string]string{"cluster": clusterName, "service": v.Name}}, nil
}

// idIs matches an imported object by its id.
func idIs(id string) func(protocol.ImportObject) bool {
	return func(x protocol.ImportObject) bool {
		var v struct {
			ID string `json:"id"`
		}
		return kit.Values(x, &v) == nil && v.ID == id
	}
}

func (serviceKind) find(c *client, ids map[string]string) (*object, error) {
	if ids["cluster"] == "" || ids["service"] == "" {
		return nil, nil
	}
	got, err := describeServicesOf(c, ids["cluster"], []string{ids["service"]})
	if err != nil || len(got) != 1 || got[0].Status != "ACTIVE" {
		return nil, err
	}
	o := serviceObject(ids["cluster"], got[0], nil)
	return &o, nil
}

func (serviceKind) adopt(o protocol.Observed, desired protocol.Resource) (protocol.Change, error) {
	return tagChangeECS(o, desired, map[string]string{"cluster": o.IDs["cluster"], "service": o.IDs["service"]},
		fmt.Sprintf("ECS service %s of cluster %s", o.IDs["service"], o.IDs["cluster"]))
}
