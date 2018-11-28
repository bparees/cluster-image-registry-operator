package operator

import (
	"fmt"
	"time"

	"github.com/golang/glog"

	coreapi "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metaapi "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	operatorapi "github.com/openshift/api/operator/v1alpha1"

	regopapi "github.com/openshift/cluster-image-registry-operator/pkg/apis/imageregistry/v1alpha1"
	regopset "github.com/openshift/cluster-image-registry-operator/pkg/generated/clientset/versioned/typed/imageregistry/v1alpha1"
	osapi "github.com/openshift/cluster-version-operator/pkg/apis/operatorstatus.openshift.io/v1"

	"github.com/openshift/cluster-image-registry-operator/pkg/client"
	"github.com/openshift/cluster-image-registry-operator/pkg/clusteroperator"
	"github.com/openshift/cluster-image-registry-operator/pkg/metautil"
	"github.com/openshift/cluster-image-registry-operator/pkg/parameters"
	"github.com/openshift/cluster-image-registry-operator/pkg/resource"
	"github.com/openshift/cluster-image-registry-operator/pkg/storage"

	operatorcontroller "github.com/openshift/cluster-image-registry-operator/pkg/operator/controller"
	clusterrolebindingscontroller "github.com/openshift/cluster-image-registry-operator/pkg/operator/controller/clusterrolebindings"
	clusterrolescontroller "github.com/openshift/cluster-image-registry-operator/pkg/operator/controller/clusterroles"
	configmapscontroller "github.com/openshift/cluster-image-registry-operator/pkg/operator/controller/configmaps"
	deploymentscontroller "github.com/openshift/cluster-image-registry-operator/pkg/operator/controller/deployments"
	imageregistrycontroller "github.com/openshift/cluster-image-registry-operator/pkg/operator/controller/imageregistry"
	routescontroller "github.com/openshift/cluster-image-registry-operator/pkg/operator/controller/routes"
	secretscontroller "github.com/openshift/cluster-image-registry-operator/pkg/operator/controller/secrets"
	servicesaccountscontroller "github.com/openshift/cluster-image-registry-operator/pkg/operator/controller/serviceaccounts"
	servicescontroller "github.com/openshift/cluster-image-registry-operator/pkg/operator/controller/services"
)

const (
	WORKQUEUE_KEY = "changes"
)

type permanentError struct {
	Err error
}

func (e permanentError) Error() string {
	return e.Err.Error()
}

func NewController(kubeconfig *restclient.Config, namespace string) (*Controller, error) {
	operatorNamespace, err := client.GetWatchNamespace()
	if err != nil {
		glog.Fatalf("Failed to get watch namespace: %v", err)
	}

	operatorName, err := client.GetOperatorName()
	if err != nil {
		glog.Fatalf("Failed to get operator name: %v", err)
	}

	p := parameters.Globals{}

	p.Deployment.Namespace = namespace
	p.Deployment.Labels = map[string]string{"docker-registry": "default"}

	p.Pod.ServiceAccount = "registry"
	p.Container.Port = 5000

	p.Healthz.Route = "/healthz"
	p.Healthz.TimeoutSeconds = 5

	p.Service.Name = "image-registry"
	p.ImageConfig.Name = "cluster"

	c := &Controller{
		kubeconfig:    kubeconfig,
		params:        p,
		generator:     resource.NewGenerator(kubeconfig, &p),
		clusterStatus: clusteroperator.NewStatusHandler(kubeconfig, operatorName, operatorNamespace),
		workqueue:     workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Changes"),
	}

	if err = c.Bootstrap(); err != nil {
		return nil, err
	}

	return c, nil
}

type Controller struct {
	kubeconfig    *restclient.Config
	params        parameters.Globals
	generator     *resource.Generator
	clusterStatus *clusteroperator.StatusHandler
	workqueue     workqueue.RateLimitingInterface

	watchers map[string]operatorcontroller.Watcher
}

