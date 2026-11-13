package aws

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/provider/kit"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// A task definition: its family is mamela-<boundary>-<name> (ECS lists task definitions by family,
// and the provider finds its own by the prefix); a change registers a new revision and
// deregisters the old one. Each revision Mamela registers is tagged with the digest of its shape
// (mamela:digest): what AwsEcsTaskDefinition declares of it, normalized. An adopted revision is
// tagged with the digest of the shape ECS answers for it, so that it is in sync with a declaration
// that says the same, and revised when the declaration says something else.

var (
	listTaskDefs      = jsonCall(APIECS, "ListTaskDefinitions", "status", "nextToken")
	describeTaskDef   = jsonCall(APIECS, "DescribeTaskDefinition", "taskDefinition", "include")
	registerTaskDef   = jsonCall(APIECS, "RegisterTaskDefinition", "family", "containerDefinitions", "cpu", "memory", "networkMode", "requiresCompatibilities", "executionRoleArn", "tags")
	deregisterTaskDef = jsonCall(APIECS, "DeregisterTaskDefinition", "taskDefinition")
)

type taskDefinitionKind struct{}

// Container is one container of a task definition.
type Container struct {
	ContainerName string   `json:"containerName" doc:"the container's name"`
	Image         string   `json:"image" doc:"its image (registry/path:tag)"`
	Command       []string `json:"command,omitempty" doc:"its arguments (the image's entrypoint runs them)"`
	Environment   []Env    `json:"environment,omitempty"`
	Ports         []Port   `json:"ports,omitempty"`
	MemoryMiB     int64    `json:"memoryMiB,omitzero" doc:"its hard limit of memory on EC2 (MiB)"`
}

// Env is an environment variable: Value, or Prefix and the private IP address of the
// AwsInstance InstanceIP ("http://10.0.1.5").
type Env struct {
	Variable   string `json:"variable" doc:"the variable's name"`
	Value      string `json:"value,omitempty"`
	InstanceIP string `json:"instanceIp,omitempty" mamela:"ref=AwsInstance" doc:"or the private IP address of this instance"`
	Prefix     string `json:"prefix,omitempty" doc:"before the instance's address (http://)"`
}

// Port is a container's port, and on EC2 the instance's port it is reached on.
type Port struct {
	Container int64 `json:"container" doc:"the container's TCP port"`
	Host      int64 `json:"host,omitzero" doc:"on EC2, the instance's port (none: a port ECS chooses)"`
}

type taskDefinitionSpec struct {
	Boundary      string      `json:"boundary" doc:"the boundary (the guard's): the family is mamela-<boundary>-<name>"`
	Launch        string      `json:"launch" doc:"where its tasks run: FARGATE (Fargate and Fargate Spot; awsvpc) or EC2 (container instances; bridge)"`
	CPU           int64       `json:"cpu" doc:"CPU units, 1024 to a vCPU"`
	MemoryMiB     int64       `json:"memoryMiB" doc:"memory in MiB"`
	ExecutionRole string      `json:"executionRole,omitempty" mamela:"ref=AwsIamRole" doc:"the role ECS pulls images and writes logs as (trust ecs-tasks, policy ecs-task-execution)"`
	LogGroup      string      `json:"logGroup,omitempty" mamela:"ref=AwsLogGroup" doc:"where its containers' logs go"`
	Containers    []Container `json:"containers"`
}

type taskDefinitionState struct {
	Revision int64  `json:"revision"`
	Digest   string `json:"digest"`          // mamela:digest, of what it was registered (or adopted) with
	Shape    string `json:"shape,omitempty"` // the digest of the shape ECS answers for it
	// Other is what it has that AwsEcsTaskDefinition does not declare (its shape is then not one
	// a declaration can have).
	Other []string `json:"other,omitempty"`
}

// Family is the family of the task definitions Mamela registers.
func Family(boundary, name string) string { return familyPrefix + awsName(boundary, name) }

func (taskDefinitionKind) kind() protocol.ResourceKind { return KindTaskDefinition }
func (taskDefinitionKind) reads() []protocol.Call {
	return []protocol.Call{listTaskDefs, describeTaskDef}
}
func (taskDefinitionKind) operations() []protocol.Operation {
	return []protocol.Operation{op(OpCreate, registerTaskDef), op(OpRevise, registerTaskDef, deregisterTaskDef), deleteOp(deregisterTaskDef),
		adoptOp(tagECS)}
}
func (taskDefinitionKind) spec() any { return taskDefinitionSpec{} }
func (taskDefinitionKind) doc() string {
	return "An ECS task definition of the family mamela-<boundary>-<name>. A change registers a new revision and deregisters the old one."
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
	Definition map[string]any `json:"taskDefinition"`
	Tags       []ecsTag       `json:"tags"`
}

