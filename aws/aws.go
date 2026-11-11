// Package aws is a Mamela provider of AWS resources (ADR 0007 of Mamela): a VPC of its own and its
// network, IAM roles for ECS, CloudWatch log groups, and ECS (clusters, container instances on
// EC2, task definitions, services, one-off task runs). It runs as a separate, isolated program
// (WASM): it reaches AWS only through the Mamela host, which signs each request (SigV4, with the
// profile's temporary credentials), checks it against Mamela's catalogs (builtin:ec2, ecs, logs,
// iam, ssm) and the change's permit, and never shows the provider a key.
//
// Everything it makes carries the tags mamela:name (the resource's name) and mamela:boundary
// (the declaration's boundary), and AWS names that start with the boundary. It changes only what
// it made (Mamela vouches for that with its own records), never deletes as a plan's change, and
// its guard holds every apply to the operator's boundary and budget.
package aws

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"encoding/xml"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/provider/protocol"
	"github.com/ieyoukan/mamela/provider/sdk"
)

// Name and Version are the provider's, as its manifest and Pome name them.
const (
	Name    = "aws"
	Version = "0.1.0"
)

// The APIs, as mamela.providers.toml names them ([provider.aws.apis.<name>]).
const (
	APIEC2  = "ec2"
	APIECS  = "ecs"
	APILogs = "logs"
	APIIAM  = "iam"
	APISSM  = "ssm"

	ec2Version = "2016-11-15"
	iamVersion = "2010-05-08"
)

// Tags Mamela puts on what this provider makes.
const (
	TagName     = "mamela:name"
	TagBoundary = "mamela:boundary"
)

// OpDelete is every kind's explicit delete (ADR 0006 of Mamela): never planned.
const OpDelete protocol.Op = "delete"

// object is one AWS object this provider made, as observed.
type object struct {
	Name, Boundary string
	IDs            map[string]string
	Version        string
	State          any
}

// kind is one resource kind.
type kind interface {
	kind() protocol.ResourceKind
	// reads are the calls that observe the kind (and that apply and lookup use to check).
	reads() []protocol.Call
	// operations are its changes (their calls, without the reads).
	operations() []protocol.Operation
	// list returns every object of the kind this provider tagged.
	list(c *client) ([]object, error)
	// missing is what a declared resource that does not exist needs observed to be created (the
	// ECS-optimized AMI of an instance); nil when nothing.
	missing(c *client, r protocol.Resource) (any, error)
	diff(r protocol.Resource, o protocol.Observed, deps map[string]protocol.Observed) ([]protocol.Change, error)
	// remove is the change of the explicit delete of an observed object.
	remove(o protocol.Observed) (protocol.Change, error)
	// done says whether an approved request is in effect already (for apply to skip it, and for
	// lookup).
	done(c *client, ch protocol.Change, a protocol.Approved) (bool, error)
}

var kinds = []kind{vpcKind{}, gatewayKind{}, subnetKind{}, routeTableKind{}, groupKind{}, logGroupKind{}, roleKind{},
	clusterKind{}, instanceKind{}, taskDefinitionKind{}, serviceKind{}}

func kindOf(k protocol.ResourceKind) (kind, bool) {
	for _, x := range kinds {
		if x.kind() == k {
			return x, true
		}
	}
	return nil, false
}

// Provider implements sdk.Provider (and sdk.Guarder, sdk.Preparer).
type Provider struct{}

// Manifest implements sdk.Provider.
func (Provider) Manifest() protocol.Manifest {
	m := protocol.Manifest{Schema: protocol.Schema, Provider: Name, Version: Version,
		APIs: []string{APIEC2, APIECS, APILogs, APIIAM, APISSM}, Guard: &protocol.Guard{Calls: allReads()}}
	m.ObserveCalls = allReads()
	m.Observe = needs("ec2:Describe*", "ecs:Describe*", "ecs:List*", "logs:DescribeLogGroups", "iam:GetRole", "iam:ListAttachedRolePolicies",
		"iam:GetInstanceProfile", "ssm:GetParameter")
	for _, k := range kinds {
		var ops []protocol.Operation
		for _, o := range k.operations() {
			if o.Support == protocol.Supported {
				// An operation reads what it changes, to skip what is in effect and to look it up.
				o.Calls = append(slices.Clone(k.reads()), o.Calls...)
			}
			ops = append(ops, o)
		}
		m.Kinds = append(m.Kinds, protocol.Kind{Kind: k.kind(), Operations: ops})
	}
	return m
}

