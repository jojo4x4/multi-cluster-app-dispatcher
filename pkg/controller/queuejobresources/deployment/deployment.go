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

package deployment

import (
	"fmt"
	"github.com/golang/glog"
	arbv1 "github.com/IBM/multi-cluster-app-dispatcher/pkg/apis/controller/v1alpha1"
	clientset "github.com/IBM/multi-cluster-app-dispatcher/pkg/client/clientset/controller-versioned"
	"github.com/IBM/multi-cluster-app-dispatcher/pkg/controller/queuejobresources"
	//schedulerapi "github.com/IBM/multi-cluster-app-dispatcher/pkg/scheduler/api"
	clusterstateapi "github.com/IBM/multi-cluster-app-dispatcher/pkg/controller/clusterstate/api"
	"k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	apps "k8s.io/api/apps/v1beta1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/informers"
	extinformer "k8s.io/client-go/informers/apps/v1beta1"
	"k8s.io/client-go/kubernetes"
	extlister "k8s.io/client-go/listers/apps/v1beta1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"sync"
	"time"
)

var queueJobKind = arbv1.SchemeGroupVersion.WithKind("XQueueJob")
var queueJobName = "xqueuejob.arbitrator.k8s.io"

const (
	// QueueJobNameLabel label string for queuejob name
	QueueJobNameLabel string = "xqueuejob-name"

	// ControllerUIDLabel label string for queuejob controller uid
	ControllerUIDLabel string = "controller-uid"
)

//QueueJobResDeployment contains the resources of this queuejob
type QueueJobResDeployment struct {
	clients    *kubernetes.Clientset
	arbclients *clientset.Clientset
	// A store of deployments, populated by the deploymentController
	deploymentStore   extlister.DeploymentLister
	deployInformer extinformer.DeploymentInformer
	rtScheme       *runtime.Scheme
	jsonSerializer *json.Serializer
	// Reference manager to manage membership of queuejob resource and its members
	refManager queuejobresources.RefManager
}

//Register registers a queue job resource type
func Register(regs *queuejobresources.RegisteredResources) {
	regs.Register(arbv1.ResourceTypeDeployment, func(config *rest.Config) queuejobresources.Interface {
		return NewQueueJobResDeployment(config)
	})
}

//NewQueueJobResDeployment returns a new deployment controller
func NewQueueJobResDeployment(config *rest.Config) queuejobresources.Interface {
	qjrDeployment := &QueueJobResDeployment{
		clients:    kubernetes.NewForConfigOrDie(config),
		arbclients: clientset.NewForConfigOrDie(config),
	}

	qjrDeployment.deployInformer = informers.NewSharedInformerFactory(qjrDeployment.clients, 0).Apps().V1beta1().Deployments()
	qjrDeployment.deployInformer.Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				switch obj.(type) {
				case *apps.Deployment:
					return true
				default:
					return false
				}
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc:    qjrDeployment.addDeployment,
				UpdateFunc: qjrDeployment.updateDeployment,
				DeleteFunc: qjrDeployment.deleteDeployment,
			},
		})

	qjrDeployment.rtScheme = runtime.NewScheme()
	v1.AddToScheme(qjrDeployment.rtScheme)
	v1beta1.AddToScheme(qjrDeployment.rtScheme)
	apps.AddToScheme(qjrDeployment.rtScheme)
	qjrDeployment.jsonSerializer = json.NewYAMLSerializer(json.DefaultMetaFactory, qjrDeployment.rtScheme, qjrDeployment.rtScheme)

	qjrDeployment.refManager = queuejobresources.NewLabelRefManager()

	return qjrDeployment
}


func (qjrDeployment *QueueJobResDeployment) GetPodTemplate(qjobRes *arbv1.XQueueJobResource) (*v1.PodTemplateSpec, int32, error) {
	res, err := qjrDeployment.getDeploymentTemplate(qjobRes)
	if err != nil {
	        return nil, -1, err
	}
	return &res.Spec.Template, *res.Spec.Replicas, nil
}

