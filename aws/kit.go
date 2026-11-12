package aws

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"maps"

	"github.com/ieyoukan/mamela/provider/kit"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// Provider is the provider, on Mamela's provider kit (ADR 0009 of Mamela): kit does the protocol,
// each kind of this package says how to list its objects, what a declaration changes, and whether a
// request is in effect.
var Provider = &kit.Provider{
	Name: Name, Version: Version,
	APIs: []string{APIEC2, APIECS, APILogs, APIIAM, APISSM},
	ObserveNeeds: needs("ec2:Describe*", "ecs:Describe*", "ecs:List*", "logs:DescribeLogGroups", "iam:GetRole", "iam:ListAttachedRolePolicies",
		"iam:GetInstanceProfile", "ssm:GetParameter"),
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
}

func kitKinds() []kit.Kind {
	var out []kit.Kind
	for _, k := range kinds {
		if im, ok := k.(importer); ok {
			out = append(out, importerAdapter{adapter{k}, im})
			continue
		}
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

// List: what this provider tagged; the IDs always name the declaration (mamela-name).
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
	ids := map[string]string{"mamela-name": x.Name}
	maps.Copy(ids, x.IDs)
	return kit.Object{Name: x.Name, Scope: x.Boundary, IDs: ids, Version: x.Version, State: x.State}
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

// Prepare: the explicit delete, a service's run-task, and adopting an imported object.
func (a adapter) Prepare(op protocol.Op, o protocol.Observed, desired *protocol.Resource) (protocol.Change, error) {
	switch op {
	case OpDelete:
		if !o.Owned {
			return protocol.Change{}, fmt.Errorf("%s: only what Mamela owns can be deleted", o.ID)
		}
		return a.k.remove(o)
	case OpRunTask:
		return runTaskChange(o)
	case OpAdopt:
		im, ok := a.k.(importer)
		if !ok || !o.Adoptable || desired == nil {
			return protocol.Change{}, fmt.Errorf("%s: only an imported object (mamela import terraform) can be adopted", o.ID)
		}
		return im.adopt(o, *desired)
	}
	return protocol.Change{}, fmt.Errorf("%s is not an explicit operation of %s", op, o.Kind)
}

// importerAdapter is a kind that imports from Terraform (importer).
type importerAdapter struct {
	adapter
	im importer
}

// List: as every kind's, and each object's ID is also the ResourceId.1 that tags it (CreateTags is
// bound to it in the catalog: an adopt names the object Mamela observed, and nothing else).
func (a importerAdapter) List(c *kit.Client) ([]kit.Object, error) {
	objs, err := a.adapter.List(c)
	for i := range objs {
		objs[i].IDs["ResourceId.1"] = objs[i].IDs[a.im.idKey()]
	}
	return objs, err
}

func (a importerAdapter) Imports() []string { return a.im.imports() }
func (a importerAdapter) Spec() any         { return a.im.spec() }
func (a importerAdapter) Doc() string       { return a.im.doc() }

func (a importerAdapter) Import(o protocol.ImportObject, r kit.Resolver) (kit.Imported, error) {
	var s GuardSettings
	if ok, err := r.Settings(&s); !ok || err != nil || s.Boundary == "" {
		return kit.Imported{}, kit.Skip("[provider.aws.guard] boundary is not set (mamela.providers.toml): the boundary the imported resources go into")
	}
	return a.im.importObject(o, r, s.Boundary)
}

func (a importerAdapter) Find(c *kit.Client, ids map[string]string) (*kit.Object, error) {
	x, err := a.im.find(clientOf(c), ids)
	if err != nil || x == nil {
		return nil, err
	}
	o := kitObject(*x)
	delete(o.IDs, "mamela-name") // not Mamela's yet: no name of a declaration
	o.IDs["ResourceId.1"] = o.IDs[a.im.idKey()]
	return &o, nil
}