// needs are the IAM actions an operation needs of the profile's role (shown by mamela check).
func needs(actions ...string) []protocol.Need {
	var out []protocol.Need
	for _, a := range actions {
		out = append(out, protocol.Need{Capability: a, Target: protocol.Target{Type: "aws-iam", Ref: "*"}, Always: true})
	}
	return out
}

// Observe implements sdk.Provider.
func (Provider) Observe(ctx context.Context, h *sdk.Host, in protocol.ObserveParams) (protocol.ObserveResult, error) {
	c := &client{ctx: ctx, h: h}
	var out protocol.ObserveResult
	declared := map[string]bool{}
	byKind := map[protocol.ResourceKind][]object{}
	for _, k := range kinds {
		objs, err := k.list(c)
		if err != nil {
			return out, fmt.Errorf("%s: %w", k.kind(), err)
		}
		byKind[k.kind()] = objs
	}
	for _, r := range in.Resources {
		declared[r.ID] = true
		k, ok := kindOf(r.Kind)
		if !ok {
			return out, fmt.Errorf("%s: kind %s is not this provider's", r.ID, r.Kind)
		}
		var s base
		if err := json.Unmarshal(r.Spec, &s); err != nil || s.Boundary == "" {
			return out, fmt.Errorf("%s: the spec has no boundary", r.ID)
		}
		o := protocol.Observed{ID: r.ID, Kind: r.Kind, Name: r.Name}
		i := slices.IndexFunc(byKind[r.Kind], func(x object) bool { return x.Name == r.Name && x.Boundary == s.Boundary })
		if i >= 0 {
			fill(&o, byKind[r.Kind][i])
		} else {
			st, err := k.missing(c, r)
			if err != nil {
				return out, fmt.Errorf("%s: %w", r.ID, err)
			}
			if st != nil {
				o.State, _ = json.Marshal(st, json.Deterministic(true))
			}
		}
		out.Resources = append(out.Resources, o)
	}
	// What it made that is no longer declared: reported, so that Mamela keeps it.
	for _, k := range kinds {
		for _, x := range byKind[k.kind()] {
			id := string(k.kind()) + " " + x.Name
			if declared[id] {
				continue
			}
			declared[id] = true
			o := protocol.Observed{ID: id, Kind: k.kind(), Name: x.Name}
			fill(&o, x)
			out.Resources = append(out.Resources, o)
		}
	}
	return out, nil
}

func fill(o *protocol.Observed, x object) {
	o.Exists, o.Owned, o.Version = true, true, x.Version
	o.IDs = map[string]string{"mamela-name": x.Name}
	maps.Copy(o.IDs, x.IDs)
	if x.State != nil {
		o.State, _ = json.Marshal(x.State, json.Deterministic(true))
	}
}

// Diff implements sdk.Provider.
func (Provider) Diff(_ context.Context, in protocol.DiffParams) (protocol.DiffResult, error) {
	obs := map[string]protocol.Observed{}
	for _, o := range in.Observed {
		obs[o.ID] = o
	}
	desired := slices.Clone(in.Desired)
	slices.SortFunc(desired, func(a, b protocol.Resource) int { return strings.Compare(a.ID, b.ID) })
	var out protocol.DiffResult
	for _, r := range desired {
		k, ok := kindOf(r.Kind)
		if !ok {
			return out, fmt.Errorf("%s: kind %s is not this provider's", r.ID, r.Kind)
		}
		o := obs[r.ID]
		if o.Exists && !o.Owned {
			continue // not Mamela's: nothing is planned for it
		}
		cs, err := k.diff(r, o, in.Dependencies)
		if err != nil {
			return out, fmt.Errorf("%s: %w", r.ID, err)
		}
		out.Changes = append(out.Changes, cs...)
	}
	return out, nil
}

