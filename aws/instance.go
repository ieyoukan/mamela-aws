package aws

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/ieyoukan/mamela/provider/kit"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// An EC2 instance of the ECS-optimized Amazon Linux 2023 image (its ID read from AWS's public SSM
// parameter), in a subnet and a security group of the boundary, with a public IP address (to reach
// ECS and pull images without a NAT gateway). It does one of two things, which its user data, written
// by this provider, sets up when it first boots:
//
//   - it joins an ECS cluster as a container instance, with the instance profile of a role (trust
//     ec2, policy ecs-for-ec2): the operator's AWS role must be able to create instance profiles
//     and pass the role to EC2;
//   - or it runs one container image with Docker, publishing one port, with no IAM role at all.
//
// The declaration never writes the script: it names a cluster, or an image and a port, which the
// provider checks before writing them into it.
//
// An instance a person adopts (one Terraform launched) does neither: it keeps its image and user
// data, which Mamela never compares, and the instance profile of its role, if any. Mamela keeps its
// type (resize, an explicit operation that stops and starts it), its security group and its
// running.

// KindInstance is the kind and its operations.
const (
	KindInstance protocol.ResourceKind = "AwsInstance"
	OpStart      protocol.Op           = "start"  // a stopped instance starts
	OpGroups     protocol.Op           = "groups" // its security group is the declared one
	OpRetype     protocol.Op           = "retype" // never carried out by a plan: resize
	OpResize     protocol.Op           = "resize" // explicit: stop, change the type, start
	OpReboot     protocol.Op           = "reboot" // explicit
	// TagSetup is the digest of the user data an instance was launched with.
	TagSetup = "mamela:setup"
)

var (
	describeInstances = queryCall(APIEC2, "DescribeInstances", "Filter.1.Name", "Filter.1.Value.1", "Filter.2.Name", "Filter.2.Value.1",
		"Filter.2.Value.2", "Filter.2.Value.3", "Filter.2.Value.4", "MaxResults", "NextToken")
	getParameter = jsonCall(APISSM, "GetParameter", "Name")
	// One instance a request: MinCount and MaxCount are fixed, as the catalog fixes them.
	runInstances = fix(queryCall(APIEC2, "RunInstances", append([]string{"MinCount", "MaxCount", "ImageId", "InstanceType", "ClientToken", "UserData",
		"MetadataOptions.HttpTokens", "IamInstanceProfile.Name", "NetworkInterface.1.DeviceIndex", "NetworkInterface.1.SubnetId",
		"NetworkInterface.1.AssociatePublicIpAddress", "NetworkInterface.1.SecurityGroupId.1", "TagSpecification.1.Tag.4.Key",
		"TagSpecification.1.Tag.4.Value"}, ec2TagParams...)...), "MinCount", "1", "MaxCount", "1")
	startInstances     = queryCall(APIEC2, "StartInstances", "InstanceId.1")
	stopInstances      = queryCall(APIEC2, "StopInstances", "InstanceId.1")
	rebootInstances    = queryCall(APIEC2, "RebootInstances", "InstanceId.1")
	modifyInstance     = queryCall(APIEC2, "ModifyInstanceAttribute", "InstanceId", "InstanceType.Value", "GroupId.1")
	terminateInstances = queryCall(APIEC2, "TerminateInstances", "InstanceId.1")
)

type instanceKind struct{}

// HostContainer is the one container an instance without a cluster runs: Port on the instance to
// the same port of the container.
type HostContainer struct {
	Image string `json:"image" doc:"the container image (registry/path:tag)"`
	Port  int64  `json:"port" doc:"its TCP port, published on the instance"`
}

