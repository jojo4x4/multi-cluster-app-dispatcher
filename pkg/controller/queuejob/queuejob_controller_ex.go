/*
Copyright 2017 The Kubernetes Authors.

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

package queuejob

import (
	"fmt"
	"github.com/golang/glog"
	"github.com/IBM/multi-cluster-app-dispatcher/pkg/controller/metrics/adapter"
	"math/rand"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"strconv"
	"time"

	"k8s.io/api/core/v1"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"

	"github.com/IBM/multi-cluster-app-dispatcher/pkg/controller/queuejobresources"
	resconfigmap "github.com/IBM/multi-cluster-app-dispatcher/pkg/controller/queuejobresources/configmap" // ConfigMap
	resdeployment "github.com/IBM/multi-cluster-app-dispatcher/pkg/controller/queuejobresources/deployment"
	resnamespace "github.com/IBM/multi-cluster-app-dispatcher/pkg/controller/queuejobresources/namespace"                         // NP
	resnetworkpolicy "github.com/IBM/multi-cluster-app-dispatcher/pkg/controller/queuejobresources/networkpolicy"                 // NetworkPolicy
	respersistentvolume "github.com/IBM/multi-cluster-app-dispatcher/pkg/controller/queuejobresources/persistentvolume"           // PV
	respersistentvolumeclaim "github.com/IBM/multi-cluster-app-dispatcher/pkg/controller/queuejobresources/persistentvolumeclaim" // PVC
	respod "github.com/IBM/multi-cluster-app-dispatcher/pkg/controller/queuejobresources/pod"
	ressecret "github.com/IBM/multi-cluster-app-dispatcher/pkg/controller/queuejobresources/secret" // Secret
	resservice "github.com/IBM/multi-cluster-app-dispatcher/pkg/controller/queuejobresources/service"
	resstatefulset "github.com/IBM/multi-cluster-app-dispatcher/pkg/controller/queuejobresources/statefulset"
	"k8s.io/apimachinery/pkg/labels"

	arbv1 "github.com/IBM/multi-cluster-app-dispatcher/pkg/apis/controller/v1alpha1"
	clientset "github.com/IBM/multi-cluster-app-dispatcher/pkg/client/clientset/controller-versioned"
	"github.com/IBM/multi-cluster-app-dispatcher/pkg/client/clientset/controller-versioned/clients"
	arbinformers "github.com/IBM/multi-cluster-app-dispatcher/pkg/client/informers/controller-externalversion"
	informersv1 "github.com/IBM/multi-cluster-app-dispatcher/pkg/client/informers/controller-externalversion/v1"
	listersv1 "github.com/IBM/multi-cluster-app-dispatcher/pkg/client/listers/controller/v1"

	"github.com/IBM/multi-cluster-app-dispatcher/pkg/controller/queuejobdispatch"

	clusterstatecache "github.com/IBM/multi-cluster-app-dispatcher/pkg/controller/clusterstate/cache"
	clusterstateapi "github.com/IBM/multi-cluster-app-dispatcher/pkg/controller/clusterstate/api"
)

const (
	// QueueJobNameLabel label string for queuejob name
	QueueJobNameLabel string = "xqueuejob-name"

	// ControllerUIDLabel label string for queuejob controller uid
	ControllerUIDLabel string = "controller-uid"

	initialGetBackoff = 30 * time.Second

)

// controllerKind contains the schema.GroupVersionKind for this controller type.
var controllerKind = arbv1.SchemeGroupVersion.WithKind("XQueueJob")

//XController the XQueueJob Controller type
type XController struct {
	config           *rest.Config
	queueJobInformer informersv1.XQueueJobInformer
	// resources registered for the XQueueJob
	qjobRegisteredResources queuejobresources.RegisteredResources
	// controllers for these resources
	qjobResControls map[arbv1.ResourceType]queuejobresources.Interface

	clients    *kubernetes.Clientset
	arbclients *clientset.Clientset

	// A store of jobs
	queueJobLister listersv1.XQueueJobLister
	queueJobSynced func() bool

	// QueueJobs that need to be initialized
	// Add labels and selectors to XQueueJob
	initQueue *cache.FIFO

	// QueueJobs that need to sync up after initialization
	updateQueue *cache.FIFO

	// eventQueue that need to sync up
	eventQueue *cache.FIFO

	//QJ queue that needs to be allocated
	qjqueue SchedulingQueue

	// our own local cache, used for computing total amount of resources
	cache clusterstatecache.Cache

	// Reference manager to manage membership of queuejob resource and its members
	refManager queuejobresources.RefManager

	// is dispatcher or deployer?
	isDispatcher bool

	// Agent map: agentID -> XQueueJobAgent
	agentMap map[string]*queuejobdispatch.XQueueJobAgent
	agentList []string

	// Map for XQueueJob -> XQueueJobAgent
	dispatchMap map[string]string

	// Metrics API Server
	metricsAdapter *adapter.MetricsAdpater

	// EventQueueforAgent
	agentEventQueue *cache.FIFO
}

type XQueueJobAndAgent struct{
	queueJobKey string
	queueJobAgentKey string
}

func NewXQueueJobAndAgent(qjKey string, qaKey string) *XQueueJobAndAgent {
	return &XQueueJobAndAgent{
		queueJobKey: qjKey,
		queueJobAgentKey: qaKey,
	}
}

// func getQueueJobAndAgentKey(obj interface{}) (string, error) {
// 	qja, ok := obj.(*XQueueJobAndAgent)
// 	if !ok {
// 		return "", fmt.Errorf("not a XQueueJobAndAgent")
// 	}
// 	return fmt.Sprintf("%s", qja.queueJobKey), nil
// }

//RegisterAllQueueJobResourceTypes - gegisters all resources
func RegisterAllQueueJobResourceTypes(regs *queuejobresources.RegisteredResources) {
	respod.Register(regs)
	resservice.Register(regs)
	resdeployment.Register(regs)
	resstatefulset.Register(regs)
	respersistentvolume.Register(regs)
	respersistentvolumeclaim.Register(regs)
	resnamespace.Register(regs)
	resconfigmap.Register(regs)
	ressecret.Register(regs)
	resnetworkpolicy.Register(regs)
}

func GetQueueJobAgentKey(obj interface{}) (string, error) {
	qa, ok := obj.(*queuejobdispatch.XQueueJobAgent)
	if !ok {
		return "", fmt.Errorf("not a XQueueAgent")
	}
	return fmt.Sprintf("%s;%s", qa.AgentId, qa.DeploymentName), nil
}


func GetQueueJobKey(obj interface{}) (string, error) {
	qj, ok := obj.(*arbv1.XQueueJob)
	if !ok {
		return "", fmt.Errorf("not a XQueueJob")
	}

	return fmt.Sprintf("%s/%s", qj.Namespace, qj.Name), nil
}

//NewXQueueJobController create new XQueueJob Controller
func NewXQueueJobController(config *rest.Config, schedulerName string, isDispatcher bool, agentconfigs string) *XController {
	cc := &XController{
		config:				config,
		clients:			kubernetes.NewForConfigOrDie(config),
		arbclients:  		clientset.NewForConfigOrDie(config),
		eventQueue:  		cache.NewFIFO(GetQueueJobKey),
		agentEventQueue:	cache.NewFIFO(GetQueueJobKey),
		initQueue:	 		cache.NewFIFO(GetQueueJobKey),
		updateQueue:		cache.NewFIFO(GetQueueJobKey),
		qjqueue:			NewSchedulingQueue(),
		cache: 				clusterstatecache.New(config),
	}
	cc.metricsAdapter =  adapter.New(config, cc.cache)

	cc.qjobResControls = map[arbv1.ResourceType]queuejobresources.Interface{}
	RegisterAllQueueJobResourceTypes(&cc.qjobRegisteredResources)

	//initialize pod sub-resource control
	resControlPod, found, err := cc.qjobRegisteredResources.InitQueueJobResource(arbv1.ResourceTypePod, config)
	if err != nil {
		glog.Errorf("fail to create queuejob resource control")
		return nil
	}
	if !found {
		glog.Errorf("queuejob resource type Pod not found")
		return nil
	}
	cc.qjobResControls[arbv1.ResourceTypePod] = resControlPod

	// initialize service sub-resource control
	resControlService, found, err := cc.qjobRegisteredResources.InitQueueJobResource(arbv1.ResourceTypeService, config)
	if err != nil {
		glog.Errorf("fail to create queuejob resource control")
		return nil
	}
	if !found {
		glog.Errorf("queuejob resource type Service not found")
		return nil
	}
	cc.qjobResControls[arbv1.ResourceTypeService] = resControlService

	// initialize PV sub-resource control
	resControlPersistentVolume, found, err := cc.qjobRegisteredResources.InitQueueJobResource(arbv1.ResourceTypePersistentVolume, config)
	if err != nil {
		glog.Errorf("fail to create queuejob resource control")
		return nil
	}
	if !found {
		glog.Errorf("queuejob resource type PersistentVolume not found")
		return nil
	}
	cc.qjobResControls[arbv1.ResourceTypePersistentVolume] = resControlPersistentVolume

	resControlPersistentVolumeClaim, found, err := cc.qjobRegisteredResources.InitQueueJobResource(arbv1.ResourceTypePersistentVolumeClaim, config)
	if err != nil {
		glog.Errorf("fail to create queuejob resource control")
		return nil
	}
	if !found {
		glog.Errorf("queuejob resource type PersistentVolumeClaim not found")
		return nil
	}
	cc.qjobResControls[arbv1.ResourceTypePersistentVolumeClaim] = resControlPersistentVolumeClaim

	resControlNamespace, found, err := cc.qjobRegisteredResources.InitQueueJobResource(arbv1.ResourceTypeNamespace, config)
	if err != nil {
		glog.Errorf("fail to create queuejob resource control")
		return nil
	}
	if !found {
		glog.Errorf("queuejob resource type Namespace not found")
		return nil
	}
	cc.qjobResControls[arbv1.ResourceTypeNamespace] = resControlNamespace

	resControlConfigMap, found, err := cc.qjobRegisteredResources.InitQueueJobResource(arbv1.ResourceTypeConfigMap, config)
	if err != nil {
		glog.Errorf("fail to create queuejob resource control")
		return nil
	}
	if !found {
		glog.Errorf("queuejob resource type ConfigMap not found")
		return nil
	}
	cc.qjobResControls[arbv1.ResourceTypeConfigMap] = resControlConfigMap

	resControlSecret, found, err := cc.qjobRegisteredResources.InitQueueJobResource(arbv1.ResourceTypeSecret, config)
	if err != nil {
		glog.Errorf("fail to create queuejob resource control")
		return nil
	}
	if !found {
		glog.Errorf("queuejob resource type Secret not found")
		return nil
	}
	cc.qjobResControls[arbv1.ResourceTypeSecret] = resControlSecret

	resControlNetworkPolicy, found, err := cc.qjobRegisteredResources.InitQueueJobResource(arbv1.ResourceTypeNetworkPolicy, config)
	if err != nil {
		glog.Errorf("fail to create queuejob resource control")
		return nil
	}
	if !found {
		glog.Errorf("queuejob resource type NetworkPolicy not found")
		return nil
	}
	cc.qjobResControls[arbv1.ResourceTypeNetworkPolicy] = resControlNetworkPolicy



	// initialize deployment sub-resource control
	resControlDeployment, found, err := cc.qjobRegisteredResources.InitQueueJobResource(arbv1.ResourceTypeDeployment, config)
	if err != nil {
		glog.Errorf("fail to create queuejob resource control")
		return nil
	}
	if !found {
		glog.Errorf("queuejob resource type Service not found")
		return nil
	}
	cc.qjobResControls[arbv1.ResourceTypeDeployment] = resControlDeployment

	// initialize SS sub-resource
	resControlSS, found, err := cc.qjobRegisteredResources.InitQueueJobResource(arbv1.ResourceTypeStatefulSet, config)
	if err != nil {
		glog.Errorf("fail to create queuejob resource control")
		return nil
	}
	if !found {
		glog.Errorf("queuejob resource type StatefulSet not found")
		return nil
	}
	cc.qjobResControls[arbv1.ResourceTypeStatefulSet] = resControlSS

	queueJobClient, _, err := clients.NewClient(cc.config)
	if err != nil {
		panic(err)
	}
	cc.queueJobInformer = arbinformers.NewSharedInformerFactory(queueJobClient, 0).XQueueJob().XQueueJobs()
	cc.queueJobInformer.Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				switch t := obj.(type) {
				case *arbv1.XQueueJob:
					glog.V(4).Infof("Filter XQueueJob name(%s) namespace(%s) State:%+v\n", t.Name, t.Namespace,t.Status)
					return true
				default:
					return false
				}
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc:    cc.addQueueJob,
				UpdateFunc: cc.updateQueueJob,
				DeleteFunc: cc.deleteQueueJob,
			},
		})
	cc.queueJobLister = cc.queueJobInformer.Lister()

	cc.queueJobSynced = cc.queueJobInformer.Informer().HasSynced

	//create sub-resource reference manager
	cc.refManager = queuejobresources.NewLabelRefManager()

	// Set dispatcher mode or agent mode
	cc.isDispatcher=isDispatcher
	if isDispatcher {
		glog.Infof("[Controller] Dispatcher mode")
 	}	else {
		glog.Infof("[Controller] Agent mode")
	}

	//create agents and agentMap
	cc.agentMap=map[string]*queuejobdispatch.XQueueJobAgent{}
	cc.agentList=[]string{}
	for _, agentconfig := range strings.Split(agentconfigs,",") {
		agentData := strings.Split(agentconfig,":")
		cc.agentMap["/root/kubernetes/" + agentData[0]]=queuejobdispatch.NewXQueueJobAgent(agentconfig, cc.agentEventQueue)
		cc.agentList=append(cc.agentList, "/root/kubernetes/" + agentData[0])
	}

	if isDispatcher && len(cc.agentMap)==0 {
		glog.Errorf("Dispatcher mode: no agent information")
		return nil
	}

	//create (empty) dispatchMap
	cc.dispatchMap=map[string]string{}

	return cc
}

func (qjm *XController) PreemptQueueJobs() {
	qjobs := qjm.GetQueueJobsEligibleForPreemption()
	for _, q := range qjobs {
		newjob, e := qjm.queueJobLister.XQueueJobs(q.Namespace).Get(q.Name)
		if e != nil {
			continue
		}
		newjob.Status.CanRun = false
		if _, err := qjm.arbclients.ArbV1().XQueueJobs(q.Namespace).Update(newjob); err != nil {
			glog.Errorf("Failed to update status of XQueueJob %v/%v: %v",
				q.Namespace, q.Name, err)
		}
	}
}

func (qjm *XController) GetQueueJobsEligibleForPreemption() []*arbv1.XQueueJob {
	qjobs := make([]*arbv1.XQueueJob, 0)

	queueJobs, err := qjm.queueJobLister.XQueueJobs("").List(labels.Everything())
	if err != nil {
		glog.Errorf("List of queueJobs %+v", qjobs)
		return qjobs
	}

	if !qjm.isDispatcher {		// Agent Mode
		for _, value := range queueJobs {
			replicas := value.Spec.SchedSpec.MinAvailable

			if int(value.Status.Succeeded) == replicas {
				if (replicas>0) {
					qjm.arbclients.ArbV1().XQueueJobs(value.Namespace).Delete(value.Name, &metav1.DeleteOptions{})
					continue
				}
			}
			if value.Status.State == arbv1.QueueJobStateEnqueued {
				continue
			}

			if int(value.Status.Running) < replicas {
				if (replicas>0) {
					glog.V(4).Infof("XQJ %s is eligible for preemption %v - %v , %v !!! \n", value.Name, value.Status.Running, replicas, value.Status.Succeeded)
					qjobs = append(qjobs, value)
				}
			}
		}
	}
	return qjobs
}

func GetPodTemplate(qjobRes *arbv1.XQueueJobResource) (*v1.PodTemplateSpec, error) {
	rtScheme := runtime.NewScheme()
	v1.AddToScheme(rtScheme)

	jsonSerializer := json.NewYAMLSerializer(json.DefaultMetaFactory, rtScheme, rtScheme)

	podGVK := schema.GroupVersion{Group: v1.GroupName, Version: "v1"}.WithKind("PodTemplate")

	obj, _, err := jsonSerializer.Decode(qjobRes.Template.Raw, &podGVK, nil)
	if err != nil {
		return nil, err
	}

	template, ok := obj.(*v1.PodTemplate)
	if !ok {
		return nil, fmt.Errorf("Queuejob resource template not define a Pod")
	}

	return &template.Template, nil

}

func (qjm *XController) GetAggregatedResources(cqj *arbv1.XQueueJob) *clusterstateapi.Resource {
	allocated := clusterstateapi.EmptyResource()
        for _, resctrl := range qjm.qjobResControls {
                qjv     := resctrl.GetAggregatedResources(cqj)
                allocated = allocated.Add(qjv)
        }

        return allocated
}


func (qjm *XController) getAggregatedAvailableResourcesPriority(targetpr int, cqj string) *clusterstateapi.Resource {
	r := qjm.cache.GetUnallocatedResources()
	preemptable := clusterstateapi.EmptyResource()

	glog.V(4).Infof("Idle cluster resources %+v", r)

	queueJobs, err := qjm.queueJobLister.XQueueJobs("").List(labels.Everything())
	if err != nil {
		glog.Errorf("Unable to obtain the list of queueJobs %+v", err)
		return r
	}

	for _, value := range queueJobs {
		if value.Name == cqj {
			continue
		}
		if !value.Status.CanRun {
			continue
		}
		if value.Spec.Priority < targetpr {
			for _, resctrl := range qjm.qjobResControls {
				qjv := resctrl.GetAggregatedResources(value)
				preemptable = preemptable.Add(qjv)
			}
		}
	}

	glog.V(6).Infof("Schedulable idle cluster resources: %+v and preemptable cluster resources: %+v", r, preemptable)

	r = r.Add(preemptable)
	glog.V(4).Infof("%+v available resources to schedule", r)
	return r
}

func (qjm *XController) chooseAgent(qj *arbv1.XQueueJob) string{

	qjAggrResources := qjm.GetAggregatedResources(qj)

	glog.V(2).Infof("[Controller: Dispatcher Mode] Aggr Resources of XQJ %s: %v\n", qj.Name, qjAggrResources)

	agentId := qjm.agentList[rand.Int() % len(qjm.agentList)]
	glog.V(2).Infof("[Controller: Dispatcher Mode] Agent %s is chosen randomly\n", agentId)
	resources := qjm.agentMap[agentId].AggrResources
	glog.V(2).Infof("[Controller: Dispatcher Mode] Aggr Resources of Agent %s: %v\n", agentId, resources)
	if qjAggrResources.LessEqual(resources) {
		glog.V(2).Infof("[Controller: Dispatcher Mode] Agent %s has enough resources\n", agentId)
	return agentId
	}
	glog.V(2).Infof("[Controller: Dispatcher Mode] Agent %s does not have enough resources\n", agentId)

	return ""
}


// Thread to find queue-job(QJ) for next schedule
func (qjm *XController) ScheduleNext() {
	// get next QJ from the queue
	// check if we have enough compute resources for it
	// if we have enough compute resources then we set the AllocatedReplicas to the total
	// amount of resources asked by the job
	qj, err := qjm.qjqueue.Pop()
	if err != nil {
		glog.V(4).Infof("Cannot pop QueueJob from the queue!")
	}
	glog.V(10).Infof("[TTime] %s, %s: ScheduleNextStart", qj.Name, time.Now().Sub(qj.CreationTimestamp.Time))
	// glog.Infof("I have queuejob %+v", qj)
	if(qjm.isDispatcher) {
		glog.V(2).Infof("[Controller: Dispatcher Mode] Dispatch Next QueueJob: %s\n", qj.Name)
	}else{
		glog.V(2).Infof("[Controller: Agent Mode] Deploy Next QueueJob: %s\n", qj.Name)
	}

	if qj.Status.CanRun {
		return
	}

	if qjm.isDispatcher {			// Dispatcher Mode
		agentId:=qjm.chooseAgent(qj)
		if agentId != "" {			// A proper agent is found.
														// Update states (CanRun=True) of XQJ in API Server
														// Add XQJ -> Agent Map
			apiQueueJob, err := qjm.queueJobLister.XQueueJobs(qj.Namespace).Get(qj.Name)
			if err != nil {
				return
			}
			apiQueueJob.Status.CanRun = true
			// qj.Status.CanRun = true
			glog.V(10).Infof("[TTime] %s, %s: ScheduleNextBeforeEtcd", qj.Name, time.Now().Sub(qj.CreationTimestamp.Time))
			if _, err := qjm.arbclients.ArbV1().XQueueJobs(qj.Namespace).Update(apiQueueJob); err != nil {
				glog.Errorf("Failed to update status of XQueueJob %v/%v: %v",
																	qj.Namespace, qj.Name, err)
			}
			queueJobKey,_:=GetQueueJobKey(qj)
			qjm.dispatchMap[queueJobKey]=agentId
			glog.V(10).Infof("[TTime] %s, %s: ScheduleNextAfterEtcd", qj.Name, time.Now().Sub(qj.CreationTimestamp.Time))
			return
		} else {
			glog.V(2).Infof("[Controller: Dispatcher Mode] Cannot find an Agent with enough Resources\n")
			go qjm.backoff(qj)
		}
	} else {						// Agent Mode
		aggqj := qjm.GetAggregatedResources(qj)

		resources := qjm.getAggregatedAvailableResourcesPriority(qj.Spec.Priority, qj.Name)
		glog.V(2).Infof("[ScheduleNext] XQJ %s with resources %v to be scheduled on aggregated idle resources %v", qj.Name, aggqj, resources)

		if aggqj.LessEqual(resources) {
			// qj is ready to go!
			apiQueueJob, e := qjm.queueJobLister.XQueueJobs(qj.Namespace).Get(qj.Name)
			if e != nil {
				return
			}
			desired := int32(0)
			for i, ar := range apiQueueJob.Spec.AggrResources.Items {
				desired += ar.Replicas
				apiQueueJob.Spec.AggrResources.Items[i].AllocatedReplicas = ar.Replicas
			}
			glog.V(10).Infof("[TTime] %s, %s: ScheduleNextBeforeEtcd", qj.Name, time.Now().Sub(qj.CreationTimestamp.Time))
			apiQueueJob.Status.CanRun = true
			qj.Status.CanRun = true
			if _, err := qjm.arbclients.ArbV1().XQueueJobs(qj.Namespace).Update(apiQueueJob); err != nil {
													glog.Errorf("Failed to update status of XQueueJob %v/%v: %v",
																	qj.Namespace, qj.Name, err)
			}
			glog.V(10).Infof("[TTime] %s, %s: ScheduleNextAfterEtcd", qj.Name, time.Now().Sub(qj.CreationTimestamp.Time))
		} else {
			// start thread to backoff
			go qjm.backoff(qj)
		}
	}
}


func (qjm *XController) backoff(q *arbv1.XQueueJob) {
	qjm.qjqueue.AddUnschedulableIfNotPresent(q)
	time.Sleep(initialGetBackoff)
	qjm.qjqueue.MoveToActiveQueueIfExists(q)
}

// Run start XQueueJob Controller
func (cc *XController) Run(stopCh chan struct{}) {
	// initialized
	createXQueueJobKind(cc.config)

	go cc.queueJobInformer.Informer().Run(stopCh)

	go cc.qjobResControls[arbv1.ResourceTypePod].Run(stopCh)
	go cc.qjobResControls[arbv1.ResourceTypeService].Run(stopCh)
	go cc.qjobResControls[arbv1.ResourceTypeDeployment].Run(stopCh)
	go cc.qjobResControls[arbv1.ResourceTypeStatefulSet].Run(stopCh)
	go cc.qjobResControls[arbv1.ResourceTypePersistentVolume].Run(stopCh)
	go cc.qjobResControls[arbv1.ResourceTypePersistentVolumeClaim].Run(stopCh)
	go cc.qjobResControls[arbv1.ResourceTypeNamespace].Run(stopCh)
	go cc.qjobResControls[arbv1.ResourceTypeConfigMap].Run(stopCh)
	go cc.qjobResControls[arbv1.ResourceTypeSecret].Run(stopCh)
	go cc.qjobResControls[arbv1.ResourceTypeNetworkPolicy].Run(stopCh)

	cache.WaitForCacheSync(stopCh, cc.queueJobSynced)

	cc.cache.Run(stopCh)

	// go wait.Until(cc.ScheduleNext, 2*time.Second, stopCh)
	go wait.Until(cc.ScheduleNext, 0, stopCh)
	// start preempt thread based on preemption of pods

	// TODO - scheduleNext...Job....
	// start preempt thread based on preemption of pods
	go wait.Until(cc.PreemptQueueJobs, 60*time.Second, stopCh)

	go wait.Until(cc.UpdateQueueJobs, 2*time.Second, stopCh)

	if cc.isDispatcher {
		go wait.Until(cc.UpdateAgent, 2*time.Second, stopCh)			// In the Agent?
		for _, xqueueAgent:= range cc.agentMap {
			go xqueueAgent.Run(stopCh)
		}
		go wait.Until(cc.agentEventQueueWorker, time.Second, stopCh)		// Update Agent Worker
	}

	// go wait.Until(cc.worker, time.Second, stopCh)
	go wait.Until(cc.worker, 0, stopCh)
}

func (qjm *XController) UpdateAgent() {
	glog.V(4).Infof("[Controller] Update AggrResources for All Agents\n")
	for _, xqueueAgent:= range qjm.agentMap {
		xqueueAgent.UpdateAggrResources()
	}
}


func (qjm *XController) UpdateQueueJobs() {
	queueJobs, err := qjm.queueJobLister.XQueueJobs("").List(labels.Everything())
	if err != nil {
		glog.Errorf("I return list of queueJobs %+v", err)
		return
	}
	for _, newjob := range queueJobs {
		if !qjm.qjqueue.IfExist(newjob) {
				qjm.enqueue(newjob)
			}
  }
}

func (cc *XController) addQueueJob(obj interface{}) {
	qj, ok := obj.(*arbv1.XQueueJob)
	if !ok {
		glog.Errorf("obj is not XQueueJob")
		return
	}
	glog.V(10).Infof("[TTime] %s, %s: AddedQueueJob", qj.Name, time.Now().Sub(qj.CreationTimestamp.Time))
	glog.V(4).Infof("QueueJob added - info -  %+v")
	cc.enqueue(qj)
}

func (cc *XController) updateQueueJob(oldObj, newObj interface{}) {
	newQJ, ok := newObj.(*arbv1.XQueueJob)
	if !ok {
		glog.Errorf("newObj is not XQueueJob")
		return
	}

	cc.enqueue(newQJ)
}

func (cc *XController) deleteQueueJob(obj interface{}) {
	qj, ok := obj.(*arbv1.XQueueJob)
	if !ok {
		glog.Errorf("obj is not XQueueJob")
		return
	}
	cc.enqueue(qj)
}

func (cc *XController) enqueue(obj interface{}) {
	_, ok := obj.(*arbv1.XQueueJob)
	if !ok {
		glog.Errorf("obj is not XQueueJob")
		return
	}


	err := cc.eventQueue.Add(obj)
	if err != nil {
		glog.Errorf("Fail to enqueue XQueueJob to updateQueue, err %#v", err)
	}
}




func (cc *XController) agentEventQueueWorker() {
	if _, err := cc.agentEventQueue.Pop(func(obj interface{}) error {
		var queuejob *arbv1.XQueueJob
		switch v := obj.(type) {
		case *arbv1.XQueueJob:
			queuejob = v
		default:
			glog.Errorf("Un-supported type of %v", obj)
			return nil
		}

		if queuejob == nil {
			if acc, err := meta.Accessor(obj); err != nil {
				glog.Warningf("Failed to get XQueueJob for %v/%v", acc.GetNamespace(), acc.GetName())
			}

			return nil
		}
		glog.V(4).Infof("[Controller: Dispatcher Mode] XQJ Status Update from AGENT: Name:%s, Status: %+v\n", queuejob.Name, queuejob.Status)


		// sync XQueueJob
		if err := cc.updateQueueJobStatus(queuejob); err != nil {
			glog.Errorf("Failed to sync XQueueJob %s, err %#v", queuejob.Name, err)
			// If any error, requeue it.
			return err
		}

		return nil
	}); err != nil {
		glog.Errorf("Fail to pop item from updateQueue, err %#v", err)
		return
	}
}

func (cc *XController) updateQueueJobStatus(queueJobFromAgent *arbv1.XQueueJob) error {
	queueJobInEtcd, err := cc.queueJobLister.XQueueJobs(queueJobFromAgent.Namespace).Get(queueJobFromAgent.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if (cc.isDispatcher) {
				cc.Cleanup(queueJobFromAgent)
				cc.qjqueue.Delete(queueJobFromAgent)
			}
			return nil
		}
		return err
	}
	if(len(queueJobFromAgent.Status.State)==0 || queueJobInEtcd.Status.State == queueJobFromAgent.Status.State) {
		return nil
	}
	new_flag := queueJobFromAgent.Status.State
	queueJobInEtcd.Status.State = new_flag
	_, err = cc.arbclients.ArbV1().XQueueJobs(queueJobInEtcd.Namespace).Update(queueJobInEtcd)
	if err != nil {
		return err
	}
	return nil
}




func (cc *XController) worker() {
	if _, err := cc.eventQueue.Pop(func(obj interface{}) error {
		var queuejob *arbv1.XQueueJob
		switch v := obj.(type) {
		case *arbv1.XQueueJob:
			queuejob = v
		default:
			glog.Errorf("Un-supported type of %v", obj)
			return nil
		}
		glog.V(10).Infof("[TTime] %s, %s: WorkerFromEventQueue", queuejob.Name, time.Now().Sub(queuejob.CreationTimestamp.Time))

		if queuejob == nil {
			if acc, err := meta.Accessor(obj); err != nil {
				glog.Warningf("Failed to get XQueueJob for %v/%v", acc.GetNamespace(), acc.GetName())
			}

			return nil
		}
		// sync XQueueJob
		if err := cc.syncQueueJob(queuejob); err != nil {
			glog.Errorf("Failed to sync XQueueJob %s, err %#v", queuejob.Name, err)
			// If any error, requeue it.
			return err
		}

		return nil
	}); err != nil {
		glog.Errorf("Fail to pop item from updateQueue, err %#v", err)
		return
	}
}

func (cc *XController) syncQueueJob(qj *arbv1.XQueueJob) error {
	queueJob, err := cc.queueJobLister.XQueueJobs(qj.Namespace).Get(qj.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			if (cc.isDispatcher) {
				cc.Cleanup(qj)
				cc.qjqueue.Delete(qj)
			}
			return nil
		}
		return err
	}

	// If it is Agent (not a dispatcher), update pod information
	if(!cc.isDispatcher){
		// we call sync for each controller
	  // update pods running, pending,...
	  cc.qjobResControls[arbv1.ResourceTypePod].UpdateQueueJobStatus(qj)
	}

	return cc.manageQueueJob(queueJob)
}

// manageQueueJob is the core method responsible for managing the number of running
// pods according to what is specified in the job.Spec.
// Does NOT modify <activePods>.
func (cc *XController) manageQueueJob(qj *arbv1.XQueueJob) error {
	var err error
	startTime := time.Now()
	defer func() {
		glog.V(4).Infof("[Controller] Finished syncing queue job %q (%v)", qj.Name, time.Now().Sub(startTime))
		glog.V(10).Infof("[TTime] %s, %s: WorkerEnds", qj.Name, time.Now().Sub(qj.CreationTimestamp.Time))
	}()

	if(!cc.isDispatcher) {					// Agent Mode

		if qj.DeletionTimestamp != nil {

			// cleanup resources for running job
			err = cc.Cleanup(qj)
			if err != nil {
				return err
			}
			//empty finalizers and delete the queuejob again
			accessor, err := meta.Accessor(qj)
			if err != nil {
				return err
			}
			accessor.SetFinalizers(nil)

			// we delete the job from the queue if it is there
			cc.qjqueue.Delete(qj)

			return nil
			//var result arbv1.XQueueJob
			//return cc.arbclients.Put().
			//	Namespace(qj.Namespace).Resource(arbv1.QueueJobPlural).
			//	Name(qj.Name).Body(qj).Do().Into(&result)
		}

		// glog.Infof("I have job with name %s status %+v ", qj.Name, qj.Status)

		if !qj.Status.CanRun && (qj.Status.State != arbv1.QueueJobStateEnqueued && qj.Status.State != arbv1.QueueJobStateDeleted) {
			// if there are running resources for this job then delete them because the job was put in
			// pending state...
			glog.V(2).Infof("[Agent Mode] Deleting resources for XQJ %s because it will be preempted (newjob)\n", qj.Name)
			err = cc.Cleanup(qj)
			if err != nil {
				return err
			}

			qj.Status.State = arbv1.QueueJobStateEnqueued
			glog.V(10).Infof("[TTime] %s, %s: WorkerBeforeEtcd",  qj.Name, time.Now().Sub(qj.CreationTimestamp.Time) )
			_, err = cc.arbclients.ArbV1().XQueueJobs(qj.Namespace).Update(qj)
			if err != nil {
				return err
			}
			return nil
		}


		if !qj.Status.CanRun && qj.Status.State == arbv1.QueueJobStateEnqueued {
			cc.qjqueue.AddIfNotPresent(qj)
			return nil
		}

		is_first_deployment:=false
		if qj.Status.CanRun && qj.Status.State != arbv1.QueueJobStateActive {
			is_first_deployment=true
			qj.Status.State =  arbv1.QueueJobStateActive
		}

		if qj.Spec.AggrResources.Items != nil {
			for i := range qj.Spec.AggrResources.Items {
				err := cc.refManager.AddTag(&qj.Spec.AggrResources.Items[i], func() string {
					return strconv.Itoa(i)
				})
				if err != nil {
					return err
				}
			}
		}
		if glog.V(10) {
			if(is_first_deployment) {
					current_time:=time.Now()
					glog.V(10).Infof("[TTime] %s, %s: WorkerBeforeCreatingResouces", qj.Name, time.Now().Sub(qj.CreationTimestamp.Time))
					glog.V(10).Infof("[Agent Controller] XQJ %s has Overhead Before Creating Resouces: %s", qj.Name,current_time.Sub(qj.CreationTimestamp.Time))
			}
		}
		for _, ar := range qj.Spec.AggrResources.Items {
			err00 := cc.qjobResControls[ar.Type].SyncQueueJob(qj, &ar)
			if err00 != nil {
				glog.V(4).Infof("I have error from sync job: %v", err00)
			}
		}
		if glog.V(10) {
			if(is_first_deployment) {
				current_time:=time.Now()
				glog.V(10).Infof("[TTime] %s, %s: WorkerAfterCreatingResouces", qj.Name, time.Now().Sub(qj.CreationTimestamp.Time))
				glog.V(10).Infof("[Agent Controller] XQJ %s has Overhead After Creating Resouces: %s", qj.Name,current_time.Sub(qj.CreationTimestamp.Time))
			}
		}

		// TODO(k82cn): replaced it with `UpdateStatus`
		if _, err := cc.arbclients.ArbV1().XQueueJobs(qj.Namespace).Update(qj); err != nil {
			glog.Errorf("Failed to update status of XQueueJob %v/%v: %v",
				qj.Namespace, qj.Name, err)
			return err
		}

	}	else { 				// Dispatcher Mode

		if qj.DeletionTimestamp != nil {
			// cleanup resources for running job
			err = cc.Cleanup(qj)
			if err != nil {
				return err
			}
			//empty finalizers and delete the queuejob again
			accessor, err := meta.Accessor(qj)
			if err != nil {
				return err
			}
			accessor.SetFinalizers(nil)

			cc.qjqueue.Delete(qj)

			return nil
		}

		if !qj.Status.CanRun && (qj.Status.State != arbv1.QueueJobStateEnqueued && qj.Status.State != arbv1.QueueJobStateDeleted) {
			// if there are running resources for this job then delete them because the job was put in
			// pending state...
			glog.V(4).Infof("Deleting queuejob resources because it will be preempted! %s", qj.Name)
			err = cc.Cleanup(qj)
			if err != nil {
				return err
			}

			qj.Status.State = arbv1.QueueJobStateEnqueued
			_, err = cc.arbclients.ArbV1().XQueueJobs(qj.Namespace).Update(qj)
			if err != nil {
				return err
			}
			return nil
		}

		// if !qj.Status.CanRun && qj.Status.State == arbv1.QueueJobStateEnqueued {
		if !qj.Status.CanRun && qj.Status.State == arbv1.QueueJobStateEnqueued {
			cc.qjqueue.AddIfNotPresent(qj)
			return nil
		}

		if qj.Status.CanRun && !qj.Status.IsDispatched{

			if glog.V(10) {
			current_time:=time.Now()
			glog.V(10).Infof("[Dispatcher Controller] XQJ %s has Overhead Before Dispatching: %s", qj.Name,current_time.Sub(qj.CreationTimestamp.Time))
			glog.V(10).Infof("[TTime] %s, %s: WorkerBeforeDispatch", qj.Name, time.Now().Sub(qj.CreationTimestamp.Time))
		  }

			qj.Status.IsDispatched = true
			queuejobKey, _:=GetQueueJobKey(qj)
			// obj:=cc.dispatchMap[queuejobKey]
			// if obj!=nil {
			if obj, ok:=cc.dispatchMap[queuejobKey]; ok {
				cc.agentMap[obj].CreateXQueueJob(qj)
			}
			if glog.V(10) {
			current_time:=time.Now()
			glog.V(10).Infof("[Dispatcher Controller] XQJ %s has Overhead After Dispatching: %s", qj.Name,current_time.Sub(qj.CreationTimestamp.Time))
			glog.V(10).Infof("[TTime] %s, %s: WorkerAfterDispatch", qj.Name, time.Now().Sub(qj.CreationTimestamp.Time))
			}
			if _, err := cc.arbclients.ArbV1().XQueueJobs(qj.Namespace).Update(qj); err != nil {
				glog.Errorf("Failed to update status of XQueueJob %v/%v: %v",
					qj.Namespace, qj.Name, err)
				return err
			}
		}

	}
	return err
}

//Cleanup function
func (cc *XController) Cleanup(queuejob *arbv1.XQueueJob) error {
	glog.V(4).Infof("Calling cleanup for XQueueJob %s \n", queuejob.Name)

	if !cc.isDispatcher {
		if queuejob.Spec.AggrResources.Items != nil {
			// we call clean-up for each controller
			for _, ar := range queuejob.Spec.AggrResources.Items {
				cc.qjobResControls[ar.Type].Cleanup(queuejob, &ar)
			}
		}
	} else {
		// glog.Infof("[Dispatcher] Cleanup: State=%s\n", queuejob.Status.State)
		if queuejob.Status.CanRun && queuejob.Status.IsDispatched {
			queuejobKey, _:=GetQueueJobKey(queuejob)
			if obj, ok:=cc.dispatchMap[queuejobKey]; ok {
				cc.agentMap[obj].DeleteXQueueJob(queuejob)
			}
		}
	}

	old_flag := queuejob.Status.CanRun
	queuejob.Status = arbv1.XQueueJobStatus{
                Pending:      0,
                Running:      0,
                Succeeded:    0,
                Failed:       0,
                MinAvailable: int32(queuejob.Spec.SchedSpec.MinAvailable),
	}
	queuejob.Status.CanRun = old_flag

	return nil
}