// Prepare implements sdk.Preparer: the explicit delete, and a service's run-task.
func (Provider) Prepare(_ context.Context, in protocol.PrepareParams) (protocol.PrepareResult, error) {
	k, ok := kindOf(in.Observed.Kind)
	if !ok || !in.Observed.Exists {
		return protocol.PrepareResult{}, fmt.Errorf("%s: nothing to %s", in.Observed.ID, in.Op)
	}
	switch in.Op {
	case OpDelete:
		ch, err := k.remove(in.Observed)
		return protocol.PrepareResult{Change: ch}, err
	case OpRunTask:
		ch, err := runTaskChange(in.Observed)
		return protocol.PrepareResult{Change: ch}, err
	}
	return protocol.PrepareResult{}, fmt.Errorf("%s is not an explicit operation of %s", in.Op, in.Observed.Kind)
}

// Apply implements sdk.Provider: it sends the approved requests in order, each at most once, and
// skips those in effect already (an earlier apply that was interrupted).
func (Provider) Apply(ctx context.Context, h *sdk.Host, in protocol.ApplyParams) (protocol.ApplyResult, error) {
	c := &client{ctx: ctx, h: h}
	k, ok := kindOf(in.Change.Kind)
	if !ok {
		return protocol.ApplyResult{}, fmt.Errorf("kind %s is not this provider's", in.Change.Kind)
	}
	var sent []string
	for _, a := range in.Change.Requests {
		skip, err := k.done(c, in.Change, a)
		if err != nil {
			return protocol.ApplyResult{}, err
		}
		if skip {
			continue
		}
		if _, err := c.send(a); err != nil {
			var ae *awsError
			if errors.As(err, &ae) && alreadyDone(ae.Code) {
				continue
			}
			return protocol.ApplyResult{}, err
		}
		sent = append(sent, action(a))
	}
	return protocol.ApplyResult{Message: fmt.Sprintf("%s %s: %s", in.Change.Op, in.Change.Resource, strings.Join(sent, ", "))}, nil
}

// alreadyDone: AWS's answers to a create of something that exists (a retry after a lost answer).
func alreadyDone(code string) bool {
	return slices.Contains([]string{"InvalidGroup.Duplicate", "InvalidPermission.Duplicate", "RouteAlreadyExists", "Resource.AlreadyAssociated",
		"EntityAlreadyExists", "ResourceAlreadyExistsException", "LimitExceeded.AlreadyAdded"}, code)
}

// Lookup implements sdk.Provider: how many of the approved requests are in effect.
func (Provider) Lookup(ctx context.Context, h *sdk.Host, in protocol.LookupParams) (protocol.LookupResult, error) {
	c := &client{ctx: ctx, h: h}
	k, ok := kindOf(in.Change.Kind)
	if !ok {
		return protocol.LookupResult{}, fmt.Errorf("kind %s is not this provider's", in.Change.Kind)
	}
	n := 0
	for _, a := range in.Change.Requests {
		ok, err := k.done(c, in.Change, a)
		if err != nil {
			return protocol.LookupResult{}, err
		}
		if ok {
			n++
		}
	}
	switch {
	case n == len(in.Change.Requests):
		return protocol.LookupResult{Status: protocol.LookupDone}, nil
	case n == 0:
		return protocol.LookupResult{Status: protocol.LookupNotDone}, nil
	}
	return protocol.LookupResult{Status: protocol.LookupPartly}, nil
}

// base is what every spec has.
type base struct {
	Boundary string `json:"boundary"`
}

func spec[T any](r protocol.Resource) (T, error) {
	var s T
	if err := json.Unmarshal(r.Spec, &s, json.RejectUnknownMembers(true)); err != nil {
		return s, err
	}
	return s, nil
}

func state[T any](o protocol.Observed) T {
	var s T
	if len(o.State) > 0 {
		json.Unmarshal(o.State, &s)
	}
	return s
}

// awsName is the AWS name of a resource: the boundary, then its name ("locust-target").
func awsName(boundary, name string) string { return boundary + "-" + name }

// dep returns the observation of a dependency, by kind and name.
func dep(deps map[string]protocol.Observed, k protocol.ResourceKind, name string) (protocol.Observed, error) {
	o, ok := deps[string(k)+" "+name]
	if !ok || !o.Exists {
		return o, fmt.Errorf("%s %s is not among its dependencies (declare it, and this one after it)", k, name)
	}
	return o, nil
}

// --- requests -------------------------------------------------------------------------------

const target = "X-Amz-Target"

