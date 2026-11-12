package aws

import (
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"

	"github.com/ieyoukan/mamela/provider/kit"
	"github.com/ieyoukan/mamela/provider/protocol"
)

// Moving from Terraform (ADR 0009 of Mamela): `mamela import terraform` gives this provider the
// resources of a Terraform state; the network kinds turn theirs into declarations in the guard's
// boundary, and adopt (an explicit operation) tags each object with mamela:name and
// mamela:boundary, which makes it Mamela's. Nothing else of the object changes: what Mamela cannot
// declare (a rule it does not express, an object it does not make) is left as it is, or not
// imported at all, with why.

// OpAdopt is the explicit operation that adopts an imported object: it tags it.
const OpAdopt protocol.Op = "adopt"

var createTags = queryCall(APIEC2, "CreateTags", "ResourceId.1", "Tag.1.Key", "Tag.1.Value", "Tag.2.Key", "Tag.2.Value")

// adoptOp is the adopt operation of a kind tagged with CreateTags.
func adoptOp() protocol.Operation {
	o := explicitOp(OpAdopt, createTags)
	o.Adopts = true
	return o
}

// importer is a kind that imports Terraform's resources.
type importer interface {
	imports() []string
	spec() any
	doc() string
	importObject(o protocol.ImportObject, r kit.Resolver, boundary string) (kit.Imported, error)
	// idKey is the ID that names an object ("VpcId").
	idKey() string
	// find finds an object by its IDs, tagged or not.
	find(c *client, ids map[string]string) (*object, error)
	adopt(o protocol.Observed, desired protocol.Resource) (protocol.Change, error)
}

// tfValues are the attributes the network's Terraform resources have that the import reads.
type tfValues struct {
	ID               string   `json:"id"`
	VpcID            string   `json:"vpc_id"`
	CIDR             string   `json:"cidr_block"`
	Zone             string   `json:"availability_zone"`
	Description      string   `json:"description"`
	RouteTableID     string   `json:"route_table_id"`
	SubnetID         string   `json:"subnet_id"`
	GatewayID        string   `json:"gateway_id"`
	Destination      string   `json:"destination_cidr_block"`
	Route            []tfRule `json:"route"`
	Ingress          []tfRule `json:"ingress"`
	SecurityGroupID  string   `json:"security_group_id"`
	Type             string   `json:"type"`
	FromPort         *int64   `json:"from_port"`
	ToPort           *int64   `json:"to_port"`
	Protocol         string   `json:"protocol"`
	IPProtocol       string   `json:"ip_protocol"`
	CIDRBlocks       []string `json:"cidr_blocks"`
	CIDRIPv4         string   `json:"cidr_ipv4"`
	SourceGroup      string   `json:"source_security_group_id"`
	ReferencedGroup  string   `json:"referenced_security_group_id"`
	Self             bool     `json:"self"`
	InternetGateway  string   `json:"internet_gateway_id"`
	GatewayIDs       []string `json:"gateway_ids"`
	SecurityGroupIDs []string `json:"security_groups"`
}

// tfRule is a route or an ingress rule written inside its resource.
type tfRule struct {
	CIDR           string   `json:"cidr_block"`
	GatewayID      string   `json:"gateway_id"`
	FromPort       int64    `json:"from_port"`
	ToPort         int64    `json:"to_port"`
	Protocol       string   `json:"protocol"`
	CIDRBlocks     []string `json:"cidr_blocks"`
	SecurityGroups []string `json:"security_groups"`
	Self           bool     `json:"self"`
}

func values(o protocol.ImportObject) (tfValues, error) {
	var v tfValues
	if err := json.Unmarshal(o.Values, &v); err != nil {
		return v, fmt.Errorf("the attributes of %s: %w", o.Address, err)
	}
	if v.ID == "" {
		return v, kit.Skip("no id: the state does not say which object it is")
	}
	return v, nil
}

// refer is the declaration of an imported object a reference names, or why there is none.
func refer(r kit.Resolver, tfType, id, what string) (string, error) {
	if id == "" {
		return "", kit.Skip("it has no " + what)
	}
	n, ok := r.Name(tfType, id)
	if !ok {
		return "", kit.Skip(fmt.Sprintf("its %s %s is not imported: import it too (a %s of the same state)", what, id, tfType))
	}
	return n, nil
}

