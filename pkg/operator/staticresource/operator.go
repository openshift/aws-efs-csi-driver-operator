package staticresource

import (
	"context"
	"time"

	opv1 "github.com/openshift/api/operator/v1"
	"github.com/openshift/library-go/pkg/controller/factory"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/management"
	"github.com/openshift/library-go/pkg/operator/resource/resourceapply"
	operatorv1helpers "github.com/openshift/library-go/pkg/operator/v1helpers"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// CSIStaticResourceController creates, manages and deletes static resources of a CSI driver, such as RBAC rules.
// It's more hardcoded variant of library-go's StaticResourceController, which does not implement removal
// of objects yet.
type CSIStaticResourceController struct {
	operatorName      string
	operatorNamespace string
	operatorClient    operatorv1helpers.OperatorClientWithFinalizers
	kubeClient        kubernetes.Interface
	eventRecorder     events.Recorder

	// Objects to sync
	csiDriver          *storagev1.CSIDriver
	nodeServiceAccount *corev1.ServiceAccount
	nodeRole           *rbacv1.ClusterRole
	nodeRoleBinding    *rbacv1.ClusterRoleBinding
}

func NewCSIStaticResourceController(
	name string,
	operatorNamespace string,
	operatorClient operatorv1helpers.OperatorClientWithFinalizers,
	kubeClient kubernetes.Interface,
	informers operatorv1helpers.KubeInformersForNamespaces,
	recorder events.Recorder,
	csiDriver *storagev1.CSIDriver,
	nodeServiceAccount *corev1.ServiceAccount,
	nodeRole *rbacv1.ClusterRole,
	nodeRoleBinding *rbacv1.ClusterRoleBinding,
) factory.Controller {
	c := &CSIStaticResourceController{
		operatorName:       name,
		operatorNamespace:  operatorNamespace,
		operatorClient:     operatorClient,
		kubeClient:         kubeClient,
		eventRecorder:      recorder,
		csiDriver:          csiDriver,
		nodeServiceAccount: nodeServiceAccount,
		nodeRole:           nodeRole,
		nodeRoleBinding:    nodeRoleBinding,
	}

	operatorInformers := []factory.Informer{
		operatorClient.Informer(),
		informers.InformersFor(operatorNamespace).Core().V1().ServiceAccounts().Informer(),
		informers.InformersFor(operatorNamespace).Storage().V1().CSIDrivers().Informer(),
		informers.InformersFor(operatorNamespace).Rbac().V1().ClusterRoles().Informer(),
		informers.InformersFor(operatorNamespace).Rbac().V1().ClusterRoleBindings().Informer(),
	}
	return factory.New().
		WithSyncDegradedOnError(operatorClient).
		WithInformers(operatorInformers...).
		WithSync(c.sync).
		ResyncEvery(time.Minute).
		ToController(name, recorder.WithComponentSuffix("csi-static-resource-controller"))
}

func (c *CSIStaticResourceController) sync(ctx context.Context, controllerContext factory.SyncContext) error {
	opSpec, opStatus, _, err := c.operatorClient.GetOperatorState()
	if apierrors.IsNotFound(err) {
		// TODO: proceed with removal?
		return nil
	}
	if err != nil {
		return err
	}

	if opSpec.ManagementState != opv1.Managed {
		return nil
	}

	meta, err := c.operatorClient.GetObjectMeta()
	if err != nil {
		return err
	}
	if management.IsOperatorRemovable() && meta.DeletionTimestamp != nil {
		return c.syncDeleting(ctx, opSpec, opStatus, controllerContext)
	}
	return c.syncManaged(ctx, opSpec, opStatus, controllerContext)
}

func (c *CSIStaticResourceController) syncManaged(ctx context.Context, opSpec *opv1.OperatorSpec, opStatus *opv1.OperatorStatus, controllerContext factory.SyncContext) error {
	err := operatorv1helpers.EnsureFinalizer(c.operatorClient, c.operatorName)
	if err != nil {
		return err
	}

	var errs []error
	_, _, err = resourceapply.ApplyCSIDriver(ctx, c.kubeClient.StorageV1(), c.eventRecorder, c.csiDriver)
	if err != nil {
		errs = append(errs, err)
	}
	_, _, err = resourceapply.ApplyClusterRole(ctx, c.kubeClient.RbacV1(), c.eventRecorder, c.nodeRole)
	if err != nil {
		errs = append(errs, err)
	}
	_, _, err = resourceapply.ApplyClusterRoleBinding(ctx, c.kubeClient.RbacV1(), c.eventRecorder, c.nodeRoleBinding)
	if err != nil {
		errs = append(errs, err)
	}
	_, _, err = resourceapply.ApplyServiceAccount(ctx, c.kubeClient.CoreV1(), c.eventRecorder, c.nodeServiceAccount)
	if err != nil {
		errs = append(errs, err)
	}

	return errors.NewAggregate(errs)
}

func (c *CSIStaticResourceController) syncDeleting(ctx context.Context, opSpec *opv1.OperatorSpec, opStatus *opv1.OperatorStatus, controllerContext factory.SyncContext) error {
	var errs []error
	if err := c.kubeClient.StorageV1().CSIDrivers().Delete(ctx, c.csiDriver.Name, metav1.DeleteOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		} else {
			klog.V(4).Infof("CSIDriver %s already removed", c.csiDriver.Name)
		}
	}

	if err := c.kubeClient.RbacV1().ClusterRoles().Delete(ctx, c.nodeRole.Name, metav1.DeleteOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		} else {
			klog.V(4).Infof("ClusterRole %s already removed", c.nodeRole.Name)
		}
	}

	if err := c.kubeClient.RbacV1().ClusterRoleBindings().Delete(ctx, c.nodeRoleBinding.Name, metav1.DeleteOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		} else {
			klog.V(4).Infof("ClusterRoleBinding %s already removed", c.nodeRoleBinding.Name)
		}
	}

	if err := c.kubeClient.CoreV1().ServiceAccounts(c.operatorNamespace).Delete(ctx, c.nodeServiceAccount.Name, metav1.DeleteOptions{}); err != nil {
		if !apierrors.IsNotFound(err) {
			errs = append(errs, err)
		} else {
			klog.V(4).Infof("ServiceAccount %s already removed", c.nodeServiceAccount.Name)
		}
	}
	if err := errors.NewAggregate(errs); err != nil {
		return err
	}

	// All removed, remove the finalizer as the last step
	return operatorv1helpers.RemoveFinalizer(c.operatorClient, c.operatorName)
}