// queryReq is an approved request to a query API (EC2, IAM).
func queryReq(api, action string, params map[string]string) protocol.Approved {
	version := ec2Version
	if api == APIIAM {
		version = iamVersion
	}
	p := map[string][]string{"Action": {action}, "Version": {version}}
	for k, v := range params {
		p[k] = []string{v}
	}
	return protocol.Approved{API: api, Method: "POST", Path: "/", Params: p}
}

// jsonReq is an approved request to a JSON API (ECS, Logs, SSM): its members in canonical form.
func jsonReq(api, action string, members map[string]any) protocol.Approved {
	p := map[string][]string{target: {targetOf(api, action)}}
	raw := map[string]jsontext.Value{}
	for k, v := range members {
		b, err := json.Marshal(v, json.Deterministic(true))
		if err != nil {
			panic(err)
		}
		raw[k] = b
	}
	ps, err := protocol.JSONParams(raw)
	if err != nil {
		panic(err)
	}
	maps.Copy(p, ps)
	return protocol.Approved{API: api, Method: "POST", Path: "/", Params: p}
}

func targetOf(api, action string) string {
	switch api {
	case APIECS:
		return "AmazonEC2ContainerServiceV20141113." + action
	case APILogs:
		return "Logs_20140328." + action
	case APISSM:
		return "AmazonSSM." + action
	}
	return ""
}

// queryCall and jsonCall are the declared calls.
func queryCall(api, action string, params ...string) protocol.Call {
	version := ec2Version
	if api == APIIAM {
		version = iamVersion
	}
	return protocol.Call{API: api, Method: "POST", Path: "/", Form: append([]string{"Action", "Version"}, params...),
		Fixed: map[string]string{"Action": action, "Version": version}}
}

func jsonCall(api, action string, members ...string) protocol.Call {
	return protocol.Call{API: api, Method: "POST", Path: "/", Header: []string{target}, JSON: members,
		Fixed: map[string]string{target: targetOf(api, action)}}
}

func action(a protocol.Approved) string {
	if t := a.Params[target]; len(t) == 1 {
		_, act, _ := strings.Cut(t[0], ".")
		return act
	}
	if v := a.Params["Action"]; len(v) == 1 {
		return v[0]
	}
	return a.Method + " " + a.Path
}

// param is an approved request's parameter (a JSON member as its text).
func param(a protocol.Approved, k string) string {
	v := a.Params[k]
	if len(v) != 1 {
		return ""
	}
	if _, isJSON := a.Params[target]; isJSON {
		return protocol.ParamText("json", v[0])
	}
	return v[0]
}

// client sends requests through the host.
type client struct {
	ctx context.Context
	h   *sdk.Host
}

// awsError is an AWS API's refusal.
type awsError struct {
	Status        int
	Code, Message string
}

func (e *awsError) Error() string {
	return fmt.Sprintf("%s: %s (status %d)", e.Code, e.Message, e.Status)
}

// send sends an approved request (or a read built like one).
func (c *client) send(a protocol.Approved) ([]byte, error) {
	r := protocol.HostRequest{API: a.API, Method: a.Method, Path: a.Path}
	if t, ok := a.Params[target]; ok {
		r.Header = map[string][]string{target: t}
		r.JSON = map[string]jsontext.Value{}
		for k, v := range a.Params {
			if k != target && len(v) == 1 {
				r.JSON[k] = jsontext.Value(v[0])
			}
		}
	} else {
		r.Form = a.Params
	}
	resp, err := c.h.Request(c.ctx, r)
	if err != nil {
		return nil, err
	}
	if resp.Status == 200 {
		return []byte(resp.Body), nil
	}
	return nil, parseError(resp.Status, []byte(resp.Body))
}

func parseError(status int, body []byte) error {
	e := &awsError{Status: status}
	var x struct {
		Code    string `xml:"Errors>Error>Code"`
		Message string `xml:"Errors>Error>Message"`
		Code2   string `xml:"Error>Code"`
		Msg2    string `xml:"Error>Message"`
	}
	var j struct {
		Type    string `json:"__type"`
		Message string `json:"message"`
		Msg2    string `json:"Message"`
	}
	switch {
	case xml.Unmarshal(body, &x) == nil && (x.Code != "" || x.Code2 != ""):
		e.Code, e.Message = x.Code+x.Code2, x.Message+x.Msg2
	case json.Unmarshal(body, &j) == nil && j.Type != "":
		_, e.Code, _ = strings.Cut(j.Type, "#")
		if e.Code == "" {
			e.Code = j.Type
		}
		e.Message = j.Message + j.Msg2
	default:
		e.Code, e.Message = "HTTP", fmt.Sprintf("%.300s", body)
	}
	return e
}