func (d ecsTaskDefinition) text(k string) string {
	s, _ := d.Definition[k].(string)
	return s
}

func describeTaskDefinition(c *client, td string) (ecsTaskDefinition, error) {
	var resp ecsTaskDefinition
	err := c.call(APIECS, "DescribeTaskDefinition", map[string]any{"taskDefinition": td, "include": []string{"TAGS"}}, &resp)
	return resp, err
}

// taskDefinitionObject is the latest revision of a family as an object. Its resourceArn is the
// revision its lineage started with (mamela:origin, which each revision Mamela registers carries):
// what Mamela recorded of a task definition it adopted names it through every later revision.
func taskDefinitionObject(d ecsTaskDefinition) object {
	family := d.text("family")
	rev, _ := d.Definition["revision"].(float64)
	shape, other := tdShape(d.Definition)
	origin := ecsTagOf(d.Tags, TagOrigin)
	if origin == "" {
		origin = d.text("taskDefinitionArn")
	}
	return object{Name: ecsTagOf(d.Tags, TagName), Boundary: ecsTagOf(d.Tags, TagBoundary),
		IDs:   map[string]string{"family": family, "taskDefinition": fmt.Sprintf("%s:%d", family, int64(rev)), "resourceArn": origin},
		State: taskDefinitionState{Revision: int64(rev), Digest: ecsTagOf(d.Tags, TagDigest), Shape: shape, Other: other}}
}

func (taskDefinitionKind) list(c *client) ([]object, error) {
	latest, err := latestTaskDefinitions(c)
	if err != nil {
		return nil, err
	}
	var out []object
	for _, family := range slices.Sorted(maps.Keys(latest)) {
		d, err := describeTaskDefinition(c, fmt.Sprintf("%s:%d", family, latest[family]))
		if err != nil {
			return nil, err
		}
		if ecsTagOf(d.Tags, TagName) == "" {
			continue
		}
		out = append(out, taskDefinitionObject(d))
	}
	return out, nil
}

func (taskDefinitionKind) missing(*client, protocol.Resource) (any, error) { return nil, nil }