// findBy finds one object of a kind by a Describe filter on its ID.
func findBy(c *client, k interface {
	listBy(*client, map[string]string) ([]object, error)
}, filter, id string) (*object, error) {
	if id == "" {
		return nil, nil
	}
	objs, err := k.listBy(c, map[string]string{"Filter.1.Name": filter, "Filter.1.Value.1": id})
	if err != nil || len(objs) == 0 {
		return nil, err
	}
	return &objs[0], nil
}

// tagChange is the adopt change: tag the object (named by its ID parameter) with the declaration's
// name and boundary. Nothing else is sent.
func tagChange(o protocol.Observed, desired protocol.Resource, idKey string) (protocol.Change, error) {
	var b base
	if err := json.Unmarshal(desired.Spec, &b); err != nil || b.Boundary == "" {
		return protocol.Change{}, fmt.Errorf("%s: the spec has no boundary", o.ID)
	}
	id := o.IDs[idKey]
	if id == "" {
		return protocol.Change{}, fmt.Errorf("%s: no %s observed", o.ID, idKey)
	}
	return protocol.Change{Resource: o.ID, Kind: o.Kind, Op: OpAdopt, IDs: map[string]string{idKey: id, "ResourceId.1": id},
		Reasons: []string{fmt.Sprintf("adopt %s: tag it %s=%s and %s=%s, which makes it Mamela's; nothing else changes", id, TagName, desired.Name, TagBoundary, b.Boundary)},
		Requests: []protocol.Approved{queryReq(APIEC2, "CreateTags", map[string]string{"ResourceId.1": id,
			"Tag.1.Key": TagName, "Tag.1.Value": desired.Name, "Tag.2.Key": TagBoundary, "Tag.2.Value": b.Boundary})}}, nil
}

