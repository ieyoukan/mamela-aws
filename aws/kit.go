package aws

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/ieyoukan/mamela/provider/kit"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// Provider is the provider, on Mamela's provider kit (ADR 0009 of Mamela): kit does the protocol,
// each kind of this package says how to list its objects, what a declaration changes, and whether a
// request is in effect.
var Provider = &kit.Provider{
	Name: Name, Version: Version,
	APIs: []string{APIEC2, APIECS, APILogs, APIIAM, APISSM},
	ObserveNeeds: needs("ec2:Describe*", "ecs:Describe*", "ecs:List*", "logs:DescribeLogGroups", "logs:ListTagsForResource", "iam:GetRole",
		"iam:ListRoles", "iam:ListAttachedRolePolicies", "iam:GetInstanceProfile", "ssm:GetParameter"),
	Kinds: kitKinds(),
	GuardFunc: func(c *kit.Client, in protocol.GuardParams) (protocol.GuardResult, error) {
		return guard(clientOf(c), in)
	},
	ParseError: func(status int, _, body string) error {
		return parseError(status, []byte(body))
	},
	AlreadyDone: func(err error) bool {
		var ae *awsError
		return errors.As(err, &ae) && alreadyDone(ae.Code)
	},
	AfterSend: afterSend,
}

func kitKinds() []kit.Kind {
	var out []kit.Kind
	for _, k := range kinds {
		out = append(out, adapter{k})
	}
	return out
}

func clientOf(c *kit.Client) *client { return &client{ctx: c.Context(), h: c.Host()} }

// adapter is a kind of this package as a kit.Kind.
type adapter struct{ k kind }

func (a adapter) Kind() protocol.ResourceKind      { return a.k.kind() }
func (a adapter) Reads() []protocol.Call           { return a.k.reads() }
func (a adapter) Operations() []protocol.Operation { return a.k.operations() }
func (a adapter) Imports() []string                { return a.k.imports() }
func (a adapter) Spec() any                        { return a.k.spec() }
func (a adapter) Doc() string                      { return a.k.doc() }

// List: what this provider marked; the IDs of Mamela's objects always name the declaration
// (mamela-name).
func (a adapter) List(c *kit.Client) ([]kit.Object, error) {
	objs, err := a.k.list(clientOf(c))
	if err != nil {
		return nil, err
	}
	out := make([]kit.Object, 0, len(objs))
	for _, x := range objs {
		out = append(out, kitObject(x))
	}
	return out, nil
}

func kitObject(x object) kit.Object {
	ids := maps.Clone(x.IDs)
	if !x.Foreign && x.Name != "" {
		ids["mamela-name"] = x.Name
	}
	return kit.Object{Name: x.Name, Scope: x.Boundary, IDs: ids, Version: x.Version, State: x.State, Foreign: x.Foreign}
}

// Scope: a declaration is told apart by its boundary as well as its name.
func (a adapter) Scope(r protocol.Resource) (string, error) {
	var s base
	if err := json.Unmarshal(r.Spec, &s); err != nil || s.Boundary == "" {
		return "", fmt.Errorf("the spec has no boundary")
	}
	return s.Boundary, nil
}

func (a adapter) Missing(c *kit.Client, r protocol.Resource) (any, error) {
	return a.k.missing(clientOf(c), r)
}

func (a adapter) Diff(d kit.Desired) ([]protocol.Change, error) {
	cs, err := a.k.diff(d.Resource, d.Observed, d.Dependencies)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", d.Resource.ID, err)
	}
	return cs, nil
}

func (a adapter) Done(c *kit.Client, ch protocol.Change, ap protocol.Approved) (bool, error) {
	if ch.Op == OpAdopt {
		return adopted(clientOf(c), a.k, ch, ap)
	}
	return a.k.done(clientOf(c), ch, ap)
}

// Lookup: a reboot leaves no trace to tell whether it was done.
func (a adapter) Lookup(_ *kit.Client, ch protocol.Change) (protocol.LookupStatus, bool, error) {
	if ch.Op == OpReboot {
		return protocol.LookupUnknown, true, nil
	}
	return "", false, nil
}