func (qjrDeployment *QueueJobResDeployment) GetAggregatedResources(job *arbv1.XQueueJob) *clusterstateapi.Resource {
	total := clusterstateapi.EmptyResource()
	if job.Spec.AggrResources.Items != nil {
	    //calculate scaling
	    for _, ar := range job.Spec.AggrResources.Items {
	        if ar.Type == arbv1.ResourceTypeDeployment {
	                template, replicas, _ := qjrDeployment.GetPodTemplate(&ar)
	                myres := queuejobresources.GetPodResources(template)
	                myres.MilliCPU = float64(replicas) * myres.MilliCPU
	                myres.Memory = float64(replicas) * myres.Memory
	                myres.GPU = int64(replicas) * myres.GPU
	                total = total.Add(myres)
	        }
	    }
	}
	return total
}

func (qjrDeployment *QueueJobResDeployment) GetAggregatedResourcesByPriority(priority int, job *arbv1.XQueueJob) *clusterstateapi.Resource {
        total := clusterstateapi.EmptyResource()
        if job.Spec.AggrResources.Items != nil {
            //calculate scaling
            for _, ar := range job.Spec.AggrResources.Items {
                  if ar.Priority < float64(priority) {
                        continue
                  }
                  if ar.Type == arbv1.ResourceTypeDeployment {
                        template, replicas, _ := qjrDeployment.GetPodTemplate(&ar)
                        myres := queuejobresources.GetPodResources(template)
                        myres.MilliCPU = float64(replicas) * myres.MilliCPU
                        myres.Memory = float64(replicas) * myres.Memory
                        myres.GPU = int64(replicas) * myres.GPU
                        total = total.Add(myres)
                }
            }
        }
        return total
}


//Run the main goroutine responsible for watching and deployments.
func (qjrDeployment *QueueJobResDeployment) Run(stopCh <-chan struct{}) {
	qjrDeployment.deployInformer.Informer().Run(stopCh)
}

func (qjrDeployment *QueueJobResDeployment) addDeployment(obj interface{}) {

	return
}

func (qjrDeployment *QueueJobResDeployment) updateDeployment(old, cur interface{}) {

	return
}

func (qjrDeployment *QueueJobResDeployment) deleteDeployment(obj interface{}) {

	return
}


// Parse queue job api object to get Service template
func (qjrDeployment *QueueJobResDeployment) getDeploymentTemplate(qjobRes *arbv1.XQueueJobResource) (*apps.Deployment, error) {
	deploymentGVK := schema.GroupVersion{Group: apps.GroupName, Version: "v1beta1"}.WithKind("Deployment")
	obj, _, err := qjrDeployment.jsonSerializer.Decode(qjobRes.Template.Raw, &deploymentGVK, nil)
	if err != nil {
		return nil, err
	}

	deployment, ok := obj.(*apps.Deployment)
	if !ok {
		return nil, fmt.Errorf("Queuejob resource not defined as a Deployment")
	}

	return deployment, nil

}

func (qjrDeployment *QueueJobResDeployment) createDeploymentWithControllerRef(namespace string, deployment *apps.Deployment, controllerRef *metav1.OwnerReference) error {
	if controllerRef != nil {
		deployment.OwnerReferences = append(deployment.OwnerReferences, *controllerRef)
	}

	if _, err := qjrDeployment.clients.AppsV1beta1().Deployments(namespace).Create(deployment); err != nil {
		return err
	}

	return nil
}

func (qjrDeployment *QueueJobResDeployment) delDeployment(namespace string, name string) error {
	if err := qjrDeployment.clients.AppsV1beta1().Deployments(namespace).Delete(name, nil); err != nil {
		return err
	}
	return nil
}

func (qjrDeployment *QueueJobResDeployment) UpdateQueueJobStatus(queuejob *arbv1.XQueueJob) error {
	return nil
}

