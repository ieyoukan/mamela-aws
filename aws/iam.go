package aws

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela-aws/catalogs"
	"github.com/ieyoukan/mamela/provider/kit"
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
// for it, as for everything. Adopting an imported role writes the marker (UpdateRole), and only
// a role under /mamela/ can be adopted: IAM cannot move a role to another path.

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
	updateRole        = queryCall(APIIAM, "UpdateRole", "RoleName", "Description")
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
	Boundary        string `json:"boundary" doc:"the boundary (the guard's): the role's marker names it"`
	Trust           string `json:"trust" values:"ecs-tasks|ec2" doc:"who assumes it: ECS tasks, or EC2 instances"`
	Policy          string `json:"policy" values:"ecs-task-execution|ecs-for-ec2" doc:"the AWS-managed ECS policy it has: AmazonECSTaskExecutionRolePolicy or AmazonEC2ContainerServiceforEC2Role"`
	InstanceProfile bool   `json:"instanceProfile,omitzero" doc:"an instance profile of its name (for EC2 instances)"`
}

type roleState struct {
	Arn             string   `json:"arn"`
	Trust           string   `json:"trust"`
	Policies        []string `json:"policies,omitempty"`
	InstanceProfile bool     `json:"instanceProfile,omitempty"` // a profile of its name holds it
	Description     string   `json:"description,omitempty"`     // of a role to adopt
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
		adoptOp(updateRole),
	}
}
func (roleKind) spec() any { return roleSpec{} }
func (roleKind) doc() string {
	return "An IAM role under /mamela/ for ECS: assumed by ECS tasks or EC2 instances, with one AWS-managed ECS policy (and an instance profile of its name for EC2). Its description is Mamela's marker; it carries no tags."
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

// trustOf names a trust policy (as IAM answers it: URL-encoded): one statement that allows one
// service (ECS tasks or EC2) sts:AssumeRole, with no condition. Its Sid and the way it is written
// (a string or a list of one, spacing) do not matter; anything more makes it no trust Mamela makes.
func trustOf(doc string) string {
	text, err := url.QueryUnescape(doc)
	if err != nil {
		return ""
	}
	var p struct {
		Version   string                      `json:"Version"`
		Statement []map[string]jsontext.Value `json:"Statement"`
	}
	if json.Unmarshal([]byte(text), &p) != nil || p.Version != "2012-10-17" || len(p.Statement) != 1 {
		return ""
	}
	st := p.Statement[0]
	one := func(v jsontext.Value) string {
		var s string
		if json.Unmarshal(v, &s) == nil {
			return s
		}
		var l []string
		if json.Unmarshal(v, &l) == nil && len(l) == 1 {
			return l[0]
		}
		return ""
	}
	var principal map[string]jsontext.Value
	json.Unmarshal(st["Principal"], &principal)
	for k := range st {
		if !slices.Contains([]string{"Sid", "Effect", "Principal", "Action"}, k) {
			return ""
		}
	}
	if one(st["Effect"]) != "Allow" || one(st["Action"]) != "sts:AssumeRole" || len(principal) != 1 {
		return ""
	}
	switch one(principal["Service"]) {
	case "ecs-tasks.amazonaws.com":
		return "ecs-tasks"
	case "ec2.amazonaws.com":
		return "ec2"
	}
	return ""
}

type iamRole struct {
	Arn         string `xml:"GetRoleResult>Role>Arn"`
	Path        string `xml:"GetRoleResult>Role>Path"`
	Trust       string `xml:"GetRoleResult>Role>AssumeRolePolicyDocument"`
	Description string `xml:"GetRoleResult>Role>Description"`
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
		o, err := readRole(c, name, true)
		if err != nil {
			return nil, err
		}
		if o != nil {
			out = append(out, *o)
		}
	}
	return out, nil
}

