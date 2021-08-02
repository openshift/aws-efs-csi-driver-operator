package efscreate

import (
	"fmt"
	v1 "github.com/openshift/api/config/v1"
	corev1 "k8s.io/api/core/v1"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	awsefs "github.com/aws/aws-sdk-go/service/efs"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	volumeCreateInitialDelay  = 5 * time.Second
	volumeCreateBackoffFactor = 1.2
	volumeCreateBackoffSteps  = 10

	operationDelay          = 2 * time.Second
	operationBackoffFactor  = 1.2
	operationRetryCount     = 5
	tagFormat               = "kubernetes.io/cluster/%s"
	efsVolumeNameFormat     = "%s-efs"
	securityGroupNameFormat = "%s-sg"
)

type EFS struct {
	infra     *v1.Infrastructure
	client    *ec2.EC2
	efsClient *awsefs.EFS
	vpcID     string
	cidrBlock string
	subnetIDs []string
	resources *ResourceInfo
}

// store resources that the code created
type ResourceInfo struct {
	securityGrouID string
	efsID          string
	mountTargets   []string
}

func NewEFS_Session(infra *v1.Infrastructure, sess *session.Session) *EFS {
	service := ec2.New(sess)
	efsClient := awsefs.New(sess)
	return &EFS{
		client:    service,
		efsClient: efsClient,
		infra:     infra,
		subnetIDs: []string{},
		resources: &ResourceInfo{},
	}
}

func (efs *EFS) CreateEFSVolume(nodes *corev1.NodeList) (string, error) {
	instances := efs.getInstanceIDs(nodes)
	err := efs.getSecurityInfo(instances)
	if err != nil {
		return "", err
	}
	sgid, err := efs.createSecurityGroup()
	if err != nil {
		return "", err
	}
	efs.resources.securityGrouID = sgid
	ok, err := efs.addFireWallRule(sgid)
	if err != nil || !ok {
		return "", fmt.Errorf("error adding firewall rule: %v", err)
	}

	fileSystemID, err := efs.createEFSFileSystem()
	if err != nil {
		return "", err
	}
	efs.resources.efsID = fileSystemID
	mts, err := efs.createMountTargets(fileSystemID, sgid)
	if err != nil {
		return "", err
	}
	efs.resources.mountTargets = mts
	log("successfully created file system %s", fileSystemID)
	return fileSystemID, nil
}

func (efs *EFS) getInstanceIDs(nodes *corev1.NodeList) []string {
	nodeIDs := sets.NewString()
	for _, node := range nodes.Items {
		//get providerID of the form aws:///us-west-2a/i-0304804a704fefb7d
		instanceString := node.Spec.ProviderID
		instanceStringArray := strings.Split(instanceString, "/")
		if len(instanceStringArray) > 0 {
			nodeIDs.Insert(instanceStringArray[len(instanceStringArray)-1])
		}
	}
	return nodeIDs.List()
}

func (efs *EFS) createSecurityGroup() (string, error) {
	infraID := efs.infra.Status.InfrastructureName
	groupName := fmt.Sprintf(securityGroupNameFormat, infraID)
	securityGroupInput := ec2.CreateSecurityGroupInput{
		Description:       aws.String("for testing efs driver"),
		GroupName:         aws.String(groupName),
		VpcId:             &efs.vpcID,
		TagSpecifications: efs.getTags(ec2.ResourceTypeSecurityGroup, groupName),
	}
	response, err := efs.client.CreateSecurityGroup(&securityGroupInput)
	if err != nil {
		return "", fmt.Errorf("error creating security group")
	}
	return *response.GroupId, nil
}

func (efs *EFS) getTags(resourceType string, resourceName string) []*ec2.TagSpecification {
	var tagList []*ec2.Tag
	tags := map[string]string{
		"Name":                 resourceName,
		efs.getClusterTagKey(): "owned",
	}
	for k, v := range tags {
		tagList = append(tagList, &ec2.Tag{
			Key: aws.String(k), Value: aws.String(v),
		})
	}
	return []*ec2.TagSpecification{
		{
			Tags:         tagList,
			ResourceType: aws.String(resourceType),
		},
	}
}

