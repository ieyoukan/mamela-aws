package aws

import (
	"encoding/json/v2"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela-aws/catalogs"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// The guard (ADR 0006 of Mamela) holds every apply to the operator's boundary and budget, from
// mamela.providers.toml [provider.aws.guard], and watches what is not Mamela's:
//
//   - every object a change names is of the boundary (tagged mamela:boundary, a role marked with
//     it, or named after it); every object a change creates is tagged with it and named after it;
//     an imported object is adopted into it (its marks name this boundary), and never one that is
//     another boundary's;
//   - instances only of the allowed types (launched, resized or adopted), at most max_instances
//     of them; at most max_tasks tasks (services' counts, adopted ones' too, and run-task's
//     running tasks);
//   - after the apply, the account's other EC2 instances and ECS clusters are as they were (what
//     the apply adopted aside).

// GuardSettings are [provider.aws.guard].
type GuardSettings struct {
	Boundary      string   `json:"boundary"`
	InstanceTypes []string `json:"instance_types"`
	MaxInstances  int64    `json:"max_instances"`
	MaxTasks      int64    `json:"max_tasks"`
}

// guardBaseline is what the check before saw outside the boundary.
type guardBaseline struct {
	Instances map[string]string `json:"instances"` // others' EC2 instances: ID -> state
	Clusters  []string          `json:"clusters"`  // others' ECS clusters
}

// world is what the guard reads.
type world struct {
	byKind map[protocol.ResourceKind][]object
	ours   map[string]bool // IDs, names and ARNs of the boundary's objects
	others map[string]bool // those of other boundaries' objects
	// adopting are what the guard read of the objects the changes adopt, by their ID.
	adopting map[string]adoptFact
}

// adoptFact is what an adopted object brings into the boundary.
type adoptFact struct {
	InstanceType string // an instance's
	Tasks        int64  // a service's count
	Cluster      string // a service's cluster
	Path         string // a role's
}

func readWorld(c *client, boundary string) (world, error) {
	w := world{byKind: map[protocol.ResourceKind][]object{}, ours: map[string]bool{}, others: map[string]bool{}, adopting: map[string]adoptFact{}}
	for _, k := range kinds {
		objs, err := k.list(c)
		if err != nil {
			return w, fmt.Errorf("%s: %w", k.kind(), err)
		}
		for _, o := range objs {
			if o.Foreign || o.Boundary == "" {
				continue
			}
			set := w.others
			if o.Boundary == boundary {
				w.byKind[k.kind()] = append(w.byKind[k.kind()], o)
				set = w.ours
			}
			for _, v := range o.IDs {
				set[v] = true
			}
			if st, ok := o.State.(roleState); ok && st.Arn != "" {
				set[st.Arn] = true
			}
		}
	}
	return w, nil
}

// readAdopted reads what the guard needs to know of the objects the changes adopt: an instance's
// type, a service's count and cluster, a role's path.
func readAdopted(c *client, w world, changes []protocol.Change) error {
	for _, ch := range changes {
		if ch.Op != OpAdopt {
			continue
		}
		for _, a := range ch.Requests {
			switch act, arn := action(a), param(a, "resourceArn"); {
			case act == "CreateTags" && strings.HasPrefix(param(a, "ResourceId.1"), "i-"):
				id := param(a, "ResourceId.1")
				got, err := instances(c, map[string]string{"Filter.1.Name": "instance-id", "Filter.1.Value.1": id})
				if err != nil {
					return err
				}
				if len(got) == 1 {
					w.adopting[id] = adoptFact{InstanceType: got[0].Type}
				}
			case act == "TagResource" && a.API == APIECS && ecsArnKind(arn) == "service":
				cluster, name := serviceOfArn(arn)
				got, err := describeServicesOf(c, cluster, []string{name})
				if err != nil {
					return err
				}
				if len(got) == 1 {
					w.adopting[arn] = adoptFact{Tasks: got[0].Desired, Cluster: cluster}
				}
			case act == "UpdateRole":
				var role iamRole
				if err := c.query(APIIAM, "GetRole", map[string]string{"RoleName": param(a, "RoleName")}, &role); err != nil && !notFound(err) {
					return err
				}
				w.adopting[param(a, "RoleName")] = adoptFact{Path: role.Path}
			}
		}
	}
	return nil
}

// ecsArnKind is what an ECS ARN names: cluster, service, task-definition ("arn:aws:ecs:<region>:
// <account>:service/<cluster>/<name>").
func ecsArnKind(arn string) string {
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) != 6 || parts[0] != "arn" || parts[2] != "ecs" {
		return ""
	}
	k, _, _ := strings.Cut(parts[5], "/")
	return k
}