type instanceSpec struct {
	Boundary      string         `json:"boundary" doc:"the boundary (the guard's): it tags the instance"`
	Subnet        string         `json:"subnet" mamela:"ref=AwsSubnet"`
	SecurityGroup string         `json:"securityGroup" mamela:"ref=AwsSecurityGroup"`
	InstanceType  string         `json:"instanceType" doc:"its type (the guard's instance_types; resize changes it)"`
	Cluster       string         `json:"cluster,omitempty" mamela:"ref=AwsEcsCluster" doc:"with role: a container instance of this cluster"`
	Role          string         `json:"role,omitempty" mamela:"ref=AwsIamRole" doc:"the role (trust ec2) whose instance profile it has"`
	Container     *HostContainer `json:"container,omitempty" doc:"or: the container it runs, with no role"`
}

type instanceState struct {
	State        string   `json:"state,omitempty"` // pending, running, stopping or stopped
	InstanceType string   `json:"instanceType,omitempty"`
	ImageID      string   `json:"imageId"`
	PrivateIP    string   `json:"privateIp,omitempty"`
	Setup        string   `json:"setup,omitempty"` // the digest of its user data
	Subnet       string   `json:"subnet,omitempty"`
	Groups       []string `json:"groups,omitempty"`
	Profile      string   `json:"profile,omitempty"` // its instance profile's name
	// Generation (of one that does not exist) counts the instances launched for the declaration
	// that AWS still knows (terminated): their client tokens are spent.
	Generation int `json:"generation,omitempty"`
}

// ImageParameter is the public SSM parameter of the ECS-optimized image for an instance type.
func ImageParameter(instanceType string) string {
	family, _, _ := strings.Cut(instanceType, ".")
	// Graviton families have a g after their generation (t4g, m7g, c7gn).
	if i := strings.IndexAny(family, "0123456789"); i >= 0 && strings.Contains(family[i:], "g") {
		return "/aws/service/ecs/optimized-ami/amazon-linux-2023/arm64/recommended/image_id"
	}
	return "/aws/service/ecs/optimized-ami/amazon-linux-2023/recommended/image_id"
}

// clientToken is what an instance of a declaration is launched with: the same for the same
// declaration, image and generation, so that a retried RunInstances never launches a second.
func clientToken(r protocol.Resource, image string, generation int) string {
	sum := sha256.Sum256([]byte(r.ID + "\x00" + string(r.Spec) + "\x00" + image + "\x00" + strconv.Itoa(generation)))
	return "mamela-" + hex.EncodeToString(sum[:12])
}

