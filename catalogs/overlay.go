package catalogs

import (
	"fmt"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// The overlay is what people review and AWS's models do not say: which operations each catalog
// has, which parameters each may be sent (as the protocol writes them), which of them must name
// the change's object (bind), which values some may have (values: IAM is held to ECS's needs), how
// big what they make is (measures), and the parameters an operation fixes (one EC2 instance a
// request). The generator checks every operation and parameter here against the model, and takes
// the rest from it: where each parameter goes, the version, the X-Amz-Target, and the effect (by
// the rules of effects.go).

// API is one catalog: an API the provider calls, as mamela.providers.toml names it.
type API struct {
	Name  string // the catalog's and the API's name ("ec2")
	Model string // its model in models.lock.json
	Ops   []Op
}

// Op is one operation of an API, and what Mamela holds its requests to.
type Op struct {
	Name     string            // the AWS operation ("RunInstances")
	Params   []string          // the parameters it may be sent, as the protocol writes them
	Fixed    map[string]string // parameters of one value, which tell the call apart (query APIs)
	Bind     []string          // parameters that must name the change's object
	Values   map[string][]string
	Measures []protocol.Measure
}

// APIs are the catalogs, in the order the Pome ships them.
var APIs = []API{EC2, ECS, Logs, IAM, SSM}

// Names are the catalogs' names.
func Names() []string {
	var out []string
	for _, a := range APIs {
		out = append(out, a.Name)
	}
	return out
}

// --- EC2 ------------------------------------------------------------------------------------

// EC2 is the part of the Amazon EC2 query API that the provider uses. RunInstances fixes MinCount
// and MaxCount to 1: one change makes one instance. An instance's size is its type, which the
// measures read with a table of the types the catalog knows (vCPUs, MiB): a type not in it is
// refused, never guessed.
var EC2 = API{Name: "ec2", Model: "ec2", Ops: []Op{
	{Name: "DescribeInstances", Params: filters("InstanceId")},
	{Name: "RunInstances", Fixed: map[string]string{"MinCount": "1", "MaxCount": "1"},
		Params: append([]string{"MinCount", "MaxCount", "ImageId", "InstanceType", "SubnetId", "SecurityGroupId.1", "SecurityGroupId.2", "UserData", "ClientToken",
			"BlockDeviceMapping.1.DeviceName", "BlockDeviceMapping.1.Ebs.VolumeSize", "BlockDeviceMapping.1.Ebs.VolumeType",
			"MetadataOptions.HttpTokens", "IamInstanceProfile.Name",
			"NetworkInterface.1.DeviceIndex", "NetworkInterface.1.SubnetId", "NetworkInterface.1.AssociatePublicIpAddress",
			"NetworkInterface.1.SecurityGroupId.1", "NetworkInterface.1.SecurityGroupId.2"}, tagSpecification...),
		Measures: []protocol.Measure{
			{Param: "InstanceType", Name: protocol.MeasureCPU, Format: protocol.FormatTable, Table: ec2VCPUs()},
			{Param: "InstanceType", Name: protocol.MeasureMemoryMiB, Format: protocol.FormatTable, Table: ec2MemoryMiB()},
			{Param: "BlockDeviceMapping.1.Ebs.VolumeSize", Name: protocol.MeasureDiskGiB, Format: protocol.FormatCount}}},
	{Name: "StartInstances", Params: []string{"InstanceId.1"}, Bind: []string{"InstanceId.1"}},
	{Name: "StopInstances", Params: []string{"InstanceId.1"}, Bind: []string{"InstanceId.1"}},
	{Name: "RebootInstances", Params: []string{"InstanceId.1"}, Bind: []string{"InstanceId.1"}},
	// One attribute a request: the type (of a stopped instance: resize) or the security group.
	{Name: "ModifyInstanceAttribute", Params: []string{"InstanceId", "InstanceType.Value", "GroupId.1"}, Bind: []string{"InstanceId"},
		Measures: []protocol.Measure{
			{Param: "InstanceType.Value", Name: protocol.MeasureCPU, Format: protocol.FormatTable, Table: ec2VCPUs()},
			{Param: "InstanceType.Value", Name: protocol.MeasureMemoryMiB, Format: protocol.FormatTable, Table: ec2MemoryMiB()}}},
	// Tags what the provider made, and what a person adopts (an imported object).
	{Name: "CreateTags", Params: []string{"ResourceId.1", "Tag.1.Key", "Tag.1.Value", "Tag.2.Key", "Tag.2.Value", "Tag.3.Key", "Tag.3.Value"},
		Bind: []string{"ResourceId.1"}},
	{Name: "TerminateInstances", Params: []string{"InstanceId.1"}, Bind: []string{"InstanceId.1"}},

	// The network a provider makes its machines in: a VPC of its own, its subnets, an internet
	// gateway, a route table and security groups. Deleting any of them is lose-data.
	{Name: "DescribeVpcs", Params: filters("VpcId")},
	{Name: "CreateVpc", Params: append([]string{"CidrBlock"}, tagSpecification...)},
	{Name: "DeleteVpc", Params: []string{"VpcId"}, Bind: []string{"VpcId"}},
	{Name: "DescribeSubnets", Params: filters("SubnetId")},
	{Name: "CreateSubnet", Params: append([]string{"VpcId", "CidrBlock", "AvailabilityZone"}, tagSpecification...)},
	{Name: "DeleteSubnet", Params: []string{"SubnetId"}, Bind: []string{"SubnetId"}},
	{Name: "DescribeInternetGateways", Params: filters("InternetGatewayId")},
	{Name: "CreateInternetGateway", Params: tagSpecification},
	{Name: "AttachInternetGateway", Params: []string{"InternetGatewayId", "VpcId"}, Bind: []string{"InternetGatewayId"}},
	{Name: "DetachInternetGateway", Params: []string{"InternetGatewayId", "VpcId"}, Bind: []string{"InternetGatewayId"}},
	{Name: "DeleteInternetGateway", Params: []string{"InternetGatewayId"}, Bind: []string{"InternetGatewayId"}},
	{Name: "DescribeRouteTables", Params: filters("RouteTableId")},
	{Name: "CreateRouteTable", Params: append([]string{"VpcId"}, tagSpecification...)},
	{Name: "CreateRoute", Params: []string{"RouteTableId", "DestinationCidrBlock", "GatewayId"}, Bind: []string{"RouteTableId"}},
	{Name: "AssociateRouteTable", Params: []string{"RouteTableId", "SubnetId"}, Bind: []string{"RouteTableId"}},
	{Name: "DisassociateRouteTable", Params: []string{"AssociationId"}, Bind: []string{"AssociationId"}},
	{Name: "DeleteRouteTable", Params: []string{"RouteTableId"}, Bind: []string{"RouteTableId"}},
	{Name: "DescribeSecurityGroups", Params: filters("GroupId")},
	{Name: "CreateSecurityGroup", Params: append([]string{"GroupName", "GroupDescription", "VpcId"}, tagSpecification...)},
	{Name: "AuthorizeSecurityGroupIngress", Params: append([]string{"GroupId"}, ipPermissions...), Bind: []string{"GroupId"}},
	{Name: "RevokeSecurityGroupIngress", Params: append([]string{"GroupId"}, ipPermissions...), Bind: []string{"GroupId"}},
	{Name: "DeleteSecurityGroup", Params: []string{"GroupId"}, Bind: []string{"GroupId"}},
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

// --- ECS, CloudWatch Logs, SSM (JSON APIs) ---------------------------------------------------

// ECS is the part of the Amazon ECS API that a provider of clusters, task definitions, services
// and tasks uses. A task definition's size is its task-level cpu (CPU units, 1024 to a vCPU) and
// memory (MiB): the site's limits hold for each.
var ECS = API{Name: "ecs", Model: "ecs", Ops: []Op{
	{Name: "CreateCluster", Params: []string{"clusterName", "capacityProviders", "defaultCapacityProviderStrategy", "tags", "settings"}},
	{Name: "ListClusters", Params: []string{"nextToken", "maxResults"}},
	{Name: "DescribeClusters", Params: []string{"clusters", "include"}},
	{Name: "DeleteCluster", Params: []string{"cluster"}, Bind: []string{"cluster"}},
	{Name: "RegisterTaskDefinition", Params: []string{"family", "containerDefinitions", "cpu", "memory", "networkMode",
		"requiresCompatibilities", "executionRoleArn", "taskRoleArn", "tags", "runtimePlatform"},
		Measures: []protocol.Measure{
			{Param: "cpu", Name: protocol.MeasureCPU, Format: protocol.FormatCPUUnits},
			{Param: "memory", Name: protocol.MeasureMemoryMiB, Format: protocol.FormatCount}}},
	{Name: "DescribeTaskDefinition", Params: []string{"taskDefinition", "include"}},
	{Name: "ListTaskDefinitions", Params: []string{"familyPrefix", "status", "sort", "nextToken", "maxResults"}},
	{Name: "DeregisterTaskDefinition", Params: []string{"taskDefinition"}, Bind: []string{"taskDefinition"}},
	{Name: "CreateService", Params: []string{"cluster", "serviceName", "taskDefinition", "desiredCount", "launchType",
		"capacityProviderStrategy", "networkConfiguration", "placementConstraints", "placementStrategy", "schedulingStrategy",
		"deploymentConfiguration", "tags", "clientToken", "enableECSManagedTags", "propagateTags"}},
	{Name: "ListServices", Params: []string{"cluster", "launchType", "schedulingStrategy", "nextToken", "maxResults"}},
	// A service's count and task definition, and how its tasks run on Fargate (subnet, security
	// group, public IP, Fargate or Fargate Spot): a new deployment.
	{Name: "UpdateService", Params: []string{"cluster", "service", "desiredCount", "taskDefinition", "capacityProviderStrategy",
		"networkConfiguration", "forceNewDeployment"}, Bind: []string{"cluster", "service"}},
	{Name: "DescribeServices", Params: []string{"cluster", "services", "include"}},
	{Name: "DeleteService", Params: []string{"cluster", "service", "force"}, Bind: []string{"cluster", "service"}},
	// A task runs in the cluster of the object the change is for (a service whose way of running
	// it reuses): never in someone else's.
	{Name: "RunTask", Params: []string{"cluster", "taskDefinition", "count", "launchType", "capacityProviderStrategy",
		"networkConfiguration", "overrides", "startedBy", "clientToken", "tags", "enableECSManagedTags", "propagateTags"}, Bind: []string{"cluster"}},
	{Name: "ListTasks", Params: []string{"cluster", "startedBy", "family", "desiredStatus", "serviceName", "containerInstance", "nextToken", "maxResults"}},
	{Name: "DescribeTasks", Params: []string{"cluster", "tasks", "include"}},
	{Name: "StopTask", Params: []string{"cluster", "task", "reason"}, Bind: []string{"cluster", "task"}},
	{Name: "ListContainerInstances", Params: []string{"cluster", "filter", "status", "nextToken", "maxResults"}},
	{Name: "DescribeContainerInstances", Params: []string{"cluster", "containerInstances"}},
	// Tags the object the change is for (a cluster, a service or a task definition a person
	// adopts): never another.
	{Name: "TagResource", Params: []string{"resourceArn", "tags"}, Bind: []string{"resourceArn"}},
}}

// Logs is the part of the Amazon CloudWatch Logs API for a log group of a provider's tasks.
var Logs = API{Name: "logs", Model: "logs", Ops: []Op{
	{Name: "CreateLogGroup", Params: []string{"logGroupName", "tags"}},
	{Name: "PutRetentionPolicy", Params: []string{"logGroupName", "retentionInDays"}, Bind: []string{"logGroupName"}},
	{Name: "DescribeLogGroups", Params: []string{"logGroupNamePrefix", "limit", "nextToken"}},
	{Name: "DeleteLogGroup", Params: []string{"logGroupName"}, Bind: []string{"logGroupName"}},
	{Name: "DescribeLogStreams", Params: []string{"logGroupName", "logStreamNamePrefix", "orderBy", "descending", "limit", "nextToken"}},
	{Name: "GetLogEvents", Params: []string{"logGroupName", "logStreamName", "startFromHead", "limit", "nextToken"}},
	{Name: "ListTagsForResource", Params: []string{"resourceArn"}},
	// Tags the log group the change is for (one a person adopts): never another.
	{Name: "TagResource", Params: []string{"resourceArn", "tags"}, Bind: []string{"resourceArn"}},
}}

// SSM is the part of the AWS Systems Manager API for reading public parameters (the ECS-optimized
// AMI's ID).
var SSM = API{Name: "ssm", Model: "ssm", Ops: []Op{
	{Name: "GetParameter", Params: []string{"Name", "WithDecryption"}},
}}

// --- IAM --------------------------------------------------------------------------------------

// IAM trust policies and managed policies the IAM catalog allows: the provider makes roles only
// under the path /mamela/, only for ECS tasks or EC2 instances to assume, and attaches only the two
// AWS-managed ECS policies. Anything else is refused by Mamela, whatever the program sends.
const (
	TrustECSTasks = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ecs-tasks.amazonaws.com"},"Action":"sts:AssumeRole"}]}`
	TrustEC2      = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"ec2.amazonaws.com"},"Action":"sts:AssumeRole"}]}`

	PolicyECSTaskExecution = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
	PolicyECSForEC2        = "arn:aws:iam::aws:policy/service-role/AmazonEC2ContainerServiceforEC2Role"

	IAMPath = "/mamela/"
)

// IAM is the part of the AWS IAM API for the roles and instance profiles of ECS: what may be made
// is held to the values above, and every change names the role (or profile) it is for.
var IAM = API{Name: "iam", Model: "iam", Ops: []Op{
	{Name: "GetRole", Params: []string{"RoleName"}},
	{Name: "CreateRole", Params: []string{"RoleName", "Path", "AssumeRolePolicyDocument", "Description",
		"Tags.member.1.Key", "Tags.member.1.Value", "Tags.member.2.Key", "Tags.member.2.Value"},
		Values: map[string][]string{"Path": {IAMPath}, "AssumeRolePolicyDocument": {TrustECSTasks, TrustEC2}}},
	// Only a role's description: the marker of a role a person adopts.
	{Name: "UpdateRole", Params: []string{"RoleName", "Description"}, Bind: []string{"RoleName"}},
	{Name: "DeleteRole", Params: []string{"RoleName"}, Bind: []string{"RoleName"}},
	{Name: "AttachRolePolicy", Params: []string{"RoleName", "PolicyArn"}, Bind: []string{"RoleName"},
		Values: map[string][]string{"PolicyArn": {PolicyECSTaskExecution, PolicyECSForEC2}}},
	{Name: "DetachRolePolicy", Params: []string{"RoleName", "PolicyArn"}, Bind: []string{"RoleName"}},
	{Name: "ListAttachedRolePolicies", Params: []string{"RoleName", "PathPrefix", "Marker", "MaxItems"}},
	{Name: "ListRoles", Params: []string{"PathPrefix", "Marker", "MaxItems"}},
	{Name: "CreateInstanceProfile", Params: []string{"InstanceProfileName", "Path",
		"Tags.member.1.Key", "Tags.member.1.Value", "Tags.member.2.Key", "Tags.member.2.Value"},
		Values: map[string][]string{"Path": {IAMPath}}},
	{Name: "GetInstanceProfile", Params: []string{"InstanceProfileName"}},
	{Name: "AddRoleToInstanceProfile", Params: []string{"InstanceProfileName", "RoleName"}, Bind: []string{"RoleName"}},
	{Name: "RemoveRoleFromInstanceProfile", Params: []string{"InstanceProfileName", "RoleName"}, Bind: []string{"RoleName"}},
	{Name: "DeleteInstanceProfile", Params: []string{"InstanceProfileName"}, Bind: []string{"InstanceProfileName"}},
}}