func (qjrDeployment *QueueJobResDeployment) SyncQueueJob(queuejob *arbv1.XQueueJob, qjobRes *arbv1.XQueueJobResource) error {

	startTime := time.Now()

	defer func() {
		// glog.V(4).Infof("Finished syncing queue job resource %q (%v)", qjobRes.Template, time.Now().Sub(startTime))
		glog.V(4).Infof("Finished syncing queue job resource %s (%v)", queuejob.Name, time.Now().Sub(startTime))
	}()

	_namespace, deploymentInQjr, deploymentsInEtcd, err := qjrDeployment.getDeploymentForQueueJobRes(qjobRes, queuejob)
	if err != nil {
		return err
	}

	deploymentLen := len(deploymentsInEtcd)
	replicas := qjobRes.Replicas

	diff := int(replicas) - int(deploymentLen)

	glog.V(4).Infof("QJob: %s had %d Deployments and %d desired Deployments", queuejob.Name, deploymentLen, replicas)

	if diff > 0 {
		//TODO: need set reference after Service has been really added
		tmpDeployment := apps.Deployment{}
		err = qjrDeployment.refManager.AddReference(qjobRes, &tmpDeployment)
		if err != nil {
			glog.Errorf("Cannot add reference to configmap resource %+v", err)
			return err
		}
		if deploymentInQjr.Labels == nil {
			deploymentInQjr.Labels = map[string]string{}
		}
		for k, v := range tmpDeployment.Labels {
			deploymentInQjr.Labels[k] = v
		}
		deploymentInQjr.Labels[queueJobName] = queuejob.Name
    if deploymentInQjr.Spec.Template.Labels == nil {
            deploymentInQjr.Labels = map[string]string{}
    }
    deploymentInQjr.Spec.Template.Labels[queueJobName] = queuejob.Name

		wait := sync.WaitGroup{}
		wait.Add(int(diff))
		for i := 0; i < diff; i++ {
			go func() {
				defer wait.Done()

				err := qjrDeployment.createDeploymentWithControllerRef(*_namespace, deploymentInQjr, metav1.NewControllerRef(queuejob, queueJobKind))

				if err != nil && errors.IsTimeout(err) {
					return
				}
				if err != nil {
					defer utilruntime.HandleError(err)
				}
			}()
		}
		wait.Wait()
	}

	return nil
}


func (qjrDeployment *QueueJobResDeployment) getDeploymentForQueueJobRes(qjobRes *arbv1.XQueueJobResource, queuejob *arbv1.XQueueJob) (*string, *apps.Deployment, []*apps.Deployment, error) {

	// Get "a" Deployment from XQJ Resource
	deploymentInQjr, err := qjrDeployment.getDeploymentTemplate(qjobRes)
	if err != nil {
		glog.Errorf("Cannot read template from resource %+v %+v", qjobRes, err)
		return nil, nil, nil, err
	}

	// Get Deployment"s" in Etcd Server
	var _namespace *string
	if deploymentInQjr.Namespace!=""{
		_namespace = &deploymentInQjr.Namespace
	} else {
		_namespace = &queuejob.Namespace
	}

	// deploymentList, err := qjrDeployment.clients.CoreV1().Deployments(*_namespace).List(metav1.ListOptions{})
	deploymentList, err := qjrDeployment.clients.AppsV1beta1().Deployments(*_namespace).List(metav1.ListOptions{LabelSelector: fmt.Sprintf("%s=%s", queueJobName, queuejob.Name),})
	// deploymentList, err := qjrDeployment.clients.AppsV1().Deployments(*_namespace).List(metav1.ListOptions{})
	if err != nil {
		return nil, nil, nil, err
	}
	deploymentsInEtcd := []*apps.Deployment{}
	// for i, deployment := range deploymentList.Items {
	// 	metaDeployment, err := meta.Accessor(&deployment)
	// 	if err != nil {
	// 		return nil, nil, nil, err
	// 	}
	// 	controllerRef := metav1.GetControllerOf(metaDeployment)
	// 	if controllerRef != nil {
	// 		if controllerRef.UID == queuejob.UID {
	// 			deploymentsInEtcd = append(deploymentsInEtcd, &deploymentList.Items[i])
	// 		}
	// 	}
	// }
	for i, _ := range deploymentList.Items {
				deploymentsInEtcd = append(deploymentsInEtcd, &deploymentList.Items[i])
	}

	// glog.Infof("[Tonghoon] Number of Deployment: %d\n", len(deploymentsInEtcd))

	myDeploymentsInEtcd := []*apps.Deployment{}
	for i, deployment := range deploymentsInEtcd {
		// glog.Infof("[Tonghoon] Deployment Name: %s\n", deployment.Name)
		// glog.Infof("Comapre: %s, %s\n", qjobRes, deployment)
		if qjrDeployment.refManager.BelongTo(qjobRes, deployment) {
			myDeploymentsInEtcd = append(myDeploymentsInEtcd, deploymentsInEtcd[i])
		}
	}

	return _namespace, deploymentInQjr, myDeploymentsInEtcd, nil
}