// adopted says whether an adopt's tags are on the object: it is listed (tagged) under the name.
func adopted(c *client, k kind, ch protocol.Change, a protocol.Approved) (bool, error) {
	objs, err := k.list(c)
	if err != nil {
		return false, err
	}
	id, name := param(a, "ResourceId.1"), nameOf(ch)
	return slices.ContainsFunc(objs, func(x object) bool {
		return x.Name == name && slices.Contains(mapValues(x.IDs), id)
	}), nil
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// --- VPC ---

func (vpcKind) idKey() string { return "VpcId" }

func (vpcKind) imports() []string { return []string{"aws_vpc"} }
func (vpcKind) spec() any         { return vpcSpec{} }
func (vpcKind) doc() string       { return "A VPC of the boundary's own." }

func (vpcKind) importObject(o protocol.ImportObject, _ kit.Resolver, boundary string) (kit.Imported, error) {
	v, err := values(o)
	if err != nil {
		return kit.Imported{}, err
	}
	return kit.Imported{Spec: vpcSpec{Boundary: boundary, CIDR: v.CIDR}, IDs: map[string]string{"VpcId": v.ID}}, nil
}

func (k vpcKind) find(c *client, ids map[string]string) (*object, error) {
	return findBy(c, k, "vpc-id", ids["VpcId"])
}

func (vpcKind) adopt(o protocol.Observed, d protocol.Resource) (protocol.Change, error) {
	return tagChange(o, d, "VpcId")
}

// --- internet gateway ---

func (gatewayKind) idKey() string { return "InternetGatewayId" }

func (gatewayKind) imports() []string { return []string{"aws_internet_gateway"} }
func (gatewayKind) spec() any         { return gatewaySpec{} }
func (gatewayKind) doc() string       { return "The VPC's internet gateway." }

func (gatewayKind) importObject(o protocol.ImportObject, r kit.Resolver, boundary string) (kit.Imported, error) {
	v, err := values(o)
	if err != nil {
		return kit.Imported{}, err
	}
	vpc, err := refer(r, "aws_vpc", v.VpcID, "VPC")
	if err != nil {
		return kit.Imported{}, err
	}
	return kit.Imported{Spec: gatewaySpec{Boundary: boundary, Vpc: vpc}, IDs: map[string]string{"InternetGatewayId": v.ID}}, nil
}

func (k gatewayKind) find(c *client, ids map[string]string) (*object, error) {
	return findBy(c, k, "internet-gateway-id", ids["InternetGatewayId"])
}

func (gatewayKind) adopt(o protocol.Observed, d protocol.Resource) (protocol.Change, error) {
	return tagChange(o, d, "InternetGatewayId")
}

// --- subnet ---

func (subnetKind) idKey() string { return "SubnetId" }

func (subnetKind) imports() []string { return []string{"aws_subnet"} }
func (subnetKind) spec() any         { return subnetSpec{} }
func (subnetKind) doc() string       { return "A subnet in one availability zone." }

func (subnetKind) importObject(o protocol.ImportObject, r kit.Resolver, boundary string) (kit.Imported, error) {
	v, err := values(o)
	if err != nil {
		return kit.Imported{}, err
	}
	vpc, err := refer(r, "aws_vpc", v.VpcID, "VPC")
	if err != nil {
		return kit.Imported{}, err
	}
	return kit.Imported{Spec: subnetSpec{Boundary: boundary, Vpc: vpc, CIDR: v.CIDR, Zone: v.Zone}, IDs: map[string]string{"SubnetId": v.ID}}, nil
}

func (k subnetKind) find(c *client, ids map[string]string) (*object, error) {
	return findBy(c, k, "subnet-id", ids["SubnetId"])
}

func (subnetKind) adopt(o protocol.Observed, d protocol.Resource) (protocol.Change, error) {
	return tagChange(o, d, "SubnetId")
}

// --- route table ---

func (routeTableKind) idKey() string { return "RouteTableId" }

func (routeTableKind) imports() []string {
	return []string{"aws_route_table", "aws_route_table_association", "aws_route"}
}
func (routeTableKind) spec() any { return routeTableSpec{} }
func (routeTableKind) doc() string {
	return "A route table that routes 0.0.0.0/0 to the internet gateway, for one subnet: a public subnet."
}

// importObject declares a route table with its associations and routes (aws_route_table_association,
// aws_route: they move with it). Mamela's route table routes 0.0.0.0/0 to an internet gateway
// and serves one subnet; any other is not imported.
func (routeTableKind) importObject(o protocol.ImportObject, r kit.Resolver, boundary string) (kit.Imported, error) {
	v, err := values(o)
	if err != nil {
		return kit.Imported{}, err
	}
	switch o.Type {
	case "aws_route_table_association", "aws_route":
		return kit.Imported{}, kit.Skip("not part of an imported route table: import the route table (aws_route_table) of the same state")
	}
	vpc, err := refer(r, "aws_vpc", v.VpcID, "VPC")
	if err != nil {
		return kit.Imported{}, err
	}
	var also []string
	gateways := map[string]bool{}
	for _, rt := range v.Route {
		if rt.CIDR == "0.0.0.0/0" && strings.HasPrefix(rt.GatewayID, "igw-") {
			gateways[rt.GatewayID] = true
		}
	}
	for _, x := range r.Objects("aws_route") {
		var rv tfValues
		if json.Unmarshal(x.Values, &rv) == nil && rv.RouteTableID == v.ID {
			if rv.Destination != "0.0.0.0/0" || !strings.HasPrefix(rv.GatewayID, "igw-") {
				return kit.Imported{}, kit.Skip(fmt.Sprintf("its route %s (to %s) is not 0.0.0.0/0 to an internet gateway, the only route Mamela's route table has", rv.Destination, rv.GatewayID))
			}
			gateways[rv.GatewayID] = true
			also = append(also, x.Address)
		}
	}
	var subnets []string
	for _, x := range r.Objects("aws_route_table_association") {
		var av tfValues
		if json.Unmarshal(x.Values, &av) == nil && av.RouteTableID == v.ID {
			subnets = append(subnets, av.SubnetID)
			also = append(also, x.Address)
		}
	}
	if len(gateways) != 1 {
		return kit.Imported{}, kit.Skip(fmt.Sprintf("it routes 0.0.0.0/0 to %d internet gateways; Mamela's route table routes it to one", len(gateways)))
	}
	if len(v.Route) > len(gateways) {
		return kit.Imported{}, kit.Skip("it has routes besides 0.0.0.0/0 to the internet gateway, which Mamela's route table does not have")
	}
	if len(subnets) != 1 {
		return kit.Imported{}, kit.Skip(fmt.Sprintf("it serves %d subnets (aws_route_table_association); Mamela's route table serves one", len(subnets)))
	}
	var gwID string
	for id := range gateways {
		gwID = id
	}
	gw, err := refer(r, "aws_internet_gateway", gwID, "internet gateway")
	if err != nil {
		return kit.Imported{}, err
	}
	subnet, err := refer(r, "aws_subnet", subnets[0], "subnet")
	if err != nil {
		return kit.Imported{}, err
	}
	slices.Sort(also)
	return kit.Imported{Spec: routeTableSpec{Boundary: boundary, Vpc: vpc, InternetGateway: gw, Subnet: subnet},
		IDs: map[string]string{"RouteTableId": v.ID}, Also: also}, nil
}

func (k routeTableKind) find(c *client, ids map[string]string) (*object, error) {
	return findBy(c, k, "route-table-id", ids["RouteTableId"])
}

func (routeTableKind) adopt(o protocol.Observed, d protocol.Resource) (protocol.Change, error) {
	return tagChange(o, d, "RouteTableId")
}

// --- security group ---

func (groupKind) idKey() string { return "GroupId" }

func (groupKind) imports() []string {
	return []string{"aws_security_group", "aws_security_group_rule", "aws_vpc_security_group_ingress_rule"}
}
func (groupKind) spec() any { return groupSpec{} }
func (groupKind) doc() string {
	return "A security group and its ingress rules. Mamela adds rules, and never removes one."
}

// importObject declares a security group with the ingress rules Mamela expresses: TCP to one port,
// from a CIDR block or from another imported group. The rules it does not express (egress, other
// protocols, port ranges, from itself) stay as they are: Mamela never removes a rule. Rules written
// as their own resources move with the group.
func (groupKind) importObject(o protocol.ImportObject, r kit.Resolver, boundary string) (kit.Imported, error) {
	v, err := values(o)
	if err != nil {
		return kit.Imported{}, err
	}
	if o.Type != "aws_security_group" {
		return kit.Imported{}, kit.Skip("not a rule of an imported security group that Mamela expresses (TCP to one port, from a CIDR block or another imported group): it stays as it is")
	}
	vpc, err := refer(r, "aws_vpc", v.VpcID, "VPC")
	if err != nil {
		return kit.Imported{}, err
	}
	s := groupSpec{Boundary: boundary, Vpc: vpc, Description: v.Description}
	// add adds a rule Mamela expresses, and says whether it is one.
	add := func(proto string, from, to int64, cidr, group string) bool {
		if proto != "tcp" && proto != "6" || from != to || from <= 0 {
			return false
		}
		var in Ingress
		switch {
		case cidr != "":
			in = Ingress{Port: from, CIDR: cidr}
		case group != "" && group != v.ID: // from itself would refer to its own declaration
			name, ok := r.Name("aws_security_group", group)
			if !ok {
				return false
			}
			in = Ingress{Port: from, FromGroup: name}
		default:
			return false
		}
		if !slices.Contains(s.Ingress, in) {
			s.Ingress = append(s.Ingress, in)
		}
		return true
	}
	for _, rule := range v.Ingress {
		for _, c := range rule.CIDRBlocks {
			add(rule.Protocol, rule.FromPort, rule.ToPort, c, "")
		}
		for _, g := range rule.SecurityGroups {
			add(rule.Protocol, rule.FromPort, rule.ToPort, "", g)
		}
	}
	var also []string
	for _, x := range append(r.Objects("aws_security_group_rule"), r.Objects("aws_vpc_security_group_ingress_rule")...) {
		var rv tfValues
		if json.Unmarshal(x.Values, &rv) != nil || rv.SecurityGroupID != v.ID || x.Type == "aws_security_group_rule" && rv.Type != "ingress" {
			continue
		}
		from, to := int64(-1), int64(-2)
		if rv.FromPort != nil && rv.ToPort != nil {
			from, to = *rv.FromPort, *rv.ToPort
		}
		proto := rv.Protocol + rv.IPProtocol
		ok := true
		switch {
		case len(rv.CIDRBlocks) > 0:
			for _, c := range rv.CIDRBlocks {
				ok = add(proto, from, to, c, "") && ok
			}
		case rv.CIDRIPv4 != "":
			ok = add(proto, from, to, rv.CIDRIPv4, "")
		default:
			ok = add(proto, from, to, "", rv.SourceGroup+rv.ReferencedGroup)
		}
		if ok {
			also = append(also, x.Address)
		}
	}
	slices.Sort(also)
	return kit.Imported{Spec: s, IDs: map[string]string{"GroupId": v.ID}, Also: also}, nil
}

func (k groupKind) find(c *client, ids map[string]string) (*object, error) {
	return findBy(c, k, "group-id", ids["GroupId"])
}

func (groupKind) adopt(o protocol.Observed, d protocol.Resource) (protocol.Change, error) {
	return tagChange(o, d, "GroupId")
}