func (c *Controller) createOrUpdateResources(cr *regopapi.ImageRegistry, modified *bool) error {
	appendFinalizer(cr, modified)

	err := verifyResource(cr, &c.params)
	if err != nil {
		return fmt.Errorf("unable to complete resource: %s", err)
	}

	driver, err := storage.NewDriver(cr.Name, c.params.Deployment.Namespace, &cr.Spec.Storage)
	if err == storage.ErrStorageNotConfigured {
		return permanentError{Err: err}
	} else if err != nil {
		return fmt.Errorf("unable to create storage driver: %s", err)
	}

	err = driver.ValidateConfiguration(cr, modified)
	if err != nil {
		return permanentError{Err: fmt.Errorf("invalid configuration: %s", err)}
	}

	err = c.generator.Apply(cr, modified)
	if err != nil {
		return err
	}

	return nil
}

func (c *Controller) CreateOrUpdateResources(cr *regopapi.ImageRegistry, modified *bool) error {
	if cr.Spec.ManagementState != operatorapi.Managed {
		return nil
	}

	return c.createOrUpdateResources(cr, modified)
}

func (c *Controller) Handle(action string, o interface{}) {
	object, ok := o.(metaapi.Object)

	if !ok {
		tombstone, ok := o.(cache.DeletedFinalStateUnknown)
		if !ok {
			glog.Errorf("error decoding object, invalid type")
			return
		}
		object, ok = tombstone.Obj.(metaapi.Object)
		if !ok {
			glog.Errorf("error decoding object tombstone, invalid type")
			return
		}
		glog.V(4).Infof("Recovered deleted object '%s' from tombstone", object.GetName())
	}

	objectInfo := fmt.Sprintf("Type=%T ", o)
	if namespace := object.GetNamespace(); namespace != "" {
		objectInfo += fmt.Sprintf("Namespace=%s ", namespace)
	}
	objectInfo += fmt.Sprintf("Name=%s", object.GetName())

	glog.V(1).Infof("Processing %s object %s", action, objectInfo)

	if cr, ok := o.(*regopapi.ImageRegistry); ok {
		dgst, err := resource.Checksum(resource.ImageRegistryChecksumInput{cr.Spec, cr.Status})
		if err != nil {
			glog.Errorf("unable to generate checksum for ImageRegistry: %s", err)
			dgst = ""
		}

		curdgst, ok := object.GetAnnotations()[parameters.ChecksumOperatorAnnotation]
		if ok && dgst == curdgst {
			glog.V(1).Infof("ImageRegistry %s has not changed", object.GetName())
			return
		}
	} else {
		ownerRef := metaapi.GetControllerOf(object)

		if ownerRef == nil || ownerRef.Kind != "ImageRegistry" || ownerRef.APIVersion != regopapi.SchemeGroupVersion.String() {
			return
		}
	}

	glog.V(1).Infof("add event to workqueue due to %s (%s)", objectInfo, action)
	c.workqueue.AddRateLimited(WORKQUEUE_KEY)
}

