// Package fakeaws is a fake of the parts of AWS the provider uses, for its tests: EC2 and IAM
// (query APIs, XML answers) and ECS, CloudWatch Logs and SSM (JSON APIs). It keeps one region of
// one account in memory, answers lists a page of two at a time (so that paging is exercised), and
// refuses what AWS refuses (deleting a VPC that still has subnets, a cluster that still has
// instances, a role that still has policies).
package fakeaws

import (
	"encoding/base64"
	"encoding/json/v2"
	"encoding/xml"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Account and Region of the fake.
const (
	Account = "123456789012"
	Region  = "ap-northeast-1"
	// Images are the ECS-optimized images the public SSM parameters name.
	ImageX86   = "ami-0ecsx86fake"
	ImageArm64 = "ami-0ecsarm64fake"
)

const pageSize = 2

type tags map[string]string

type vpc struct {
	id, cidr string
	tags     tags
}

type gateway struct {
	id, vpc string
	tags    tags
}

type subnet struct {
	id, vpc, cidr, zone string
	tags                tags
	next                int
}

type routeTable struct {
	id, vpc            string
	routes             map[string]string // destination -> gateway
	assocID, assocWith string
	tags               tags
}

type rule struct {
	port       int64
	group, ip  string
	ruleString string
}

type group struct {
	id, vpc, name string
	rules         []rule
	tags          tags
}

type instance struct {
	id, typ, image, ip, token, state, subnet, profile, cluster string
	groups                                                     []string
	tags                                                       tags
}

type role struct {
	name, trust string
	policies    []string
	tags        tags
}

type profile struct {
	name  string
	roles []string
}

type cluster struct {
	name, status string
	tags         []map[string]string
}

type taskDef struct {
	family     string
	revision   int64
	status     string
	tags       []map[string]string
	compatible []string
}

type service struct {
	cluster, name, status, taskDef, launch string
	desired                                int64
	strategy                               []map[string]any
	network                                map[string]any
	tags                                   []map[string]string
}

type task struct {
	arn, cluster, taskDef, startedBy, token, desired, last, reason string
	tags                                                           []map[string]string
}

type logGroup struct {
	name      string
	retention int64
	tags      map[string]string
}

// API is the fake. Its zero value is not usable: use New.
type API struct {
	mu       sync.Mutex
	n        int
	vpcs     []*vpc
	gateways []*gateway
	subnets  []*subnet
	routes   []*routeTable
	groups   []*group
	insts    []*instance
	roles    []*role
	profiles []*profile
	clusters []*cluster
	taskDefs []*taskDef
	services []*service
	tasks    []*task
	logs     []*logGroup
	// Requests counts the requests of each action ("ec2:RunInstances").
	Requests map[string]int
}

// New returns an empty account.
func New() *API { return &API{Requests: map[string]int{}} }

// AddForeignInstance adds an EC2 instance that is not Mamela's (someone else's).
func (a *API) AddForeignInstance(id, state string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.insts = append(a.insts, &instance{id: id, typ: "m6i.large", image: "ami-other", state: state, tags: tags{"Name": "someone-else"}})
}

// AddForeignCluster adds an ECS cluster that is not Mamela's.
func (a *API) AddForeignCluster(name string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.clusters = append(a.clusters, &cluster{name: name, status: "ACTIVE"})
}

// AddForeignVpc adds a VPC that is not Mamela's and returns its ID.
func (a *API) AddForeignVpc() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	v := &vpc{id: a.id("vpc"), cidr: "172.31.0.0/16"}
	a.vpcs = append(a.vpcs, v)
	return v.id
}

// SetInstanceState sets an instance's state (to stop one, as someone might).
func (a *API) SetInstanceState(id, state string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, i := range a.insts {
		if i.id == id {
			i.state = state
		}
	}
}

// Instances returns the instances' IDs by state.
func (a *API) Instances() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]string{}
	for _, i := range a.insts {
		out[i.id] = i.state
	}
	return out
}

// Tasks returns run tasks: their ARNs by last status.
func (a *API) Tasks() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]string{}
	for _, t := range a.tasks {
		out[t.arn] = t.last
	}
	return out
}

// Empty says whether the account has nothing of Mamela's left (everything deleted).
func (a *API) Empty() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var left []string
	for _, v := range a.vpcs {
		if v.tags != nil {
			left = append(left, v.id)
		}
	}
	for _, x := range a.subnets {
		left = append(left, x.id)
	}
	for _, x := range a.gateways {
		left = append(left, x.id)
	}
	for _, x := range a.routes {
		left = append(left, x.id)
	}
	for _, x := range a.groups {
		left = append(left, x.id)
	}
	for _, x := range a.insts {
		if x.tags["mamela:name"] != "" && x.state != "terminated" {
			left = append(left, x.id)
		}
	}
	for _, x := range a.roles {
		left = append(left, x.name)
	}
	for _, x := range a.profiles {
		left = append(left, x.name)
	}
	for _, x := range a.clusters {
		if x.status == "ACTIVE" && x.tags != nil {
			left = append(left, x.name)
		}
	}
	for _, x := range a.services {
		if x.status == "ACTIVE" {
			left = append(left, x.name)
		}
	}
	for _, x := range a.taskDefs {
		if x.status == "ACTIVE" {
			left = append(left, fmt.Sprintf("%s:%d", x.family, x.revision))
		}
	}
	for _, x := range a.logs {
		left = append(left, x.name)
	}
	return left
}

