package aws

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/provider/kit"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// A CloudWatch log group of tasks' logs: /mamela/<boundary>/<name>, kept for a number of days,
// tagged mamela:name and mamela:boundary. Mamela keeps its log groups under /mamela/<boundary>/
// (its guard holds every log group change there); one a person adopts keeps its name there.

// KindLogGroup is the kind and its operations: retention keeps its logs longer (planned);
// shortening the retention deletes the older logs, so a person does it (shorten-retention).
const (
	KindLogGroup       protocol.ResourceKind = "AwsLogGroup"
	OpRetention        protocol.Op           = "retention"
	OpShorten          protocol.Op           = "shorten" // never carried out by a plan
	OpShortenRetention protocol.Op           = "shorten-retention"
)

var (
	describeLogGroups = jsonCall(APILogs, "DescribeLogGroups", "logGroupNamePrefix", "nextToken")
	listLogTags       = jsonCall(APILogs, "ListTagsForResource", "resourceArn")
	createLogGroup    = jsonCall(APILogs, "CreateLogGroup", "logGroupName", "tags")
	putRetention      = jsonCall(APILogs, "PutRetentionPolicy", "logGroupName", "retentionInDays")
	deleteLogGroup    = jsonCall(APILogs, "DeleteLogGroup", "logGroupName")
	tagLogGroup       = jsonCall(APILogs, "TagResource", "resourceArn", "tags")
)

type logGroupKind struct{}

type logGroupSpec struct {
	Boundary      string `json:"boundary" doc:"the boundary (the guard's): the log group is /mamela/<boundary>/<name>"`
	RetentionDays int64  `json:"retentionDays" doc:"how many days its logs are kept: 1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827, 2192, 2557, 2922, 3288 or 3653"`
}

type logGroupState struct {
	RetentionDays int64  `json:"retentionDays,omitempty"` // 0: kept forever
	Region        string `json:"region"`                  // tasks' log configuration names it
	StoredBytes   int64  `json:"storedBytes,omitempty"`   // what its logs take (CloudWatch counts it with a delay)
}

// LogGroupName is the name of a log group Mamela makes.
func LogGroupName(boundary, name string) string { return "/mamela/" + boundary + "/" + name }

// retentions are the numbers of days CloudWatch Logs keeps logs for.
var retentions = []int64{1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827, 2192, 2557, 2922, 3288, 3653}

func (logGroupKind) kind() protocol.ResourceKind { return KindLogGroup }
func (logGroupKind) reads() []protocol.Call      { return []protocol.Call{describeLogGroups, listLogTags} }
func (logGroupKind) operations() []protocol.Operation {
	return []protocol.Operation{op(OpCreate, createLogGroup, putRetention), op(OpRetention, putRetention),
		unsupportedOp(OpShorten, "a shorter retention deletes the stored logs older than it: a person shortens it with the explicit operation shorten-retention"),
		explicitOp(OpShortenRetention, putRetention), deleteOp(deleteLogGroup), adoptOp(tagLogGroup)}
}
func (logGroupKind) spec() any { return logGroupSpec{} }
func (logGroupKind) doc() string {
	return "A CloudWatch Logs log group, /mamela/<boundary>/<name>, for tasks' logs. Mamela keeps its logs longer when the declaration says so; it never shortens the retention in a plan (that deletes logs): `mamela act --confirm <name> shorten-retention` does."
}

type cwLogGroup struct {
	Name        string `json:"logGroupName"`
	Arn         string `json:"arn"`
	GroupArn    string `json:"logGroupArn"`
	Retention   int64  `json:"retentionInDays"`
	StoredBytes int64  `json:"storedBytes"`
}

// logGroups are the account's log groups whose names start with prefix.
func logGroups(c *client, prefix string) ([]cwLogGroup, error) {
	var out []cwLogGroup
	next := ""
	for {
		var resp struct {
			Groups []cwLogGroup `json:"logGroups"`
			Next   string       `json:"nextToken"`
		}
		m := map[string]any{"logGroupNamePrefix": prefix}
		if next != "" {
			m["nextToken"] = next
		}
		if err := c.call(APILogs, "DescribeLogGroups", m, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Groups...)
		if resp.Next == "" {
			return out, nil
		}
		next = resp.Next
	}
}

// logObject is a log group as an object: Mamela's when it is tagged as one of the boundary its
// name is under; Foreign when it carries no tag of Mamela's; nil otherwise (it is not Mamela's to
// touch). Its name is /mamela/<boundary>/<name>.
func logObject(c *client, g cwLogGroup) (*object, error) {
	parts := strings.Split(strings.TrimPrefix(g.Name, "/mamela/"), "/")
	if !strings.HasPrefix(g.Name, "/mamela/") || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, nil
	}
	arn := g.GroupArn
	if arn == "" {
		arn = strings.TrimSuffix(g.Arn, ":*")
	}
	var tags struct {
		Tags map[string]string `json:"tags"`
	}
	if err := c.call(APILogs, "ListTagsForResource", map[string]any{"resourceArn": arn}, &tags); err != nil {
		if notFound(err) {
			return nil, nil // deleted meanwhile
		}
		return nil, err
	}
	o := &object{Boundary: parts[0], IDs: map[string]string{"logGroupName": g.Name, "resourceArn": arn},
		State: logGroupState{RetentionDays: g.Retention, Region: arnRegion(arn), StoredBytes: g.StoredBytes}}
	name, boundary := tags.Tags[TagName], tags.Tags[TagBoundary]
	switch {
	case name == "" && boundary == "":
		o.Name, o.Foreign = parts[1], true
	case name != "" && boundary == parts[0]:
		o.Name = name
	default:
		return nil, nil
	}
	return o, nil
}

