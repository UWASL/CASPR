# K8s-Scheduler

We present a comprehensive empirical study of the impact partial network partitions have on cluster managers in data analysis frameworks. Our study shows that modern scheduling approaches are vulnerable to partial network partitions. Partial partitions can lead to a complete cluster pause or a significant loss of performance. 


To overcome the shortcomings of the state-of-the-art schedulers, we design CASPR, a connectivity-aware scheduler. CASPR incorporates the current network connectivity information when making scheduling decisions to allocate fully connected nodes for a given application. CASPR effectively hides partial partitions from applications. Our evaluation of a CASPR prototype shows that it can tolerate partial network partitions eliminate application halting or significant loss of performance.

We implement our Kubernetes scheduler using Kubernetes v1.20.7.

This repository is the implementation of our paper titled CASPR: Connectivity-Aware Scheduling for
Partition Resilience published in Symposium on Reliable Distributed Systems (SRDS 2023). 

For build instructions please refer to: 

https://github.com/kubernetes/kubernetes


## Files Changed

/cmd/kube-scheduler/scheduler.go 

/cmd/kube-scheduler/app/server.go 

my-scheduler.yaml

/pkg/scheduler/scheduler.go 

/pkg/scheduler/core/generic_scheduler.go