// Prepare: the explicit delete and adopt of every kind, and the kinds' own (a service's run-task,
// an instance's resize).
func (a adapter) Prepare(op protocol.Op, o protocol.Observed, desired *protocol.Resource) (protocol.Change, error) {
	switch op {
	case OpDelete:
		if !o.Owned {
			return protocol.Change{}, fmt.Errorf("%s: only what Mamela owns can be deleted", o.ID)
		}
		return a.k.remove(o)
	case OpAdopt:
		switch {
		case !o.Adoptable || desired == nil:
			return protocol.Change{}, fmt.Errorf("%s: only an imported object (mamela import terraform) can be adopted", o.ID)
		case o.IDs["mamela-name"] != "":
			return protocol.Change{}, fmt.Errorf("%s: the object carries Mamela's marks already (mamela:name=%s, of a boundary): it is not adopted again", o.ID, o.IDs["mamela-name"])
		}
		return a.k.adopt(o, *desired)
	}
	if x, ok := a.k.(explicitKind); ok {
		if !o.Owned {
			return protocol.Change{}, fmt.Errorf("%s: %s acts only on what Mamela owns", o.ID, op)
		}
		return x.prepare(op, o, desired)
	}
	return protocol.Change{}, fmt.Errorf("%s is not an explicit operation of %s", op, o.Kind)
}

// Find: an imported object, while it carries none of Mamela's marks.
func (a adapter) Find(c *kit.Client, ids map[string]string) (*kit.Object, error) {
	x, err := a.k.find(clientOf(c), ids)
	if err != nil || x == nil || x.Name != "" {
		return nil, err // an object with Mamela's marks is Mamela's: listed, never adopted
	}
	o := kitObject(*x)
	o.Foreign = false
	return &o, nil
}

func (a adapter) Import(o protocol.ImportObject, r kit.Resolver) (kit.Imported, error) {
	var s GuardSettings
	if ok, err := r.Settings(&s); !ok || err != nil || s.Boundary == "" {
		return kit.Imported{}, kit.Skip("[provider.aws.guard] boundary is not set (mamela.providers.toml): the boundary the imported resources go into")
	}
	return a.k.importObject(o, r, s.Boundary)
}

// adopted says whether an adopt's marks are on the object: it is listed under the declaration's
// name, with the ID the adopt's request names.
func adopted(c *client, k kind, ch protocol.Change, a protocol.Approved) (bool, error) {
	objs, err := k.list(c)
	if err != nil {
		return false, err
	}
	var id string
	for _, p := range []string{"ResourceId.1", "resourceArn", "RoleName"} {
		if v := param(a, p); v != "" {
			id = v
		}
	}
	name := nameOf(ch)
	return id != "" && slices.ContainsFunc(objs, func(x object) bool {
		return !x.Foreign && x.Name == name && slices.Contains(slices.Collect(maps.Values(x.IDs)), id)
	}), nil
}

// adoptChange is an adopt: the request that marks the object, with the IDs that name it.
func adoptChange(o protocol.Observed, desired protocol.Resource, ids map[string]string, why string, a protocol.Approved) (protocol.Change, error) {
	for k, v := range ids {
		if v == "" || o.IDs[k] != v {
			return protocol.Change{}, fmt.Errorf("%s: no %s observed", o.ID, k)
		}
	}
	return protocol.Change{Resource: o.ID, Kind: o.Kind, Op: OpAdopt, IDs: ids, Reasons: []string{why}, Requests: []protocol.Approved{a}}, nil
}

// boundaryOf is a declaration's boundary.
func boundaryOf(r protocol.Resource) (string, error) {
	var b base
	if err := json.Unmarshal(r.Spec, &b); err != nil || b.Boundary == "" {
		return "", fmt.Errorf("%s: the spec has no boundary", r.ID)
	}
	return b.Boundary, nil
}

// afterSend waits for what a request starts and the next one needs: a stopping instance to stop
// (a resize changes the type of a stopped instance only).
func afterSend(c *kit.Client, ch protocol.Change, a protocol.Approved, _ []byte) error {
	if action(a) != "StopInstances" {
		return nil
	}
	id := param(a, "InstanceId.1")
	for range stopPolls {
		got, err := instances(clientOf(c), map[string]string{"Filter.1.Name": "instance-id", "Filter.1.Value.1": id})
		if err != nil {
			return err
		}
		if len(got) != 1 {
			return fmt.Errorf("instance %s: %d instances answer to its ID", id, len(got))
		}
		if got[0].State == "stopped" {
			return nil
		}
		time.Sleep(stopWait)
	}
	return fmt.Errorf("instance %s did not stop within %s; run the operation again once it has stopped", id, time.Duration(stopPolls)*stopWait)
}

// How long afterSend waits for an instance to stop (within the host's 5 minutes for an apply).
const (
	stopPolls = 48
	stopWait  = 5 * time.Second
)