func (logGroupKind) list(c *client) ([]object, error) {
	groups, err := logGroups(c, "/mamela/")
	if err != nil {
		return nil, err
	}
	var out []object
	for _, g := range groups {
		o, err := logObject(c, g)
		if err != nil {
			return nil, err
		}
		if o != nil {
			out = append(out, *o)
		}
	}
	return out, nil
}

// arnRegion is the region of an ARN ("arn:aws:logs:ap-northeast-1:...").
func arnRegion(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) < 4 {
		return ""
	}
	return parts[3]
}

func (logGroupKind) missing(*client, protocol.Resource) (any, error) { return nil, nil }

func checkRetention(days int64) error {
	if !slices.Contains(retentions, days) {
		return fmt.Errorf("retentionDays %d is not one CloudWatch Logs keeps logs for (%v)", days, retentions)
	}
	return nil
}

func (logGroupKind) diff(r protocol.Resource, o protocol.Observed, _ map[string]protocol.Observed) ([]protocol.Change, error) {
	s, err := spec[logGroupSpec](r)
	if err != nil {
		return nil, err
	}
	if err := checkRetention(s.RetentionDays); err != nil {
		return nil, err
	}
	if !o.Exists {
		name := LogGroupName(s.Boundary, r.Name)
		return []protocol.Change{{Resource: r.ID, Kind: KindLogGroup, Op: OpCreate, IDs: map[string]string{"mamela-name": r.Name, "logGroupName": name},
			Reasons: []string{fmt.Sprintf("log group %s, kept %d days", name, s.RetentionDays)},
			Requests: []protocol.Approved{
				jsonReq(APILogs, "CreateLogGroup", map[string]any{"logGroupName": name, "tags": map[string]string{TagName: r.Name, TagBoundary: s.Boundary}}),
				jsonReq(APILogs, "PutRetentionPolicy", map[string]any{"logGroupName": name, "retentionInDays": s.RetentionDays})}}}, nil
	}
	name := o.IDs["logGroupName"]
	ids := map[string]string{"logGroupName": name}
	st := state[logGroupState](o)
	switch cur := st.RetentionDays; {
	case cur == s.RetentionDays:
		return nil, nil
	case cur != 0 && s.RetentionDays > cur || st.StoredBytes == 0:
		why := fmt.Sprintf("keep its logs %d days, not %s", s.RetentionDays, keptFor(cur))
		if cur == 0 || s.RetentionDays < cur {
			why += " (it stores no logs, so none are deleted)"
		}
		return []protocol.Change{{Resource: r.ID, Kind: KindLogGroup, Op: OpRetention, IDs: ids,
			Reasons:  []string{why},
			Requests: []protocol.Approved{jsonReq(APILogs, "PutRetentionPolicy", map[string]any{"logGroupName": name, "retentionInDays": s.RetentionDays})}}}, nil
	default:
		return []protocol.Change{unsupported(r, KindLogGroup, OpShorten, ids, fmt.Sprintf(
			"keep its logs %d days, not %s: CloudWatch Logs would delete the logs older than %d days. A person shortens it: mamela act --confirm %s %s %q <pome-dir>",
			s.RetentionDays, keptFor(cur), s.RetentionDays, r.Name, OpShortenRetention, r.ID))}, nil
	}
}

func keptFor(days int64) string {
	if days == 0 {
		return "forever"
	}
	return strconv.FormatInt(days, 10)
}

// prepare: shorten-retention, the shorter retention the declaration says.
func (logGroupKind) prepare(op protocol.Op, o protocol.Observed, desired *protocol.Resource) (protocol.Change, error) {
	if op != OpShortenRetention {
		return protocol.Change{}, fmt.Errorf("%s is not an explicit operation of %s", op, KindLogGroup)
	}
	if desired == nil {
		return protocol.Change{}, fmt.Errorf("%s: shorten-retention keeps the logs as many days as the declaration says, and it is not declared", o.ID)
	}
	s, err := spec[logGroupSpec](*desired)
	if err != nil {
		return protocol.Change{}, err
	}
	if err := checkRetention(s.RetentionDays); err != nil {
		return protocol.Change{}, err
	}
	cur := state[logGroupState](o).RetentionDays
	if cur != 0 && s.RetentionDays >= cur {
		return protocol.Change{}, fmt.Errorf("%s keeps its logs %d days; the declaration's %d is not shorter (a plan makes it longer)", o.ID, cur, s.RetentionDays)
	}
	name := o.IDs["logGroupName"]
	return protocol.Change{Resource: o.ID, Kind: KindLogGroup, Op: OpShortenRetention, IDs: map[string]string{"logGroupName": name},
		Reasons:  []string{fmt.Sprintf("keep the logs of %s %d days, not %s: the logs older than %d days are deleted", name, s.RetentionDays, keptFor(cur), s.RetentionDays)},
		Requests: []protocol.Approved{jsonReq(APILogs, "PutRetentionPolicy", map[string]any{"logGroupName": name, "retentionInDays": s.RetentionDays})}}, nil
}