func (qjrDeployment *QueueJobResDeployment) deleteQueueJobResDeployments(qjobRes *arbv1.XQueueJobResource, queuejob *arbv1.XQueueJob) error {

	job := *queuejob

	_namespace, _, activeDeployments, err := qjrDeployment.getDeploymentForQueueJobRes(qjobRes, queuejob)
	if err != nil {
		return err
	}

	active := int32(len(activeDeployments))

	wait := sync.WaitGroup{}
	wait.Add(int(active))
	for i := int32(0); i < active; i++ {
		go func(ix int32) {
			defer wait.Done()
			if err := qjrDeployment.delDeployment(*_namespace, activeDeployments[ix].Name); err != nil {
				defer utilruntime.HandleError(err)
				glog.V(2).Infof("Failed to delete %v, queue job %q/%q deadline exceeded", activeDeployments[ix].Name, *_namespace, job.Name)
			}
		}(i)
	}
	wait.Wait()

	return nil
}

//Cleanup deletes all services
func (qjrDeployment *QueueJobResDeployment) Cleanup(queuejob *arbv1.XQueueJob, qjobRes *arbv1.XQueueJobResource) error {
	return qjrDeployment.deleteQueueJobResDeployments(qjobRes, queuejob)
}


// func (qjrDeployment *QueueJobResDeployment) SyncQueueJob(queuejob *arbv1.XQueueJob, qjobRes *arbv1.XQueueJobResource) error {
//
// 	startTime := time.Now()
// 	defer func() {
// 		// glog.V(4).Infof("Finished syncing queue job resource %q (%v)", qjobRes.Template, time.Now().Sub(startTime))
// 		glog.V(4).Infof("Finished syncing queue job resource %s (%v)", queuejob.Name, time.Now().Sub(startTime))
// 	}()
//
// 	deployments, err := qjrDeployment.getDeploymentsForQueueJobRes(qjobRes, queuejob)
// 	if err != nil {
// 		return err
// 	}
//
// 	deploymentLen := len(deployments)
// 	replicas := qjobRes.Replicas
//
// 	diff := int(replicas) - int(deploymentLen)
//
// 	glog.V(4).Infof("QJob: %s had %d deployments and %d desired deployments", queuejob.Name, replicas, deploymentLen)
//
// 	if diff > 0 {
// 		template, err := qjrDeployment.getDeploymentTemplate(qjobRes)
// 		if err != nil {
// 			glog.Errorf("Cannot read template from resource %+v %+v", qjobRes, err)
// 			return err
// 		}
// 		//TODO: need set reference after Service has been really added
// 		tmpService := v1.Service{}
// 		err = qjrDeployment.refManager.AddReference(qjobRes, &tmpService)
// 		if err != nil {
// 			glog.Errorf("Cannot add reference to deployment resource %+v", err)
// 			return err
// 		}
//
// 		if template.Labels == nil {
// 			template.Labels = map[string]string{}
// 		}
// 		for k, v := range queuejob.Labels {
// 			template.Labels[k] = v
// 		}
// 		template.Labels[queueJobName] = queuejob.Name
// 		if template.Spec.Template.Labels == nil {
// 			template.Labels = map[string]string{}
// 		}
// 		template.Spec.Template.Labels[queueJobName] = queuejob.Name
// 		wait := sync.WaitGroup{}
// 		wait.Add(int(diff))
// 		for i := 0; i < diff; i++ {
// 			go func() {
// 				defer wait.Done()
// 				_namespace:=""
// 				if template.Namespace!=""{
// 					_namespace=template.Namespace
// 				} else {
// 					_namespace=queuejob.Namespace
// 					// err = qjrConfigMap.createConfigMapWithControllerRef(.Namespace, template, metav1.NewControllerRef(queuejob, queueJobKind))
// 				}
//
// 				err = qjrDeployment.createDeploymentWithControllerRef(_namespace, template, metav1.NewControllerRef(queuejob, queueJobKind))
//
// 				if err != nil && errors.IsTimeout(err) {
// 					return
// 				}
// 				if err != nil {
// 					defer utilruntime.HandleError(err)
// 				}
// 			}()
// 		}
// 		wait.Wait()
// 	}
//
// 	return nil
// }
//
// func (qjrDeployment *QueueJobResDeployment) getDeploymentsForQueueJob(qjobRes *arbv1.XQueueJobResource, queuejob *arbv1.XQueueJob) ([]*apps.Deployment, error) {
//
// 	template, err := qjrDeployment.getDeploymentTemplate(qjobRes)
// 	if err != nil {
// 		glog.Errorf("Cannot read template from resource %+v %+v", qjobRes, err)
// 		return nil, err
// 	}
//
// 	_namespace:=""
// 	if template.Namespace!=""{
// 		_namespace=template.Namespace
// 	} else {
// 		_namespace=queuejob.Namespace
// 	}
//
// 	deploymentlist, err := qjrDeployment.clients.AppsV1beta1().Deployments(_namespace).List(metav1.ListOptions{
//                 		LabelSelector: fmt.Sprintf("%s=%s", queueJobName, queuejob.Name),
//         	})
// 	if err != nil {
// 		return nil, err
// 	}
//
// 	deployments := []*apps.Deployment{}
// 	for i, _ := range deploymentlist.Items {
// 		deployments = append(deployments, &deploymentlist.Items[i])
// 	}
// 	return deployments, nil
//
// }
//
// func (qjrDeployment *QueueJobResDeployment) getDeploymentsForQueueJobRes(qjobRes *arbv1.XQueueJobResource, queuejob *arbv1.XQueueJob) ([]*apps.Deployment, error) {
//
// 	deployments, err := qjrDeployment.getDeploymentsForQueueJob(qjobRes, queuejob)
// 	if err != nil {
// 		return nil, err
// 	}
//
// 	myServices := []*apps.Deployment{}
// 	for i, deployment := range deployments {
// 		if qjrDeployment.refManager.BelongTo(qjobRes, deployment) {
// 			myServices = append(myServices, deployments[i])
// 		}
// 	}
//
// 	return myServices, nil
//
// }
//
// func (qjrDeployment *QueueJobResDeployment) deleteQueueJobResDeployments(qjobRes *arbv1.XQueueJobResource, queuejob *arbv1.XQueueJob) error {
// 	job := *queuejob
//
// 	activeServices, err := qjrDeployment.getDeploymentsForQueueJob(qjobRes, queuejob)
// 	if err != nil {
// 		return err
// 	}
//
// 	active := int32(len(activeServices))
//
// 	glog.Infof("Deleting %v deployments for job %s\n", active, job.Name)
//
// 	wait := sync.WaitGroup{}
// 	wait.Add(int(active))
// 	for i := int32(0); i < active; i++ {
// 		go func(ix int32) {
// 			defer wait.Done()
// 			if err := qjrDeployment.delDeployment(queuejob.Namespace, activeServices[ix].Name); err != nil {
// 				defer utilruntime.HandleError(err)
// 				glog.V(2).Infof("Failed to delete %v, queue job %q/%q deadline exceeded", activeServices[ix].Name, job.Namespace, job.Name)
// 			}
// 		}(i)
// 	}
// 	wait.Wait()
//
// 	return nil
// }
//
// //Cleanup deletes all resources with this contorller
// func (qjrDeployment *QueueJobResDeployment) Cleanup(queuejob *arbv1.XQueueJob, qjobRes *arbv1.XQueueJobResource) error {
// 	return qjrDeployment.deleteQueueJobResDeployments(qjobRes, queuejob)
// }
