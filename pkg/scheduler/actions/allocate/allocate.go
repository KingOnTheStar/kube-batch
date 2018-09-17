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

package allocate

import (
	"github.com/golang/glog"

	"github.com/kubernetes-incubator/kube-arbitrator/contrib/device-plugin-dll"
	"github.com/kubernetes-incubator/kube-arbitrator/pkg/scheduler/api"
	"github.com/kubernetes-incubator/kube-arbitrator/pkg/scheduler/framework"
	"github.com/kubernetes-incubator/kube-arbitrator/pkg/scheduler/util"
)

const (
	IdleResource      = 0
	ReleasingResource = 1
)

type allocateAction struct {
	ssn *framework.Session
}

func New() *allocateAction {
	return &allocateAction{}
}

func (alloc *allocateAction) Name() string {
	return "allocate"
}

func (alloc *allocateAction) Initialize() {}

func (alloc *allocateAction) Execute(ssn *framework.Session) {
	glog.V(3).Infof("Enter Allocate ...")
	defer glog.V(3).Infof("Leaving Allocate ...")

	queues := util.NewPriorityQueue(ssn.QueueOrderFn)
	jobsMap := map[api.QueueID]*util.PriorityQueue{}

	for _, job := range ssn.Jobs {
		if _, found := jobsMap[job.Queue]; !found {
			jobsMap[job.Queue] = util.NewPriorityQueue(ssn.JobOrderFn)
		}

		if queue, found := ssn.QueueIndex[job.Queue]; found {
			queues.Push(queue)
		}

		glog.V(3).Infof("Added Job <%s/%s> into Queue <%s>", job.Namespace, job.Name, job.Queue)
		jobsMap[job.Queue].Push(job)
	}

	glog.V(3).Infof("Try to allocate resource to %d Queues", len(jobsMap))

	pendingTasks := map[api.JobID]*util.PriorityQueue{}

	for {
		device_plugin_dll.GlobalGpuSchedulerPlugin.HelloDLL("Test")
		if queues.Empty() {
			break
		}

		queue := queues.Pop().(*api.QueueInfo)
		if ssn.Overused(queue) {
			glog.V(3).Infof("Queue <%s> is overused, ignore it.", queue.Name)
			continue
		}

		jobs, found := jobsMap[queue.UID]

		glog.V(3).Infof("Try to allocate resource to Jobs in Queue <%v>", queue.Name)

		if !found || jobs.Empty() {
			glog.V(3).Infof("Can not find jobs for queue %s.", queue.Name)
			continue
		}

		job := jobs.Pop().(*api.JobInfo)
		if _, found := pendingTasks[job.UID]; !found {
			tasks := util.NewPriorityQueue(ssn.TaskOrderFn)
			for _, task := range job.TaskStatusIndex[api.Pending] {
				tasks.Push(task)
			}
			pendingTasks[job.UID] = tasks
		}
		tasks := pendingTasks[job.UID]

		glog.V(3).Infof("Try to allocate resource to %d tasks of Job <%v/%v>",
			tasks.Len(), job.Namespace, job.Name)

		for !tasks.Empty() {
			task := tasks.Pop().(*api.TaskInfo)
			assigned := false

			glog.V(3).Infof("There are <%d> nodes for Job <%v/%v>",
				len(ssn.Nodes), job.Namespace, job.Name)

			// Search all nodes
			var bestNode *api.NodeInfo
			var bestNodeAnnotation map[string]string
			var resourceType int
			bestNodeScore := 0
			for _, node := range ssn.Nodes {
				glog.V(3).Infof("Considering Task <%v/%v> on node <%v>: <%v> vs. <%v>",
					task.Namespace, task.Name, node.Name, task.Resreq, node.Idle)

				// TODO (k82cn): Enable eCache for performance improvement.
				if err := ssn.PredicateFn(task, node); err != nil {
					glog.V(3).Infof("Predicates failed for task <%s/%s> on node <%s>: %v",
						task.Namespace, task.Name, node.Name, err)
					continue
				}

				// Allocate
				if task.Resreq.LessEqual(node.Idle) {
					// Allocate idle resource to the task.
					glog.V(3).Infof("Binding Task <%v/%v> to node <%v>",
						task.Namespace, task.Name, node.Name)

					nodeScore, nodeAnnotation := device_plugin_dll.GlobalGpuSchedulerPlugin.AssessTaskAndNode(node.Name, int(task.Resreq.MilliGPU/1000))
					if nodeScore > bestNodeScore {
						bestNode = node
						bestNodeScore = nodeScore
						bestNodeAnnotation = nodeAnnotation
					}

					resourceType = IdleResource
				} else if task.Resreq.LessEqual(node.Releasing) {
					// Allocate releasing resource to the task if any.
					glog.V(3).Infof("Pipelining Task <%v/%v> to node <%v> for <%v> on <%v>",
						task.Namespace, task.Name, node.Name, task.Resreq, node.Releasing)

					nodeScore, nodeAnnotation := device_plugin_dll.GlobalGpuSchedulerPlugin.AssessTaskAndNode(node.Name, int(task.Resreq.MilliGPU/1000))
					if nodeScore > bestNodeScore {
						bestNode = node
						bestNodeScore = nodeScore
						bestNodeAnnotation = nodeAnnotation
					}

					resourceType = ReleasingResource
				}
			}

			if bestNode != nil {
				if resourceType == IdleResource {
					for k, v := range bestNodeAnnotation {
						task.Pod.Annotations[k] = v
					}
					if err := ssn.Allocate(task, bestNode.Name); err != nil {
						glog.Errorf("Failed to bind Task %v on %v in Session %v",
							task.UID, bestNode.Name, ssn.UID)
						break
					}
				} else if resourceType == ReleasingResource {
					for k, v := range bestNodeAnnotation {
						task.Pod.Annotations[k] = v
					}
					if err := ssn.Pipeline(task, bestNode.Name); err != nil {
						glog.Errorf("Failed to pipeline Task %v on %v in Session %v",
							task.UID, bestNode.Name, ssn.UID)
						break
					}
				} else {
					glog.Errorf("Unknow resource type")
					break
				}
				assigned = true
			}

			if assigned {
				jobs.Push(job)
			}

			// Handle one pending task in each loop.
			break
		}

		// Added Queue back until no job in Queue.
		queues.Push(queue)
	}
}

