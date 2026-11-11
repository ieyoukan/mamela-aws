package aws

import (
	"bytes"
	"encoding/json/v2"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/provider/catalogs"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// An IAM role for ECS (ADR 0007 of Mamela): under the path /mamela/, assumed by ECS tasks (to pull
// images and write logs) or by EC2 instances (to run the ECS agent), with one AWS-managed ECS
// policy, and for EC2 an instance profile of the same name. Mamela's IAM catalog allows nothing
// else, whatever this provider sends.
//
// Roles carry no tags: tagging takes iam:TagRole, which an operator's role may well lack (and
// does not need for anything else). The role's description marks it instead
// ("Mamela: mamela:boundary=locust mamela:name=web"); Mamela's record of what it created vouches
// for it, as for everything.

// KindIAMRole is the kind; OpPolicy attaches a missing policy or instance profile.
const (
	KindIAMRole protocol.ResourceKind = "AwsIamRole"
	OpPolicy    protocol.Op           = "policy"
)

// Trusts and policies a declaration names.
var (
	trusts   = map[string]string{"ecs-tasks": catalogs.TrustECSTasks, "ec2": catalogs.TrustEC2}
	policies = map[string]string{"ecs-task-execution": catalogs.PolicyECSTaskExecution, "ecs-for-ec2": catalogs.PolicyECSForEC2}
)

var (
	listRoles         = queryCall(APIIAM, "ListRoles", "PathPrefix", "Marker")
	getRole           = queryCall(APIIAM, "GetRole", "RoleName")
	listAttached      = queryCall(APIIAM, "ListAttachedRolePolicies", "RoleName")
	getProfile        = queryCall(APIIAM, "GetInstanceProfile", "InstanceProfileName")
	createRole        = queryCall(APIIAM, "CreateRole", "RoleName", "Path", "AssumeRolePolicyDocument", "Description")
	attachPolicy      = queryCall(APIIAM, "AttachRolePolicy", "RoleName", "PolicyArn")
	detachPolicy      = queryCall(APIIAM, "DetachRolePolicy", "RoleName", "PolicyArn")
	createProfile     = queryCall(APIIAM, "CreateInstanceProfile", "InstanceProfileName", "Path")
	addRoleToProfile  = queryCall(APIIAM, "AddRoleToInstanceProfile", "InstanceProfileName", "RoleName")
	removeRoleProfile = queryCall(APIIAM, "RemoveRoleFromInstanceProfile", "InstanceProfileName", "RoleName")
	deleteProfile     = queryCall(APIIAM, "DeleteInstanceProfile", "InstanceProfileName")
	deleteRole        = queryCall(APIIAM, "DeleteRole", "RoleName")
)

type roleKind struct{}

type roleSpec struct {
	Boundary        string `json:"boundary"`
	Trust           string `json:"trust"`  // ecs-tasks or ec2
	Policy          string `json:"policy"` // ecs-task-execution or ecs-for-ec2
	InstanceProfile bool   `json:"instanceProfile,omitempty"`
}

type roleState struct {
	Arn             string   `json:"arn"`
	Trust           string   `json:"trust"`
	Policies        []string `json:"policies,omitempty"`
	InstanceProfile bool     `json:"instanceProfile,omitempty"` // a profile of its name holds it
}

func (roleKind) kind() protocol.ResourceKind { return KindIAMRole }
func (roleKind) reads() []protocol.Call {
	return []protocol.Call{listRoles, getRole, listAttached, getProfile}
}
func (roleKind) operations() []protocol.Operation {
	return []protocol.Operation{
		op(OpCreate, createRole, attachPolicy, createProfile, addRoleToProfile),
		op(OpPolicy, attachPolicy, createProfile, addRoleToProfile),
		unsupportedOp(OpReplace, "a role keeps who may assume it, and Mamela never detaches a policy: declare a new role"),
		deleteOp(removeRoleProfile, deleteProfile, detachPolicy, deleteRole),
	}
}

// RoleMarker is the description that marks a role this provider made.
func RoleMarker(boundary, name string) string {
	return "Mamela: " + TagBoundary + "=" + boundary + " " + TagName + "=" + name
}

var roleMarker = regexp.MustCompile(`^Mamela: mamela:boundary=(\S+) mamela:name=(\S+)$`)

// markedBy reads a role's marker: its boundary and name.
func markedBy(description string) (string, string, bool) {
	m := roleMarker.FindStringSubmatch(description)
	if m == nil {
		return "", "", false
	}
	return m[1], m[2], true
}

// trustOf names a trust policy as IAM answers it (URL-encoded, maybe spaced differently).
func trustOf(doc string) string {
	text, err := url.QueryUnescape(doc)
	if err != nil {
		return ""
	}
	canon := func(s string) []byte {
		var v any
		if json.Unmarshal([]byte(s), &v) != nil {
			return nil
		}
		b, _ := json.Marshal(v, json.Deterministic(true))
		return b
	}
	got := canon(text)
	for _, name := range []string{"ec2", "ecs-tasks"} {
		if got != nil && bytes.Equal(got, canon(trusts[name])) {
			return name
		}
	}
	return ""
}

func (roleKind) list(c *client) ([]object, error) {
	var names []string
	marker := ""
	for {
		var resp struct {
			Names     []string `xml:"ListRolesResult>Roles>member>RoleName"`
			Truncated bool     `xml:"ListRolesResult>IsTruncated"`
			Marker    string   `xml:"ListRolesResult>Marker"`
		}
		params := map[string]string{"PathPrefix": catalogs.IAMPath}
		if marker != "" {
			params["Marker"] = marker
		}
		if err := c.query(APIIAM, "ListRoles", params, &resp); err != nil {
			return nil, err
		}
		names = append(names, resp.Names...)
		if !resp.Truncated || resp.Marker == "" {
			break
		}
		marker = resp.Marker
	}
	slices.Sort(names)
	var out []object
	for _, name := range names {
		var role struct {
			Arn         string `xml:"GetRoleResult>Role>Arn"`
			Trust       string `xml:"GetRoleResult>Role>AssumeRolePolicyDocument"`
			Description string `xml:"GetRoleResult>Role>Description"`
		}
		if err := c.query(APIIAM, "GetRole", map[string]string{"RoleName": name}, &role); err != nil {
			if notFound(err) {
				continue
			}
			return nil, err
		}
		boundary, mname, ok := markedBy(role.Description)
		if !ok {
			continue
		}
		var attached struct {
			Arns []string `xml:"ListAttachedRolePoliciesResult>AttachedPolicies>member>PolicyArn"`
		}
		if err := c.query(APIIAM, "ListAttachedRolePolicies", map[string]string{"RoleName": name}, &attached); err != nil {
			return nil, err
		}
		st := roleState{Arn: role.Arn, Trust: trustOf(role.Trust), Policies: slices.Sorted(slices.Values(attached.Arns))}
		ids := map[string]string{"RoleName": name}
		var profile struct {
			Roles []string `xml:"GetInstanceProfileResult>InstanceProfile>Roles>member>RoleName"`
		}
		switch err := c.query(APIIAM, "GetInstanceProfile", map[string]string{"InstanceProfileName": name}, &profile); {
		case err == nil:
			st.InstanceProfile = slices.Contains(profile.Roles, name)
			ids["InstanceProfileName"] = name
		case !notFound(err):
			return nil, err
		}
		out = append(out, object{Name: mname, Boundary: boundary, IDs: ids, State: st})
	}
	return out, nil
}

func (roleKind) missing(*client, protocol.Resource) (any, error) { return nil, nil }

// checkName: an AWS name (a role's, a cluster's) is up to 64 letters, digits and +=,.@_-.
func checkName(n string) error {
	if n == "" || len(n) > 64 || strings.Trim(n, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+=,.@_-") != "" {
		return fmt.Errorf("%q is not an AWS name (up to 64 letters, digits and +=,.@_-)", n)
	}
	return nil
}

func (roleKind) diff(r protocol.Resource, o protocol.Observed, _ map[string]protocol.Observed) ([]protocol.Change, error) {
	s, err := spec[roleSpec](r)
	if err != nil {
		return nil, err
	}
	trust, ok := trusts[s.Trust]
	policy, ok2 := policies[s.Policy]
	if !ok || !ok2 {
		return nil, fmt.Errorf("trust %q (ecs-tasks or ec2) or policy %q (ecs-task-execution or ecs-for-ec2) is not one this provider makes", s.Trust, s.Policy)
	}
	name := awsName(s.Boundary, r.Name)
	if err := checkName(name); err != nil {
		return nil, err
	}
	createProfile := queryReq(APIIAM, "CreateInstanceProfile", map[string]string{"InstanceProfileName": name, "Path": catalogs.IAMPath})
	addRole := queryReq(APIIAM, "AddRoleToInstanceProfile", map[string]string{"InstanceProfileName": name, "RoleName": name})
	if !o.Exists {
		ch := protocol.Change{Resource: r.ID, Kind: KindIAMRole, Op: OpCreate, IDs: map[string]string{"mamela-name": r.Name, "RoleName": name},
			Reasons: []string{fmt.Sprintf("role %s%s, assumed by %s, with %s", catalogs.IAMPath, name, s.Trust, s.Policy)},
			Requests: []protocol.Approved{
				queryReq(APIIAM, "CreateRole", map[string]string{"RoleName": name, "Path": catalogs.IAMPath,
					"AssumeRolePolicyDocument": trust, "Description": RoleMarker(s.Boundary, r.Name)}),
				queryReq(APIIAM, "AttachRolePolicy", map[string]string{"RoleName": name, "PolicyArn": policy}),
			}}
		if s.InstanceProfile {
			ch.Reasons = append(ch.Reasons, "and an instance profile of the same name")
			ch.Requests = append(ch.Requests, createProfile, addRole)
		}
		return []protocol.Change{ch}, nil
	}
	st := state[roleState](o)
	if st.Trust != s.Trust {
		return []protocol.Change{unsupported(r, KindIAMRole, OpReplace, map[string]string{"RoleName": name},
			fmt.Sprintf("assumed by %q, declared %s", st.Trust, s.Trust))}, nil
	}
	ch := protocol.Change{Resource: r.ID, Kind: KindIAMRole, Op: OpPolicy, IDs: map[string]string{"RoleName": name}}
	if !slices.Contains(st.Policies, policy) {
		ch.Reasons = append(ch.Reasons, "attach "+s.Policy)
		ch.Requests = append(ch.Requests, queryReq(APIIAM, "AttachRolePolicy", map[string]string{"RoleName": name, "PolicyArn": policy}))
	}
	if s.InstanceProfile && !st.InstanceProfile {
		ch.Reasons = append(ch.Reasons, "an instance profile of the same name")
		if o.IDs["InstanceProfileName"] == "" {
			ch.Requests = append(ch.Requests, createProfile)
		}
		ch.Requests = append(ch.Requests, addRole)
	}
	if len(ch.Requests) == 0 {
		return nil, nil
	}
	return []protocol.Change{ch}, nil
}

func (roleKind) remove(o protocol.Observed) (protocol.Change, error) {
	name := o.IDs["RoleName"]
	st := state[roleState](o)
	ids := map[string]string{"RoleName": name}
	ch := protocol.Change{Resource: o.ID, Kind: KindIAMRole, Op: OpDelete, Reasons: []string{"delete role " + catalogs.IAMPath + name}}
	if p := o.IDs["InstanceProfileName"]; p != "" {
		ids["InstanceProfileName"] = p
		ch.Reasons = append(ch.Reasons, "and its instance profile (no instance may be using it)")
		if st.InstanceProfile {
			ch.Requests = append(ch.Requests, queryReq(APIIAM, "RemoveRoleFromInstanceProfile", map[string]string{"InstanceProfileName": p, "RoleName": name}))
		}
		ch.Requests = append(ch.Requests, queryReq(APIIAM, "DeleteInstanceProfile", map[string]string{"InstanceProfileName": p}))
	}
	for _, arn := range st.Policies {
		ch.Requests = append(ch.Requests, queryReq(APIIAM, "DetachRolePolicy", map[string]string{"RoleName": name, "PolicyArn": arn}))
	}
	ch.IDs = ids
	ch.Requests = append(ch.Requests, queryReq(APIIAM, "DeleteRole", map[string]string{"RoleName": name}))
	return ch, nil
}

func (k roleKind) done(c *client, _ protocol.Change, a protocol.Approved) (bool, error) {
	objs, err := k.list(c)
	if err != nil {
		return false, err
	}
	name := param(a, "RoleName")
	if name == "" {
		name = param(a, "InstanceProfileName") // a profile has its role's name
	}
	var role object
	i := slices.IndexFunc(objs, func(x object) bool { return x.IDs["RoleName"] == name })
	if i >= 0 {
		role = objs[i]
	}
	st, _ := role.State.(roleState)
	exists := i >= 0
	switch action(a) {
	case "CreateRole":
		return exists, nil
	case "AttachRolePolicy":
		return exists && slices.Contains(st.Policies, param(a, "PolicyArn")), nil
	case "CreateInstanceProfile":
		return exists && role.IDs["InstanceProfileName"] != "", nil
	case "AddRoleToInstanceProfile":
		return exists && st.InstanceProfile, nil
	case "RemoveRoleFromInstanceProfile":
		return !exists || !st.InstanceProfile, nil
	case "DeleteInstanceProfile":
		return !exists || role.IDs["InstanceProfileName"] == "", nil
	case "DetachRolePolicy":
		return !exists || !slices.Contains(st.Policies, param(a, "PolicyArn")), nil
	case "DeleteRole":
		return !exists, nil
	}
	return false, nil
}