func (c *Controller) sync() error {
	client, err := regopset.NewForConfig(c.kubeconfig)
	if err != nil {
		return err
	}

	cr, err := client.ImageRegistries().Get(resourceName(c.params.Deployment.Namespace), metaapi.GetOptions{})
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to get %q custom resource: %s", cr.Name, err)
		}
		glog.Infof("ImageRegistry Name=%s not found. ignore.", resourceName(c.params.Deployment.Namespace))
		return nil
	}

	if cr == nil {
		return c.Bootstrap()
	}

	if cr.ObjectMeta.DeletionTimestamp != nil {
		return c.finalizeResources(cr)
	}

	var statusChanged bool
	var applyError error
	switch cr.Spec.ManagementState {
	case operatorapi.Removed:
		err = c.RemoveResources(cr)
		if err != nil {
			errOp := c.clusterStatus.Update(osapi.OperatorFailing, osapi.ConditionTrue, "unable to remove registry")
			if errOp != nil {
				glog.Errorf("unable to update cluster status to %s=%s: %s", osapi.OperatorFailing, osapi.ConditionTrue, errOp)
			}
			conditionProgressing(cr, operatorapi.ConditionTrue, fmt.Sprintf("unable to remove objects: %s", err), &statusChanged)
		} else {
			conditionRemoved(cr, operatorapi.ConditionTrue, "", &statusChanged)
			conditionAvailable(cr, operatorapi.ConditionFalse, "", &statusChanged)
			conditionProgressing(cr, operatorapi.ConditionFalse, "", &statusChanged)
			conditionFailing(cr, operatorapi.ConditionFalse, "", &statusChanged)
		}
	case operatorapi.Managed:
		conditionRemoved(cr, operatorapi.ConditionFalse, "", &statusChanged)
		applyError = c.CreateOrUpdateResources(cr, &statusChanged)
	case operatorapi.Unmanaged:
		// ignore
	default:
		glog.Warningf("unknown custom resource state: %s", cr.Spec.ManagementState)
	}

	var deployInterface runtime.Object
	deploy, err := c.watchers["deployments"].Get(cr.ObjectMeta.Name, c.params.Deployment.Namespace)
	deployInterface = deploy
	if errors.IsNotFound(err) {
		deployInterface = nil
	} else if err != nil {
		return fmt.Errorf("failed to get %q deployment: %s", cr.ObjectMeta.Name, err)
	}

	if applyError == nil {
		svc, err := c.watchers["services"].Get(c.params.Service.Name, c.params.Deployment.Namespace)
		if err == nil {
			svcObj := svc.(*coreapi.Service)
			svcHostname := fmt.Sprintf("%s.%s.svc.cluster.local:%d", svcObj.Name, svcObj.Namespace, svcObj.Spec.Ports[0].Port)
			if cr.Status.InternalRegistryHostname != svcHostname {
				cr.Status.InternalRegistryHostname = svcHostname
				statusChanged = true
			}
		} else if !errors.IsNotFound(err) {
			return fmt.Errorf("failed to get %q service %s", c.params.Service.Name, err)
		}
	}

	c.syncStatus(cr, deployInterface, applyError, &statusChanged)

	if statusChanged {
		glog.Infof("%s changed", metautil.TypeAndName(cr))

		cr.Status.ObservedGeneration = cr.Generation
		addImageRegistryChecksum(cr)

		_, err = client.ImageRegistries().Update(cr)
		if err != nil && !errors.IsConflict(err) {
			glog.Errorf("unable to update %s: %s", metautil.TypeAndName(cr), err)
		}
	}

	return nil
}

func (c *Controller) eventProcessor() {
	for {
		obj, shutdown := c.workqueue.Get()

		if shutdown {
			return
		}

		err := func(obj interface{}) error {
			defer c.workqueue.Done(obj)

			if _, ok := obj.(string); !ok {
				c.workqueue.Forget(obj)
				glog.Errorf("expected string in workqueue but got %#v", obj)
				return nil
			}

			if err := c.sync(); err != nil {
				c.workqueue.AddRateLimited(WORKQUEUE_KEY)
				return fmt.Errorf("unable to sync: %s, requeuing", err)
			}

			c.workqueue.Forget(obj)

			glog.Infof("event from workqueue successfully processed")
			return nil
		}(obj)

		if err != nil {
			glog.Errorf("unable to process event: %s", err)
		}
	}
}

func (c *Controller) Run(stopCh <-chan struct{}) error {
	defer c.workqueue.ShutDown()

	err := c.clusterStatus.Create()
	if err != nil {
		glog.Errorf("unable to create cluster operator resource: %s", err)
	}

	c.watchers = map[string]operatorcontroller.Watcher{
		"deployments":         &deploymentscontroller.Controller{},
		"services":            &servicescontroller.Controller{},
		"secrets":             &secretscontroller.Controller{},
		"configmaps":          &configmapscontroller.Controller{},
		"servicesaccounts":    &servicesaccountscontroller.Controller{},
		"routes":              &routescontroller.Controller{},
		"clusterroles":        &clusterrolescontroller.Controller{},
		"clusterrolebindings": &clusterrolebindingscontroller.Controller{},
		"imageregistry":       &imageregistrycontroller.Controller{},
	}

	for _, watcher := range c.watchers {
		err = watcher.Start(c.Handle, c.params.Deployment.Namespace, stopCh)
		if err != nil {
			return err
		}
	}

	glog.Info("all controllers are running")

	go wait.Until(c.eventProcessor, time.Second, stopCh)

	glog.Info("started events processor")
	<-stopCh
	glog.Info("shutting down events processor")

	return nil
}