func (efs *EFS) getClusterTagKey() string {
	return fmt.Sprintf(tagFormat, efs.infra.Status.InfrastructureName)
}

func (efs *EFS) addFireWallRule(groupId string) (bool, error) {
	ruleInput := ec2.AuthorizeSecurityGroupIngressInput{
		CidrIp:     aws.String(efs.cidrBlock),
		GroupId:    aws.String(groupId),
		IpProtocol: aws.String("tcp"),
		ToPort:     aws.Int64(2049),
		FromPort:   aws.Int64(2049),
	}
	response, err := efs.client.AuthorizeSecurityGroupIngress(&ruleInput)
	if err != nil {
		return false, fmt.Errorf("error creating firewall rule: %v", err)
	}
	return *response.Return, nil
}

func (efs *EFS) DestroyAll() error {
	var err error
	if len(efs.resources.mountTargets) > 0 {
		err = efs.deleteMountTarget()
	}
	if len(efs.resources.efsID) != 0 {
		err = efs.deleteEFS()
	}

	if len(efs.resources.securityGrouID) != 0 {
		err = efs.deleteSecurityGroup()
	}
	return err
}

func (efs *EFS) deleteMountTarget() error {
	for _, mt := range efs.resources.mountTargets {
		log("Deleting mount target id: %s\n", mt)
		deleteTargetInput := &awsefs.DeleteMountTargetInput{
			MountTargetId: aws.String(mt),
		}
		_, err := efs.efsClient.DeleteMountTarget(deleteTargetInput)
		if err != nil {
			return fmt.Errorf("failed to delete mount target: %v", err)
		}
		log("successfully deleted mount target %s", mt)
	}
	return nil
}

func log(msg string, args ...interface{}) {
	msgString := fmt.Sprintf("%s\n", msg)
	fmt.Printf(msgString, args...)
}

func (efs *EFS) deleteEFS() error {
	backoff := wait.Backoff{
		Duration: volumeCreateInitialDelay,
		Factor:   operationBackoffFactor,
		Steps:    operationRetryCount,
	}
	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		deleteEFSInput := &awsefs.DeleteFileSystemInput{FileSystemId: aws.String(efs.resources.efsID)}
		_, delError := efs.efsClient.DeleteFileSystem(deleteEFSInput)
		if delError != nil {
			log("error deleting filesystem %s: %v", efs.resources.efsID, delError)
			return false, nil
		}
		log("successfully deleted filesystem %s", efs.resources.efsID)
		return true, nil
	})
	return err
}

func (efs *EFS) deleteSecurityGroup() error {
	backoff := wait.Backoff{
		Duration: operationDelay,
		Factor:   operationBackoffFactor,
		Steps:    operationRetryCount,
	}
	err := wait.ExponentialBackoff(backoff, func() (bool, error) {
		deleteGroupInput := &ec2.DeleteSecurityGroupInput{GroupId: aws.String(efs.resources.securityGrouID)}
		_, delError := efs.client.DeleteSecurityGroup(deleteGroupInput)
		if delError != nil {
			log("error deleting security group %s: %v", efs.resources.securityGrouID, delError)
			return false, nil
		}
		log("successfully deleted securityGroup %s", efs.resources.securityGrouID)
		return true, nil
	})
	return err

}