// imageName is a container image reference that is safe in a script: registry, path, tag and
// digest characters only.
var imageName = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]*(:[A-Za-z0-9._-]+)?(@sha256:[a-f0-9]{64})?$`)

// adopted says whether a declaration is of an instance Mamela did not set up (one a person
// adopted): it neither joins a cluster nor runs a container.
func (s instanceSpec) adopted() bool { return s.Cluster == "" && s.Container == nil }

// UserData is the script an instance runs when it first boots: it joins the cluster, or runs the
// container (the ECS agent of the image is then turned off).
func UserData(s instanceSpec, cluster string) (string, error) {
	var script string
	switch {
	case s.Container == nil && s.Cluster != "" && s.Role != "":
		script = "#!/bin/bash\necho ECS_CLUSTER=" + cluster + " >> /etc/ecs/ecs.config\n"
	case s.Container != nil && s.Cluster == "" && s.Role == "":
		c := s.Container
		if len(c.Image) > 255 || !imageName.MatchString(c.Image) {
			return "", fmt.Errorf("image %q is not a container image reference (registry/path:tag)", c.Image)
		}
		if c.Port < 1 || c.Port > 65535 {
			return "", fmt.Errorf("port %d is not a TCP port", c.Port)
		}
		p := strconv.FormatInt(c.Port, 10)
		script = "#!/bin/bash\nset -eu\nsystemctl disable --now ecs || true\nsystemctl enable --now docker\n" +
			"docker run -d --restart always --name mamela -p " + p + ":" + p + " " + c.Image + "\n"
	default:
		return "", fmt.Errorf("an instance Mamela launches joins a cluster (cluster and role) or runs a container (container), one of the two; " +
			"one with neither is an instance a person adopted (mamela import terraform)")
	}
	return base64.StdEncoding.EncodeToString([]byte(script)), nil
}

func setupDigest(userData string) string {
	sum := sha256.Sum256([]byte(userData))
	return hex.EncodeToString(sum[:8])
}

type ec2Instance struct {
	ID          string   `xml:"instanceId"`
	Type        string   `xml:"instanceType"`
	Image       string   `xml:"imageId"`
	PrivateIP   string   `xml:"privateIpAddress"`
	ClientToken string   `xml:"clientToken"`
	State       string   `xml:"instanceState>name"`
	Subnet      string   `xml:"subnetId"`
	Groups      []string `xml:"groupSet>item>groupId"`
	Profile     string   `xml:"iamInstanceProfile>arn"`
	Tags        []xmlTag `xml:"tagSet>item"`
}

// instances lists EC2 instances (paging) with the given parameters.
func instances(c *client, params map[string]string) ([]ec2Instance, error) {
	var out []ec2Instance
	next := ""
	for {
		p := map[string]string{"MaxResults": "1000"}
		for k, v := range params {
			p[k] = v
		}
		if next != "" {
			p["NextToken"] = next
		}
		var resp struct {
			Items []ec2Instance `xml:"reservationSet>item>instancesSet>item"`
			Next  string        `xml:"nextToken"`
		}
		if err := c.query(APIEC2, "DescribeInstances", p, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Items...)
		if resp.Next == "" {
			return out, nil
		}
		next = resp.Next
	}
}

func instanceObject(i ec2Instance) object {
	ids := map[string]string{"InstanceId.1": i.ID, "InstanceId": i.ID, "ResourceId.1": i.ID}
	if i.ClientToken != "" {
		ids["ClientToken"] = i.ClientToken
	}
	groups := slices.Sorted(slices.Values(i.Groups))
	profile := ""
	if i.Profile != "" {
		profile = lastPart(i.Profile)
	}
	return object{Name: tagOf(i.Tags, TagName), Boundary: tagOf(i.Tags, TagBoundary), IDs: ids,
		State: instanceState{State: i.State, InstanceType: i.Type, ImageID: i.Image, PrivateIP: i.PrivateIP, Setup: tagOf(i.Tags, TagSetup),
			Subnet: i.Subnet, Groups: groups, Profile: profile}}
}

func (instanceKind) kind() protocol.ResourceKind { return KindInstance }
func (instanceKind) reads() []protocol.Call      { return []protocol.Call{describeInstances, getParameter} }
func (instanceKind) operations() []protocol.Operation {
	return []protocol.Operation{op(OpCreate, runInstances), op(OpStart, startInstances), op(OpGroups, modifyInstance),
		unsupportedOp(OpRetype, "changing the type stops the instance: a person resizes it with the explicit operation resize (mamela act --confirm <name> resize <resource>)"),
		unsupportedOp(OpReplace, "an instance keeps its subnet, its role and what it was set up to do: declare a new one (and delete this one with mamela act)"),
		explicitOp(OpResize, stopInstances, modifyInstance, startInstances), explicitOp(OpReboot, rebootInstances),
		deleteOp(terminateInstances), adoptOp(createTags)}
}
func (instanceKind) spec() any { return instanceSpec{} }
func (instanceKind) doc() string {
	return "An EC2 instance of the ECS-optimized Amazon Linux 2023 that joins a cluster (with a role's instance profile) or runs one container; " +
		"or, adopted, one that keeps its own image and setup. The explicit operations resize and reboot stop or reboot it."
}

func (instanceKind) list(c *client) ([]object, error) {
	got, err := instances(c, map[string]string{"Filter.1.Name": "tag-key", "Filter.1.Value.1": TagName, "Filter.2.Name": "instance-state-name",
		"Filter.2.Value.1": "pending", "Filter.2.Value.2": "running", "Filter.2.Value.3": "stopping", "Filter.2.Value.4": "stopped"})
	if err != nil {
		return nil, err
	}
	var out []object
	for _, i := range got {
		out = append(out, instanceObject(i))
	}
	return out, nil
}

func (instanceKind) missing(c *client, r protocol.Resource) (any, error) {
	s, err := spec[instanceSpec](r)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Parameter struct {
			Value string `json:"Value"`
		} `json:"Parameter"`
	}
	if err := c.call(APISSM, "GetParameter", map[string]any{"Name": ImageParameter(s.InstanceType)}, &resp); err != nil {
		return nil, fmt.Errorf("the ECS-optimized image: %w", err)
	}
	st := instanceState{ImageID: resp.Parameter.Value}
	for st.Generation = 0; st.Generation < 8; st.Generation++ {
		spent, err := instances(c, map[string]string{"Filter.1.Name": "client-token", "Filter.1.Value.1": clientToken(r, st.ImageID, st.Generation)})
		if err != nil {
			return nil, err
		}
		if len(spent) == 0 {
			return st, nil
		}
	}
	return nil, fmt.Errorf("%d instances launched for this declaration were terminated recently; wait an hour", st.Generation)
}

func (instanceKind) diff(r protocol.Resource, o protocol.Observed, deps map[string]protocol.Observed) ([]protocol.Change, error) {
	s, err := spec[instanceSpec](r)
	if err != nil {
		return nil, err
	}
	cluster, profile, purpose := "", "", ""
	if s.Role != "" {
		role, err := dep(deps, KindIAMRole, s.Role)
		if err != nil {
			return nil, err
		}
		profile = role.IDs["InstanceProfileName"]
		if rs := state[roleState](role); profile == "" || !rs.InstanceProfile || rs.Trust != "ec2" {
			return nil, fmt.Errorf("role %s is not one an instance runs as: declare it with trust ec2 and an instance profile", s.Role)
		}
	}
	switch {
	case s.Cluster != "":
		cl, err := dep(deps, KindCluster, s.Cluster)
		if err != nil {
			return nil, err
		}
		cluster, purpose = cl.IDs["cluster"], "join cluster "+cl.IDs["cluster"]+" as role "+s.Role
	case s.Container != nil:
		purpose = fmt.Sprintf("run %s on port %d", s.Container.Image, s.Container.Port)
	default:
		purpose = "keep what it was set up to do (adopted)"
	}
	setup := ""
	if !s.adopted() || !o.Exists {
		userData, err := UserData(s, cluster)
		if err != nil {
			return nil, err
		}
		setup = setupDigest(userData)
	}
	subnet, err := dep(deps, KindSubnet, s.Subnet)
	if err != nil {
		return nil, err
	}
	group, err := dep(deps, KindSecurityGroup, s.SecurityGroup)
	if err != nil {
		return nil, err
	}
	st := state[instanceState](o)
	if o.Exists {
		id := o.IDs["InstanceId.1"]
		ids := map[string]string{"InstanceId.1": id}
		switch {
		case st.InstanceType != s.InstanceType:
			return []protocol.Change{unsupported(r, KindInstance, OpRetype, ids, fmt.Sprintf(
				"instance type %s, declared %s: a person resizes it (it stops and starts): mamela act --confirm %s %s %q <pome-dir>",
				st.InstanceType, s.InstanceType, r.Name, OpResize, r.ID))}, nil
		case !s.adopted() && st.Setup != setup:
			return []protocol.Change{unsupported(r, KindInstance, OpReplace, ids, fmt.Sprintf("it was set up as %s, declared %s (%s)", st.Setup, setup, purpose))}, nil
		case st.Subnet != subnet.IDs["SubnetId"]:
			return []protocol.Change{unsupported(r, KindInstance, OpReplace, ids, fmt.Sprintf("it is in subnet %s, declared %s (%s)", st.Subnet, subnet.IDs["SubnetId"], s.Subnet))}, nil
		case st.Profile != profile:
			return []protocol.Change{unsupported(r, KindInstance, OpReplace, ids, fmt.Sprintf("it has instance profile %q, declared %q", st.Profile, profile))}, nil
		case !slices.Equal(st.Groups, []string{group.IDs["GroupId"]}):
			return []protocol.Change{{Resource: r.ID, Kind: KindInstance, Op: OpGroups, IDs: map[string]string{"InstanceId": id},
				Reasons:  []string{fmt.Sprintf("security group %s (%s), not %v", group.IDs["GroupId"], s.SecurityGroup, st.Groups)},
				Requests: []protocol.Approved{queryReq(APIEC2, "ModifyInstanceAttribute", map[string]string{"InstanceId": id, "GroupId.1": group.IDs["GroupId"]})}}}, nil
		case st.State == "stopped":
			return []protocol.Change{{Resource: r.ID, Kind: KindInstance, Op: OpStart, IDs: ids, Reasons: []string{"it is stopped"},
				Requests: []protocol.Approved{queryReq(APIEC2, "StartInstances", ids)}}}, nil
		}
		return nil, nil
	}
	if st.ImageID == "" {
		return nil, fmt.Errorf("the ECS-optimized image was not observed")
	}
	userData, _ := UserData(s, cluster)
	token := clientToken(r, st.ImageID, st.Generation)
	params := ec2Tags("instance", s.Boundary, r.Name, map[string]string{
		"MinCount": "1", "MaxCount": "1", "ImageId": st.ImageID, "InstanceType": s.InstanceType, "ClientToken": token,
		"UserData": userData, "MetadataOptions.HttpTokens": "required",
		"NetworkInterface.1.DeviceIndex": "0", "NetworkInterface.1.SubnetId": subnet.IDs["SubnetId"],
		"NetworkInterface.1.AssociatePublicIpAddress": "true", "NetworkInterface.1.SecurityGroupId.1": group.IDs["GroupId"],
		"TagSpecification.1.Tag.4.Key": TagSetup, "TagSpecification.1.Tag.4.Value": setup,
	})
	if profile != "" {
		params["IamInstanceProfile.Name"] = profile
	}
	return []protocol.Change{{Resource: r.ID, Kind: KindInstance, Op: OpCreate, IDs: map[string]string{"mamela-name": r.Name, "ClientToken": token},
		Reasons: []string{fmt.Sprintf("launch %s from %s (ECS-optimized AL2023) in subnet %s and group %s, to %s", s.InstanceType, st.ImageID,
			s.Subnet, s.SecurityGroup, purpose)},
		Requests: []protocol.Approved{queryReq(APIEC2, "RunInstances", params)}}}, nil
}

func (instanceKind) remove(o protocol.Observed) (protocol.Change, error) {
	id := map[string]string{"InstanceId.1": o.IDs["InstanceId.1"]}
	return protocol.Change{Resource: o.ID, Kind: KindInstance, Op: OpDelete, IDs: id,
		Reasons:  []string{"terminate instance " + id["InstanceId.1"] + ": its disk is deleted, and what runs on it stops"},
		Requests: []protocol.Approved{queryReq(APIEC2, "TerminateInstances", id)}}, nil
}

// prepare: resize (stop, change the type, start) and reboot.
func (instanceKind) prepare(op protocol.Op, o protocol.Observed, desired *protocol.Resource) (protocol.Change, error) {
	id := o.IDs["InstanceId.1"]
	st := state[instanceState](o)
	switch op {
	case OpReboot:
		if st.State != "running" {
			return protocol.Change{}, fmt.Errorf("%s is %s: only a running instance reboots", o.ID, st.State)
		}
		return protocol.Change{Resource: o.ID, Kind: KindInstance, Op: OpReboot, IDs: map[string]string{"InstanceId.1": id},
			Reasons:  []string{"reboot instance " + id + ": what runs on it is interrupted (a reboot leaves no trace: after a lost answer, Mamela cannot tell whether it rebooted)"},
			Requests: []protocol.Approved{queryReq(APIEC2, "RebootInstances", map[string]string{"InstanceId.1": id})}}, nil
	case OpResize:
		if desired == nil {
			return protocol.Change{}, fmt.Errorf("%s: resize gives it the declared type, and it is not declared", o.ID)
		}
		s, err := spec[instanceSpec](*desired)
		if err != nil {
			return protocol.Change{}, err
		}
		if s.InstanceType == st.InstanceType {
			return protocol.Change{}, fmt.Errorf("%s is %s already, as declared", o.ID, st.InstanceType)
		}
		ch := protocol.Change{Resource: o.ID, Kind: KindInstance, Op: OpResize, IDs: map[string]string{"InstanceId.1": id, "InstanceId": id}}
		modify := queryReq(APIEC2, "ModifyInstanceAttribute", map[string]string{"InstanceId": id, "InstanceType.Value": s.InstanceType})
		switch st.State {
		case "running":
			ch.Reasons = []string{fmt.Sprintf("resize instance %s from %s to %s: it stops, its type changes, and it starts again "+
				"(what runs on it is interrupted; its public IP address changes)", id, st.InstanceType, s.InstanceType)}
			ch.Requests = []protocol.Approved{queryReq(APIEC2, "StopInstances", map[string]string{"InstanceId.1": id}), modify,
				queryReq(APIEC2, "StartInstances", map[string]string{"InstanceId.1": id})}
		case "stopped":
			ch.Reasons = []string{fmt.Sprintf("resize stopped instance %s from %s to %s (a plan then starts it)", id, st.InstanceType, s.InstanceType)}
			ch.Requests = []protocol.Approved{modify}
		default:
			return protocol.Change{}, fmt.Errorf("%s is %s: resize it once it is running or stopped", o.ID, st.State)
		}
		return ch, nil
	}
	return protocol.Change{}, fmt.Errorf("%s is not an explicit operation of %s", op, KindInstance)
}

// resizeTo is the type a resize changes an instance to ("" for another change).
func resizeTo(ch protocol.Change) string {
	if ch.Op != OpResize {
		return ""
	}
	for _, a := range ch.Requests {
		if t := param(a, "InstanceType.Value"); t != "" {
			return t
		}
	}
	return ""
}

func (k instanceKind) done(c *client, ch protocol.Change, a protocol.Approved) (bool, error) {
	objs, err := k.list(c)
	if err != nil {
		return false, err
	}
	byID := func(id string) (instanceState, bool) {
		i := slices.IndexFunc(objs, func(x object) bool { return x.IDs["InstanceId.1"] == id })
		if i < 0 {
			return instanceState{}, false
		}
		return objs[i].State.(instanceState), true
	}
	up := func(st instanceState) bool { return st.State == "running" || st.State == "pending" }
	target := resizeTo(ch)
	switch action(a) {
	case "RunInstances":
		return slices.ContainsFunc(objs, func(x object) bool { return x.IDs["ClientToken"] == param(a, "ClientToken") }), nil
	case "StartInstances":
		st, ok := byID(param(a, "InstanceId.1"))
		return ok && up(st) && (target == "" || st.InstanceType == target), nil
	case "StopInstances": // of a resize: stopped, or past it (the new type)
		st, ok := byID(param(a, "InstanceId.1"))
		return ok && (st.State == "stopped" || target != "" && st.InstanceType == target), nil
	case "ModifyInstanceAttribute":
		st, ok := byID(param(a, "InstanceId"))
		if t := param(a, "InstanceType.Value"); t != "" {
			return ok && st.InstanceType == t, nil
		}
		return ok && slices.Equal(st.Groups, []string{param(a, "GroupId.1")}), nil
	case "TerminateInstances":
		_, ok := byID(param(a, "InstanceId.1"))
		return !ok, nil
	}
	return false, nil // a reboot leaves no trace
}

// --- import and adopt ---

func (instanceKind) imports() []string { return []string{"aws_instance"} }

// importObject declares an instance Terraform launched as one Mamela adopts: it keeps its image
// and setup; Mamela keeps its type, its one security group (imported) in its subnet (imported),
// and its running. An instance profile must be of an imported role (trust ec2), whose profile it
// is; any other instance is left to Terraform.
func (instanceKind) importObject(o protocol.ImportObject, r kit.Resolver, boundary string) (kit.Imported, error) {
	var v struct {
		ID      string   `json:"id"`
		Type    string   `json:"instance_type"`
		Subnet  string   `json:"subnet_id"`
		Groups  []string `json:"vpc_security_group_ids"`
		Profile string   `json:"iam_instance_profile"`
		State   string   `json:"instance_state"`
	}
	if err := kit.Values(o, &v); err != nil {
		return kit.Imported{}, fmt.Errorf("the attributes of %s: %w", o.Address, err)
	}
	switch {
	case v.ID == "":
		return kit.Imported{}, kit.Skip("no id: the state does not say which instance it is")
	case v.State == "terminated" || v.State == "shutting-down":
		return kit.Imported{}, kit.Skip("it is " + v.State)
	case len(v.Groups) != 1:
		return kit.Imported{}, kit.Skip(fmt.Sprintf("it has %d security groups (vpc_security_group_ids); AwsInstance has one", len(v.Groups)))
	}
	s := instanceSpec{Boundary: boundary, InstanceType: v.Type}
	var err error
	if s.Subnet, err = referTo(subnetKind{}, r, "aws_subnet", "subnet "+v.Subnet, boundary, idIs(v.Subnet)); err != nil {
		return kit.Imported{}, err
	}
	if s.SecurityGroup, err = referTo(groupKind{}, r, "aws_security_group", "security group "+v.Groups[0], boundary, idIs(v.Groups[0])); err != nil {
		return kit.Imported{}, err
	}
	if v.Profile != "" {
		role := ""
		for _, x := range r.Objects("aws_iam_instance_profile") {
			if pv, err := roleValues(x); err == nil && pv.Name == v.Profile {
				role = pv.Role
			}
		}
		if role == "" {
			return kit.Imported{}, kit.Skip(fmt.Sprintf("its instance profile %s is not in the state: Mamela adopts an instance only with the profile of a role it adopts too", v.Profile))
		}
		if s.Role, err = referTo(roleKind{}, r, "aws_iam_role", "role "+role, boundary, func(x protocol.ImportObject) bool {
			rv, err := roleValues(x)
			return err == nil && rv.Name == role
		}); err != nil {
			return kit.Imported{}, err
		}
		var rs roleSpec
		for _, x := range r.Objects("aws_iam_role") {
			if x.Name == s.Role {
				im, _ := roleKind{}.importObject(x, r, boundary)
				rs, _ = im.Spec.(roleSpec)
			}
		}
		if rs.Trust != "ec2" || !rs.InstanceProfile {
			return kit.Imported{}, kit.Skip(fmt.Sprintf("its role %s is not one an instance runs as (trust ec2, an instance profile of its name)", role))
		}
	}
	return kit.Imported{Spec: s, IDs: map[string]string{"InstanceId.1": v.ID}}, nil
}

func (instanceKind) find(c *client, ids map[string]string) (*object, error) {
	id := ids["InstanceId.1"]
	if id == "" {
		return nil, nil
	}
	got, err := instances(c, map[string]string{"Filter.1.Name": "instance-id", "Filter.1.Value.1": id})
	if err != nil || len(got) != 1 || got[0].State == "terminated" || got[0].State == "shutting-down" {
		return nil, err
	}
	o := instanceObject(got[0])
	return &o, nil
}

func (instanceKind) adopt(o protocol.Observed, desired protocol.Resource) (protocol.Change, error) {
	return tagChange(o, desired, "InstanceId.1")
}