// register is the RegisterTaskDefinition of a declaration in a family, with the digest of its
// shape it is tagged with (and, after the first, the revision the lineage started with).
func register(r protocol.Resource, s taskDefinitionSpec, family, origin string, deps map[string]protocol.Observed) (protocol.Approved, string, error) {
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
		d := map[string]any{"name": ct.ContainerName, "image": ct.Image, "essential": true}
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
			env = append(env, map[string]string{"name": e.Variable, "value": v})
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
	members := map[string]any{"family": family, "containerDefinitions": defs, "cpu": strconv.FormatInt(s.CPU, 10),
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
	var asJSON map[string]any
	json.Unmarshal(text, &asJSON)
	digest, other := tdShape(asJSON)
	if len(other) > 0 {
		return protocol.Approved{}, "", fmt.Errorf("the task definition has %v, which its shape does not hold", other) // never: register writes only what it holds
	}
	more := [][2]string{{TagDigest, digest}}
	if origin != "" {
		more = append(more, [2]string{TagOrigin, origin})
	}
	members["tags"] = ecsTagsWith(s.Boundary, r.Name, more...)
	return jsonReq(APIECS, "RegisterTaskDefinition", members), digest, nil
}

func (taskDefinitionKind) diff(r protocol.Resource, o protocol.Observed, deps map[string]protocol.Observed) ([]protocol.Change, error) {
	s, err := spec[taskDefinitionSpec](r)
	if err != nil {
		return nil, err
	}
	family, origin := Family(s.Boundary, r.Name), ""
	if o.Exists {
		family, origin = o.IDs["family"], o.IDs["resourceArn"] // an adopted one keeps its family
	}
	a, digest, err := register(r, s, family, origin, deps)
	if err != nil {
		return nil, err
	}
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
	why := fmt.Sprintf("revision %d was registered with digest %q: register a new one (%s) and deregister it", st.Revision, st.Digest, what)
	if len(st.Other) > 0 {
		why += fmt.Sprintf("; the new one has none of its %v, which AwsEcsTaskDefinition does not declare", st.Other)
	}
	return []protocol.Change{{Resource: r.ID, Kind: KindTaskDefinition, Op: OpRevise, IDs: map[string]string{"family": family, "taskDefinition": old},
		Reasons:  []string{why},
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
		return d.text("status") != "ACTIVE", nil
	}
	return false, nil
}

// --- shape ---

// tdShape is the digest of a task definition's shape: what AwsEcsTaskDefinition declares of it
// (cpu, memory, network mode, compatibilities, execution role, and each container's name, image,
// command, environment, ports, memory and log configuration), normalized so that a revision as
// registered and as ECS describes it (with its defaults) have the same. other is what else it has
// (a task role, volumes, a health check): a declaration has none of it.
func tdShape(td map[string]any) (digest string, other []string) {
	ignored := []string{"family", "taskDefinitionArn", "revision", "status", "compatibilities", "requiresAttributes", "registeredAt",
		"registeredBy", "deregisteredAt", "deleteRequestedAt", "tags"}
	mode := text(td["networkMode"])
	shape := map[string]any{"cpu": text(td["cpu"]), "memory": text(td["memory"]), "networkMode": mode,
		"requiresCompatibilities": sortedTexts(td["requiresCompatibilities"]), "executionRoleArn": text(td["executionRoleArn"])}
	var containers []any
	defs, _ := td["containerDefinitions"].([]any)
	for _, d := range defs {
		ct, _ := d.(map[string]any)
		name := text(ct["name"])
		c := map[string]any{"name": name, "image": text(ct["image"]), "command": texts(ct["command"]), "memory": number(ct["memory"])}
		if e, ok := ct["essential"].(bool); ok && !e {
			other = append(other, fmt.Sprintf("container %s: not essential", name))
		}
		var env [][2]string
		for _, x := range list(ct["environment"]) {
			m, _ := x.(map[string]any)
			env = append(env, [2]string{text(m["name"]), text(m["value"])})
		}
		slices.SortFunc(env, func(a, b [2]string) int { return strings.Compare(a[0], b[0]) })
		c["environment"] = env
		var ports [][3]any
		for _, x := range list(ct["portMappings"]) {
			m, _ := x.(map[string]any)
			cp, hp, proto := number(m["containerPort"]), number(m["hostPort"]), text(m["protocol"])
			if proto == "" {
				proto = "tcp"
			}
			if mode == "awsvpc" && hp == cp {
				hp = 0
			}
			ports = append(ports, [3]any{cp, hp, proto})
			for _, k := range sortedKeys(m) {
				if !slices.Contains([]string{"containerPort", "hostPort", "protocol"}, k) && !empty(m[k]) {
					other = append(other, fmt.Sprintf("container %s: port %s", name, k))
				}
			}
		}
		c["ports"] = ports
		if lc, ok := ct["logConfiguration"].(map[string]any); ok && !empty(lc) {
			opts := map[string]string{}
			if m, ok := lc["options"].(map[string]any); ok {
				for k, v := range m {
					opts[k] = text(v)
				}
			}
			c["log"] = map[string]any{"driver": text(lc["logDriver"]), "options": opts}
			if !empty(lc["secretOptions"]) {
				other = append(other, fmt.Sprintf("container %s: log secrets", name))
			}
		}
		for _, k := range sortedKeys(ct) {
			if !slices.Contains([]string{"name", "image", "essential", "command", "environment", "portMappings", "memory", "logConfiguration"}, k) && !empty(ct[k]) {
				other = append(other, fmt.Sprintf("container %s: %s", name, k))
			}
		}
		containers = append(containers, c)
	}
	shape["containers"] = containers
	for _, k := range sortedKeys(td) {
		if !slices.Contains(ignored, k) && shape[k] == nil && k != "containerDefinitions" && !empty(td[k]) {
			other = append(other, k)
		}
	}
	b, _ := json.Marshal(shape, json.Deterministic(true))
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:12]), other
}

func sortedKeys[V any](m map[string]V) []string { return slices.Sorted(maps.Keys(m)) }

func text(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	}
	return ""
}

func texts(v any) []string {
	var out []string
	for _, x := range list(v) {
		out = append(out, text(x))
	}
	return out
}

func sortedTexts(v any) []string {
	out := texts(v)
	slices.Sort(out)
	return out
}

func number(v any) int64 {
	switch x := v.(type) {
	case float64:
		if x == math.Trunc(x) {
			return int64(x)
		}
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

func list(v any) []any {
	l, _ := v.([]any)
	return l
}

// empty: a value ECS answers for what was not set (0, false, "", [], {}).
func empty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case float64:
		return x == 0
	case []any:
		return len(x) == 0
	case map[string]any:
		for _, y := range x {
			if !empty(y) {
				return false
			}
		}
		return true
	}
	return false
}

