package catalogs

import (
	"fmt"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// EC2Version is the version of the EC2 query API the catalog describes.
const EC2Version = "2016-11-15"

// EC2 is the part of the Amazon EC2 query API that the provider uses.
// Every call is POST / with a form body; the fixed parameters Action and Version tell them apart.
// RunInstances also fixes MinCount and MaxCount to 1: one change makes one instance. Requests are
// signed with SigV4 (auth "aws-sigv4" in mamela.providers.toml).
//
// An instance's size is its instance type, which the measures read with a table of the types
// the catalog knows (vCPUs, MiB): a type not in it is refused, never guessed.
var EC2 = protocol.Catalog{API: "ec2", Entries: []protocol.Entry{
	ec2Call("DescribeInstances", protocol.EffectRead, nil, filters("InstanceId")...),
	withMeasures(ec2Call("RunInstances", protocol.EffectCreate, map[string]string{"MinCount": "1", "MaxCount": "1"},
		append([]string{"MinCount", "MaxCount", "ImageId", "InstanceType", "SubnetId", "SecurityGroupId.1", "SecurityGroupId.2", "UserData", "ClientToken",
			"BlockDeviceMapping.1.DeviceName", "BlockDeviceMapping.1.Ebs.VolumeSize", "BlockDeviceMapping.1.Ebs.VolumeType",
			"MetadataOptions.HttpTokens", "IamInstanceProfile.Name",
			"NetworkInterface.1.DeviceIndex", "NetworkInterface.1.SubnetId", "NetworkInterface.1.AssociatePublicIpAddress",
			"NetworkInterface.1.SecurityGroupId.1", "NetworkInterface.1.SecurityGroupId.2"}, tagSpecification...)...),
		protocol.Measure{Param: "InstanceType", Name: protocol.MeasureCPU, Format: protocol.FormatTable, Table: ec2VCPUs()},
		protocol.Measure{Param: "InstanceType", Name: protocol.MeasureMemoryMiB, Format: protocol.FormatTable, Table: ec2MemoryMiB()},
		protocol.Measure{Param: "BlockDeviceMapping.1.Ebs.VolumeSize", Name: protocol.MeasureDiskGiB, Format: protocol.FormatCount}),
	bind(ec2Call("StartInstances", protocol.EffectRestart, nil, "InstanceId.1"), "InstanceId.1"),
	bind(ec2Call("StopInstances", protocol.EffectRestart, nil, "InstanceId.1"), "InstanceId.1"),
	bind(ec2Call("RebootInstances", protocol.EffectRestart, nil, "InstanceId.1"), "InstanceId.1"),
	withMeasures(bind(ec2Call("ModifyInstanceAttribute", protocol.EffectChange, nil, "InstanceId", "InstanceType.Value"), "InstanceId"),
		protocol.Measure{Param: "InstanceType.Value", Name: protocol.MeasureCPU, Format: protocol.FormatTable, Table: ec2VCPUs()},
		protocol.Measure{Param: "InstanceType.Value", Name: protocol.MeasureMemoryMiB, Format: protocol.FormatTable, Table: ec2MemoryMiB()}),
	bind(ec2Call("CreateTags", protocol.EffectChange, nil, "ResourceId.1", "Tag.1.Key", "Tag.1.Value", "Tag.2.Key", "Tag.2.Value",
		"Tag.3.Key", "Tag.3.Value"), "ResourceId.1"),
	bind(ec2Call("TerminateInstances", protocol.EffectLoseData, nil, "InstanceId.1"), "InstanceId.1"),

	// The network a provider makes its machines in (ADR 0007): a VPC of its own, its subnets, an
	// internet gateway, a route table and security groups. Deleting any of them is lose-data.
	ec2Call("DescribeVpcs", protocol.EffectRead, nil, filters("VpcId")...),
	ec2Call("CreateVpc", protocol.EffectCreate, nil, append([]string{"CidrBlock"}, tagSpecification...)...),
	bind(ec2Call("DeleteVpc", protocol.EffectLoseData, nil, "VpcId"), "VpcId"),
	ec2Call("DescribeSubnets", protocol.EffectRead, nil, filters("SubnetId")...),
	ec2Call("CreateSubnet", protocol.EffectCreate, nil, append([]string{"VpcId", "CidrBlock", "AvailabilityZone"}, tagSpecification...)...),
	bind(ec2Call("DeleteSubnet", protocol.EffectLoseData, nil, "SubnetId"), "SubnetId"),
	ec2Call("DescribeInternetGateways", protocol.EffectRead, nil, filters("InternetGatewayId")...),
	ec2Call("CreateInternetGateway", protocol.EffectCreate, nil, tagSpecification...),
	bind(ec2Call("AttachInternetGateway", protocol.EffectChange, nil, "InternetGatewayId", "VpcId"), "InternetGatewayId"),
	bind(ec2Call("DetachInternetGateway", protocol.EffectChange, nil, "InternetGatewayId", "VpcId"), "InternetGatewayId"),
	bind(ec2Call("DeleteInternetGateway", protocol.EffectLoseData, nil, "InternetGatewayId"), "InternetGatewayId"),
	ec2Call("DescribeRouteTables", protocol.EffectRead, nil, filters("RouteTableId")...),
	ec2Call("CreateRouteTable", protocol.EffectCreate, nil, append([]string{"VpcId"}, tagSpecification...)...),
	bind(ec2Call("CreateRoute", protocol.EffectChange, nil, "RouteTableId", "DestinationCidrBlock", "GatewayId"), "RouteTableId"),
	bind(ec2Call("AssociateRouteTable", protocol.EffectChange, nil, "RouteTableId", "SubnetId"), "RouteTableId"),
	bind(ec2Call("DisassociateRouteTable", protocol.EffectChange, nil, "AssociationId"), "AssociationId"),
	bind(ec2Call("DeleteRouteTable", protocol.EffectLoseData, nil, "RouteTableId"), "RouteTableId"),
	ec2Call("DescribeSecurityGroups", protocol.EffectRead, nil, filters("GroupId")...),
	ec2Call("CreateSecurityGroup", protocol.EffectCreate, nil, append([]string{"GroupName", "GroupDescription", "VpcId"}, tagSpecification...)...),
	bind(ec2Call("AuthorizeSecurityGroupIngress", protocol.EffectChange, nil, append([]string{"GroupId"}, ipPermissions...)...), "GroupId"),
	bind(ec2Call("RevokeSecurityGroupIngress", protocol.EffectChange, nil, append([]string{"GroupId"}, ipPermissions...)...), "GroupId"),
	bind(ec2Call("DeleteSecurityGroup", protocol.EffectLoseData, nil, "GroupId"), "GroupId"),
}}

// filters are the parameters of a Describe call: up to 2 IDs, up to 3 filters of up to 4 values,
// paging.
func filters(id string) []string {
	out := []string{id + ".1", id + ".2", "MaxResults", "NextToken"}
	for f := 1; f <= 3; f++ {
		out = append(out, fmt.Sprintf("Filter.%d.Name", f))
		for v := 1; v <= 4; v++ {
			out = append(out, fmt.Sprintf("Filter.%d.Value.%d", f, v))
		}
	}
	return out
}

// tagSpecification are the tags a create puts on what it makes (up to 4).
var tagSpecification = func() []string {
	out := []string{"TagSpecification.1.ResourceType"}
	for t := 1; t <= 4; t++ {
		out = append(out, fmt.Sprintf("TagSpecification.1.Tag.%d.Key", t), fmt.Sprintf("TagSpecification.1.Tag.%d.Value", t))
	}
	return out
}()

// ipPermissions are up to 4 ingress rules, each from a security group or a CIDR.
var ipPermissions = func() []string {
	var out []string
	for i := 1; i <= 4; i++ {
		for _, f := range []string{"IpProtocol", "FromPort", "ToPort", "Groups.1.GroupId", "IpRanges.1.CidrIp"} {
			out = append(out, fmt.Sprintf("IpPermissions.%d.%s", i, f))
		}
	}
	return out
}()

func ec2Call(action string, effect protocol.Effect, fixed map[string]string, form ...string) protocol.Entry {
	f := map[string]string{"Action": action, "Version": EC2Version}
	for k, v := range fixed {
		f[k] = v
	}
	return protocol.Entry{Method: "POST", Path: "/", Effect: effect, Form: append([]string{"Action", "Version"}, form...), Fixed: f}
}

func bind(e protocol.Entry, params ...string) protocol.Entry {
	e.Bind = params
	return e
}

func withMeasures(e protocol.Entry, ms ...protocol.Measure) protocol.Entry {
	e.Measures = ms
	return e
}

// ec2Types are the instance types the catalog knows the size of: vCPUs and memory in MiB.
var ec2Types = map[string][2]int64{
	"t3.nano": {2, 512}, "t3.micro": {2, 1024}, "t3.small": {2, 2048}, "t3.medium": {2, 4096},
	"t3.large": {2, 8192}, "t3.xlarge": {4, 16384}, "t3.2xlarge": {8, 32768},
	"t3a.nano": {2, 512}, "t3a.micro": {2, 1024}, "t3a.small": {2, 2048}, "t3a.medium": {2, 4096},
	"t3a.large": {2, 8192}, "t3a.xlarge": {4, 16384}, "t3a.2xlarge": {8, 32768},
	"t4g.nano": {2, 512}, "t4g.micro": {2, 1024}, "t4g.small": {2, 2048}, "t4g.medium": {2, 4096},
	"t4g.large": {2, 8192}, "t4g.xlarge": {4, 16384}, "t4g.2xlarge": {8, 32768},
	"m6i.large": {2, 8192}, "m6i.xlarge": {4, 16384}, "m6i.2xlarge": {8, 32768},
	"m7i.large": {2, 8192}, "m7i.xlarge": {4, 16384}, "m7i.2xlarge": {8, 32768},
	"c6i.large": {2, 4096}, "c6i.xlarge": {4, 8192}, "c6i.2xlarge": {8, 16384},
	"r6i.large": {2, 16384}, "r6i.xlarge": {4, 32768},
}

func ec2VCPUs() map[string]int64 {
	out := map[string]int64{}
	for t, s := range ec2Types {
		out[t] = s[0]
	}
	return out
}

func ec2MemoryMiB() map[string]int64 {
	out := map[string]int64{}
	for t, s := range ec2Types {
		out[t] = s[1]
	}
	return out
}