func (a *API) id(prefix string) string {
	a.n++
	return fmt.Sprintf("%s-%017x", prefix, a.n)
}

// apiError is an AWS refusal.
type apiError struct {
	status     int
	code, text string
}

func (e *apiError) Error() string { return e.code + ": " + e.text }

func fail(status int, code, format string, args ...any) error {
	return &apiError{status: status, code: code, text: fmt.Sprintf(format, args...)}
}

// ServeHTTP implements http.Handler.
func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	api := r.Header.Get("X-Mamela-Api")
	if t := r.Header.Get("X-Amz-Target"); t != "" {
		prefix, act, _ := strings.Cut(t, ".")
		want := map[string]string{"AmazonEC2ContainerServiceV20141113": "ecs", "Logs_20140328": "logs", "AmazonSSM": "ssm"}[prefix]
		if want == "" || api != "" && api != want || r.Header.Get("Content-Type") != "application/x-amz-json-1.1" {
			http.Error(w, "unknown target "+t+" for api "+api, http.StatusBadRequest)
			return
		}
		var in map[string]any
		if err := json.Unmarshal(body, &in); err != nil {
			http.Error(w, "the body is not a JSON object: "+err.Error(), http.StatusBadRequest)
			return
		}
		a.Requests[want+":"+act]++
		var out any
		var err error
		switch want {
		case "ecs":
			out, err = a.ecs(act, in)
		case "logs":
			out, err = a.logsCall(act, in)
		case "ssm":
			out, err = a.ssm(act, in)
		}
		if err != nil {
			e := err.(*apiError)
			w.Header().Set("Content-Type", "application/x-amz-json-1.1")
			w.WriteHeader(e.status)
			b, _ := json.Marshal(map[string]string{"__type": e.code, "message": e.text})
			w.Write(b)
			return
		}
		b, err := json.Marshal(out)
		if err != nil {
			panic(err)
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.1")
		w.Write(b)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p := map[string]string{}
	for k, v := range form {
		if len(v) != 1 {
			http.Error(w, "parameter "+k+" given twice", http.StatusBadRequest)
			return
		}
		p[k] = v[0]
	}
	act := p["Action"]
	var out any
	switch p["Version"] {
	case "2016-11-15":
		if api != "" && api != "ec2" {
			http.Error(w, "EC2 action on api "+api, http.StatusBadRequest)
			return
		}
		a.Requests["ec2:"+act]++
		out, err = a.ec2(act, p)
	case "2010-05-08":
		if api != "" && api != "iam" {
			http.Error(w, "IAM action on api "+api, http.StatusBadRequest)
			return
		}
		a.Requests["iam:"+act]++
		out, err = a.iam(act, p)
	default:
		http.Error(w, "unknown version "+p["Version"], http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/xml")
	if err != nil {
		e := err.(*apiError)
		w.WriteHeader(e.status)
		if p["Version"] == "2010-05-08" {
			fmt.Fprintf(w, "<ErrorResponse><Error><Code>%s</Code><Message>%s</Message></Error></ErrorResponse>", e.code, xmlText(e.text))
		} else {
			fmt.Fprintf(w, "<Response><Errors><Error><Code>%s</Code><Message>%s</Message></Error></Errors></Response>", e.code, xmlText(e.text))
		}
		return
	}
	b, err := xml.Marshal(out)
	if err != nil {
		panic(err)
	}
	w.Write(b)
}

func xmlText(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// --- EC2 --------------------------------------------------------------------------------------

type xmlTag struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

func xmlTags(t tags) []xmlTag {
	var out []xmlTag
	for _, k := range slices.Sorted(maps.Keys(t)) {
		out = append(out, xmlTag{k, t[k]})
	}
	return out
}

// tagSpec reads the TagSpecification of a create.
func tagSpec(p map[string]string, resourceType string) (tags, error) {
	if p["TagSpecification.1.ResourceType"] != resourceType {
		return nil, fail(400, "InvalidParameterValue", "TagSpecification.1.ResourceType must be %s", resourceType)
	}
	t := tags{}
	for i := 1; ; i++ {
		k, ok := p[fmt.Sprintf("TagSpecification.1.Tag.%d.Key", i)]
		if !ok {
			return t, nil
		}
		t[k] = p[fmt.Sprintf("TagSpecification.1.Tag.%d.Value", i)]
	}
}

// filter is one Describe filter.
type filter struct {
	name   string
	values []string
}

func filters(p map[string]string) ([]filter, error) {
	var out []filter
	for i := 1; ; i++ {
		name, ok := p[fmt.Sprintf("Filter.%d.Name", i)]
		if !ok {
			return out, nil
		}
		f := filter{name: name}
		for j := 1; ; j++ {
			v, ok := p[fmt.Sprintf("Filter.%d.Value.%d", i, j)]
			if !ok {
				break
			}
			f.values = append(f.values, v)
		}
		if len(f.values) == 0 {
			return nil, fail(400, "InvalidParameterValue", "filter %s has no value", name)
		}
		out = append(out, f)
	}
}

// matches says whether an object passes the filters (tag-key, and the given others).
func matches(fs []filter, t tags, other map[string]string) bool {
	for _, f := range fs {
		switch f.name {
		case "tag-key":
			if !slices.ContainsFunc(f.values, func(k string) bool { _, ok := t[k]; return ok }) {
				return false
			}
		default:
			v, ok := other[f.name]
			if !ok {
				panic("fakeaws: filter " + f.name + " is not supported")
			}
			if !slices.Contains(f.values, v) {
				return false
			}
		}
	}
	return true
}

type ec2Result struct {
	XMLName xml.Name
	Return  bool `xml:"return"`
}

func ok(action string) ec2Result {
	return ec2Result{XMLName: xml.Name{Local: action + "Response"}, Return: true}
}

func (a *API) need(p map[string]string, keys ...string) error {
	for _, k := range keys {
		if p[k] == "" {
			return fail(400, "MissingParameter", "the request must contain the parameter %s", k)
		}
	}
	return nil
}

func (a *API) findVpc(id string) *vpc {
	for _, v := range a.vpcs {
		if v.id == id {
			return v
		}
	}
	return nil
}

func (a *API) findSubnet(id string) *subnet {
	for _, s := range a.subnets {
		if s.id == id {
			return s
		}
	}
	return nil
}

func (a *API) findGroup(id string) *group {
	for _, g := range a.groups {
		if g.id == id {
			return g
		}
	}
	return nil
}

func (a *API) ec2(act string, p map[string]string) (any, error) {
	fs, err := filters(p)
	if err != nil {
		return nil, err
	}
	switch act {
	case "DescribeVpcs":
		type item struct {
			ID   string   `xml:"vpcId"`
			CIDR string   `xml:"cidrBlock"`
			Tags []xmlTag `xml:"tagSet>item"`
		}
		var items []item
		for _, v := range a.vpcs {
			if matches(fs, v.tags, nil) {
				items = append(items, item{v.id, v.cidr, xmlTags(v.tags)})
			}
		}
		return struct {
			XMLName xml.Name `xml:"DescribeVpcsResponse"`
			Items   []item   `xml:"vpcSet>item"`
		}{Items: items}, nil
	case "CreateVpc":
		t, err := tagSpec(p, "vpc")
		if err != nil {
			return nil, err
		}
		if err := a.need(p, "CidrBlock"); err != nil {
			return nil, err
		}
		v := &vpc{id: a.id("vpc"), cidr: p["CidrBlock"], tags: t}
		a.vpcs = append(a.vpcs, v)
		// AWS makes a main route table and a default security group of its own; they carry no tag.
		return struct {
			XMLName xml.Name `xml:"CreateVpcResponse"`
			ID      string   `xml:"vpc>vpcId"`
		}{ID: v.id}, nil
	case "DeleteVpc":
		v := a.findVpc(p["VpcId"])
		if v == nil {
			return nil, fail(400, "InvalidVpcID.NotFound", "the vpc ID '%s' does not exist", p["VpcId"])
		}
		for _, s := range a.subnets {
			if s.vpc == v.id {
				return nil, fail(400, "DependencyViolation", "the vpc '%s' has dependencies and cannot be deleted", v.id)
			}
		}
		for _, g := range a.gateways {
			if g.vpc == v.id {
				return nil, fail(400, "DependencyViolation", "the vpc '%s' has an internet gateway attached", v.id)
			}
		}
		for _, x := range a.routes {
			if x.vpc == v.id {
				return nil, fail(400, "DependencyViolation", "the vpc '%s' has a route table", v.id)
			}
		}
		for _, x := range a.groups {
			if x.vpc == v.id {
				return nil, fail(400, "DependencyViolation", "the vpc '%s' has a security group", v.id)
			}
		}
		a.vpcs = slices.DeleteFunc(a.vpcs, func(x *vpc) bool { return x == v })
		return ok(act), nil

	case "DescribeInternetGateways":
		type attachment struct {
			VpcID string `xml:"vpcId"`
			State string `xml:"state"`
		}
		type item struct {
			ID          string       `xml:"internetGatewayId"`
			Attachments []attachment `xml:"attachmentSet>item"`
			Tags        []xmlTag     `xml:"tagSet>item"`
		}
		var items []item
		for _, g := range a.gateways {
			if matches(fs, g.tags, nil) {
				it := item{ID: g.id, Tags: xmlTags(g.tags)}
				if g.vpc != "" {
					it.Attachments = []attachment{{g.vpc, "available"}}
				}
				items = append(items, it)
			}
		}
		return struct {
			XMLName xml.Name `xml:"DescribeInternetGatewaysResponse"`
			Items   []item   `xml:"internetGatewaySet>item"`
		}{Items: items}, nil
	case "CreateInternetGateway":
		t, err := tagSpec(p, "internet-gateway")
		if err != nil {
			return nil, err
		}
		g := &gateway{id: a.id("igw"), tags: t}
		a.gateways = append(a.gateways, g)
		return ok(act), nil
	case "AttachInternetGateway", "DetachInternetGateway", "DeleteInternetGateway":
		i := slices.IndexFunc(a.gateways, func(g *gateway) bool { return g.id == p["InternetGatewayId"] })
		if i < 0 {
			return nil, fail(400, "InvalidInternetGatewayID.NotFound", "the internet gateway ID '%s' does not exist", p["InternetGatewayId"])
		}
		g := a.gateways[i]
		switch act {
		case "AttachInternetGateway":
			if a.findVpc(p["VpcId"]) == nil {
				return nil, fail(400, "InvalidVpcID.NotFound", "the vpc ID '%s' does not exist", p["VpcId"])
			}
			if g.vpc != "" {
				return nil, fail(400, "Resource.AlreadyAssociated", "resource %s is already associated with network %s", g.id, g.vpc)
			}
			g.vpc = p["VpcId"]
		case "DetachInternetGateway":
			if g.vpc != p["VpcId"] {
				return nil, fail(400, "Gateway.NotAttached", "resource %s is not attached to network %s", g.id, p["VpcId"])
			}
			g.vpc = ""
		default:
			if g.vpc != "" {
				return nil, fail(400, "DependencyViolation", "the internet gateway '%s' has dependencies and cannot be deleted", g.id)
			}
			a.gateways = slices.Delete(a.gateways, i, i+1)
		}
		return ok(act), nil

	case "DescribeSubnets":
		type item struct {
			ID    string   `xml:"subnetId"`
			VpcID string   `xml:"vpcId"`
			CIDR  string   `xml:"cidrBlock"`
			Zone  string   `xml:"availabilityZone"`
			Tags  []xmlTag `xml:"tagSet>item"`
		}
		var items []item
		for _, s := range a.subnets {
			if matches(fs, s.tags, nil) {
				items = append(items, item{s.id, s.vpc, s.cidr, s.zone, xmlTags(s.tags)})
			}
		}
		return struct {
			XMLName xml.Name `xml:"DescribeSubnetsResponse"`
			Items   []item   `xml:"subnetSet>item"`
		}{Items: items}, nil
	case "CreateSubnet":
		t, err := tagSpec(p, "subnet")
		if err != nil {
			return nil, err
		}
		if err := a.need(p, "VpcId", "CidrBlock", "AvailabilityZone"); err != nil {
			return nil, err
		}
		if a.findVpc(p["VpcId"]) == nil {
			return nil, fail(400, "InvalidVpcID.NotFound", "the vpc ID '%s' does not exist", p["VpcId"])
		}
		a.subnets = append(a.subnets, &subnet{id: a.id("subnet"), vpc: p["VpcId"], cidr: p["CidrBlock"], zone: p["AvailabilityZone"], tags: t})
		return ok(act), nil
	case "DeleteSubnet":
		s := a.findSubnet(p["SubnetId"])
		if s == nil {
			return nil, fail(400, "InvalidSubnetID.NotFound", "the subnet ID '%s' does not exist", p["SubnetId"])
		}
		for _, i := range a.insts {
			if i.subnet == s.id && i.state != "terminated" {
				return nil, fail(400, "DependencyViolation", "the subnet '%s' has dependencies and cannot be deleted", s.id)
			}
		}
		for _, x := range a.routes {
			if x.assocWith == s.id {
				return nil, fail(400, "DependencyViolation", "the subnet '%s' is associated with route table %s", s.id, x.id)
			}
		}
		a.subnets = slices.DeleteFunc(a.subnets, func(x *subnet) bool { return x == s })
		return ok(act), nil

	case "DescribeRouteTables":
		type route struct {
			Destination string `xml:"destinationCidrBlock"`
			Gateway     string `xml:"gatewayId"`
		}
		type assoc struct {
			ID     string `xml:"routeTableAssociationId"`
			Subnet string `xml:"subnetId"`
		}
		type item struct {
			ID           string   `xml:"routeTableId"`
			Routes       []route  `xml:"routeSet>item"`
			Associations []assoc  `xml:"associationSet>item"`
			Tags         []xmlTag `xml:"tagSet>item"`
		}
		var items []item
		for _, x := range a.routes {
			if !matches(fs, x.tags, nil) {
				continue
			}
			v := a.findVpc(x.vpc)
			it := item{ID: x.id, Routes: []route{{v.cidr, "local"}}, Tags: xmlTags(x.tags)}
			for _, d := range slices.Sorted(maps.Keys(x.routes)) {
				it.Routes = append(it.Routes, route{d, x.routes[d]})
			}
			if x.assocID != "" {
				it.Associations = []assoc{{x.assocID, x.assocWith}}
			}
			items = append(items, it)
		}
		return struct {
			XMLName xml.Name `xml:"DescribeRouteTablesResponse"`
			Items   []item   `xml:"routeTableSet>item"`
		}{Items: items}, nil
	case "CreateRouteTable":
		t, err := tagSpec(p, "route-table")
		if err != nil {
			return nil, err
		}
		if a.findVpc(p["VpcId"]) == nil {
			return nil, fail(400, "InvalidVpcID.NotFound", "the vpc ID '%s' does not exist", p["VpcId"])
		}
		a.routes = append(a.routes, &routeTable{id: a.id("rtb"), vpc: p["VpcId"], routes: map[string]string{}, tags: t})
		return ok(act), nil
	case "CreateRoute", "AssociateRouteTable", "DeleteRouteTable":
		i := slices.IndexFunc(a.routes, func(x *routeTable) bool { return x.id == p["RouteTableId"] })
		if i < 0 {
			return nil, fail(400, "InvalidRouteTableID.NotFound", "the route table ID '%s' does not exist", p["RouteTableId"])
		}
		x := a.routes[i]
		switch act {
		case "CreateRoute":
			gw := slices.IndexFunc(a.gateways, func(g *gateway) bool { return g.id == p["GatewayId"] })
			if gw < 0 || a.gateways[gw].vpc != x.vpc {
				return nil, fail(400, "InvalidGatewayID.NotFound", "the gateway ID '%s' does not exist in the route table's network", p["GatewayId"])
			}
			if _, dup := x.routes[p["DestinationCidrBlock"]]; dup {
				return nil, fail(400, "RouteAlreadyExists", "the route identified by %s already exists", p["DestinationCidrBlock"])
			}
			x.routes[p["DestinationCidrBlock"]] = p["GatewayId"]
		case "AssociateRouteTable":
			s := a.findSubnet(p["SubnetId"])
			if s == nil || s.vpc != x.vpc {
				return nil, fail(400, "InvalidSubnetID.NotFound", "the subnet ID '%s' does not exist in the route table's network", p["SubnetId"])
			}
			if x.assocID != "" {
				return nil, fail(400, "Resource.AlreadyAssociated", "the route table %s is already associated", x.id)
			}
			x.assocID, x.assocWith = a.id("rtbassoc"), s.id
			return struct {
				XMLName xml.Name `xml:"AssociateRouteTableResponse"`
				ID      string   `xml:"associationId"`
			}{ID: x.assocID}, nil
		default:
			if x.assocID != "" {
				return nil, fail(400, "DependencyViolation", "the routeTable '%s' has dependencies and cannot be deleted", x.id)
			}
			a.routes = slices.Delete(a.routes, i, i+1)
		}
		return ok(act), nil
	case "DisassociateRouteTable":
		for _, x := range a.routes {
			if x.assocID != "" && x.assocID == p["AssociationId"] {
				x.assocID, x.assocWith = "", ""
				return ok(act), nil
			}
		}
		return nil, fail(400, "InvalidAssociationID.NotFound", "the association ID '%s' does not exist", p["AssociationId"])

	case "DescribeSecurityGroups":
		type idItem struct {
			GroupID string `xml:"groupId"`
		}
		type ipItem struct {
			CIDR string `xml:"cidrIp"`
		}
		type perm struct {
			Protocol string   `xml:"ipProtocol"`
			From     int64    `xml:"fromPort"`
			To       int64    `xml:"toPort"`
			Groups   []idItem `xml:"groups>item"`
			Ranges   []ipItem `xml:"ipRanges>item"`
		}
		type item struct {
			ID    string   `xml:"groupId"`
			Name  string   `xml:"groupName"`
			VpcID string   `xml:"vpcId"`
			Perms []perm   `xml:"ipPermissions>item"`
			Tags  []xmlTag `xml:"tagSet>item"`
		}
		var items []item
		for _, g := range a.groups {
			if !matches(fs, g.tags, nil) {
				continue
			}
			it := item{ID: g.id, Name: g.name, VpcID: g.vpc, Tags: xmlTags(g.tags)}
			for _, r := range g.rules {
				pm := perm{Protocol: "tcp", From: r.port, To: r.port}
				if r.group != "" {
					pm.Groups = []idItem{{r.group}}
				} else {
					pm.Ranges = []ipItem{{r.ip}}
				}
				it.Perms = append(it.Perms, pm)
			}
			items = append(items, it)
		}
		return struct {
			XMLName xml.Name `xml:"DescribeSecurityGroupsResponse"`
			Items   []item   `xml:"securityGroupInfo>item"`
		}{Items: items}, nil
	case "CreateSecurityGroup":
		t, err := tagSpec(p, "security-group")
		if err != nil {
			return nil, err
		}
		if err := a.need(p, "GroupName", "GroupDescription", "VpcId"); err != nil {
			return nil, err
		}
		if a.findVpc(p["VpcId"]) == nil {
			return nil, fail(400, "InvalidVpcID.NotFound", "the vpc ID '%s' does not exist", p["VpcId"])
		}
		for _, g := range a.groups {
			if g.vpc == p["VpcId"] && g.name == p["GroupName"] {
				return nil, fail(400, "InvalidGroup.Duplicate", "the security group '%s' already exists for VPC '%s'", g.name, g.vpc)
			}
		}
		a.groups = append(a.groups, &group{id: a.id("sg"), vpc: p["VpcId"], name: p["GroupName"], tags: t})
		return ok(act), nil
	case "AuthorizeSecurityGroupIngress":
		g := a.findGroup(p["GroupId"])
		if g == nil {
			return nil, fail(400, "InvalidGroup.NotFound", "the security group '%s' does not exist", p["GroupId"])
		}
		var add []rule
		for i := 1; ; i++ {
			pre := fmt.Sprintf("IpPermissions.%d.", i)
			if _, ok := p[pre+"IpProtocol"]; !ok {
				break
			}
			if p[pre+"IpProtocol"] != "tcp" || p[pre+"FromPort"] != p[pre+"ToPort"] {
				return nil, fail(400, "InvalidParameterValue", "the fake takes one TCP port per rule")
			}
			port, _ := strconv.ParseInt(p[pre+"FromPort"], 10, 64)
			r := rule{port: port, group: p[pre+"Groups.1.GroupId"], ip: p[pre+"IpRanges.1.CidrIp"]}
			if r.group != "" && a.findGroup(r.group) == nil {
				return nil, fail(400, "InvalidGroup.NotFound", "the security group '%s' does not exist", r.group)
			}
			r.ruleString = fmt.Sprintf("%d/%s/%s", r.port, r.group, r.ip)
			if slices.ContainsFunc(g.rules, func(x rule) bool { return x.ruleString == r.ruleString }) {
				return nil, fail(400, "InvalidPermission.Duplicate", "the specified rule already exists")
			}
			add = append(add, r)
		}
		g.rules = append(g.rules, add...)
		return ok(act), nil
	case "DeleteSecurityGroup":
		g := a.findGroup(p["GroupId"])
		if g == nil {
			return nil, fail(400, "InvalidGroup.NotFound", "the security group '%s' does not exist", p["GroupId"])
		}
		for _, i := range a.insts {
			if slices.Contains(i.groups, g.id) && i.state != "terminated" {
				return nil, fail(400, "DependencyViolation", "resource %s has a dependent object", g.id)
			}
		}
		for _, x := range a.groups {
			if slices.ContainsFunc(x.rules, func(r rule) bool { return r.group == g.id }) {
				return nil, fail(400, "DependencyViolation", "resource %s has a dependent object (a rule of %s)", g.id, x.id)
			}
		}
		a.groups = slices.DeleteFunc(a.groups, func(x *group) bool { return x == g })
		return ok(act), nil

	case "DescribeInstances":
		type item struct {
			ID          string   `xml:"instanceId"`
			Type        string   `xml:"instanceType"`
			Image       string   `xml:"imageId"`
			PrivateIP   string   `xml:"privateIpAddress,omitempty"`
			ClientToken string   `xml:"clientToken"`
			State       string   `xml:"instanceState>name"`
			Tags        []xmlTag `xml:"tagSet>item"`
		}
		var items []item
		for _, i := range a.insts {
			if matches(fs, i.tags, map[string]string{"instance-state-name": i.state, "client-token": i.token}) {
				items = append(items, item{i.id, i.typ, i.image, i.ip, i.token, i.state, xmlTags(i.tags)})
			}
		}
		start := 0
		if t := p["NextToken"]; t != "" {
			start, _ = strconv.Atoi(t)
		}
		next := ""
		if end := start + pageSize; end < len(items) {
			items, next = items[start:end], strconv.Itoa(end)
		} else {
			items = items[min(start, len(items)):]
		}
		type reservation struct {
			Instances []item `xml:"instancesSet>item"`
		}
		var rs []reservation
		for _, it := range items {
			rs = append(rs, reservation{[]item{it}})
		}
		return struct {
			XMLName      xml.Name      `xml:"DescribeInstancesResponse"`
			Reservations []reservation `xml:"reservationSet>item"`
			Next         string        `xml:"nextToken,omitempty"`
		}{Reservations: rs, Next: next}, nil
	case "RunInstances":
		if p["MinCount"] != "1" || p["MaxCount"] != "1" {
			return nil, fail(400, "InvalidParameterValue", "the fake launches one instance at a time")
		}
		if err := a.need(p, "ImageId", "InstanceType", "ClientToken", "NetworkInterface.1.SubnetId", "NetworkInterface.1.SecurityGroupId.1"); err != nil {
			return nil, err
		}
		for _, i := range a.insts {
			if i.token == p["ClientToken"] {
				return struct {
					XMLName xml.Name `xml:"RunInstancesResponse"`
					ID      string   `xml:"instancesSet>item>instanceId"`
				}{ID: i.id}, nil // idempotent
			}
		}
		t, err := tagSpec(p, "instance")
		if err != nil {
			return nil, err
		}
		if p["ImageId"] != ImageX86 && p["ImageId"] != ImageArm64 {
			return nil, fail(400, "InvalidAMIID.NotFound", "the image id '[%s]' does not exist", p["ImageId"])
		}
		s := a.findSubnet(p["NetworkInterface.1.SubnetId"])
		if s == nil {
			return nil, fail(400, "InvalidSubnetID.NotFound", "the subnet ID '%s' does not exist", p["NetworkInterface.1.SubnetId"])
		}
		g := a.findGroup(p["NetworkInterface.1.SecurityGroupId.1"])
		if g == nil || g.vpc != s.vpc {
			return nil, fail(400, "InvalidGroup.NotFound", "the security group '%s' does not exist in the subnet's VPC", p["NetworkInterface.1.SecurityGroupId.1"])
		}
		prof := p["IamInstanceProfile.Name"]
		if prof != "" && !slices.ContainsFunc(a.profiles, func(x *profile) bool { return x.name == prof && len(x.roles) == 1 }) {
			return nil, fail(400, "InvalidParameterValue", "value (%s) for parameter iamInstanceProfile.name is invalid. Invalid IAM Instance Profile name", prof)
		}
		if p["MetadataOptions.HttpTokens"] != "required" {
			return nil, fail(400, "InvalidParameterValue", "the fake wants IMDSv2 (MetadataOptions.HttpTokens=required)")
		}
		cl := ""
		if ud, err := base64.StdEncoding.DecodeString(p["UserData"]); err == nil {
			if m := regexp.MustCompile(`ECS_CLUSTER=([A-Za-z0-9_-]+)`).FindStringSubmatch(string(ud)); m != nil {
				cl = m[1]
			}
		}
		s.next++
		ip := strings.TrimSuffix(strings.SplitN(s.cidr, "/", 2)[0], "0") + strconv.Itoa(10+s.next)
		i := &instance{id: a.id("i"), typ: p["InstanceType"], image: p["ImageId"], ip: ip, token: p["ClientToken"], state: "running",
			subnet: s.id, groups: []string{g.id}, profile: prof, cluster: cl, tags: t}
		a.insts = append(a.insts, i)
		return struct {
			XMLName xml.Name `xml:"RunInstancesResponse"`
			ID      string   `xml:"instancesSet>item>instanceId"`
		}{ID: i.id}, nil
	case "StartInstances", "StopInstances", "TerminateInstances":
		i := slices.IndexFunc(a.insts, func(x *instance) bool { return x.id == p["InstanceId.1"] })
		if i < 0 {
			return nil, fail(400, "InvalidInstanceID.NotFound", "the instance ID '%s' does not exist", p["InstanceId.1"])
		}
		x := a.insts[i]
		switch act {
		case "StartInstances":
			if x.state == "terminated" {
				return nil, fail(400, "IncorrectInstanceState", "the instance '%s' is not in a state from which it can be started", x.id)
			}
			x.state = "running"
		case "StopInstances":
			x.state = "stopped"
		default:
			x.state = "terminated"
		}
		return ok(act), nil
	}
	return nil, fail(400, "InvalidAction", "the fake does not know %s", act)
}

// --- IAM --------------------------------------------------------------------------------------

func (a *API) findRole(name string) *role {
	for _, r := range a.roles {
		if r.name == name {
			return r
		}
	}
	return nil
}

func (a *API) findProfile(name string) *profile {
	for _, p := range a.profiles {
		if p.name == name {
			return p
		}
	}
	return nil
}

// RoleArn is a role's ARN in the fake account.
func RoleArn(name string) string { return "arn:aws:iam::" + Account + ":role/mamela/" + name }

type iamTag struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

func iamTagsOf(p map[string]string) (tags, []iamTag) {
	t := tags{}
	var out []iamTag
	for i := 1; ; i++ {
		k, ok := p[fmt.Sprintf("Tags.member.%d.Key", i)]
		if !ok {
			return t, out
		}
		t[k] = p[fmt.Sprintf("Tags.member.%d.Value", i)]
		out = append(out, iamTag{k, t[k]})
	}
}

func (a *API) iam(act string, p map[string]string) (any, error) {
	noRole := func(n string) error { return fail(404, "NoSuchEntity", "the role with name %s cannot be found", n) }
	noProfile := func(n string) error { return fail(404, "NoSuchEntity", "instance profile %s cannot be found", n) }
	type result struct {
		XMLName xml.Name
	}
	done := result{XMLName: xml.Name{Local: act + "Response"}}
	switch act {
	case "ListRoles":
		var names []string
		for _, r := range a.roles {
			if strings.HasPrefix("/mamela/", p["PathPrefix"]) {
				names = append(names, r.name)
			}
		}
		slices.Sort(names)
		start := 0
		if m := p["Marker"]; m != "" {
			start, _ = strconv.Atoi(m)
		}
		end := min(start+pageSize, len(names))
		resp := struct {
			XMLName   xml.Name `xml:"ListRolesResponse"`
			Names     []string `xml:"ListRolesResult>Roles>member>RoleName"`
			Truncated bool     `xml:"ListRolesResult>IsTruncated"`
			Marker    string   `xml:"ListRolesResult>Marker,omitempty"`
		}{Names: names[min(start, len(names)):end]}
		if end < len(names) {
			resp.Truncated, resp.Marker = true, strconv.Itoa(end)
		}
		return resp, nil
	case "GetRole":
		r := a.findRole(p["RoleName"])
		if r == nil {
			return nil, noRole(p["RoleName"])
		}
		var ts []iamTag
		for _, k := range slices.Sorted(maps.Keys(r.tags)) {
			ts = append(ts, iamTag{k, r.tags[k]})
		}
		return struct {
			XMLName xml.Name `xml:"GetRoleResponse"`
			Arn     string   `xml:"GetRoleResult>Role>Arn"`
			Trust   string   `xml:"GetRoleResult>Role>AssumeRolePolicyDocument"`
			Tags    []iamTag `xml:"GetRoleResult>Role>Tags>member"`
		}{Arn: RoleArn(r.name), Trust: url.QueryEscape(r.trust), Tags: ts}, nil
	case "CreateRole":
		if a.findRole(p["RoleName"]) != nil {
			return nil, fail(409, "EntityAlreadyExists", "role with name %s already exists", p["RoleName"])
		}
		if p["Path"] != "/mamela/" {
			return nil, fail(400, "ValidationError", "the fake makes roles under /mamela/ only")
		}
		var doc any
		if err := json.Unmarshal([]byte(p["AssumeRolePolicyDocument"]), &doc); err != nil {
			return nil, fail(400, "MalformedPolicyDocument", "the trust policy is not JSON")
		}
		t, _ := iamTagsOf(p)
		a.roles = append(a.roles, &role{name: p["RoleName"], trust: p["AssumeRolePolicyDocument"], tags: t})
		return done, nil
	case "ListAttachedRolePolicies":
		r := a.findRole(p["RoleName"])
		if r == nil {
			return nil, noRole(p["RoleName"])
		}
		return struct {
			XMLName xml.Name `xml:"ListAttachedRolePoliciesResponse"`
			Arns    []string `xml:"ListAttachedRolePoliciesResult>AttachedPolicies>member>PolicyArn"`
		}{Arns: r.policies}, nil
	case "AttachRolePolicy", "DetachRolePolicy":
		r := a.findRole(p["RoleName"])
		if r == nil {
			return nil, noRole(p["RoleName"])
		}
		has := slices.Contains(r.policies, p["PolicyArn"])
		switch {
		case act == "AttachRolePolicy" && !has:
			r.policies = append(r.policies, p["PolicyArn"])
		case act == "DetachRolePolicy" && !has:
			return nil, fail(404, "NoSuchEntity", "policy %s was not found", p["PolicyArn"])
		case act == "DetachRolePolicy":
			r.policies = slices.DeleteFunc(r.policies, func(x string) bool { return x == p["PolicyArn"] })
		}
		return done, nil
	case "GetInstanceProfile":
		x := a.findProfile(p["InstanceProfileName"])
		if x == nil {
			return nil, noProfile(p["InstanceProfileName"])
		}
		return struct {
			XMLName xml.Name `xml:"GetInstanceProfileResponse"`
			Roles   []string `xml:"GetInstanceProfileResult>InstanceProfile>Roles>member>RoleName"`
		}{Roles: x.roles}, nil
	case "CreateInstanceProfile":
		if a.findProfile(p["InstanceProfileName"]) != nil {
			return nil, fail(409, "EntityAlreadyExists", "instance profile %s already exists", p["InstanceProfileName"])
		}
		a.profiles = append(a.profiles, &profile{name: p["InstanceProfileName"]})
		return done, nil
	case "AddRoleToInstanceProfile":
		x := a.findProfile(p["InstanceProfileName"])
		if x == nil {
			return nil, noProfile(p["InstanceProfileName"])
		}
		if a.findRole(p["RoleName"]) == nil {
			return nil, noRole(p["RoleName"])
		}
		if len(x.roles) > 0 {
			return nil, fail(409, "LimitExceeded", "cannot exceed quota for InstanceSessionsPerInstanceProfile: 1")
		}
		x.roles = []string{p["RoleName"]}
		return done, nil
	case "RemoveRoleFromInstanceProfile":
		x := a.findProfile(p["InstanceProfileName"])
		if x == nil || !slices.Contains(x.roles, p["RoleName"]) {
			return nil, noRole(p["RoleName"])
		}
		x.roles = nil
		return done, nil
	case "DeleteInstanceProfile":
		x := a.findProfile(p["InstanceProfileName"])
		if x == nil {
			return nil, noProfile(p["InstanceProfileName"])
		}
		if len(x.roles) > 0 {
			return nil, fail(409, "DeleteConflict", "cannot delete entity, must remove roles from instance profile first")
		}
		a.profiles = slices.DeleteFunc(a.profiles, func(y *profile) bool { return y == x })
		return done, nil
	case "DeleteRole":
		r := a.findRole(p["RoleName"])
		if r == nil {
			return nil, noRole(p["RoleName"])
		}
		if len(r.policies) > 0 {
			return nil, fail(409, "DeleteConflict", "cannot delete entity, must detach all policies first")
		}
		for _, x := range a.profiles {
			if slices.Contains(x.roles, r.name) {
				return nil, fail(409, "DeleteConflict", "cannot delete entity, must remove roles from instance profile first")
			}
		}
		a.roles = slices.DeleteFunc(a.roles, func(x *role) bool { return x == r })
		return done, nil
	}
	return nil, fail(400, "InvalidAction", "the fake does not know %s", act)
}

// --- CloudWatch Logs, SSM ---------------------------------------------------------------------

func str(in map[string]any, k string) string {
	s, _ := in[k].(string)
	return s
}

func num(in map[string]any, k string) int64 {
	f, _ := in[k].(float64)
	return int64(f)
}

func (a *API) logsCall(act string, in map[string]any) (any, error) {
	name := str(in, "logGroupName")
	i := slices.IndexFunc(a.logs, func(g *logGroup) bool { return g.name == name })
	switch act {
	case "DescribeLogGroups":
		var groups []map[string]any
		for _, g := range a.logs {
			if strings.HasPrefix(g.name, str(in, "logGroupNamePrefix")) {
				m := map[string]any{"logGroupName": g.name, "arn": "arn:aws:logs:" + Region + ":" + Account + ":log-group:" + g.name + ":*"}
				if g.retention > 0 {
					m["retentionInDays"] = g.retention
				}
				groups = append(groups, m)
			}
		}
		start, _ := strconv.Atoi(str(in, "nextToken"))
		out := map[string]any{}
		if end := start + pageSize; end < len(groups) {
			out["logGroups"], out["nextToken"] = groups[start:end], strconv.Itoa(end)
		} else {
			out["logGroups"] = groups[min(start, len(groups)):]
		}
		return out, nil
	case "CreateLogGroup":
		if i >= 0 {
			return nil, fail(400, "ResourceAlreadyExistsException", "the specified log group already exists")
		}
		t := map[string]string{}
		if m, ok := in["tags"].(map[string]any); ok {
			for k, v := range m {
				t[k], _ = v.(string)
			}
		}
		a.logs = append(a.logs, &logGroup{name: name, tags: t})
		return map[string]any{}, nil
	case "PutRetentionPolicy", "DeleteLogGroup":
		if i < 0 {
			return nil, fail(400, "ResourceNotFoundException", "the specified log group does not exist")
		}
		if act == "DeleteLogGroup" {
			a.logs = slices.Delete(a.logs, i, i+1)
		} else {
			a.logs[i].retention = num(in, "retentionInDays")
		}
		return map[string]any{}, nil
	}
	return nil, fail(400, "UnknownOperationException", "the fake does not know %s", act)
}

func (a *API) ssm(act string, in map[string]any) (any, error) {
	if act != "GetParameter" {
		return nil, fail(400, "UnknownOperationException", "the fake does not know %s", act)
	}
	image := map[string]string{
		"/aws/service/ecs/optimized-ami/amazon-linux-2023/recommended/image_id":       ImageX86,
		"/aws/service/ecs/optimized-ami/amazon-linux-2023/arm64/recommended/image_id": ImageArm64,
	}[str(in, "Name")]
	if image == "" {
		return nil, fail(400, "ParameterNotFound", "parameter %s not found", str(in, "Name"))
	}
	return map[string]any{"Parameter": map[string]any{"Name": str(in, "Name"), "Type": "String", "Value": image}}, nil
}