// --- import and adopt ---

func (taskDefinitionKind) imports() []string { return []string{"aws_ecs_task_definition"} }

// tfTaskDefinition are the attributes of a Terraform task definition.
type tfTaskDefinition struct {
	Family        string   `json:"family"`
	Arn           string   `json:"arn"`
	Containers    string   `json:"container_definitions"`
	CPU           string   `json:"cpu"`
	Memory        string   `json:"memory"`
	NetworkMode   string   `json:"network_mode"`
	Compatibility []string `json:"requires_compatibilities"`
	ExecutionRole string   `json:"execution_role_arn"`
	TaskRole      string   `json:"task_role_arn"`
	PidMode       string   `json:"pid_mode"`
	IpcMode       string   `json:"ipc_mode"`
	Volume        []any    `json:"volume"`
	Placement     []any    `json:"placement_constraints"`
	Proxy         []any    `json:"proxy_configuration"`
	Accelerators  []any    `json:"inference_accelerator"`
	Ephemeral     []any    `json:"ephemeral_storage"`
	Runtime       []any    `json:"runtime_platform"`
}

// importObject declares a task definition of the family mamela-<boundary>-*, as
// AwsEcsTaskDefinition says one: on Fargate (awsvpc) or EC2 (bridge), with task-level cpu and
// memory, an imported execution role, its containers' logs (if any) in one imported log group, and
// nothing else (no task role, volumes, health checks...). Anything else is left to Terraform.
func (taskDefinitionKind) importObject(o protocol.ImportObject, r kit.Resolver, boundary string) (kit.Imported, error) {
	var v tfTaskDefinition
	if err := kit.Values(o, &v); err != nil {
		return kit.Imported{}, fmt.Errorf("the attributes of %s: %w", o.Address, err)
	}
	prefix := familyPrefix + boundary + "-"
	if !strings.HasPrefix(v.Family, prefix) {
		return kit.Imported{}, kit.Skip(fmt.Sprintf("its family %s is not %s<name>, by which Mamela finds the boundary's task definitions (its guard holds every registration there): "+
			"set family = \"%s%s\" in Terraform (a new revision of a new family), apply, and import again; or declare a new AwsEcsTaskDefinition and point the service at it",
			v.Family, prefix, prefix, o.Name))
	}
	s := taskDefinitionSpec{Boundary: boundary}
	var why []string
	switch {
	case slices.Equal(v.Compatibility, []string{LaunchFargate}) && v.NetworkMode == "awsvpc":
		s.Launch = LaunchFargate
	case slices.Equal(v.Compatibility, []string{LaunchEC2}) && (v.NetworkMode == "bridge" || v.NetworkMode == ""):
		s.Launch = LaunchEC2
	default:
		why = append(why, fmt.Sprintf("it runs on %v with network mode %q (AwsEcsTaskDefinition: FARGATE with awsvpc, or EC2 with bridge)", v.Compatibility, v.NetworkMode))
	}
	cpu, err1 := strconv.ParseInt(v.CPU, 10, 64)
	mem, err2 := strconv.ParseInt(v.Memory, 10, 64)
	if err1 != nil || err2 != nil {
		why = append(why, "it has no task-level cpu and memory")
	}
	s.CPU, s.MemoryMiB = cpu, mem
	for name, set := range map[string]bool{"a task role": v.TaskRole != "", "a pid mode": v.PidMode != "", "an ipc mode": v.IpcMode != "",
		"volumes": len(v.Volume) > 0, "placement constraints": len(v.Placement) > 0, "a proxy": len(v.Proxy) > 0,
		"inference accelerators": len(v.Accelerators) > 0, "ephemeral storage": len(v.Ephemeral) > 0, "a runtime platform": len(v.Runtime) > 0} {
		if set {
			why = append(why, "it has "+name)
		}
	}
	var defs []any
	if err := json.Unmarshal([]byte(v.Containers), &defs); err != nil || len(defs) == 0 {
		return kit.Imported{}, kit.Skip("its container definitions are not a list of containers")
	}
	_, other := tdShape(map[string]any{"networkMode": v.NetworkMode, "containerDefinitions": defs})
	for _, x := range other {
		why = append(why, "it has "+x)
	}
	logGroups := map[string]bool{}
	for _, d := range defs {
		ct, _ := d.(map[string]any)
		c := Container{ContainerName: text(ct["name"]), Image: text(ct["image"]), Command: texts(ct["command"]), MemoryMiB: number(ct["memory"])}
		for _, x := range list(ct["environment"]) {
			m, _ := x.(map[string]any)
			c.Environment = append(c.Environment, Env{Variable: text(m["name"]), Value: text(m["value"])})
		}
		slices.SortFunc(c.Environment, func(a, b Env) int { return strings.Compare(a.Variable, b.Variable) })
		for _, x := range list(ct["portMappings"]) {
			m, _ := x.(map[string]any)
			p := Port{Container: number(m["containerPort"]), Host: number(m["hostPort"])}
			if s.Launch == LaunchFargate {
				p.Host = 0
			}
			if proto := text(m["protocol"]); proto != "" && proto != "tcp" {
				why = append(why, fmt.Sprintf("container %s has a %s port", c.ContainerName, proto))
			}
			c.Ports = append(c.Ports, p)
		}
		lc, _ := ct["logConfiguration"].(map[string]any)
		opts, _ := lc["options"].(map[string]any)
		switch {
		case empty(lc):
			logGroups[""] = true
		case text(lc["logDriver"]) != "awslogs":
			why = append(why, fmt.Sprintf("container %s logs with %s (AwsEcsTaskDefinition: awslogs)", c.ContainerName, text(lc["logDriver"])))
		default:
			logGroups[text(opts["awslogs-group"])] = true
		}
		s.Containers = append(s.Containers, c)
	}
	switch {
	case len(logGroups) > 1:
		why = append(why, "its containers log to different log groups (or some to none); AwsEcsTaskDefinition has one for all")
	case !logGroups[""]:
		for g := range logGroups {
			n, err := referTo(logGroupKind{}, r, "aws_cloudwatch_log_group", "log group "+g, boundary, func(x protocol.ImportObject) bool {
				var lv struct {
					Name string `json:"name"`
				}
				return kit.Values(x, &lv) == nil && lv.Name == g
			})
			if err != nil {
				return kit.Imported{}, err
			}
			s.LogGroup = n
		}
	}
	if len(why) > 0 {
		slices.Sort(why)
		return kit.Imported{}, kit.Skip(strings.Join(why, "; ") + ": AwsEcsTaskDefinition does not declare that, and Terraform keeps managing it")
	}
	if v.ExecutionRole != "" {
		n, err := referTo(roleKind{}, r, "aws_iam_role", "execution role "+v.ExecutionRole, boundary, func(x protocol.ImportObject) bool {
			var rv struct {
				Arn string `json:"arn"`
			}
			return kit.Values(x, &rv) == nil && rv.Arn == v.ExecutionRole
		})
		if err != nil {
			return kit.Imported{}, err
		}
		s.ExecutionRole = n
	}
	if s.LogGroup != "" && s.ExecutionRole == "" {
		return kit.Imported{}, kit.Skip("its containers log to CloudWatch without an execution role")
	}
	return kit.Imported{Spec: s, IDs: map[string]string{"family": v.Family}}, nil
}

