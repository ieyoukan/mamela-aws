package aws

import (
	"fmt"
	"slices"
	"strconv"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// The network: a VPC of the boundary's own, its internet gateway, subnets, a route table and
// security groups. Each create sends one request (AWS gives the ID); what needs the new object's
// ID (attaching, routing, ingress rules) is the next plan's change on the object that exists.

// Kinds and operations of the network.
const (
	KindVpc             protocol.ResourceKind = "AwsVpc"
	KindInternetGateway protocol.ResourceKind = "AwsInternetGateway"
	KindSubnet          protocol.ResourceKind = "AwsSubnet"
	KindRouteTable      protocol.ResourceKind = "AwsRouteTable"
	KindSecurityGroup   protocol.ResourceKind = "AwsSecurityGroup"

	OpCreate  protocol.Op = "create"
	OpAttach  protocol.Op = "attach"
	OpRoute   protocol.Op = "route"
	OpIngress protocol.Op = "ingress"
	OpReplace protocol.Op = "replace" // never carried out
)

var (
	describeVpcs      = queryCall(APIEC2, "DescribeVpcs", "Filter.1.Name", "Filter.1.Value.1")
	createVpc         = queryCall(APIEC2, "CreateVpc", append([]string{"CidrBlock"}, ec2TagParams...)...)
	deleteVpc         = queryCall(APIEC2, "DeleteVpc", "VpcId")
	describeGateways  = queryCall(APIEC2, "DescribeInternetGateways", "Filter.1.Name", "Filter.1.Value.1")
	createGateway     = queryCall(APIEC2, "CreateInternetGateway", ec2TagParams...)
	attachGateway     = queryCall(APIEC2, "AttachInternetGateway", "InternetGatewayId", "VpcId")
	detachGateway     = queryCall(APIEC2, "DetachInternetGateway", "InternetGatewayId", "VpcId")
	deleteGateway     = queryCall(APIEC2, "DeleteInternetGateway", "InternetGatewayId")
	describeSubnets   = queryCall(APIEC2, "DescribeSubnets", "Filter.1.Name", "Filter.1.Value.1")
	createSubnet      = queryCall(APIEC2, "CreateSubnet", append([]string{"VpcId", "CidrBlock", "AvailabilityZone"}, ec2TagParams...)...)
	deleteSubnet      = queryCall(APIEC2, "DeleteSubnet", "SubnetId")
	describeRoutes    = queryCall(APIEC2, "DescribeRouteTables", "Filter.1.Name", "Filter.1.Value.1")
	createRouteTable  = queryCall(APIEC2, "CreateRouteTable", append([]string{"VpcId"}, ec2TagParams...)...)
	createRoute       = queryCall(APIEC2, "CreateRoute", "RouteTableId", "DestinationCidrBlock", "GatewayId")
	associateRoutes   = queryCall(APIEC2, "AssociateRouteTable", "RouteTableId", "SubnetId")
	disassociate      = queryCall(APIEC2, "DisassociateRouteTable", "AssociationId")
	deleteRouteTable  = queryCall(APIEC2, "DeleteRouteTable", "RouteTableId")
	describeGroups    = queryCall(APIEC2, "DescribeSecurityGroups", "Filter.1.Name", "Filter.1.Value.1")
	createGroup       = queryCall(APIEC2, "CreateSecurityGroup", append([]string{"GroupName", "GroupDescription", "VpcId"}, ec2TagParams...)...)
	authorizeIngress  = queryCall(APIEC2, "AuthorizeSecurityGroupIngress", append([]string{"GroupId"}, ingressParams...)...)
	deleteGroup       = queryCall(APIEC2, "DeleteSecurityGroup", "GroupId")
	ingressParams     = ingressParamNames()
	maxIngressPerCall = 4
)

func ingressParamNames() []string {
	var out []string
	for i := 1; i <= 4; i++ {
		for _, f := range []string{"IpProtocol", "FromPort", "ToPort", "Groups.1.GroupId", "IpRanges.1.CidrIp"} {
			out = append(out, fmt.Sprintf("IpPermissions.%d.%s", i, f))
		}
	}
	return out
}

func createChange(r protocol.Resource, k protocol.ResourceKind, why string, a protocol.Approved) protocol.Change {
	return protocol.Change{Resource: r.ID, Kind: k, Op: OpCreate, IDs: map[string]string{"mamela-name": r.Name}, Reasons: []string{why},
		Requests: []protocol.Approved{a}}
}

// --- VPC --------------------------------------------------------------------------------------

type vpcKind struct{}

type vpcSpec struct {
	Boundary string `json:"boundary" doc:"the boundary (the guard's): it tags everything made"`
	CIDR     string `json:"cidr" doc:"its IPv4 CIDR block"`
}

type vpcState struct {
	CIDR string `json:"cidr"`
}

func (vpcKind) kind() protocol.ResourceKind { return KindVpc }
func (vpcKind) reads() []protocol.Call      { return []protocol.Call{describeVpcs} }
func (vpcKind) operations() []protocol.Operation {
	return []protocol.Operation{op(OpCreate, createVpc), unsupportedOp(OpReplace, "a VPC keeps its CIDR block: declare a new one"), deleteOp(deleteVpc), adoptOp()}
}

func (k vpcKind) list(c *client) ([]object, error) { return k.listBy(c, tagged()) }

// listBy lists with Describe parameters (a filter by ID finds an object that carries no tag).
func (vpcKind) listBy(c *client, params map[string]string) ([]object, error) {
	var resp struct {
		Items []struct {
			ID   string   `xml:"vpcId"`
			CIDR string   `xml:"cidrBlock"`
			Tags []xmlTag `xml:"tagSet>item"`
		} `xml:"vpcSet>item"`
	}
	if err := c.query(APIEC2, "DescribeVpcs", params, &resp); err != nil {
		return nil, err
	}
	var out []object
	for _, v := range resp.Items {
		out = append(out, object{Name: tagOf(v.Tags, TagName), Boundary: tagOf(v.Tags, TagBoundary), IDs: map[string]string{"VpcId": v.ID},
			State: vpcState{CIDR: v.CIDR}})
	}
	return out, nil
}

func (vpcKind) missing(*client, protocol.Resource) (any, error) { return nil, nil }

func (vpcKind) diff(r protocol.Resource, o protocol.Observed, _ map[string]protocol.Observed) ([]protocol.Change, error) {
	s, err := spec[vpcSpec](r)
	if err != nil {
		return nil, err
	}
	if !o.Exists {
		return []protocol.Change{createChange(r, KindVpc, "VPC "+s.CIDR,
			queryReq(APIEC2, "CreateVpc", ec2Tags("vpc", s.Boundary, r.Name, map[string]string{"CidrBlock": s.CIDR})))}, nil
	}
	if st := state[vpcState](o); st.CIDR != s.CIDR {
		return []protocol.Change{unsupported(r, KindVpc, OpReplace, map[string]string{"VpcId": o.IDs["VpcId"]}, fmt.Sprintf("CIDR %s -> %s", st.CIDR, s.CIDR))}, nil
	}
	return nil, nil
}

func (vpcKind) remove(o protocol.Observed) (protocol.Change, error) {
	id := o.IDs["VpcId"]
	return protocol.Change{Resource: o.ID, Kind: KindVpc, Op: OpDelete, IDs: map[string]string{"VpcId": id},
		Reasons:  []string{"delete VPC " + id + " (its subnets, gateway, route table and groups must be deleted first)"},
		Requests: []protocol.Approved{queryReq(APIEC2, "DeleteVpc", map[string]string{"VpcId": id})}}, nil
}

func (k vpcKind) done(c *client, ch protocol.Change, a protocol.Approved) (bool, error) {
	objs, err := k.list(c)
	if err != nil {
		return false, err
	}
	switch action(a) {
	case "CreateVpc":
		return slices.ContainsFunc(objs, func(x object) bool {
			return x.Name == ch.IDs["mamela-name"] || x.IDs["VpcId"] != "" && x.Name == nameOf(ch)
		}), nil
	case "DeleteVpc":
		return !slices.ContainsFunc(objs, func(x object) bool { return x.IDs["VpcId"] == param(a, "VpcId") }), nil
	}
	return false, nil
}

// nameOf is the declared name of a change's resource ("AwsVpc net" -> "net").
func nameOf(ch protocol.Change) string {
	if n := ch.IDs["mamela-name"]; n != "" {
		return n
	}
	return ch.Resource[len(ch.Kind)+1:]
}

// --- internet gateway -------------------------------------------------------------------------

type gatewayKind struct{}

type gatewaySpec struct {
	Boundary string `json:"boundary" doc:"the boundary (the guard's)"`
	Vpc      string `json:"vpc" mamela:"ref=AwsVpc" doc:"the VPC it is attached to"`
}

type gatewayState struct {
	AttachedTo string `json:"attachedTo,omitempty"`
}

func (gatewayKind) kind() protocol.ResourceKind { return KindInternetGateway }
func (gatewayKind) reads() []protocol.Call      { return []protocol.Call{describeGateways} }
func (gatewayKind) operations() []protocol.Operation {
	return []protocol.Operation{op(OpCreate, createGateway), op(OpAttach, attachGateway),
		unsupportedOp(OpReplace, "the gateway is attached to another VPC: detach it by hand"), deleteOp(detachGateway, deleteGateway), adoptOp()}
}

func (k gatewayKind) list(c *client) ([]object, error) { return k.listBy(c, tagged()) }

// listBy lists with Describe parameters (a filter by ID finds an object that carries no tag).
func (gatewayKind) listBy(c *client, params map[string]string) ([]object, error) {
	var resp struct {
		Items []struct {
			ID          string `xml:"internetGatewayId"`
			Attachments []struct {
				VpcID string `xml:"vpcId"`
			} `xml:"attachmentSet>item"`
			Tags []xmlTag `xml:"tagSet>item"`
		} `xml:"internetGatewaySet>item"`
	}
	if err := c.query(APIEC2, "DescribeInternetGateways", params, &resp); err != nil {
		return nil, err
	}
	var out []object
	for _, g := range resp.Items {
		st := gatewayState{}
		if len(g.Attachments) > 0 {
			st.AttachedTo = g.Attachments[0].VpcID
		}
		out = append(out, object{Name: tagOf(g.Tags, TagName), Boundary: tagOf(g.Tags, TagBoundary), IDs: map[string]string{"InternetGatewayId": g.ID}, State: st})
	}
	return out, nil
}

func (gatewayKind) missing(*client, protocol.Resource) (any, error) { return nil, nil }

func (gatewayKind) diff(r protocol.Resource, o protocol.Observed, deps map[string]protocol.Observed) ([]protocol.Change, error) {
	s, err := spec[gatewaySpec](r)
	if err != nil {
		return nil, err
	}
	if !o.Exists {
		return []protocol.Change{createChange(r, KindInternetGateway, "an internet gateway for VPC "+s.Vpc,
			queryReq(APIEC2, "CreateInternetGateway", ec2Tags("internet-gateway", s.Boundary, r.Name, map[string]string{})))}, nil
	}
	vpc, err := dep(deps, KindVpc, s.Vpc)
	if err != nil {
		return nil, err
	}
	id, vpcID := o.IDs["InternetGatewayId"], vpc.IDs["VpcId"]
	switch st := state[gatewayState](o); st.AttachedTo {
	case vpcID:
		return nil, nil
	case "":
		return []protocol.Change{{Resource: r.ID, Kind: KindInternetGateway, Op: OpAttach, IDs: map[string]string{"InternetGatewayId": id},
			Reasons:  []string{"attach it to VPC " + s.Vpc + " (" + vpcID + ")"},
			Requests: []protocol.Approved{queryReq(APIEC2, "AttachInternetGateway", map[string]string{"InternetGatewayId": id, "VpcId": vpcID})}}}, nil
	default:
		return []protocol.Change{unsupported(r, KindInternetGateway, OpReplace, map[string]string{"InternetGatewayId": id}, "attached to "+st.AttachedTo+", not "+vpcID)}, nil
	}
}

func (gatewayKind) remove(o protocol.Observed) (protocol.Change, error) {
	id := o.IDs["InternetGatewayId"]
	ch := protocol.Change{Resource: o.ID, Kind: KindInternetGateway, Op: OpDelete, IDs: map[string]string{"InternetGatewayId": id},
		Reasons: []string{"delete internet gateway " + id}}
	if vpc := state[gatewayState](o).AttachedTo; vpc != "" {
		ch.Requests = append(ch.Requests, queryReq(APIEC2, "DetachInternetGateway", map[string]string{"InternetGatewayId": id, "VpcId": vpc}))
	}
	ch.Requests = append(ch.Requests, queryReq(APIEC2, "DeleteInternetGateway", map[string]string{"InternetGatewayId": id}))
	return ch, nil
}

func (k gatewayKind) done(c *client, ch protocol.Change, a protocol.Approved) (bool, error) {
	objs, err := k.list(c)
	if err != nil {
		return false, err
	}
	find := func(id string) (object, bool) {
		i := slices.IndexFunc(objs, func(x object) bool { return x.IDs["InternetGatewayId"] == id })
		if i < 0 {
			return object{}, false
		}
		return objs[i], true
	}
	switch action(a) {
	case "CreateInternetGateway":
		return slices.ContainsFunc(objs, func(x object) bool { return x.Name == nameOf(ch) }), nil
	case "AttachInternetGateway":
		g, ok := find(param(a, "InternetGatewayId"))
		return ok && g.State.(gatewayState).AttachedTo == param(a, "VpcId"), nil
	case "DetachInternetGateway":
		g, ok := find(param(a, "InternetGatewayId"))
		return !ok || g.State.(gatewayState).AttachedTo == "", nil
	case "DeleteInternetGateway":
		_, ok := find(param(a, "InternetGatewayId"))
		return !ok, nil
	}
	return false, nil
}

// --- subnet -----------------------------------------------------------------------------------

type subnetKind struct{}

type subnetSpec struct {
	Boundary string `json:"boundary" doc:"the boundary (the guard's)"`
	Vpc      string `json:"vpc" mamela:"ref=AwsVpc"`
	CIDR     string `json:"cidr"`
	Zone     string `json:"zone" doc:"its availability zone (ap-northeast-1a)"`
}

type subnetState struct {
	CIDR  string `json:"cidr"`
	Zone  string `json:"zone"`
	VpcID string `json:"vpcId"`
}

func (subnetKind) kind() protocol.ResourceKind { return KindSubnet }
func (subnetKind) reads() []protocol.Call      { return []protocol.Call{describeSubnets} }
func (subnetKind) operations() []protocol.Operation {
	return []protocol.Operation{op(OpCreate, createSubnet), unsupportedOp(OpReplace, "a subnet keeps its VPC, CIDR block and zone: declare a new one"), deleteOp(deleteSubnet), adoptOp()}
}

func (k subnetKind) list(c *client) ([]object, error) { return k.listBy(c, tagged()) }

// listBy lists with Describe parameters (a filter by ID finds an object that carries no tag).
func (subnetKind) listBy(c *client, params map[string]string) ([]object, error) {
	var resp struct {
		Items []struct {
			ID    string   `xml:"subnetId"`
			VpcID string   `xml:"vpcId"`
			CIDR  string   `xml:"cidrBlock"`
			Zone  string   `xml:"availabilityZone"`
			Tags  []xmlTag `xml:"tagSet>item"`
		} `xml:"subnetSet>item"`
	}
	if err := c.query(APIEC2, "DescribeSubnets", params, &resp); err != nil {
		return nil, err
	}
	var out []object
	for _, s := range resp.Items {
		out = append(out, object{Name: tagOf(s.Tags, TagName), Boundary: tagOf(s.Tags, TagBoundary), IDs: map[string]string{"SubnetId": s.ID},
			State: subnetState{CIDR: s.CIDR, Zone: s.Zone, VpcID: s.VpcID}})
	}
	return out, nil
}

func (subnetKind) missing(*client, protocol.Resource) (any, error) { return nil, nil }

func (subnetKind) diff(r protocol.Resource, o protocol.Observed, deps map[string]protocol.Observed) ([]protocol.Change, error) {
	s, err := spec[subnetSpec](r)
	if err != nil {
		return nil, err
	}
	vpc, err := dep(deps, KindVpc, s.Vpc)
	if err != nil {
		return nil, err
	}
	if !o.Exists {
		return []protocol.Change{createChange(r, KindSubnet, fmt.Sprintf("subnet %s in %s of VPC %s", s.CIDR, s.Zone, s.Vpc),
			queryReq(APIEC2, "CreateSubnet", ec2Tags("subnet", s.Boundary, r.Name,
				map[string]string{"VpcId": vpc.IDs["VpcId"], "CidrBlock": s.CIDR, "AvailabilityZone": s.Zone})))}, nil
	}
	if st := state[subnetState](o); st.CIDR != s.CIDR || st.Zone != s.Zone || st.VpcID != vpc.IDs["VpcId"] {
		return []protocol.Change{unsupported(r, KindSubnet, OpReplace, map[string]string{"SubnetId": o.IDs["SubnetId"]},
			fmt.Sprintf("%s in %s of %s -> %s in %s of %s", st.CIDR, st.Zone, st.VpcID, s.CIDR, s.Zone, vpc.IDs["VpcId"]))}, nil
	}
	return nil, nil
}

func (subnetKind) remove(o protocol.Observed) (protocol.Change, error) {
	id := o.IDs["SubnetId"]
	return protocol.Change{Resource: o.ID, Kind: KindSubnet, Op: OpDelete, IDs: map[string]string{"SubnetId": id},
		Reasons:  []string{"delete subnet " + id + " (what runs in it must be deleted first)"},
		Requests: []protocol.Approved{queryReq(APIEC2, "DeleteSubnet", map[string]string{"SubnetId": id})}}, nil
}

func (k subnetKind) done(c *client, ch protocol.Change, a protocol.Approved) (bool, error) {
	objs, err := k.list(c)
	if err != nil {
		return false, err
	}
	switch action(a) {
	case "CreateSubnet":
		return slices.ContainsFunc(objs, func(x object) bool { return x.Name == nameOf(ch) }), nil
	case "DeleteSubnet":
		return !slices.ContainsFunc(objs, func(x object) bool { return x.IDs["SubnetId"] == param(a, "SubnetId") }), nil
	}
	return false, nil
}

// --- route table ------------------------------------------------------------------------------

type routeTableKind struct{}

type routeTableSpec struct {
	Boundary        string `json:"boundary" doc:"the boundary (the guard's)"`
	Vpc             string `json:"vpc" mamela:"ref=AwsVpc"`
	InternetGateway string `json:"internetGateway" mamela:"ref=AwsInternetGateway" doc:"where 0.0.0.0/0 goes"`
	Subnet          string `json:"subnet" mamela:"ref=AwsSubnet" doc:"the subnet it serves"`
}

type routeTableState struct {
	DefaultRoute string `json:"defaultRoute,omitempty"` // the gateway of 0.0.0.0/0
	Subnet       string `json:"subnet,omitempty"`       // the associated subnet
}

func (routeTableKind) kind() protocol.ResourceKind { return KindRouteTable }
func (routeTableKind) reads() []protocol.Call      { return []protocol.Call{describeRoutes} }
func (routeTableKind) operations() []protocol.Operation {
	return []protocol.Operation{op(OpCreate, createRouteTable), op(OpRoute, createRoute, associateRoutes),
		unsupportedOp(OpReplace, "the route table routes elsewhere, or serves another subnet: change it by hand"),
		deleteOp(disassociate, deleteRouteTable), adoptOp()}
}

func (k routeTableKind) list(c *client) ([]object, error) { return k.listBy(c, tagged()) }

// listBy lists with Describe parameters (a filter by ID finds an object that carries no tag).
func (routeTableKind) listBy(c *client, params map[string]string) ([]object, error) {
	var resp struct {
		Items []struct {
			ID     string `xml:"routeTableId"`
			Routes []struct {
				Destination string `xml:"destinationCidrBlock"`
				Gateway     string `xml:"gatewayId"`
			} `xml:"routeSet>item"`
			Associations []struct {
				ID     string `xml:"routeTableAssociationId"`
				Subnet string `xml:"subnetId"`
			} `xml:"associationSet>item"`
			Tags []xmlTag `xml:"tagSet>item"`
		} `xml:"routeTableSet>item"`
	}
	if err := c.query(APIEC2, "DescribeRouteTables", params, &resp); err != nil {
		return nil, err
	}
	var out []object
	for _, t := range resp.Items {
		o := object{Name: tagOf(t.Tags, TagName), Boundary: tagOf(t.Tags, TagBoundary), IDs: map[string]string{"RouteTableId": t.ID}}
		st := routeTableState{}
		for _, r := range t.Routes {
			if r.Destination == "0.0.0.0/0" {
				st.DefaultRoute = r.Gateway
			}
		}
		if len(t.Associations) > 0 {
			st.Subnet = t.Associations[0].Subnet
			o.IDs["AssociationId"] = t.Associations[0].ID
		}
		o.State = st
		out = append(out, o)
	}
	return out, nil
}

func (routeTableKind) missing(*client, protocol.Resource) (any, error) { return nil, nil }

func (routeTableKind) diff(r protocol.Resource, o protocol.Observed, deps map[string]protocol.Observed) ([]protocol.Change, error) {
	s, err := spec[routeTableSpec](r)
	if err != nil {
		return nil, err
	}
	vpc, err := dep(deps, KindVpc, s.Vpc)
	if err != nil {
		return nil, err
	}
	if !o.Exists {
		return []protocol.Change{createChange(r, KindRouteTable, "a route table of VPC "+s.Vpc,
			queryReq(APIEC2, "CreateRouteTable", ec2Tags("route-table", s.Boundary, r.Name, map[string]string{"VpcId": vpc.IDs["VpcId"]})))}, nil
	}
	gw, err := dep(deps, KindInternetGateway, s.InternetGateway)
	if err != nil {
		return nil, err
	}
	subnet, err := dep(deps, KindSubnet, s.Subnet)
	if err != nil {
		return nil, err
	}
	id, gwID, subnetID := o.IDs["RouteTableId"], gw.IDs["InternetGatewayId"], subnet.IDs["SubnetId"]
	st := state[routeTableState](o)
	if st.DefaultRoute != "" && st.DefaultRoute != gwID || st.Subnet != "" && st.Subnet != subnetID {
		return []protocol.Change{unsupported(r, KindRouteTable, OpReplace, map[string]string{"RouteTableId": id},
			fmt.Sprintf("routes 0.0.0.0/0 to %q and serves %q; declared %s and %s", st.DefaultRoute, st.Subnet, gwID, subnetID))}, nil
	}
	ch := protocol.Change{Resource: r.ID, Kind: KindRouteTable, Op: OpRoute, IDs: map[string]string{"RouteTableId": id}}
	if st.DefaultRoute == "" {
		ch.Reasons = append(ch.Reasons, "route 0.0.0.0/0 to internet gateway "+s.InternetGateway)
		ch.Requests = append(ch.Requests, queryReq(APIEC2, "CreateRoute", map[string]string{"RouteTableId": id, "DestinationCidrBlock": "0.0.0.0/0", "GatewayId": gwID}))
	}
	if st.Subnet == "" {
		ch.Reasons = append(ch.Reasons, "serve subnet "+s.Subnet)
		ch.Requests = append(ch.Requests, queryReq(APIEC2, "AssociateRouteTable", map[string]string{"RouteTableId": id, "SubnetId": subnetID}))
	}
	if len(ch.Requests) == 0 {
		return nil, nil
	}
	return []protocol.Change{ch}, nil
}

func (routeTableKind) remove(o protocol.Observed) (protocol.Change, error) {
	id := o.IDs["RouteTableId"]
	ids := map[string]string{"RouteTableId": id}
	ch := protocol.Change{Resource: o.ID, Kind: KindRouteTable, Op: OpDelete, Reasons: []string{"delete route table " + id}}
	if a := o.IDs["AssociationId"]; a != "" {
		ids["AssociationId"] = a
		ch.Requests = append(ch.Requests, queryReq(APIEC2, "DisassociateRouteTable", map[string]string{"AssociationId": a}))
	}
	ch.IDs = ids
	ch.Requests = append(ch.Requests, queryReq(APIEC2, "DeleteRouteTable", map[string]string{"RouteTableId": id}))
	return ch, nil
}

func (k routeTableKind) done(c *client, ch protocol.Change, a protocol.Approved) (bool, error) {
	objs, err := k.list(c)
	if err != nil {
		return false, err
	}
	find := func(id string) (routeTableState, bool) {
		i := slices.IndexFunc(objs, func(x object) bool { return x.IDs["RouteTableId"] == id })
		if i < 0 {
			return routeTableState{}, false
		}
		return objs[i].State.(routeTableState), true
	}
	switch action(a) {
	case "CreateRouteTable":
		return slices.ContainsFunc(objs, func(x object) bool { return x.Name == nameOf(ch) }), nil
	case "CreateRoute":
		st, ok := find(param(a, "RouteTableId"))
		return ok && st.DefaultRoute == param(a, "GatewayId"), nil
	case "AssociateRouteTable":
		st, ok := find(param(a, "RouteTableId"))
		return ok && st.Subnet == param(a, "SubnetId"), nil
	case "DisassociateRouteTable":
		return !slices.ContainsFunc(objs, func(x object) bool { return x.IDs["AssociationId"] == param(a, "AssociationId") }), nil
	case "DeleteRouteTable":
		_, ok := find(param(a, "RouteTableId"))
		return !ok, nil
	}
	return false, nil
}

// --- security group ---------------------------------------------------------------------------

type groupKind struct{}

// Ingress is one rule: TCP to Port, from the security group FromGroup (by name) or from CIDR.
type Ingress struct {
	Port      int64  `json:"port" doc:"the TCP port"`
	FromGroup string `json:"fromGroup,omitempty" mamela:"ref=AwsSecurityGroup" doc:"from the members of this group"`
	CIDR      string `json:"cidr,omitempty" doc:"or from this CIDR block"`
}

type groupSpec struct {
	Boundary    string    `json:"boundary" doc:"the boundary (the guard's)"`
	Vpc         string    `json:"vpc" mamela:"ref=AwsVpc"`
	Description string    `json:"description"`
	Ingress     []Ingress `json:"ingress,omitempty" doc:"its ingress rules (Mamela adds rules, and never removes one)"`
}

// rule is an ingress rule as observed: "tcp/80 from sg-1" or "tcp/80 from 10.0.0.0/16".
type groupState struct {
	VpcID string   `json:"vpcId"`
	Rules []string `json:"rules,omitempty"`
}

func ruleText(port int64, from string) string { return fmt.Sprintf("tcp/%d from %s", port, from) }

func (groupKind) kind() protocol.ResourceKind { return KindSecurityGroup }
func (groupKind) reads() []protocol.Call      { return []protocol.Call{describeGroups} }
func (groupKind) operations() []protocol.Operation {
	return []protocol.Operation{op(OpCreate, createGroup), op(OpIngress, authorizeIngress),
		unsupportedOp(OpReplace, "a security group keeps its VPC, and Mamela never removes a rule: remove it by hand"), deleteOp(deleteGroup), adoptOp()}
}

func (k groupKind) list(c *client) ([]object, error) { return k.listBy(c, tagged()) }

// listBy lists with Describe parameters (a filter by ID finds an object that carries no tag).
func (groupKind) listBy(c *client, params map[string]string) ([]object, error) {
	var resp struct {
		Items []struct {
			ID    string `xml:"groupId"`
			VpcID string `xml:"vpcId"`
			Perms []struct {
				Protocol string `xml:"ipProtocol"`
				From     int64  `xml:"fromPort"`
				Groups   []struct {
					ID string `xml:"groupId"`
				} `xml:"groups>item"`
				Ranges []struct {
					CIDR string `xml:"cidrIp"`
				} `xml:"ipRanges>item"`
			} `xml:"ipPermissions>item"`
			Tags []xmlTag `xml:"tagSet>item"`
		} `xml:"securityGroupInfo>item"`
	}
	if err := c.query(APIEC2, "DescribeSecurityGroups", params, &resp); err != nil {
		return nil, err
	}
	var out []object
	for _, g := range resp.Items {
		st := groupState{VpcID: g.VpcID}
		for _, p := range g.Perms {
			for _, x := range p.Groups {
				st.Rules = append(st.Rules, ruleText(p.From, x.ID))
			}
			for _, x := range p.Ranges {
				st.Rules = append(st.Rules, ruleText(p.From, x.CIDR))
			}
		}
		slices.Sort(st.Rules)
		out = append(out, object{Name: tagOf(g.Tags, TagName), Boundary: tagOf(g.Tags, TagBoundary), IDs: map[string]string{"GroupId": g.ID}, State: st})
	}
	return out, nil
}

func (groupKind) missing(*client, protocol.Resource) (any, error) { return nil, nil }

func (groupKind) diff(r protocol.Resource, o protocol.Observed, deps map[string]protocol.Observed) ([]protocol.Change, error) {
	s, err := spec[groupSpec](r)
	if err != nil {
		return nil, err
	}
	vpc, err := dep(deps, KindVpc, s.Vpc)
	if err != nil {
		return nil, err
	}
	if !o.Exists {
		return []protocol.Change{createChange(r, KindSecurityGroup, "security group "+awsName(s.Boundary, r.Name)+" in VPC "+s.Vpc,
			queryReq(APIEC2, "CreateSecurityGroup", ec2Tags("security-group", s.Boundary, r.Name,
				map[string]string{"GroupName": awsName(s.Boundary, r.Name), "GroupDescription": s.Description, "VpcId": vpc.IDs["VpcId"]})))}, nil
	}
	id := o.IDs["GroupId"]
	st := state[groupState](o)
	if st.VpcID != vpc.IDs["VpcId"] {
		return []protocol.Change{unsupported(r, KindSecurityGroup, OpReplace, map[string]string{"GroupId": id}, "in VPC "+st.VpcID+", declared "+vpc.IDs["VpcId"])}, nil
	}
	params := map[string]string{"GroupId": id}
	var why []string
	n := 0
	for _, in := range s.Ingress {
		from, key := in.CIDR, "IpRanges.1.CidrIp"
		if in.FromGroup != "" {
			g, err := dep(deps, KindSecurityGroup, in.FromGroup)
			if err != nil {
				return nil, err
			}
			from, key = g.IDs["GroupId"], "Groups.1.GroupId"
		}
		if slices.Contains(st.Rules, ruleText(in.Port, from)) {
			continue
		}
		n++
		if n > maxIngressPerCall {
			return nil, fmt.Errorf("more than %d new ingress rules at once", maxIngressPerCall)
		}
		p := fmt.Sprintf("IpPermissions.%d.", n)
		params[p+"IpProtocol"], params[p+"FromPort"], params[p+"ToPort"], params[p+key] = "tcp", strconv.FormatInt(in.Port, 10), strconv.FormatInt(in.Port, 10), from
		label := in.CIDR
		if in.FromGroup != "" {
			label = "group " + in.FromGroup
		}
		why = append(why, fmt.Sprintf("allow TCP %d from %s", in.Port, label))
	}
	if n == 0 {
		return nil, nil
	}
	return []protocol.Change{{Resource: r.ID, Kind: KindSecurityGroup, Op: OpIngress, IDs: map[string]string{"GroupId": id}, Reasons: why,
		Requests: []protocol.Approved{queryReq(APIEC2, "AuthorizeSecurityGroupIngress", params)}}}, nil
}

func (groupKind) remove(o protocol.Observed) (protocol.Change, error) {
	id := o.IDs["GroupId"]
	return protocol.Change{Resource: o.ID, Kind: KindSecurityGroup, Op: OpDelete, IDs: map[string]string{"GroupId": id},
		Reasons:  []string{"delete security group " + id + " (what uses it must be deleted first)"},
		Requests: []protocol.Approved{queryReq(APIEC2, "DeleteSecurityGroup", map[string]string{"GroupId": id})}}, nil
}

func (k groupKind) done(c *client, ch protocol.Change, a protocol.Approved) (bool, error) {
	objs, err := k.list(c)
	if err != nil {
		return false, err
	}
	switch action(a) {
	case "CreateSecurityGroup":
		return slices.ContainsFunc(objs, func(x object) bool { return x.Name == nameOf(ch) }), nil
	case "AuthorizeSecurityGroupIngress":
		i := slices.IndexFunc(objs, func(x object) bool { return x.IDs["GroupId"] == param(a, "GroupId") })
		if i < 0 {
			return false, nil
		}
		rules := objs[i].State.(groupState).Rules
		for n := 1; n <= maxIngressPerCall; n++ {
			p := fmt.Sprintf("IpPermissions.%d.", n)
			port, _ := strconv.ParseInt(param(a, p+"FromPort"), 10, 64)
			from := param(a, p+"Groups.1.GroupId") + param(a, p+"IpRanges.1.CidrIp")
			if from != "" && !slices.Contains(rules, ruleText(port, from)) {
				return false, nil
			}
		}
		return true, nil
	case "DeleteSecurityGroup":
		return !slices.ContainsFunc(objs, func(x object) bool { return x.IDs["GroupId"] == param(a, "GroupId") }), nil
	}
	return false, nil
}