func serviceOfArn(arn string) (cluster, name string) {
	parts := strings.Split(arn[strings.Index(arn, ":service/")+len(":service/"):], "/")
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

// guard is the provider's guard.
func guard(c *client, in protocol.GuardParams) (protocol.GuardResult, error) {
	var out protocol.GuardResult
	add := func(code, severity, format string, args ...any) {
		out.Findings = append(out.Findings, protocol.Finding{Code: code, Severity: severity, Message: fmt.Sprintf(format, args...)})
	}
	var s GuardSettings
	if err := json.Unmarshal(in.Settings, &s); err != nil || s.Boundary == "" || len(s.InstanceTypes) == 0 || s.MaxInstances <= 0 || s.MaxTasks <= 0 {
		add("AWS-0001", protocol.SeverityBlock, "[provider.aws.guard] must set boundary, instance_types, max_instances and max_tasks (%v)", err)
		return out, nil
	}
	w, err := readWorld(c, s.Boundary)
	if err != nil {
		return out, err
	}
	var changes []protocol.Change
	if in.Stage == protocol.GuardBefore {
		changes = in.Changes
		if err := readAdopted(c, w, changes); err != nil {
			return out, err
		}
	}
	others, err := instances(c, nil)
	if err != nil {
		return out, err
	}
	clusters, err := allClusters(c)
	if err != nil {
		return out, err
	}
	// What the changes adopt goes into the boundary: it is not compared after the apply.
	adopted := map[string]bool{}
	for _, ch := range changes {
		for _, a := range ch.Requests {
			if ch.Op == OpAdopt {
				adopted[param(a, "ResourceId.1")] = true
				if arn := param(a, "resourceArn"); ecsArnKind(arn) == "cluster" {
					adopted[lastPart(arn)] = true
				}
			}
		}
	}
	now := guardBaseline{Instances: map[string]string{}}
	for _, i := range others {
		if tagOf(i.Tags, TagBoundary) != s.Boundary && i.State != "terminated" && !adopted[i.ID] {
			now.Instances[i.ID] = i.State
		}
	}
	for _, cl := range clusters {
		if !w.ours[cl.Name] && cl.Status == "ACTIVE" && !adopted[cl.Name] {
			now.Clusters = append(now.Clusters, cl.Name)
		}
	}
	slices.Sort(now.Clusters)

	if in.Stage == protocol.GuardBefore {
		problems := 0
		for _, ch := range changes {
			created := map[string]bool{} // what the change's earlier requests create
			for _, a := range ch.Requests {
				for _, why := range checkRequest(s, w, created, a) {
					problems++
					add("AWS-0100", protocol.SeverityBlock, "%s %s: %s %s; the guard keeps every change to boundary %s", ch.Op, ch.Resource, action(a), why, s.Boundary)
				}
			}
		}
		if problems == 0 {
			add("AWS-0100", protocol.SeverityOK, "every change stays in boundary %s", s.Boundary)
		}
		out.Baseline, _ = json.Marshal(now, json.Deterministic(true))
	}
	limits(s, w, changes, add)

	if in.Stage == protocol.GuardAfter {
		var before guardBaseline
		if err := json.Unmarshal(in.Baseline, &before); err != nil {
			add("AWS-0200", protocol.SeverityWarn, "no baseline from the check before: what is outside the boundary was not compared")
			return out, nil
		}
		changed := 0
		for _, id := range slices.Sorted(maps.Keys(before.Instances)) {
			if now.Instances[id] != before.Instances[id] {
				changed++
			}
		}
		if changed > 0 {
			add("AWS-0201", protocol.SeverityWarn, "%d of the %d EC2 instances outside boundary %s changed state or went away during the apply; Mamela sent them nothing (each request is bound to Mamela's own objects), so check who did",
				changed, len(before.Instances), s.Boundary)
		} else {
			add("AWS-0201", protocol.SeverityOK, "the %d EC2 instances outside boundary %s are as they were", len(before.Instances), s.Boundary)
		}
		var gone []string
		for _, n := range before.Clusters {
			if !slices.Contains(now.Clusters, n) {
				gone = append(gone, n)
			}
		}
		if len(gone) > 0 {
			add("AWS-0202", protocol.SeverityBlock, "%d ECS clusters outside boundary %s went away during the apply: investigate", len(gone), s.Boundary)
		} else {
			add("AWS-0202", protocol.SeverityOK, "the %d ECS clusters outside boundary %s are still there", len(before.Clusters), s.Boundary)
		}
	}
	return out, nil
}

// limits checks the boundary's instances and tasks, with what the changes add.
func limits(s GuardSettings, w world, changes []protocol.Change, add func(code, severity, format string, args ...any)) {
	instances := int64(len(w.byKind[KindInstance]))
	counts := map[string]int64{} // services' counts, by cluster/service
	var running int64
	for _, o := range w.byKind[KindService] {
		st := o.State.(serviceState)
		counts[o.IDs["cluster"]+"/"+o.IDs["service"]] = st.Count
		for _, r := range st.Runs {
			if r.Status != "STOPPED" && r.Status != "DEPROVISIONING" {
				running++
			}
		}
	}
	for _, ch := range changes {
		for _, a := range ch.Requests {
			n, _ := strconv.ParseInt(param(a, "desiredCount"), 10, 64)
			switch action(a) {
			case "RunInstances":
				instances++
			case "CreateService":
				counts[param(a, "cluster")+"/"+param(a, "serviceName")] = n
			case "UpdateService":
				counts[param(a, "cluster")+"/"+param(a, "service")] = n
			case "DeleteService":
				delete(counts, param(a, "cluster")+"/"+param(a, "service"))
			case "TerminateInstances":
				instances--
			case "RunTask":
				k, _ := strconv.ParseInt(param(a, "count"), 10, 64)
				running += k
			case "CreateTags": // adopting an instance brings it into the boundary
				if id := param(a, "ResourceId.1"); strings.HasPrefix(id, "i-") && !w.ours[id] {
					instances++
				}
			case "TagResource": // and a service, its tasks
				if f, ok := w.adopting[param(a, "resourceArn")]; ok && f.Cluster != "" {
					cluster, name := serviceOfArn(param(a, "resourceArn"))
					counts[cluster+"/"+name] = f.Tasks
				}
			}
		}
	}
	if instances > s.MaxInstances {
		add("AWS-0103", protocol.SeverityBlock, "boundary %s would have %d EC2 instances; max_instances is %d", s.Boundary, instances, s.MaxInstances)
	} else {
		add("AWS-0103", protocol.SeverityOK, "boundary %s has %d EC2 instances (max_instances %d)", s.Boundary, instances, s.MaxInstances)
	}
	tasks := running
	for _, n := range counts {
		tasks += n
	}
	if tasks > s.MaxTasks {
		add("AWS-0104", protocol.SeverityBlock, "boundary %s would run %d ECS tasks (services' counts and run-task's); max_tasks is %d", s.Boundary, tasks, s.MaxTasks)
	} else {
		add("AWS-0104", protocol.SeverityOK, "boundary %s runs %d ECS tasks (max_tasks %d)", s.Boundary, tasks, s.MaxTasks)
	}
}

// EC2 and IAM parameters that name an existing object.
var refParam = regexp.MustCompile(`^(VpcId|SubnetId|GroupId|InternetGatewayId|RouteTableId|GatewayId|AssociationId|InstanceId|InstanceId\.1|SecurityGroupId\.\d+|` +
	`GroupId\.\d+|NetworkInterface\.1\.SubnetId|NetworkInterface\.1\.SecurityGroupId\.\d+|IpPermissions\.\d+\.Groups\.1\.GroupId|IamInstanceProfile\.Name|` +
	`RoleName|InstanceProfileName)$`)

// creates: what a request makes, and the parameter that names it.
var creates = map[string]string{"CreateVpc": "", "CreateInternetGateway": "", "CreateSubnet": "", "CreateRouteTable": "", "RunInstances": "",
	"CreateSecurityGroup": "GroupName", "CreateRole": "RoleName", "CreateInstanceProfile": "InstanceProfileName",
	"CreateCluster": "clusterName", "RegisterTaskDefinition": "family", "CreateService": "serviceName", "CreateLogGroup": "logGroupName", "RunTask": ""}

// checkRequest says what in a request is outside the boundary. A request may name what an
// earlier request of the same change creates (created).
func checkRequest(s GuardSettings, w world, created map[string]bool, a protocol.Approved) []string {
	var out []string
	act := action(a)
	b := s.Boundary
	ref := func(what, v string) {
		if v != "" && !w.ours[v] && !created[v] {
			out = append(out, fmt.Sprintf("names %s %s, which is not of the boundary", what, v))
		}
	}
	// Adopting marks an imported object (ADR 0009 of Mamela): with this boundary and the name of
	// a declaration, and never one of another boundary's.
	adopts := func(id, boundary, name string) {
		if boundary != b || name == "" {
			out = append(out, fmt.Sprintf("marks %s for boundary %q, not this one with a declaration's name", id, boundary))
		}
		if w.others[id] {
			out = append(out, fmt.Sprintf("marks %s, which is another boundary's", id))
		}
	}
	nameParam, isCreate := creates[act]
	// What it creates is named after the boundary...
	if isCreate && nameParam != "" {
		prefix := b + "-"
		switch act {
		case "RegisterTaskDefinition":
			prefix = familyPrefix + b + "-"
		case "CreateLogGroup":
			prefix = "/mamela/" + b + "/"
		}
		n := param(a, nameParam)
		if !strings.HasPrefix(n, prefix) {
			out = append(out, fmt.Sprintf("creates %q, not named %s...", n, prefix))
		}
		defer func() { created[n] = true }()
	}
	// ... and tagged with it.
	// (A log group is named after it; a profile, after its role.)
	if isCreate && act != "CreateLogGroup" && act != "CreateInstanceProfile" && boundaryTag(a) != b {
		out = append(out, fmt.Sprintf("creates something tagged %s=%q", TagBoundary, boundaryTag(a)))
	}
	switch {
	case act == "CreateTags":
		id := param(a, "ResourceId.1")
		name := ""
		if param(a, "Tag.1.Key") == TagName {
			name = param(a, "Tag.1.Value")
		}
		adopts(id, boundaryTag(a), name)
		if f, ok := w.adopting[id]; ok && !slices.Contains(s.InstanceTypes, f.InstanceType) {
			out = append(out, fmt.Sprintf("adopts instance %s of type %s, not one of instance_types %v", id, f.InstanceType, s.InstanceTypes))
		}
		return out
	case act == "TagResource" && a.API == APIECS:
		arn := param(a, "resourceArn")
		var tags []ecsTag
		json.Unmarshal([]byte(raw(a, "tags")), &tags)
		adopts(arn, ecsTagOf(tags, TagBoundary), ecsTagOf(tags, TagName))
		switch ecsArnKind(arn) {
		case "cluster":
		case "service":
			cluster, _ := serviceOfArn(arn)
			if !w.ours[cluster] {
				out = append(out, fmt.Sprintf("adopts service %s, whose cluster %s is not of the boundary (adopt the cluster first)", arn, cluster))
			}
		case "task-definition":
			if family, _, _ := strings.Cut(lastPart(arn), ":"); !strings.HasPrefix(family, familyPrefix+b+"-") {
				out = append(out, fmt.Sprintf("adopts task definition %s, not of the family %s%s-...", arn, familyPrefix, b))
			}
		default:
			out = append(out, fmt.Sprintf("tags %s, which is no cluster, service or task definition", arn))
		}
		return out
	case act == "TagResource" && a.API == APILogs:
		arn := param(a, "resourceArn")
		var tags struct {
			Tags map[string]string `json:"tags"`
		}
		json.Unmarshal([]byte(`{"tags":`+raw(a, "tags")+`}`), &tags)
		adopts(arn, tags.Tags[TagBoundary], tags.Tags[TagName])
		if _, name, _ := strings.Cut(strings.TrimSuffix(arn, ":*"), ":log-group:"); !strings.HasPrefix(name, "/mamela/"+b+"/") {
			out = append(out, fmt.Sprintf("tags log group %s, not of /mamela/%s/", name, b))
		}
		return out
	case act == "UpdateRole":
		role := param(a, "RoleName")
		boundary, name, _ := markedBy(param(a, "Description"))
		adopts(role, boundary, name)
		if f := w.adopting[role]; f.Path != catalogs.IAMPath {
			out = append(out, fmt.Sprintf("marks role %s, which is not under %s (it is under %q)", role, catalogs.IAMPath, f.Path))
		}
		return out
	}
	switch a.API {
	case APIEC2, APIIAM:
		for _, k := range slices.Sorted(maps.Keys(a.Params)) {
			if refParam.MatchString(k) && !(isCreate && k == nameParam) {
				ref(k, param(a, k))
			}
		}
		if act == "RunInstances" && !slices.Contains(s.InstanceTypes, param(a, "InstanceType")) {
			out = append(out, fmt.Sprintf("launches %s, not one of instance_types %v", param(a, "InstanceType"), s.InstanceTypes))
		}
		if t := param(a, "InstanceType.Value"); act == "ModifyInstanceAttribute" && t != "" && !slices.Contains(s.InstanceTypes, t) {
			out = append(out, fmt.Sprintf("resizes to %s, not one of instance_types %v", t, s.InstanceTypes))
		}
	case APIECS:
		for _, k := range []string{"cluster", "service", "taskDefinition", "executionRoleArn"} {
			ref(k, param(a, k))
		}
		if v := a.Params["taskDefinitions"]; len(v) == 1 {
			var tds []string
			json.Unmarshal([]byte(v[0]), &tds)
			if len(tds) == 0 {
				out = append(out, "deletes no task definition it names")
			}
			for _, td := range tds {
				if !strings.HasPrefix(td, familyPrefix+b+"-") {
					out = append(out, fmt.Sprintf("deletes task definition %s, not of the boundary's families %s%s-", td, familyPrefix, b))
				}
			}
		}
		if v := a.Params["networkConfiguration"]; len(v) == 1 {
			var n struct {
				Awsvpc struct {
					Subnets []string `json:"subnets"`
					Groups  []string `json:"securityGroups"`
				} `json:"awsvpcConfiguration"`
			}
			json.Unmarshal([]byte(v[0]), &n)
			for _, id := range n.Awsvpc.Subnets {
				ref("subnet", id)
			}
			for _, id := range n.Awsvpc.Groups {
				ref("security group", id)
			}
		}
	case APILogs:
		if n := param(a, "logGroupName"); !strings.HasPrefix(n, "/mamela/"+b+"/") {
			out = append(out, fmt.Sprintf("names log group %s, not of /mamela/%s/", n, b))
		}
	case APISSM:
		out = append(out, "is not a change this provider makes")
	}
	return out
}

// boundaryTag is the mamela:boundary a create tags what it makes with (EC2, IAM, ECS).
func boundaryTag(a protocol.Approved) string {
	for k, v := range a.Params {
		if len(v) != 1 || v[0] != TagBoundary {
			continue
		}
		switch {
		case strings.HasSuffix(k, ".Key"): // EC2 TagSpecification.1.Tag.N.Key and Tag.N.Key, IAM Tags.member.N.Key
			return param(a, strings.TrimSuffix(k, ".Key")+".Value")
		}
	}
	if b, _, ok := markedBy(param(a, "Description")); ok && action(a) == "CreateRole" {
		return b // a role's marker
	}
	if v := a.Params["tags"]; len(v) == 1 {
		var tags []ecsTag
		json.Unmarshal([]byte(v[0]), &tags)
		return ecsTagOf(tags, TagBoundary)
	}
	return ""
}

// raw is a parameter as approved (a JSON member's JSON text), or "".
func raw(a protocol.Approved, k string) string {
	if v := a.Params[k]; len(v) == 1 {
		return v[0]
	}
	return ""
}
