package catalogs

import (
	"strings"

	"github.com/ieyoukan/mamela/provider/protocol"
)

// The AWS APIs that ECS on EC2 and Fargate needs besides EC2 (ADR 0007): ECS, CloudWatch Logs and
// SSM are JSON APIs (POST / with an X-Amz-Target header), IAM is a query API like EC2. Requests are
// signed with SigV4 (auth "aws-sigv4" in mamela.providers.toml), each API with its own service.

// ECS is the part of the Amazon ECS API that a provider of clusters, task definitions, services
// and tasks uses. A task definition's size is its task-level cpu (CPU units, 1024 to a vCPU) and
// memory (MiB): the site's limits hold for each.
var ECS = protocol.Catalog{API: "ecs", Entries: []protocol.Entry{
	ecsCall("CreateCluster", protocol.EffectCreate, "clusterName", "capacityProviders", "defaultCapacityProviderStrategy", "tags", "settings"),
	ecsCall("ListClusters", protocol.EffectRead, "nextToken", "maxResults"),
	ecsCall("DescribeClusters", protocol.EffectRead, "clusters", "include"),
	bind(ecsCall("DeleteCluster", protocol.EffectLoseData, "cluster"), "cluster"),
	withMeasures(ecsCall("RegisterTaskDefinition", protocol.EffectCreate, "family", "containerDefinitions", "cpu", "memory", "networkMode",
		"requiresCompatibilities", "executionRoleArn", "taskRoleArn", "tags", "runtimePlatform"),
		protocol.Measure{Param: "cpu", Name: protocol.MeasureCPU, Format: protocol.FormatCPUUnits},
		protocol.Measure{Param: "memory", Name: protocol.MeasureMemoryMiB, Format: protocol.FormatCount}),
	ecsCall("DescribeTaskDefinition", protocol.EffectRead, "taskDefinition", "include"),
	ecsCall("ListTaskDefinitions", protocol.EffectRead, "familyPrefix", "status", "sort", "nextToken", "maxResults"),
	bind(ecsCall("DeregisterTaskDefinition", protocol.EffectChange, "taskDefinition"), "taskDefinition"),
	ecsCall("CreateService", protocol.EffectCreate, "cluster", "serviceName", "taskDefinition", "desiredCount", "launchType",
		"capacityProviderStrategy", "networkConfiguration", "placementConstraints", "placementStrategy", "schedulingStrategy",
		"deploymentConfiguration", "tags", "clientToken", "enableECSManagedTags", "propagateTags"),
	ecsCall("ListServices", protocol.EffectRead, "cluster", "launchType", "schedulingStrategy", "nextToken", "maxResults"),
	bind(ecsCall("UpdateService", protocol.EffectChange, "cluster", "service", "desiredCount", "taskDefinition", "forceNewDeployment"), "cluster", "service"),
	ecsCall("DescribeServices", protocol.EffectRead, "cluster", "services", "include"),
	bind(ecsCall("DeleteService", protocol.EffectLoseData, "cluster", "service", "force"), "cluster", "service"),
	// A task runs in the cluster of the object the change is for (a service whose way of running
	// it reuses): never in someone else's.
	bind(ecsCall("RunTask", protocol.EffectCreate, "cluster", "taskDefinition", "count", "launchType", "capacityProviderStrategy",
		"networkConfiguration", "overrides", "startedBy", "clientToken", "tags", "enableECSManagedTags", "propagateTags"), "cluster"),
	ecsCall("ListTasks", protocol.EffectRead, "cluster", "startedBy", "family", "desiredStatus", "serviceName", "containerInstance", "nextToken", "maxResults"),
	ecsCall("DescribeTasks", protocol.EffectRead, "cluster", "tasks", "include"),
	bind(ecsCall("StopTask", protocol.EffectRestart, "cluster", "task", "reason"), "cluster", "task"),
	ecsCall("ListContainerInstances", protocol.EffectRead, "cluster", "filter", "status", "nextToken", "maxResults"),
	ecsCall("DescribeContainerInstances", protocol.EffectRead, "cluster", "containerInstances"),
}}

// Logs is the part of the Amazon CloudWatch Logs API for a log group of a provider's tasks.
var Logs = protocol.Catalog{API: "logs", Entries: []protocol.Entry{
	logsCall("CreateLogGroup", protocol.EffectCreate, "logGroupName", "tags"),
	bind(logsCall("PutRetentionPolicy", protocol.EffectChange, "logGroupName", "retentionInDays"), "logGroupName"),
	logsCall("DescribeLogGroups", protocol.EffectRead, "logGroupNamePrefix", "limit", "nextToken"),
	bind(logsCall("DeleteLogGroup", protocol.EffectLoseData, "logGroupName"), "logGroupName"),
	logsCall("DescribeLogStreams", protocol.EffectRead, "logGroupName", "logStreamNamePrefix", "orderBy", "descending", "limit", "nextToken"),
	logsCall("GetLogEvents", protocol.EffectRead, "logGroupName", "logStreamName", "startFromHead", "limit", "nextToken"),
}}

