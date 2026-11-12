package fakeaws

import "fmt"

// TerraformNetwork is a network made by Terraform (no Mamela tags): its IDs, and the JSON of
// `terraform show -json` of the state that made it.
type TerraformNetwork struct {
	Vpc, Gateway, Subnet, RouteTable, Association, Web, Admin string
	State                                                     []byte
}

// AddTerraformNetwork makes a VPC with an attached internet gateway, a subnet, a route table that
// routes 0.0.0.0/0 to the gateway and serves the subnet, and two security groups: web (TCP 80 from
// anywhere, 443 from admin, and a port range Mamela does not express) and admin (TCP 22 from one
// address, written as an aws_security_group_rule). None carries a tag of Mamela's.
func (a *API) AddTerraformNetwork() TerraformNetwork {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := TerraformNetwork{Vpc: a.id("vpc"), Gateway: a.id("igw"), Subnet: a.id("subnet"), RouteTable: a.id("rtb"),
		Association: a.id("rtbassoc"), Web: a.id("sg"), Admin: a.id("sg")}
	tf := tags{"Name": "legacy"}
	a.vpcs = append(a.vpcs, &vpc{id: n.Vpc, cidr: "10.9.0.0/16", tags: clone(tf)})
	a.gateways = append(a.gateways, &gateway{id: n.Gateway, vpc: n.Vpc, tags: clone(tf)})
	a.subnets = append(a.subnets, &subnet{id: n.Subnet, vpc: n.Vpc, cidr: "10.9.1.0/24", zone: Region + "a", tags: clone(tf)})
	a.routes = append(a.routes, &routeTable{id: n.RouteTable, vpc: n.Vpc, routes: map[string]string{"0.0.0.0/0": n.Gateway},
		assocID: n.Association, assocWith: n.Subnet, tags: clone(tf)})
	a.groups = append(a.groups,
		&group{id: n.Web, vpc: n.Vpc, name: "legacy-web", tags: clone(tf), rules: []rule{
			{port: 80, ip: "0.0.0.0/0"}, {port: 443, group: n.Admin}}},
		&group{id: n.Admin, vpc: n.Vpc, name: "legacy-admin", tags: clone(tf), rules: []rule{{port: 22, ip: "203.0.113.7/32"}}})
	n.State = fmt.Appendf(nil, `{"format_version":"1.0","terraform_version":"1.9.8","values":{"root_module":{"resources":[
  {"address":"aws_vpc.main","mode":"managed","type":"aws_vpc","name":"main","values":{"id":%[1]q,"cidr_block":"10.9.0.0/16","tags":{"Name":"legacy"}}},
  {"address":"aws_internet_gateway.gw","mode":"managed","type":"aws_internet_gateway","name":"gw","values":{"id":%[2]q,"vpc_id":%[1]q}},
  {"address":"aws_subnet.public","mode":"managed","type":"aws_subnet","name":"public","values":{"id":%[3]q,"vpc_id":%[1]q,"cidr_block":"10.9.1.0/24","availability_zone":"ap-northeast-1a"}},
  {"address":"aws_route_table.public","mode":"managed","type":"aws_route_table","name":"public","values":{"id":%[4]q,"vpc_id":%[1]q,"route":[{"cidr_block":"0.0.0.0/0","gateway_id":%[2]q}]}},
  {"address":"aws_route_table_association.public","mode":"managed","type":"aws_route_table_association","name":"public","values":{"id":%[5]q,"route_table_id":%[4]q,"subnet_id":%[3]q}},
  {"address":"aws_security_group.web","mode":"managed","type":"aws_security_group","name":"web","values":{"id":%[6]q,"vpc_id":%[1]q,"description":"web servers","ingress":[
    {"from_port":80,"to_port":80,"protocol":"tcp","cidr_blocks":["0.0.0.0/0"],"security_groups":[],"self":false},
    {"from_port":443,"to_port":443,"protocol":"tcp","cidr_blocks":[],"security_groups":[%[7]q],"self":false},
    {"from_port":8000,"to_port":8100,"protocol":"tcp","cidr_blocks":["10.9.0.0/16"],"security_groups":[],"self":false}]}},
  {"address":"aws_security_group.admin","mode":"managed","type":"aws_security_group","name":"admin","values":{"id":%[7]q,"vpc_id":%[1]q,"description":"admin","ingress":[]}},
  {"address":"aws_security_group_rule.ssh","mode":"managed","type":"aws_security_group_rule","name":"ssh","values":{"id":"sgrule-1","type":"ingress","security_group_id":%[7]q,"from_port":22,"to_port":22,"protocol":"tcp","cidr_blocks":["203.0.113.7/32"]}},
  {"address":"aws_s3_bucket.logs","mode":"managed","type":"aws_s3_bucket","name":"logs","values":{"id":"legacy-logs"}}
]}}}`, n.Vpc, n.Gateway, n.Subnet, n.RouteTable, n.Association, n.Web, n.Admin)
	return n
}

func clone(t tags) tags {
	out := tags{}
	for k, v := range t {
		out[k] = v
	}
	return out
}