// query sends a read to a query API and decodes its XML answer.
func (c *client) query(api, action string, params map[string]string, out any) error {
	body, err := c.send(queryReq(api, action, params))
	if err != nil {
		return err
	}
	return xml.Unmarshal(body, out)
}

// call sends a read to a JSON API and decodes its answer.
func (c *client) call(api, action string, members map[string]any, out any) error {
	body, err := c.send(jsonReq(api, action, members))
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// notFound: an AWS answer that the object does not exist.
func notFound(err error) bool {
	var ae *awsError
	return errors.As(err, &ae) && (strings.Contains(ae.Code, "NotFound") || ae.Code == "NoSuchEntity" || ae.Code == "ResourceNotFoundException" ||
		ae.Code == "ClientException" && strings.Contains(ae.Message, "not found") || ae.Code == "ClusterNotFoundException" || ae.Code == "ServiceNotFoundException")
}

// --- tags -----------------------------------------------------------------------------------

// ec2Tags are the TagSpecification parameters of an EC2 create.
func ec2Tags(resourceType, boundary, name string, params map[string]string) map[string]string {
	params["TagSpecification.1.ResourceType"] = resourceType
	for i, kv := range [][2]string{{"Name", awsName(boundary, name)}, {TagName, name}, {TagBoundary, boundary}} {
		params[fmt.Sprintf("TagSpecification.1.Tag.%d.Key", i+1)] = kv[0]
		params[fmt.Sprintf("TagSpecification.1.Tag.%d.Value", i+1)] = kv[1]
	}
	return params
}

var ec2TagParams = []string{"TagSpecification.1.ResourceType", "TagSpecification.1.Tag.1.Key", "TagSpecification.1.Tag.1.Value",
	"TagSpecification.1.Tag.2.Key", "TagSpecification.1.Tag.2.Value", "TagSpecification.1.Tag.3.Key", "TagSpecification.1.Tag.3.Value"}

// xmlTag is an EC2 tag in a Describe answer.
type xmlTag struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

func tagOf(tags []xmlTag, k string) string {
	for _, t := range tags {
		if t.Key == k {
			return t.Value
		}
	}
	return ""
}

// tagged are the Describe parameters that find what this provider tagged.
func tagged() map[string]string {
	return map[string]string{"Filter.1.Name": "tag-key", "Filter.1.Value.1": TagName}
}

// ecsTags are ECS tags; logsTags CloudWatch Logs tags.
func ecsTags(boundary, name string) []map[string]string {
	return []map[string]string{{"key": TagName, "value": name}, {"key": TagBoundary, "value": boundary}}
}

type ecsTag struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func ecsTagOf(tags []ecsTag, k string) string {
	for _, t := range tags {
		if t.Key == k {
			return t.Value
		}
	}
	return ""
}

// noRequests is a change that sends nothing (an unsupported difference, shown and denied).
func unsupported(r protocol.Resource, k protocol.ResourceKind, op protocol.Op, ids map[string]string, why string) protocol.Change {
	return protocol.Change{Resource: r.ID, Kind: k, Op: op, IDs: ids, Reasons: []string{why}}
}

// deleteOp is a kind's explicit delete.
func deleteOp(calls ...protocol.Call) protocol.Operation { return explicitOp(OpDelete, calls...) }

// op is a supported operation with its calls, needing their IAM actions.
func op(o protocol.Op, calls ...protocol.Call) protocol.Operation {
	var actions []string
	for _, c := range calls {
		actions = append(actions, c.API+":"+c.Fixed["Action"]+strings.TrimPrefix(c.Fixed[target], targetOf(c.API, "")))
	}
	return protocol.Operation{Op: o, Support: protocol.Supported, Calls: calls, Needs: needs(actions...)}
}

// explicitOp is an explicit operation other than delete (a service's run-task).
func explicitOp(o protocol.Op, calls ...protocol.Call) protocol.Operation {
	x := op(o, calls...)
	x.Explicit = true
	return x
}

func unsupportedOp(o protocol.Op, why string) protocol.Operation {
	return protocol.Operation{Op: o, Support: protocol.Unsupported, Why: why}
}