// SSM is the part of the AWS Systems Manager API for reading public parameters (the ECS-optimized
// AMI's ID).
var SSM = protocol.Catalog{API: "ssm", Entries: []protocol.Entry{
	jsonCall("AmazonSSM.GetParameter", protocol.EffectRead, "Name", "WithDecryption"),
}}

// IAMVersion is the version of the IAM query API the catalog describes.
const IAMVersion = "2010-05-08"

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
// is held to the values above.
var IAM = protocol.Catalog{API: "iam", Entries: []protocol.Entry{
	iamCall("GetRole", protocol.EffectRead, "RoleName"),
	values(iamCall("CreateRole", protocol.EffectCreate, "RoleName", "Path", "AssumeRolePolicyDocument", "Description",
		"Tags.member.1.Key", "Tags.member.1.Value", "Tags.member.2.Key", "Tags.member.2.Value"),
		map[string][]string{"Path": {IAMPath}, "AssumeRolePolicyDocument": {TrustECSTasks, TrustEC2}}),
	bind(iamCall("DeleteRole", protocol.EffectLoseData, "RoleName"), "RoleName"),
	values(bind(iamCall("AttachRolePolicy", protocol.EffectChange, "RoleName", "PolicyArn"), "RoleName"),
		map[string][]string{"PolicyArn": {PolicyECSTaskExecution, PolicyECSForEC2}}),
	bind(iamCall("DetachRolePolicy", protocol.EffectChange, "RoleName", "PolicyArn"), "RoleName"),
	iamCall("ListAttachedRolePolicies", protocol.EffectRead, "RoleName", "PathPrefix", "Marker", "MaxItems"),
	iamCall("ListRoles", protocol.EffectRead, "PathPrefix", "Marker", "MaxItems"),
	values(iamCall("CreateInstanceProfile", protocol.EffectCreate, "InstanceProfileName", "Path",
		"Tags.member.1.Key", "Tags.member.1.Value", "Tags.member.2.Key", "Tags.member.2.Value"),
		map[string][]string{"Path": {IAMPath}}),
	iamCall("GetInstanceProfile", protocol.EffectRead, "InstanceProfileName"),
	bind(iamCall("AddRoleToInstanceProfile", protocol.EffectChange, "InstanceProfileName", "RoleName"), "RoleName"),
	bind(iamCall("RemoveRoleFromInstanceProfile", protocol.EffectChange, "InstanceProfileName", "RoleName"), "RoleName"),
	bind(iamCall("DeleteInstanceProfile", protocol.EffectLoseData, "InstanceProfileName"), "InstanceProfileName"),
}}

func iamCall(action string, effect protocol.Effect, form ...string) protocol.Entry {
	return protocol.Entry{Method: "POST", Path: "/", Effect: effect, Form: append([]string{"Action", "Version"}, form...),
		Fixed: map[string]string{"Action": action, "Version": IAMVersion}}
}

func ecsCall(action string, effect protocol.Effect, members ...string) protocol.Entry {
	return jsonCall("AmazonEC2ContainerServiceV20141113."+action, effect, members...)
}

func logsCall(action string, effect protocol.Effect, members ...string) protocol.Entry {
	return jsonCall("Logs_20140328."+action, effect, members...)
}

// jsonCall is a call of an AWS JSON API: POST / told apart by its X-Amz-Target header.
func jsonCall(target string, effect protocol.Effect, members ...string) protocol.Entry {
	return protocol.Entry{Method: "POST", Path: "/", Effect: effect, Header: []string{"X-Amz-Target"}, JSON: members,
		Fixed: map[string]string{"X-Amz-Target": target}}
}

func values(e protocol.Entry, v map[string][]string) protocol.Entry {
	e.Values = v
	return e
}

// AWSTarget is the X-Amz-Target of a JSON API call's action ("ecs", "RunTask").
func AWSTarget(api, action string) string {
	switch strings.ToLower(api) {
	case "ecs":
		return "AmazonEC2ContainerServiceV20141113." + action
	case "logs":
		return "Logs_20140328." + action
	case "ssm":
		return "AmazonSSM." + action
	}
	return ""
}