// readRole reads a role under /mamela/: with its marker (marked) or without (an imported role to
// adopt); nil when there is none such.
func readRole(c *client, name string, marked bool) (*object, error) {
	var role iamRole
	if err := c.query(APIIAM, "GetRole", map[string]string{"RoleName": name}, &role); err != nil {
		if notFound(err) {
			return nil, nil
		}
		return nil, err
	}
	boundary, mname, ok := markedBy(role.Description)
	if ok != marked || role.Path != catalogs.IAMPath {
		return nil, nil
	}
	var attached struct {
		Arns []string `xml:"ListAttachedRolePoliciesResult>AttachedPolicies>member>PolicyArn"`
	}
	if err := c.query(APIIAM, "ListAttachedRolePolicies", map[string]string{"RoleName": name}, &attached); err != nil {
		return nil, err
	}
	st := roleState{Arn: role.Arn, Trust: trustOf(role.Trust), Policies: slices.Sorted(slices.Values(attached.Arns))}
	if !marked {
		st.Description = role.Description
	}
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
	return &object{Name: mname, Boundary: boundary, IDs: ids, State: st}, nil
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
	if !o.Exists {
		name := awsName(s.Boundary, r.Name)
		if err := checkName(name); err != nil {
			return nil, err
		}
		ch := protocol.Change{Resource: r.ID, Kind: KindIAMRole, Op: OpCreate, IDs: map[string]string{"mamela-name": r.Name, "RoleName": name},
			Reasons: []string{fmt.Sprintf("role %s%s, assumed by %s, with %s", catalogs.IAMPath, name, s.Trust, s.Policy)},
			Requests: []protocol.Approved{
				queryReq(APIIAM, "CreateRole", map[string]string{"RoleName": name, "Path": catalogs.IAMPath,
					"AssumeRolePolicyDocument": trust, "Description": RoleMarker(s.Boundary, r.Name)}),
				queryReq(APIIAM, "AttachRolePolicy", map[string]string{"RoleName": name, "PolicyArn": policy}),
			}}
		if s.InstanceProfile {
			ch.Reasons = append(ch.Reasons, "and an instance profile of the same name")
			ch.Requests = append(ch.Requests,
				queryReq(APIIAM, "CreateInstanceProfile", map[string]string{"InstanceProfileName": name, "Path": catalogs.IAMPath}),
				queryReq(APIIAM, "AddRoleToInstanceProfile", map[string]string{"InstanceProfileName": name, "RoleName": name}))
		}
		return []protocol.Change{ch}, nil
	}
	// An existing role is the one observed (an adopted role keeps its own name).
	name := o.IDs["RoleName"]
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
			ch.Requests = append(ch.Requests, queryReq(APIIAM, "CreateInstanceProfile", map[string]string{"InstanceProfileName": name, "Path": catalogs.IAMPath}))
		}
		ch.Requests = append(ch.Requests, queryReq(APIIAM, "AddRoleToInstanceProfile", map[string]string{"InstanceProfileName": name, "RoleName": name}))
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

// --- import and adopt ---

func (roleKind) imports() []string {
	return []string{"aws_iam_role", "aws_iam_role_policy_attachment", "aws_iam_instance_profile"}
}

// tfRole are the attributes of a Terraform IAM role, attachment and instance profile.
type tfRole struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Path        string   `json:"path"`
	Arn         string   `json:"arn"`
	Trust       string   `json:"assume_role_policy"`
	Managed     []string `json:"managed_policy_arns"`
	Role        string   `json:"role"`
	PolicyArn   string   `json:"policy_arn"`
	Permissions string   `json:"permissions_boundary"`
	Inline      []struct {
		Name string `json:"name"`
	} `json:"inline_policy"`
}

func roleValues(o protocol.ImportObject) (tfRole, error) {
	var v tfRole
	if err := kit.Values(o, &v); err != nil {
		return v, fmt.Errorf("the attributes of %s: %w", o.Address, err)
	}
	if v.Name == "" {
		v.Name = v.ID
	}
	return v, nil
}