func (alloc *allocateAction) Execute_FirstFind(ssn *framework.Session) {
	glog.V(3).Infof("Enter Allocate ...")
	defer glog.V(3).Infof("Leaving Allocate ...")

	queues := util.NewPriorityQueue(ssn.QueueOrderFn)
	jobsMap := map[api.QueueID]*util.PriorityQueue{}

	for _, job := range ssn.Jobs {
		if _, found := jobsMap[job.Queue]; !found {
			jobsMap[job.Queue] = util.NewPriorityQueue(ssn.JobOrderFn)
		}

		if queue, found := ssn.QueueIndex[job.Queue]; found {
			queues.Push(queue)
		}

		glog.V(3).Infof("Added Job <%s/%s> into Queue <%s>", job.Namespace, job.Name, job.Queue)
		jobsMap[job.Queue].Push(job)
	}

	glog.V(3).Infof("Try to allocate resource to %d Queues", len(jobsMap))

	pendingTasks := map[api.JobID]*util.PriorityQueue{}

	for {
		if queues.Empty() {
			break
		}

		queue := queues.Pop().(*api.QueueInfo)
		if ssn.Overused(queue) {
			glog.V(3).Infof("Queue <%s> is overused, ignore it.", queue.Name)
			continue
		}

		jobs, found := jobsMap[queue.UID]

		glog.V(3).Infof("Try to allocate resource to Jobs in Queue <%v>", queue.Name)

		if !found || jobs.Empty() {
			glog.V(3).Infof("Can not find jobs for queue %s.", queue.Name)
			continue
		}

		job := jobs.Pop().(*api.JobInfo)
		if _, found := pendingTasks[job.UID]; !found {
			tasks := util.NewPriorityQueue(ssn.TaskOrderFn)
			for _, task := range job.TaskStatusIndex[api.Pending] {
				tasks.Push(task)
			}
			pendingTasks[job.UID] = tasks
		}
		tasks := pendingTasks[job.UID]

		glog.V(3).Infof("Try to allocate resource to %d tasks of Job <%v/%v>",
			tasks.Len(), job.Namespace, job.Name)

		for !tasks.Empty() {
			task := tasks.Pop().(*api.TaskInfo)
			assigned := false

			glog.V(3).Infof("There are <%d> nodes for Job <%v/%v>",
				len(ssn.Nodes), job.Namespace, job.Name)

			for _, node := range ssn.Nodes {
				glog.V(3).Infof("Considering Task <%v/%v> on node <%v>: <%v> vs. <%v>",
					task.Namespace, task.Name, node.Name, task.Resreq, node.Idle)

				// TODO (k82cn): Enable eCache for performance improvement.
				if err := ssn.PredicateFn(task, node); err != nil {
					glog.V(3).Infof("Predicates failed for task <%s/%s> on node <%s>: %v",
						task.Namespace, task.Name, node.Name, err)
					continue
				}

				// Allocate idle resource to the task.
				if task.Resreq.LessEqual(node.Idle) {
					glog.V(3).Infof("Binding Task <%v/%v> to node <%v>",
						task.Namespace, task.Name, node.Name)
					if err := ssn.Allocate(task, node.Name); err != nil {
						glog.Errorf("Failed to bind Task %v on %v in Session %v",
							task.UID, node.Name, ssn.UID)
						continue
					}
					assigned = true
					break
				}

				// Allocate releasing resource to the task if any.
				if task.Resreq.LessEqual(node.Releasing) {
					glog.V(3).Infof("Pipelining Task <%v/%v> to node <%v> for <%v> on <%v>",
						task.Namespace, task.Name, node.Name, task.Resreq, node.Releasing)
					if err := ssn.Pipeline(task, node.Name); err != nil {
						glog.Errorf("Failed to pipeline Task %v on %v in Session %v",
							task.UID, node.Name, ssn.UID)
						continue
					}

					assigned = true
					break
				}
			}

			if assigned {
				jobs.Push(job)
			}

			// Handle one pending task in each loop.
			break
		}

		// Added Queue back until no job in Queue.
		queues.Push(queue)
	}
}

func (alloc *allocateAction) UnInitialize() {}
