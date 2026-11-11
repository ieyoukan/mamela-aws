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

// KindInstance is the kind; OpStart starts it when it is stopped.
const (
	KindInstance protocol.ResourceKind = "AwsInstance"
	OpStart      protocol.Op           = "start"
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
	terminateInstances = queryCall(APIEC2, "TerminateInstances", "InstanceId.1")
)

type instanceKind struct{}

// HostContainer is the one container an instance without a cluster runs: Port on the instance to
// the same port of the container.
type HostContainer struct {
	Image string `json:"image"`
	Port  int64  `json:"port"`
}

type instanceSpec struct {
	Boundary      string         `json:"boundary"`
	Subnet        string         `json:"subnet"`
	SecurityGroup string         `json:"securityGroup"`
	InstanceType  string         `json:"instanceType"`
	Cluster       string         `json:"cluster,omitempty"` // with Role: a container instance of the cluster
	Role          string         `json:"role,omitempty"`
	Container     *HostContainer `json:"container,omitempty"` // or: this container, and no role
}

type instanceState struct {
	State        string `json:"state,omitempty"` // pending, running, stopping or stopped
	InstanceType string `json:"instanceType,omitempty"`
	ImageID      string `json:"imageId"`
	PrivateIP    string `json:"privateIp,omitempty"`
	Setup        string `json:"setup,omitempty"` // the digest of its user data
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
		return "", fmt.Errorf("an instance joins a cluster (cluster and role) or runs a container (container), one of the two")
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

func (instanceKind) kind() protocol.ResourceKind { return KindInstance }
func (instanceKind) reads() []protocol.Call      { return []protocol.Call{describeInstances, getParameter} }
func (instanceKind) operations() []protocol.Operation {
	return []protocol.Operation{op(OpCreate, runInstances), op(OpStart, startInstances),
		unsupportedOp(OpReplace, "an instance keeps its type and what it was set up to do: declare a new one (and delete this one with mamela act)"),
		deleteOp(terminateInstances)}
}

func (instanceKind) list(c *client) ([]object, error) {
	got, err := instances(c, map[string]string{"Filter.1.Name": "tag-key", "Filter.1.Value.1": TagName, "Filter.2.Name": "instance-state-name",
		"Filter.2.Value.1": "pending", "Filter.2.Value.2": "running", "Filter.2.Value.3": "stopping", "Filter.2.Value.4": "stopped"})
	if err != nil {
		return nil, err
	}
	var out []object
	for _, i := range got {
		out = append(out, object{Name: tagOf(i.Tags, TagName), Boundary: tagOf(i.Tags, TagBoundary),
			IDs:   map[string]string{"ClientToken": i.ClientToken, "InstanceId.1": i.ID},
			State: instanceState{State: i.State, InstanceType: i.Type, ImageID: i.Image, PrivateIP: i.PrivateIP, Setup: tagOf(i.Tags, TagSetup)}})
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
	if s.Cluster != "" {
		cl, err := dep(deps, KindCluster, s.Cluster)
		if err != nil {
			return nil, err
		}
		role, err := dep(deps, KindIAMRole, s.Role)
		if err != nil {
			return nil, err
		}
		profile = role.IDs["InstanceProfileName"]
		if rs := state[roleState](role); profile == "" || !rs.InstanceProfile || rs.Trust != "ec2" {
			return nil, fmt.Errorf("role %s is not one an instance runs as: declare it with trust ec2 and an instance profile", s.Role)
		}
		cluster, purpose = cl.IDs["cluster"], "join cluster "+cl.IDs["cluster"]+" as role "+s.Role
	} else if s.Container != nil {
		purpose = fmt.Sprintf("run %s on port %d", s.Container.Image, s.Container.Port)
	}
	userData, err := UserData(s, cluster)
	if err != nil {
		return nil, err
	}
	setup := setupDigest(userData)
	st := state[instanceState](o)
	if o.Exists {
		id := map[string]string{"InstanceId.1": o.IDs["InstanceId.1"]}
		switch {
		case st.InstanceType != s.InstanceType:
			return []protocol.Change{unsupported(r, KindInstance, OpReplace, id, fmt.Sprintf("instance type %s, declared %s", st.InstanceType, s.InstanceType))}, nil
		case st.Setup != setup:
			return []protocol.Change{unsupported(r, KindInstance, OpReplace, id, fmt.Sprintf("it was set up as %s, declared %s (%s)", st.Setup, setup, purpose))}, nil
		case st.State == "stopped":
			return []protocol.Change{{Resource: r.ID, Kind: KindInstance, Op: OpStart, IDs: id, Reasons: []string{"it is stopped"},
				Requests: []protocol.Approved{queryReq(APIEC2, "StartInstances", id)}}}, nil
		}
		return nil, nil
	}
	subnet, err := dep(deps, KindSubnet, s.Subnet)
	if err != nil {
		return nil, err
	}
	group, err := dep(deps, KindSecurityGroup, s.SecurityGroup)
	if err != nil {
		return nil, err
	}
	if st.ImageID == "" {
		return nil, fmt.Errorf("the ECS-optimized image was not observed")
	}
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

func (k instanceKind) done(c *client, _ protocol.Change, a protocol.Approved) (bool, error) {
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
	switch action(a) {
	case "RunInstances":
		return slices.ContainsFunc(objs, func(x object) bool { return x.IDs["ClientToken"] == param(a, "ClientToken") }), nil
	case "StartInstances":
		st, ok := byID(param(a, "InstanceId.1"))
		return ok && (st.State == "running" || st.State == "pending"), nil
	case "TerminateInstances":
		_, ok := byID(param(a, "InstanceId.1"))
		return !ok, nil
	}
	return false, nil
}