// importObject declares a role Mamela could have made: under /mamela/, trusted by ECS tasks or
// EC2 alone, with exactly one of the two AWS-managed ECS policies and no inline policy, and an
// instance profile of its own name (if any) under /mamela/. Its attachments and profile move with
// it. Any other role is left to Terraform, with why.
func (roleKind) importObject(o protocol.ImportObject, r kit.Resolver, boundary string) (kit.Imported, error) {
	v, err := roleValues(o)
	if err != nil {
		return kit.Imported{}, err
	}
	switch o.Type {
	case "aws_iam_role_policy_attachment":
		return kit.Imported{}, kit.Skip("an attachment moves with its role (aws_iam_role of the same state), which Mamela can adopt only as it is: under /mamela/, with one of the two AWS-managed ECS policies")
	case "aws_iam_instance_profile":
		return kit.Imported{}, kit.Skip("an instance profile moves with its role, when it has the role's name and is under /mamela/ (Mamela's role has such a profile)")
	}
	if v.Path != catalogs.IAMPath {
		return kit.Imported{}, kit.Skip(fmt.Sprintf("role %s is under %s: Mamela's IAM catalog lets it change roles under %s only, and IAM cannot move a role. "+
			"Declare a new AwsIamRole (Mamela makes it under %s), move what uses this role to it, and leave this one to Terraform", v.Name, v.Path, catalogs.IAMPath, catalogs.IAMPath))
	}
	trust := trustOf(url.QueryEscape(v.Trust))
	if trust == "" {
		return kit.Imported{}, kit.Skip("its trust policy is not one Mamela makes (ECS tasks, or EC2, assume it, with no condition): declare a new AwsIamRole")
	}
	inline := 0
	for _, p := range v.Inline {
		if p.Name != "" {
			inline++
		}
	}
	if inline > 0 {
		return kit.Imported{}, kit.Skip(fmt.Sprintf("it has %d inline policies, which Mamela's IAM catalog never writes: declare a new AwsIamRole", inline))
	}
	attached := map[string]bool{}
	for _, a := range v.Managed {
		attached[a] = true
	}
	var also []string
	for _, x := range r.Objects("aws_iam_role_policy_attachment") {
		av, err := roleValues(x)
		if err == nil && av.Role == v.Name {
			attached[av.PolicyArn] = true
			also = append(also, x.Address)
		}
	}
	policy := ""
	for arn := range attached {
		for name, p := range policies {
			if p == arn {
				policy = name
			}
		}
	}
	if len(attached) != 1 || policy == "" {
		return kit.Imported{}, kit.Skip(fmt.Sprintf("it has the policies %v; Mamela's role has exactly one of the two AWS-managed ECS policies (%s, %s): declare a new AwsIamRole",
			slicesOf(attached), catalogs.PolicyECSTaskExecution, catalogs.PolicyECSForEC2))
	}
	spec := roleSpec{Boundary: boundary, Trust: trust, Policy: policy}
	for _, x := range r.Objects("aws_iam_instance_profile") {
		pv, err := roleValues(x)
		if err != nil || pv.Role != v.Name {
			continue
		}
		if pv.Name != v.Name || pv.Path != catalogs.IAMPath {
			return kit.Imported{}, kit.Skip(fmt.Sprintf("its instance profile %s%s is not %s%s, the one Mamela's role has: declare a new AwsIamRole", pv.Path, pv.Name, catalogs.IAMPath, v.Name))
		}
		spec.InstanceProfile = true
		also = append(also, x.Address)
	}
	slices.Sort(also)
	return kit.Imported{Spec: spec, IDs: map[string]string{"RoleName": v.Name}, Also: also}, nil
}

func slicesOf(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func (roleKind) find(c *client, ids map[string]string) (*object, error) {
	if ids["RoleName"] == "" {
		return nil, nil
	}
	return readRole(c, ids["RoleName"], false)
}

// adopt writes Mamela's marker into the role's description (UpdateRole): its trust, policies and
// profile do not change. A role Mamela would never make (another trust, other policies) is not
// adopted.
func (roleKind) adopt(o protocol.Observed, desired protocol.Resource) (protocol.Change, error) {
	b, err := boundaryOf(desired)
	if err != nil {
		return protocol.Change{}, err
	}
	st := state[roleState](o)
	if st.Trust == "" {
		return protocol.Change{}, fmt.Errorf("%s: its trust policy is not one Mamela makes (ECS tasks, or EC2, assume it): it is not adopted", o.ID)
	}
	for _, p := range st.Policies {
		if p != catalogs.PolicyECSTaskExecution && p != catalogs.PolicyECSForEC2 {
			return protocol.Change{}, fmt.Errorf("%s: it has policy %s, which Mamela's IAM catalog never attaches: it is not adopted", o.ID, p)
		}
	}
	name := o.IDs["RoleName"]
	why := fmt.Sprintf("adopt role %s%s: its description becomes Mamela's marker %q", catalogs.IAMPath, name, RoleMarker(b, desired.Name))
	if st.Description != "" {
		why += fmt.Sprintf(" (it was %q)", st.Description)
	}
	return adoptChange(o, desired, map[string]string{"RoleName": name}, why+"; its trust, policies and profile do not change",
		queryReq(APIIAM, "UpdateRole", map[string]string{"RoleName": name, "Description": RoleMarker(b, desired.Name)}))
}
