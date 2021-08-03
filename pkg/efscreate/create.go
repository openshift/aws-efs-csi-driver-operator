package efscreate

import (
	"context"
	"fmt"
	v1 "github.com/openshift/api/config/v1"
	"io/ioutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/openshift/aws-efs-csi-driver-operator/assets"
	configclient "github.com/openshift/client-go/config/clientset/versioned"
	"github.com/openshift/library-go/pkg/controller/controllercmd"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubeclient "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

const (
	operatorName          = "create-efs-volume"
	infraGlobalName       = "cluster"
	secretNamespace       = "kube-system"
	secretName            = "aws-creds"
	storageClassName      = "efs-sc"
	STORAGECLASS_LOCATION = "STORAGEClASS_LOCATION"
	MANIFEST_LOCATION     = "MANIFEST_LOCATION"
	fileMode              = 0640
)

func RunOperator(ctx context.Context, controllerConfig *controllercmd.ControllerContext) error {
	// Create core clientset for core and infra objects
	kubeClient := kubeclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	nodes, err := getNodes(ctx, kubeClient)
	if err != nil {
		klog.Errorf("error listing nodes: %v", err)
		return fmt.Errorf("error listing nodes: %v", err)
	}

	configClient := configclient.NewForConfigOrDie(rest.AddUserAgent(controllerConfig.KubeConfig, operatorName))
	infra, err := getInfra(ctx, configClient)
	if err != nil {
		klog.Errorf("error listing infrastructures objects: %v", err)
		return fmt.Errorf("error listing infrastructure objects: %v", err)
	}
	ec2Session, err := getEC2Client(ctx, kubeClient, infra.Status.PlatformStatus.AWS.Region)
	if err != nil {
		klog.Errorf("error getting aws client: %v", err)
		return fmt.Errorf("error getting aws client: %v", err)
	}

	efs := NewEFSSession(infra, ec2Session)

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
	err = writeCSIManifest(storageClassName)
	if err != nil {
		klog.Errorf("error writing manifest to location %s: %v", os.Getenv(MANIFEST_LOCATION), err)
		return err
	}

	return nil
}

func getInfra(ctx context.Context, infraClient *configclient.Clientset) (infra *v1.Infrastructure, err error) {
	backoff := wait.Backoff{
		Duration: operationDelay,
		Factor:   operationBackoffFactor,
		Steps:    operationRetryCount,
	}
	err = wait.ExponentialBackoffWithContext(ctx, backoff, func() (bool, error) {
		var apiError error
		infra, apiError = infraClient.ConfigV1().Infrastructures().Get(ctx, infraGlobalName, metav1.GetOptions{})
		if apiError != nil {
			klog.Errorf("error listing infrastructures objects: %v", apiError)
			return false, nil
		}
		if infra != nil {
			return true, nil
		}
		return false, nil
	})
	return
}

func getSecret(ctx context.Context, client *kubeclient.Clientset) (*corev1.Secret, error) {
	backoff := wait.Backoff{
		Duration: operationDelay,
		Factor:   operationBackoffFactor,
		Steps:    operationRetryCount,
	}
	var awsCreds *corev1.Secret
	err := wait.ExponentialBackoffWithContext(ctx, backoff, func() (bool, error) {
		var apiError error
		awsCreds, apiError = client.CoreV1().Secrets(secretNamespace).Get(ctx, secretName, metav1.GetOptions{})
		if apiError != nil {
			klog.Errorf("error getting secret object: %v", apiError)
			return false, nil
		}
		if awsCreds != nil {
			return true, nil
		}
		return false, nil
	})
	return awsCreds, err
}

func getNodes(ctx context.Context, client *kubeclient.Clientset) (*corev1.NodeList, error) {
	backoff := wait.Backoff{
		Duration: operationDelay,
		Factor:   operationBackoffFactor,
		Steps:    operationRetryCount,
	}
	var nodes *corev1.NodeList
	err := wait.ExponentialBackoffWithContext(ctx, backoff, func() (bool, error) {
		var apiError error
		nodes, apiError = client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if apiError != nil {
			klog.Errorf("error listing node objects: %v", apiError)
			return false, nil
		}
		if nodes != nil {
			return true, nil
		}
		return false, nil
	})
	return nodes, err
}

func writeStorageClassFile(fsID string) error {
	fileName := os.Getenv(STORAGECLASS_LOCATION)
	if len(fileName) == 0 {
		return fmt.Errorf("no storageclass location specified")
	}

	scContentBytes, err := assets.ReadFile("testing/sc.yaml")
	if err != nil {
		return err
	}
	scContent := string(scContentBytes)
	replaceStrings := []string{
		"${storageclassname}", storageClassName,
		"${filesystemid}", fsID,
	}
	replacer := strings.NewReplacer(replaceStrings...)
	finalSCContent := replacer.Replace(scContent)
	err = ioutil.WriteFile(fileName, []byte(finalSCContent), fileMode)
	return err
}

func writeCSIManifest(scName string) error {
	manifestLocation := os.Getenv(MANIFEST_LOCATION)
	if len(manifestLocation) == 0 {
		return fmt.Errorf("no manifest location specified")
	}
	manifestContentBytes, err := assets.ReadFile("testing/manifest.yaml")
	manifestContent := string(manifestContentBytes)
	replaceStrings := []string{
		"${storageclassname}", scName,
	}
	replacer := strings.NewReplacer(replaceStrings...)
	finalManifestContent := replacer.Replace(manifestContent)
	err = ioutil.WriteFile(manifestLocation, []byte(finalManifestContent), fileMode)
	return err
}

func getEC2Client(ctx context.Context, client *kubeclient.Clientset, region string) (*session.Session, error) {
	// get AWS credentials
	awsCreds, err := getSecret(ctx, client)
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
