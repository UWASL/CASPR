package main

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	listersv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

const SCHEDULER_NAME = "toyscheduler"

type Scheduler struct {
	clientset  *kubernetes.Clientset
	podQueue   chan *v1.Pod
	nodeLister listersv1.NodeLister
	podLister  listersv1.PodLister
	picker     NodePicker
}

func NewScheduler(clientset *kubernetes.Clientset, picker NodePicker, podQueue chan *v1.Pod, quit chan struct{}) Scheduler {
	// init informer
	factory := informers.NewSharedInformerFactory(clientset, 0)
	nodeInfomer := factory.Core().V1().Nodes()
	nodeInfomer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			node, ok := obj.(*v1.Node)
			if !ok {
				klog.Warning("not a node")
				return
			}
			klog.InfoS("added new node", "nodeName", node.Name)
		},
	})
	podInformer := factory.Core().V1().Pods()
	podInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			pod, ok := obj.(*v1.Pod)
			if !ok {
				klog.Warning("not a pod")
				return
			}
			if pod.Spec.NodeName == "" && pod.Spec.SchedulerName == SCHEDULER_NAME {
				podQueue <- pod
			}
		},
	})
	factory.Start(quit)

	return Scheduler{
		clientset:  clientset,
		podQueue:   podQueue,
		nodeLister: nodeInfomer.Lister(),
		podLister:  podInformer.Lister(),
		picker:     picker,
	}
}

func (schd *Scheduler) Run(quit chan struct{}) {
	wait.Until(schd.Schedule, time.Second*0, quit)
}

func (schd *Scheduler) Schedule() {
	// Get Pod to be scheduled
	pod := <-schd.podQueue
	// List nodes satisfying node selectors of the pod
	nodes, err := schd.nodeLister.List(labels.SelectorFromSet(pod.Spec.NodeSelector))
	if err != nil {
		klog.Error(err, "failed to list nodes")
		panic(err.Error())
	}
	// Assign
	selectedNode := schd.picker.Pick(context.TODO(), pod, nodes, make(NodePickerArgs))
	// Bind
	errBinding := schd.Bind(context.TODO(), pod, selectedNode)
	if errBinding != nil {
		klog.ErrorS(errBinding, "failed to bind pod to node",
			"pod", pod.Name,
			"node", selectedNode,
			"namespace", pod.Namespace)
		return
	}
	// Emit
	eventMessage := fmt.Sprintf("Successfully assigned %s/%s to %s", pod.Namespace, pod.Name, selectedNode)
	errEmission := schd.Emit(context.TODO(), eventMessage, pod, selectedNode)
	if errEmission != nil {
		klog.Error(errEmission, "failed to emit scheduled event")
		return
	}
	// Done
	klog.Info(eventMessage)
}

func (schd *Scheduler) Bind(ctx context.Context, pod *v1.Pod, node string) error {
	return schd.clientset.CoreV1().Pods(pod.Namespace).Bind(ctx, &v1.Binding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		},
		Target: v1.ObjectReference{
			APIVersion: "v1",
			Kind:       "Node",
			Name:       node,
		},
	}, metav1.CreateOptions{})
}

func (schd *Scheduler) Emit(ctx context.Context, msg string, pod *v1.Pod, node string) error {
	ts := time.Now().Local()
	_, err := schd.clientset.CoreV1().Events(pod.Namespace).Create(ctx, &v1.Event{
		Count:          1,
		Type:           "Normal",
		Reason:         "Scheduled",
		Message:        msg,
		FirstTimestamp: metav1.NewTime(ts),
		LastTimestamp:  metav1.NewTime(ts),
		Source: v1.EventSource{
			Component: SCHEDULER_NAME,
		},
		InvolvedObject: v1.ObjectReference{
			Kind:      "Pod",
			Name:      pod.Name,
			Namespace: pod.Namespace,
			UID:       pod.UID,
		},
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: pod.Name + "-",
		},
	}, metav1.CreateOptions{})
	return err
}
