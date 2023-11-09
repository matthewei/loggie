/*
Copyright 2021 Loggie Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"github.com/loggie-io/loggie/pkg/discovery/kubernetes/runtime"
	"github.com/loggie-io/loggie/pkg/util/pattern"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"reflect"
	"time"

	"github.com/loggie-io/loggie/pkg/core/log"
	logconfigClientset "github.com/loggie-io/loggie/pkg/discovery/kubernetes/client/clientset/versioned"
	logconfigSchema "github.com/loggie-io/loggie/pkg/discovery/kubernetes/client/clientset/versioned/scheme"
	logconfigInformers "github.com/loggie-io/loggie/pkg/discovery/kubernetes/client/informers/externalversions/loggie/v1beta1"
	logconfigLister "github.com/loggie-io/loggie/pkg/discovery/kubernetes/client/listers/loggie/v1beta1"
	"github.com/loggie-io/loggie/pkg/discovery/kubernetes/helper"
	"github.com/loggie-io/loggie/pkg/discovery/kubernetes/index"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"

	logconfigv1beta1 "github.com/loggie-io/loggie/pkg/discovery/kubernetes/apis/loggie/v1beta1"
	corev1 "k8s.io/api/core/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	corev1Informers "k8s.io/client-go/informers/core/v1"
	corev1Listers "k8s.io/client-go/listers/core/v1"
)

const (
	EventPod            = "pod"
	EventLogConf        = "logConfig"
	EventNode           = "node"
	EventVm             = "vm"
	EventClusterLogConf = "clusterLogConfig"
	EventSink           = "sink"
	EventInterceptor    = "interceptor"

	InjectorAnnotationKey       = "sidecar.loggie.io/inject"
	InjectorAnnotationValueTrue = "true"
)

// Element the item add to queue
type Element struct {
	Type         string `json:"type"` // resource type, eg: pod
	Key          string `json:"key"`  // MetaNamespaceKey, format: <namespace>/<name>
	SelectorType string `json:"selectorType"`
}

type Controller struct {
	config    *Config
	workqueue workqueue.RateLimitingInterface

	kubeClientset      kubernetes.Interface
	logConfigClientset logconfigClientset.Interface

	podsLister             corev1Listers.PodLister
	logConfigLister        logconfigLister.LogConfigLister
	clusterLogConfigLister logconfigLister.ClusterLogConfigLister
	sinkLister             logconfigLister.SinkLister
	interceptorLister      logconfigLister.InterceptorLister
	nodeLister             corev1Listers.NodeLister

	// only in Vm mode
	vmLister logconfigLister.VmLister

	typePodIndex     *index.LogConfigTypePodIndex
	typeClusterIndex *index.LogConfigTypeClusterIndex
	typeNodeIndex    *index.LogConfigTypeNodeIndex

	nodeInfo *corev1.Node
	vmInfo   *logconfigv1beta1.Vm

	record                     record.EventRecorder
	runtime                    runtime.Runtime
	extraTypePodFieldsPattern  map[string]*pattern.Pattern
	extraTypeNodeFieldsPattern map[string]*pattern.Pattern
	extraTypeVmFieldsPattern   map[string]*pattern.Pattern
}

func NewController(
	config *Config,
	kubeClientset kubernetes.Interface,
	logConfigClientset logconfigClientset.Interface,
	podInformer corev1Informers.PodInformer,
	logConfigInformer logconfigInformers.LogConfigInformer,
	clusterLogConfigInformer logconfigInformers.ClusterLogConfigInformer,
	sinkInformer logconfigInformers.SinkInformer,
	interceptorInformer logconfigInformers.InterceptorInformer,
	nodeInformer corev1Informers.NodeInformer,
	vmInformer logconfigInformers.VmInformer,
	runtime runtime.Runtime,
) *Controller {

	log.Info("Creating event broadcaster")
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClientset.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "loggie/" + config.NodeName})

	var controller *Controller
	if config.VmMode {
		controller = &Controller{
			config:    config,
			workqueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "logConfig"),

			kubeClientset:      kubeClientset,
			logConfigClientset: logConfigClientset,

			clusterLogConfigLister: clusterLogConfigInformer.Lister(),
			sinkLister:             sinkInformer.Lister(),
			interceptorLister:      interceptorInformer.Lister(),
			vmLister:               vmInformer.Lister(),

			typeNodeIndex: index.NewLogConfigTypeNodeIndex(),

			record: recorder,
		}
	} else {
		controller = &Controller{
			config:    config,
			workqueue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "logConfig"),

			kubeClientset:      kubeClientset,
			logConfigClientset: logConfigClientset,

			podsLister:             podInformer.Lister(),
			logConfigLister:        logConfigInformer.Lister(),
			clusterLogConfigLister: clusterLogConfigInformer.Lister(),
			sinkLister:             sinkInformer.Lister(),
			interceptorLister:      interceptorInformer.Lister(),
			nodeLister:             nodeInformer.Lister(),

			typePodIndex:     index.NewLogConfigTypePodIndex(),
			typeClusterIndex: index.NewLogConfigTypeLoggieIndex(),
			typeNodeIndex:    index.NewLogConfigTypeNodeIndex(),

			record:  recorder,
			runtime: runtime,
		}
	}

	controller.InitK8sFieldsPattern()

	log.Info("Setting up event handlers")
	utilruntime.Must(logconfigSchema.AddToScheme(scheme.Scheme))

	if config.VmMode {
		vm, err := logConfigClientset.LoggieV1beta1().Vms().Get(context.Background(), config.NodeName, metav1.GetOptions{})
		if err != nil {
			log.Panic("get vm %s failed: %+v", config.NodeName, err)
		}
		controller.vmInfo = vm.DeepCopy()

	} else {
		// Since type node logic depends on node labels, we get and set node info at first.
		node, err := kubeClientset.CoreV1().Nodes().Get(context.Background(), config.NodeName, metav1.GetOptions{})
		if err != nil {
			log.Panic("get node %s failed: %+v", config.NodeName, err)
		}
		controller.nodeInfo = node.DeepCopy()
	}

	clusterLogConfigInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			config := obj.(*logconfigv1beta1.ClusterLogConfig)
			if config.Spec.Selector == nil {
				return
			}
			if !controller.belongOfCluster(config.Spec.Selector.Cluster, config.Annotations) {
				return
			}

			controller.enqueue(obj, EventClusterLogConf, config.Spec.Selector.Type)
		},
		UpdateFunc: func(new, old interface{}) {
			newConfig := new.(*logconfigv1beta1.ClusterLogConfig)
			oldConfig := old.(*logconfigv1beta1.ClusterLogConfig)
			if newConfig.ResourceVersion == oldConfig.ResourceVersion {
				return
			}
			if newConfig.Generation == oldConfig.Generation {
				return
			}
			if newConfig.Spec.Selector == nil {
				return
			}
			if !controller.belongOfCluster(newConfig.Spec.Selector.Cluster, newConfig.Annotations) {
				return
			}

			controller.handleLogConfigSelectorHasChange(newConfig.ToLogConfig(), oldConfig.ToLogConfig())
			controller.enqueue(new, EventClusterLogConf, newConfig.Spec.Selector.Type)
		},
		DeleteFunc: func(obj interface{}) {
			config, ok := obj.(*logconfigv1beta1.ClusterLogConfig)
			if !ok {
				return
			}
			if config.Spec.Selector == nil {
				return
			}
			if !controller.belongOfCluster(config.Spec.Selector.Cluster, config.Annotations) {
				return
			}

			controller.enqueueForDelete(obj, EventClusterLogConf, config.Spec.Selector.Type)
		},
	})

	interceptorInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			controller.enqueue(obj, EventInterceptor, logconfigv1beta1.SelectorTypeAll)
		},
		UpdateFunc: func(old, new interface{}) {
			newConfig := new.(*logconfigv1beta1.Interceptor)
			oldConfig := old.(*logconfigv1beta1.Interceptor)
			if newConfig.ResourceVersion == oldConfig.ResourceVersion {
				return
			}

			controller.enqueue(new, EventInterceptor, logconfigv1beta1.SelectorTypeAll)
		},
	})

	sinkInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			controller.enqueue(obj, EventSink, logconfigv1beta1.SelectorTypeAll)
		},
		UpdateFunc: func(old, new interface{}) {
			newConfig := new.(*logconfigv1beta1.Sink)
			oldConfig := old.(*logconfigv1beta1.Sink)
			if newConfig.ResourceVersion == oldConfig.ResourceVersion {
				return
			}

			controller.enqueue(new, EventSink, logconfigv1beta1.SelectorTypeAll)
		},
	})

	if config.VmMode {
		vmInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj interface{}) {
				controller.enqueue(obj, EventVm, logconfigv1beta1.SelectorTypeAll)
			},
			UpdateFunc: func(old, new interface{}) {
				newConfig := new.(*logconfigv1beta1.Vm)
				oldConfig := old.(*logconfigv1beta1.Vm)
				if newConfig.ResourceVersion == oldConfig.ResourceVersion {
					return
				}

				if reflect.DeepEqual(newConfig.Labels, oldConfig.Labels) {
					return
				}

				controller.enqueue(new, EventVm, logconfigv1beta1.SelectorTypeAll)
			},
		})

		return controller
	}

	logConfigInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			config := obj.(*logconfigv1beta1.LogConfig)
			if config.Spec.Selector == nil {
				return
			}
			if !controller.belongOfCluster(config.Spec.Selector.Cluster, config.Annotations) {
				return
			}

			controller.enqueue(obj, EventLogConf, config.Spec.Selector.Type)
		},
		UpdateFunc: func(old, new interface{}) {
			newConfig := new.(*logconfigv1beta1.LogConfig)
			oldConfig := old.(*logconfigv1beta1.LogConfig)
			if newConfig.ResourceVersion == oldConfig.ResourceVersion {
				return
			}
			if newConfig.Generation == oldConfig.Generation {
				return
			}

			if newConfig.Spec.Selector == nil {
				return
			}
			if !controller.belongOfCluster(newConfig.Spec.Selector.Cluster, newConfig.Annotations) {
				return
			}

			controller.handleLogConfigSelectorHasChange(newConfig, oldConfig)

			controller.enqueue(new, EventLogConf, newConfig.Spec.Selector.Type)
		},
		DeleteFunc: func(obj interface{}) {
			config, ok := obj.(*logconfigv1beta1.LogConfig)
			if !ok {
				return
			}
			if config.Spec.Selector == nil {
				return
			}
			if !controller.belongOfCluster(config.Spec.Selector.Cluster, config.Annotations) {
				return
			}

			controller.enqueueForDelete(obj, EventLogConf, config.Spec.Selector.Type)
		},
	})

	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			po := obj.(*corev1.Pod)
			if !helper.IsPodReady(po) {
				return
			}
			controller.enqueue(obj, EventPod, logconfigv1beta1.SelectorTypePod)
		},
		UpdateFunc: func(old, new interface{}) {
			newPod := new.(*corev1.Pod)
			oldPod := old.(*corev1.Pod)
			if newPod.ResourceVersion == oldPod.ResourceVersion {
				return
			}
			if !helper.IsPodReady(newPod) {
				return
			}
			controller.enqueue(new, EventPod, logconfigv1beta1.SelectorTypePod)
		},
		DeleteFunc: func(obj interface{}) {
			controller.enqueueForDelete(obj, EventPod, logconfigv1beta1.SelectorTypePod)
		},
	})

	nodeInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			controller.enqueue(obj, EventNode, logconfigv1beta1.SelectorTypeNode)
		},
		UpdateFunc: func(old, new interface{}) {
			newConfig := new.(*corev1.Node)
			oldConfig := old.(*corev1.Node)
			if newConfig.ResourceVersion == oldConfig.ResourceVersion {
				return
			}

			controller.enqueue(new, EventNode, logconfigv1beta1.SelectorTypeNode)
		},
	})

	return controller
}

func (c *Controller) InitK8sFieldsPattern() {
	typePodPattern := c.config.TypePodFields.initPattern()

	for k, v := range c.config.K8sFields {
		p, _ := pattern.Init(v)
		typePodPattern[k] = p
	}
	c.extraTypePodFieldsPattern = typePodPattern

	c.extraTypeNodeFieldsPattern = c.config.TypeNodeFields.initPattern()

	c.extraTypeVmFieldsPattern = c.config.TypeVmFields.initPattern()
}

// handleSelectorHasChange
// After the labelSelector and nodeSelector are changed, delete the old pipeline
func (c *Controller) handleLogConfigSelectorHasChange(new *logconfigv1beta1.LogConfig, old *logconfigv1beta1.LogConfig) {
	var err error

	if old.Spec.Selector == nil {
		return
	}

	lgcKey := helper.MetaNamespaceKey(old.Namespace, old.Name)
	switch new.Spec.Selector.Type {
	case logconfigv1beta1.SelectorTypePod, logconfigv1beta1.SelectorTypeWorkload:
		if !helper.MatchStringMap(new.Spec.Selector.LabelSelector,
			old.Spec.Selector.LabelSelector) {
			err = c.handleAllTypesDelete(lgcKey, logconfigv1beta1.SelectorTypePod)
			if err != nil {
				log.Error("delete %s failed: %s", lgcKey, err)
			}
		}

	case logconfigv1beta1.SelectorTypeNode:
		if !helper.MatchStringMap(new.Spec.Selector.NodeSelector.NodeSelector,
			old.Spec.Selector.NodeSelector.NodeSelector) {
			err = c.handleAllTypesDelete(lgcKey, logconfigv1beta1.SelectorTypeNode)
			if err != nil {
				log.Error("delete %s failed: %s", lgcKey, err)
			}
		}
	}
}

func (c *Controller) enqueue(obj interface{}, eleType string, selectorType string) {
	var key string
	var err error
	if key, err = cache.MetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	e := Element{
		Type:         eleType,
		Key:          key,
		SelectorType: selectorType,
	}
	c.workqueue.Add(e)
}

func (c *Controller) enqueueForDelete(obj interface{}, eleType string, selectorType string) {
	var key string
	var err error
	if key, err = cache.DeletionHandlingMetaNamespaceKeyFunc(obj); err != nil {
		utilruntime.HandleError(err)
		return
	}
	e := Element{
		Type:         eleType,
		Key:          key,
		SelectorType: selectorType,
	}
	c.workqueue.Add(e)
}

func (c *Controller) Run(stopCh <-chan struct{}, cacheSyncs ...cache.InformerSynced) error {
	defer utilruntime.HandleCrash()
	defer c.workqueue.ShutDown()

	// Start the informer factories to begin populating the informer caches
	log.Info("Starting controller")

	// Wait for the caches to be synced before starting workers
	log.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, cacheSyncs...); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	log.Info("Starting kubernetes discovery workers")

	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
	log.Info("Shutting down kubernetes discovery workers")

	return nil
}

func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var element Element
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if element, ok = obj.(Element); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			utilruntime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// Foo resource to be synced.
		if err := c.syncHandler(element); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
			c.workqueue.AddRateLimited(element)
			return fmt.Errorf("error syncing '%+v': %w, requeuing", element, err)
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		// log.Debug("Successfully synced '%s'", element)
		return nil
	}(obj)

	if err != nil {
		utilruntime.HandleError(err)
		return true
	}

	return true
}

func (c *Controller) syncHandler(element Element) error {
	log.Debug("syncHandler start process: %+v", element)

	var err error
	switch element.Type {
	case EventPod:
		if err = c.reconcilePod(element.Key); err != nil {
			log.Warn("reconcile pod %s err: %+v", element.Key, err)
		}

	case EventClusterLogConf:
		if err = c.reconcileClusterLogConfig(element); err != nil {
			if log.IsDebugLevel() {
				log.Warn("reconcile clusterLogConfig %s err: %+v", element.Key, err)
			} else {
				log.Warn("reconcile clusterLogConfig %s err: %v", element.Key, err)
			}
		}

	case EventLogConf:
		if err = c.reconcileLogConfig(element); err != nil {
			if log.IsDebugLevel() {
				log.Warn("reconcile logConfig %s err: %+v", element.Key, err)
			} else {
				log.Warn("reconcile logConfig %s err: %v", element.Key, err)
			}
		}

	case EventNode:
		if err = c.reconcileNode(element.Key); err != nil {
			log.Warn("reconcile node %s err: %v", element.Key, err)
		}

	case EventSink:
		if err = c.reconcileSink(element.Key); err != nil {
			log.Warn("reconcile sink %s err: %v", element.Key, err)
		}

	case EventInterceptor:
		if err = c.reconcileInterceptor(element.Key); err != nil {
			log.Warn("reconcile interceptor %s err: %v", element.Key, err)
		}

	case EventVm:
		if err = c.reconcileVm(element.Key); err != nil {
			log.Warn("reconcile interceptor %s err: %v", element.Key, err)
		}

	default:
		utilruntime.HandleError(fmt.Errorf("element type: %s not supported", element.Type))
		return nil
	}

	return nil
}

func (c *Controller) belongOfCluster(cluster string, annotations map[string]string) bool {
	if c.config.Cluster != cluster {
		return false
	}

	// If there's a Sidecar-injected annotation, just ignore it
	if annotations != nil {
		if _, ok := annotations[InjectorAnnotationKey]; ok {
			return false
		}
	}

	return true
}