func (efs *EFS) createEFSFileSystem() (string, error) {
	volumeName := fmt.Sprintf(efsVolumeNameFormat, efs.infra.Status.InfrastructureName)
	input := &awsefs.CreateFileSystemInput{
		Encrypted:       aws.Bool(true),
		PerformanceMode: aws.String(awsefs.PerformanceModeGeneralPurpose),
		Tags: []*awsefs.Tag{
			{
				Key:   aws.String("Name"),
				Value: aws.String(volumeName),
			},
			{
				Key:   aws.String(efs.getClusterTagKey()),
				Value: aws.String("owned"),
			},
		},
	}
	response, err := efs.efsClient.CreateFileSystem(input)
	if err != nil {
		log("error creating filesystem: %v", err)
		return "", fmt.Errorf("error creating filesystem: %v", err)
	}
	err = efs.waitForEFSToBeAvailable(*response.FileSystemId)
	if err != nil {
		log("error waiting for filesystem to become available: %v", err)
		return *response.FileSystemId, err
	}
	return *response.FileSystemId, nil
}

func (efs *EFS) createMountTargets(efsID string, sgID string) ([]string, error) {
	var mountTargets []string
	for i := range efs.subnetIDs {
		subnet := efs.subnetIDs[i]
		mountTargetInput := &awsefs.CreateMountTargetInput{
			FileSystemId:   aws.String(efsID),
			SecurityGroups: []*string{aws.String(sgID)},
			SubnetId:       aws.String(subnet),
		}
		mt, err := efs.efsClient.CreateMountTarget(mountTargetInput)
		if err != nil {
			return mountTargets, fmt.Errorf("error creating mount target: %v", err)
		}
		mountTargets = append(mountTargets, *mt.MountTargetId)
	}
	return mountTargets, nil
}

func (efs *EFS) waitForEFSToBeAvailable(efsID string) error {
	describeInput := &awsefs.DescribeFileSystemsInput{FileSystemId: aws.String(efsID)}
	backoff := wait.Backoff{
		Duration: volumeCreateInitialDelay,
		Factor:   volumeCreateBackoffFactor,
		Steps:    volumeCreateBackoffSteps,
	}
	err := wait.ExponentialBackoff(backoff, func() (done bool, err error) {
		response, err := efs.efsClient.DescribeFileSystems(describeInput)
		if err != nil {
			return false, err
		}
		filesystems := response.FileSystems
		if len(filesystems) < 1 {
			return false, nil
		}
		fs := filesystems[0]
		if *fs.LifeCycleState != awsefs.LifeCycleStateAvailable {
			return false, nil
		}
		return true, nil
	})
	return err
}

func (efs *EFS) getSecurityInfo(instances []string) error {
	var instancePointers []*string
	for i := range instances {
		instancePointers = append(instancePointers, &instances[i])
	}
	request := &ec2.DescribeInstancesInput{
		InstanceIds: instancePointers,
	}
	var results []*ec2.Instance
	var nextToken *string

	for {
		response, err := efs.client.DescribeInstances(request)
		if err != nil {
			return fmt.Errorf("error listing AWS instances: %v", err)
		}

		for _, reservation := range response.Reservations {
			results = append(results, reservation.Instances...)
		}

		nextToken = response.NextToken
		if nextToken == nil || len(*nextToken) == 0 {
			break
		}
		request.NextToken = nextToken
	}
	if len(results) < 1 {
		return fmt.Errorf("no matching instances found")
	}
	instance := results[0]
	efs.vpcID = *instance.VpcId

	vpcRequest := &ec2.DescribeVpcsInput{VpcIds: []*string{instance.VpcId}}
	response, err := efs.client.DescribeVpcs(vpcRequest)
	if err != nil {
		return fmt.Errorf("error listing vpc: %v", err)
	}
	clusterVPCs := response.Vpcs
	if len(clusterVPCs) < 1 {
		return fmt.Errorf("no matching vpc found for %s", efs.vpcID)
	}
	clusterVPC := clusterVPCs[0]
	efs.cidrBlock = *clusterVPC.CidrBlock

	subNetSet := sets.NewString()
	for i := range results {
		subNetSet.Insert(*results[i].SubnetId)
	}
	efs.subnetIDs = subNetSet.List()
	return nil
}