func (taskDefinitionKind) find(c *client, ids map[string]string) (*object, error) {
	family := ids["family"]
	if !strings.HasPrefix(family, familyPrefix) {
		return nil, nil
	}
	d, err := describeTaskDefinition(c, family)
	if notFound(err) || err == nil && d.text("status") != "ACTIVE" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o := taskDefinitionObject(d)
	return &o, nil
}

// adopt tags the family's latest revision with the declaration's name and boundary, and the digest
// of the shape ECS answers for it (when AwsEcsTaskDefinition can declare it): a declaration that
// says the same is then in sync, and one that says otherwise registers a new revision.
func (taskDefinitionKind) adopt(o protocol.Observed, desired protocol.Resource) (protocol.Change, error) {
	st := state[taskDefinitionState](o)
	var more [][2]string
	what := "task definition " + o.IDs["taskDefinition"]
	if len(st.Other) == 0 && st.Shape != "" {
		more = append(more, [2]string{TagDigest, st.Shape})
	} else {
		what += fmt.Sprintf(" (it has %v, which AwsEcsTaskDefinition does not declare: the next plan registers a new revision without it)", st.Other)
	}
	// Its IDs last through later revisions (the family, and resourceArn: see taskDefinitionObject).
	return tagChangeECS(o, desired, map[string]string{"family": o.IDs["family"]}, what, more...)
}