func (logGroupKind) remove(o protocol.Observed) (protocol.Change, error) {
	name := o.IDs["logGroupName"]
	return protocol.Change{Resource: o.ID, Kind: KindLogGroup, Op: OpDelete, IDs: map[string]string{"logGroupName": name},
		Reasons:  []string{"delete log group " + name + ": its logs are lost"},
		Requests: []protocol.Approved{jsonReq(APILogs, "DeleteLogGroup", map[string]any{"logGroupName": name})}}, nil
}

func (k logGroupKind) done(c *client, ch protocol.Change, a protocol.Approved) (bool, error) {
	objs, err := k.list(c)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(objs, func(x object) bool { return x.IDs["logGroupName"] == param(a, "logGroupName") })
	switch action(a) {
	case "CreateLogGroup":
		return i >= 0 && !objs[i].Foreign && objs[i].Name == nameOf(ch), nil
	case "PutRetentionPolicy":
		return i >= 0 && strconv.FormatInt(objs[i].State.(logGroupState).RetentionDays, 10) == param(a, "retentionInDays"), nil
	case "DeleteLogGroup":
		return i < 0, nil
	}
	return false, nil
}

// --- import and adopt ---

func (logGroupKind) imports() []string { return []string{"aws_cloudwatch_log_group"} }

// importObject declares a log group under /mamela/<boundary>/ that keeps its logs a number of days
// CloudWatch Logs offers. Its KMS key and class stay as they are.
func (logGroupKind) importObject(o protocol.ImportObject, _ kit.Resolver, boundary string) (kit.Imported, error) {
	var v struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Retention int64  `json:"retention_in_days"`
	}
	if err := kit.Values(o, &v); err != nil {
		return kit.Imported{}, fmt.Errorf("the attributes of %s: %w", o.Address, err)
	}
	if v.Name == "" {
		v.Name = v.ID
	}
	prefix := "/mamela/" + boundary + "/"
	if rest, ok := strings.CutPrefix(v.Name, prefix); !ok || rest == "" || strings.Contains(rest, "/") {
		return kit.Imported{}, kit.Skip(fmt.Sprintf("log group %s is not under %s<name>, where Mamela keeps the boundary's log groups (its guard holds every log group change there), "+
			"and CloudWatch Logs cannot rename it: rename it in Terraform (which makes a new one; the logs stay in the old) and import again, "+
			"or declare a new AwsLogGroup, point what logs here at it, and leave this one to Terraform", v.Name, prefix))
	}
	if !slices.Contains(retentions, v.Retention) {
		return kit.Imported{}, kit.Skip(fmt.Sprintf("it keeps its logs %s, and AwsLogGroup declares a number of days (%v): set retention_in_days in Terraform, apply, and import again",
			keptFor(v.Retention), retentions))
	}
	return kit.Imported{Spec: logGroupSpec{Boundary: boundary, RetentionDays: v.Retention}, IDs: map[string]string{"logGroupName": v.Name}}, nil
}

func (logGroupKind) find(c *client, ids map[string]string) (*object, error) {
	name := ids["logGroupName"]
	if name == "" {
		return nil, nil
	}
	groups, err := logGroups(c, name)
	if err != nil {
		return nil, err
	}
	for _, g := range groups {
		if g.Name != name {
			continue
		}
		o, err := logObject(c, g)
		if err != nil || o == nil || !o.Foreign {
			return nil, err // not under /mamela/<boundary>/, or Mamela's already
		}
		o.Name, o.Foreign = "", false
		return o, nil
	}
	return nil, nil
}

// adopt tags the log group mamela:name and mamela:boundary (TagResource): nothing else changes.
func (logGroupKind) adopt(o protocol.Observed, desired protocol.Resource) (protocol.Change, error) {
	b, err := boundaryOf(desired)
	if err != nil {
		return protocol.Change{}, err
	}
	name, arn := o.IDs["logGroupName"], o.IDs["resourceArn"]
	if !strings.HasPrefix(name, "/mamela/"+b+"/") {
		return protocol.Change{}, fmt.Errorf("%s: log group %s is not under /mamela/%s/", o.ID, name, b)
	}
	return adoptChange(o, desired, map[string]string{"logGroupName": name, "resourceArn": arn},
		fmt.Sprintf("adopt log group %s: tag it %s=%s and %s=%s, which makes it Mamela's; its logs and retention do not change", name, TagName, desired.Name, TagBoundary, b),
		jsonReq(APILogs, "TagResource", map[string]any{"resourceArn": arn, "tags": map[string]string{TagName: desired.Name, TagBoundary: b}}))
}
