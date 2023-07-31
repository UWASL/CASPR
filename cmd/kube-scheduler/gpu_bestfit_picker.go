package main

import (
	"context"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

type GpuBestfitPicker struct {
	clientset *kubernetes.Clientset
}

type nodeToGpuResourceMap map[string]int64

func (picker *GpuBestfitPicker) Name() string {
	return "GpuBestfitPicker"
}

func (picker *GpuBestfitPicker) Pick(ctx context.Context, pod *v1.Pod, nodes []*v1.Node, args NodePickerArgs) string {
	allocatable, requested := picker.calculateNodesGpuState(ctx, nodes)
	node := picker.SelectMostRequestedNodeForPod(pod, &allocatable, &requested)
	return node
}

// Calculate a node's GPU quantity in terms of "allocatable" and "actually requested"
func (picker *GpuBestfitPicker) calculateNodesGpuState(ctx context.Context, nodes []*v1.Node) (nodeToGpuResourceMap, nodeToGpuResourceMap) {
	allocatable := make(nodeToGpuResourceMap, len(nodes))
	requested := make(nodeToGpuResourceMap, len(nodes))
	for _, node := range nodes {
		gpuAllocatable, found := node.Status.Allocatable["nvidia.com/gpu"]
		if !found {
			allocatable[node.Name] = 0
			requested[node.Name] = 0
			continue
		}
		allocatable[node.Name] = gpuAllocatable.Value()
		requested[node.Name] = picker.calculateNodeGpuRequest(ctx, node)
		klog.InfoS("calculated node GPU",
			"node", node.Name,
			"allocatable", allocatable[node.Name],
			"allocated", requested[node.Name])
	}
	return allocatable, requested
}

// Calculate a node's GPU quantity actually occupied by active pods
func (picker *GpuBestfitPicker) calculateNodeGpuRequest(ctx context.Context, node *v1.Node) int64 {
	// List pods of node
	pods, err := picker.clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "spec.nodeName=" + node.Name,
	})
	if err != nil {
		klog.ErrorS(err, "failed to list pods of node", "node", node.Name)
		return 0
	}
	// Sum up active pods' GPU request
	var gpuRequested int64
	for _, pod := range pods.Items {
		// Assume that finished pods always release GPU
		if pod.Status.Phase != "Pending" && pod.Status.Phase != "Running" {
			continue
		}
		gpuRequested += calculatePodGpuRequest(&pod)
	}
	return gpuRequested
}

func calculatePodGpuRequest(pod *v1.Pod) int64 {
	var podRequest int64
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		value := GetGpuRequest("nvidia.com/gpu", &container.Resources.Requests)
		podRequest += value
	}
	return podRequest
}

func GetGpuRequest(resource v1.ResourceName, requests *v1.ResourceList) int64 {
	quantity, found := (*requests)[resource]
	if !found {
		return 0
	}
	return quantity.Value()
}

func (picker *GpuBestfitPicker) SelectMostRequestedNodeForPod(pod *v1.Pod, allocatable *nodeToGpuResourceMap, requested *nodeToGpuResourceMap) string {
	var selectedNode string
	var maxNodeRequested int64
	for node, gpu := range *requested {
		if gpu+calculatePodGpuRequest(pod) > (*allocatable)[node] {
			klog.InfoS("node has insufficient GPU",
				"node", node,
				"allocatable", (*allocatable)[node],
				"allocated", gpu)
			continue
		}
		if gpu > maxNodeRequested || (gpu == 0 && maxNodeRequested == 0) {
			selectedNode = node
			maxNodeRequested = gpu
		}
	}
	klog.InfoS("selected node", "node", selectedNode)
	return selectedNode
}
