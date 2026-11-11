package aws

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// A CloudWatch log group of tasks' logs: /mamela/<boundary>/<name>, kept for a number of days.

// KindLogGroup is the kind; OpRetention changes how long its logs are kept.
const (
	KindLogGroup protocol.ResourceKind = "AwsLogGroup"
	OpRetention  protocol.Op           = "retention"
)

var (
	describeLogGroups = jsonCall(APILogs, "DescribeLogGroups", "logGroupNamePrefix", "nextToken")
	createLogGroup    = jsonCall(APILogs, "CreateLogGroup", "logGroupName", "tags")
	putRetention      = jsonCall(APILogs, "PutRetentionPolicy", "logGroupName", "retentionInDays")
	deleteLogGroup    = jsonCall(APILogs, "DeleteLogGroup", "logGroupName")
)

type logGroupKind struct{}

type logGroupSpec struct {
	Boundary      string `json:"boundary"`
	RetentionDays int64  `json:"retentionDays"`
}

type logGroupState struct {
	RetentionDays int64  `json:"retentionDays,omitempty"`
	Region        string `json:"region"` // tasks' log configuration names it
}

// LogGroupName is a log group's name.
func LogGroupName(boundary, name string) string { return "/mamela/" + boundary + "/" + name }

// retentions are the numbers of days CloudWatch Logs keeps logs for.
var retentions = []int64{1, 3, 5, 7, 14, 30, 60, 90, 120, 150, 180, 365, 400, 545, 731, 1096, 1827, 2192, 2557, 2922, 3288, 3653}

func (logGroupKind) kind() protocol.ResourceKind { return KindLogGroup }
func (logGroupKind) reads() []protocol.Call      { return []protocol.Call{describeLogGroups} }
func (logGroupKind) operations() []protocol.Operation {
	return []protocol.Operation{op(OpCreate, createLogGroup, putRetention), op(OpRetention, putRetention), deleteOp(deleteLogGroup)}
}

func (logGroupKind) list(c *client) ([]object, error) {
	var out []object
	next := ""
	for {
		var resp struct {
			Groups []struct {
				Name      string `json:"logGroupName"`
				Arn       string `json:"arn"`
				Retention int64  `json:"retentionInDays"`
			} `json:"logGroups"`
			Next string `json:"nextToken"`
		}
		m := map[string]any{"logGroupNamePrefix": "/mamela/"}
		if next != "" {
			m["nextToken"] = next
		}
		if err := c.call(APILogs, "DescribeLogGroups", m, &resp); err != nil {
			return nil, err
		}
		for _, g := range resp.Groups {
			parts := strings.Split(strings.TrimPrefix(g.Name, "/mamela/"), "/")
			if len(parts) != 2 {
				continue
			}
			out = append(out, object{Name: parts[1], Boundary: parts[0], IDs: map[string]string{"logGroupName": g.Name},
				State: logGroupState{RetentionDays: g.Retention, Region: arnRegion(g.Arn)}})
		}
		if resp.Next == "" {
			return out, nil
		}
		next = resp.Next
	}
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

func (logGroupKind) diff(r protocol.Resource, o protocol.Observed, _ map[string]protocol.Observed) ([]protocol.Change, error) {
	s, err := spec[logGroupSpec](r)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(retentions, s.RetentionDays) {
		return nil, fmt.Errorf("retentionDays %d is not one CloudWatch Logs keeps logs for (%v)", s.RetentionDays, retentions)
	}
	name := LogGroupName(s.Boundary, r.Name)
	retention := jsonReq(APILogs, "PutRetentionPolicy", map[string]any{"logGroupName": name, "retentionInDays": s.RetentionDays})
	if !o.Exists {
		return []protocol.Change{{Resource: r.ID, Kind: KindLogGroup, Op: OpCreate, IDs: map[string]string{"mamela-name": r.Name, "logGroupName": name},
			Reasons: []string{fmt.Sprintf("log group %s, kept %d days", name, s.RetentionDays)},
			Requests: []protocol.Approved{
				jsonReq(APILogs, "CreateLogGroup", map[string]any{"logGroupName": name, "tags": map[string]string{TagName: r.Name, TagBoundary: s.Boundary}}),
				retention}}}, nil
	}
	if st := state[logGroupState](o); st.RetentionDays != s.RetentionDays {
		return []protocol.Change{{Resource: r.ID, Kind: KindLogGroup, Op: OpRetention, IDs: map[string]string{"logGroupName": name},
			Reasons: []string{fmt.Sprintf("keep its logs %d days, not %d", s.RetentionDays, st.RetentionDays)}, Requests: []protocol.Approved{retention}}}, nil
	}
	return nil, nil
}

func (logGroupKind) remove(o protocol.Observed) (protocol.Change, error) {
	name := o.IDs["logGroupName"]
	return protocol.Change{Resource: o.ID, Kind: KindLogGroup, Op: OpDelete, IDs: map[string]string{"logGroupName": name},
		Reasons:  []string{"delete log group " + name + ": its logs are lost"},
		Requests: []protocol.Approved{jsonReq(APILogs, "DeleteLogGroup", map[string]any{"logGroupName": name})}}, nil
}

func (k logGroupKind) done(c *client, _ protocol.Change, a protocol.Approved) (bool, error) {
	objs, err := k.list(c)
	if err != nil {
		return false, err
	}
	i := slices.IndexFunc(objs, func(x object) bool { return x.IDs["logGroupName"] == param(a, "logGroupName") })
	switch action(a) {
	case "CreateLogGroup":
		return i >= 0, nil
	case "PutRetentionPolicy":
		return i >= 0 && strconv.FormatInt(objs[i].State.(logGroupState).RetentionDays, 10) == param(a, "retentionInDays"), nil
	case "DeleteLogGroup":
		return i < 0, nil
	}
	return false, nil
}
