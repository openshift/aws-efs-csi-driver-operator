package efscreate

import (
	"context"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"os"
	"strings"
)

const (
	operatorName          = "create-efs-volume"
	infraGlobalName       = "cluster"
	secretNamespace       = "kube-system"
	secretName            = "aws-creds"
	STORAGECLASS_LOCATION = "STORAGEClASS_LOCATION"
	MANIFEST_LOCATION     = "MANIFEST_LOCATION"
)

func RunOperator(ctx context.Context, controllerConfig *controllercmd.ControllerContext) error {
	// Create core clientset for core and infra objects
	kubeClient := kubeclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	nodes, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		klog.Errorf("error listing nodes: %v", err)
		return fmt.Errorf("error listing nodes: %v", err)
	}

	configClient := configclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	infra, err := configClient.ConfigV1().Infrastructures().Get(ctx, infraGlobalName, metav1.GetOptions{})
	if err != nil {
		klog.Errorf("error listing infrastructures objects: %v", err)
		return fmt.Errorf("error listing infrastructure objects: %v", err)
	}
	ec2Session, err := getEC2Client(ctx, kubeClient, infra.Status.PlatformStatus.AWS.Region)
	if err != nil {
		klog.Errorf("error getting aws client: %v", err)
		return fmt.Errorf("error getting aws client: %v", err)
	}

	efs := NewEFS_Session(infra, ec2Session)

	fsID, err := efs.CreateEFSVolume(nodes)
	if err != nil {
		klog.Errorf("error creating efs volume: %v", err)
		return err
	}
	klog.Infof("created fsID: %s", fsID)
	err = writeStorageClassFile(fsID)
	if err != nil {
		klog.Errorf("error writing storageclass to location %s: %v", os.Getenv(STORAGECLASS_LOCATION), err)
		return err
	}
	err = writeCSIManifest("efs-cs")
	if err != nil {
		klog.Errorf("error writing manifest to location %s: %v", os.Getenv(MANIFEST_LOCATION), err)
		return err
	}

	return nil
}

func writeStorageClassFile(fsID string) error {
	fileName := os.Getenv(STORAGECLASS_LOCATION)
	scContent := `
kind: StorageClass
apiVersion: storage.k8s.io/v1
metadata:
  name: efs-sc
provisioner: efs.csi.aws.com
mountOptions:
  - tls
parameters:
  provisioningMode: efs-ap
  fileSystemId: ${filesystemid}
  directoryPerms: "700"
  basePath: "/dynamic_provisioning"
    `
	replaceStrings := []string{
		"${filesystemid}", fsID,
	}
	replacer := strings.NewReplacer(replaceStrings...)
	finalSCContent := replacer.Replace(scContent)
	f, err := os.Create(fileName)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(finalSCContent)
	if err != nil {
		return err
	}
	return nil
}

func writeCSIManifest(scName string) error {
	manifestLocation := os.Getenv(MANIFEST_LOCATION)
	manifestContent := `
StorageClass:
  FromExistingClassName: ${storageclassname}
SnapshotClass:
  FromName: true
DriverInfo:
  Name: efs.csi.aws.com
  SupportedSizeRange:
    Min: 1Gi
    Max: 64Ti
  Capabilities:
    persistence: true
    fsGroup: false
    block: false
    exec: true
    volumeLimits: false
    controllerExpansion: false
    nodeExpansion: false
    snapshotDataSource: false
    RWX: true
    topology: false
	`
	replaceStrings := []string{
		"${storageclassname}", scName,
	}
	replacer := strings.NewReplacer(replaceStrings...)
	finalManifestContent := replacer.Replace(manifestContent)
	f, err := os.Create(manifestLocation)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.WriteString(finalManifestContent)
	if err != nil {
		return err
	}
	return nil
}

func getEC2Client(ctx context.Context, client kubeclient.Interface, region string) (*session.Session, error) {
	// get AWS credentials
	awsCreds, err := client.CoreV1().Secrets(secretNamespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	// detect region
	// base64 decode
	id, found := awsCreds.Data["aws_access_key_id"]
	if !found {
		return nil, fmt.Errorf("cloud credential id not found")
	}
	key, found := awsCreds.Data["aws_secret_access_key"]
	if !found {
		return nil, fmt.Errorf("cloud credential key not found")
	}

	sess, err := session.NewSession(&aws.Config{
		Region:      aws.String(region),
		Credentials: credentials.NewStaticCredentials(string(id), string(key), ""),
	})
	if err != nil {
		return nil, err
	}
	return sess, nil
}
