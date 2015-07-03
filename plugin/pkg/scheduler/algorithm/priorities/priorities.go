/*
Copyright 2014 The Kubernetes Authors All rights reserved.

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

package priorities

import (
	"math"

	"github.com/GoogleCloudPlatform/kubernetes/pkg/api"
	"github.com/GoogleCloudPlatform/kubernetes/pkg/labels"
	"github.com/GoogleCloudPlatform/kubernetes/plugin/pkg/scheduler/algorithm"
	"github.com/GoogleCloudPlatform/kubernetes/plugin/pkg/scheduler/algorithm/predicates"
	"github.com/golang/glog"
)

// the unused capacity is calculated on a scale of 0-10
// 0 being the lowest priority and 10 being the highest
func calculateScore(requested, capacity int64, node string) int {
	if capacity == 0 {
		return 0
	}
	if requested > capacity {
		glog.Infof("Combined requested resources from existing pods exceeds capacity on minion: %s", node)
		return 0
	}
	return int(((capacity - requested) * 10) / capacity)
}

// Calculate the resource occupancy on a node.  'node' has information about the resources on the node.
// 'pods' is a list of pods currently scheduled on the node.
func calculateResourceOccupancy(pod *api.Pod, node api.Node, pods []*api.Pod) algorithm.HostPriority {
	totalMilliCPU := int64(0)
	totalMemory := int64(0)
	for _, existingPod := range pods {
		for _, container := range existingPod.Spec.Containers {
			totalMilliCPU += container.Resources.Limits.Cpu().MilliValue()
			totalMemory += container.Resources.Limits.Memory().Value()
		}
	}
	// Add the resources requested by the current pod being scheduled.
	// This also helps differentiate between differently sized, but empty, minions.
	for _, container := range pod.Spec.Containers {
		totalMilliCPU += container.Resources.Limits.Cpu().MilliValue()
		totalMemory += container.Resources.Limits.Memory().Value()
	}

	capacityMilliCPU := node.Status.Capacity.Cpu().MilliValue()
	capacityMemory := node.Status.Capacity.Memory().Value()

	cpuScore := calculateScore(totalMilliCPU, capacityMilliCPU, node.Name)
	memoryScore := calculateScore(totalMemory, capacityMemory, node.Name)
//	glog.V(10).Infof(
	glog.Infof(
		"%v -> %v: Least Requested Priority, Absolute/Requested: (%d, %d) / (%d, %d) Score: (%d, %d)",
		pod.Name, node.Name,
		totalMilliCPU, totalMemory,
		capacityMilliCPU, capacityMemory,
		cpuScore, memoryScore,
	)

	return algorithm.HostPriority{
		Host:  node.Name,
		Score: int((cpuScore + memoryScore) / 2),
	}
}

// LeastRequestedPriority is a priority function that favors nodes with fewer requested resources.
// It calculates the percentage of memory and CPU requested by pods scheduled on the node, and prioritizes
// based on the minimum of the average of the fraction of requested to capacity.
// Details: (Sum(requested cpu) / Capacity + Sum(requested memory) / Capacity) * 50
func LeastRequestedPriority(pod *api.Pod, podLister algorithm.PodLister, minionLister algorithm.MinionLister) (algorithm.HostPriorityList, error) {
	nodes, err := minionLister.List()
	if err != nil {
		return algorithm.HostPriorityList{}, err
	}
	podsToMachines, err := predicates.MapPodsToMachines(podLister)

	list := algorithm.HostPriorityList{}
	for _, node := range nodes.Items {
		list = append(list, calculateResourceOccupancy(pod, node, podsToMachines[node.Name]))
	}
	return list, nil
}

func min(l, r int64) (m int64) {
	m = r
	if l < r {
		m = l
	}
	return m
}

// See comment for DumbSpreadingPriority()
const dumbSpreadingDenominator int64 = 10

// DumbSpreadingPriority is a priority function that favors nodes with fewer pods.
// It works like LeastRequestedPeriority but instead of using 10 * percentage of machine free by resource,
// it uses 10 * percentage of machine free by pod, with "percentage of machine free by pod" claculated as
// (dumbSpreadingDenominator - number of pods already on the node + 1) / dumbSpreadingDenominator.
// dumbSpreadingDenominator serves like the machine capacity in LeasRequestedPriority but is chosen
// so that we equate one pod with a reasonable amount of resources when we combine all the scores together.
func DumbSpreadingPriority(pod *api.Pod, podLister algorithm.PodLister, minionLister algorithm.MinionLister) (algorithm.HostPriorityList, error) {
	nodes, err := minionLister.List()
	if err != nil {
		return algorithm.HostPriorityList{}, err
	}
	podsToMachines, err := predicates.MapPodsToMachines(podLister)

	list := algorithm.HostPriorityList{}
	for _, node := range nodes.Items {
		npods := int64(len(podsToMachines[node.Name]))
		score := calculateScore(min(npods+1, dumbSpreadingDenominator), dumbSpreadingDenominator, node.Name)
//		glog.V(10).Infof(
		glog.Infof(
			"%v -> %v: DumbSpreadPriority, Old # pods (%d) Score: (%d)",
			pod.Name, node.Name, npods, score,
		)
		list = append(list, algorithm.HostPriority{
			Host:  node.Name,
			Score: score,
		})
	}
	return list, nil
}

type NodeLabelPrioritizer struct {
	label    string
	presence bool
}

func NewNodeLabelPriority(label string, presence bool) algorithm.PriorityFunction {
	labelPrioritizer := &NodeLabelPrioritizer{
		label:    label,
		presence: presence,
	}
	return labelPrioritizer.CalculateNodeLabelPriority
}

// CalculateNodeLabelPriority checks whether a particular label exists on a minion or not, regardless of its value.
// If presence is true, prioritizes minions that have the specified label, regardless of value.
// If presence is false, prioritizes minions that do not have the specified label.
func (n *NodeLabelPrioritizer) CalculateNodeLabelPriority(pod *api.Pod, podLister algorithm.PodLister, minionLister algorithm.MinionLister) (algorithm.HostPriorityList, error) {
	var score int
	minions, err := minionLister.List()
	if err != nil {
		return nil, err
	}

	labeledMinions := map[string]bool{}
	for _, minion := range minions.Items {
		exists := labels.Set(minion.Labels).Has(n.label)
		labeledMinions[minion.Name] = (exists && n.presence) || (!exists && !n.presence)
	}

	result := []algorithm.HostPriority{}
	//score int - scale of 0-10
	// 0 being the lowest priority and 10 being the highest
	for minionName, success := range labeledMinions {
		if success {
			score = 10
		} else {
			score = 0
		}
		result = append(result, algorithm.HostPriority{Host: minionName, Score: score})
	}
	return result, nil
}

// BalancedResourceAllocation favors nodes with balanced resource usage rate.
// BalancedResourceAllocation should **NOT** be used alone, and **MUST** be used together with LeastRequestedPriority.
// It calculates the difference between the cpu and memory fracion of capacity, and prioritizes the host based on how
// close the two metrics are to each other.
// Detail: score = 10 - abs(cpuFraction-memoryFraction)*10. The algorithm is partly inspired by:
// "Wei Huang et al. An Energy Efficient Virtual Machine Placement Algorithm with Balanced Resource Utilization"
func BalancedResourceAllocation(pod *api.Pod, podLister algorithm.PodLister, minionLister algorithm.MinionLister) (algorithm.HostPriorityList, error) {
	nodes, err := minionLister.List()
	if err != nil {
		return algorithm.HostPriorityList{}, err
	}
	podsToMachines, err := predicates.MapPodsToMachines(podLister)

	list := algorithm.HostPriorityList{}
	for _, node := range nodes.Items {
		list = append(list, calculateBalancedResourceAllocation(pod, node, podsToMachines[node.Name]))
	}
	return list, nil
}

func calculateBalancedResourceAllocation(pod *api.Pod, node api.Node, pods []*api.Pod) algorithm.HostPriority {
	totalMilliCPU := int64(0)
	totalMemory := int64(0)
	score := int(0)
	for _, existingPod := range pods {
		for _, container := range existingPod.Spec.Containers {
			totalMilliCPU += container.Resources.Limits.Cpu().MilliValue()
			totalMemory += container.Resources.Limits.Memory().Value()
		}
	}
	// Add the resources requested by the current pod being scheduled.
	// This also helps differentiate between differently sized, but empty, minions.
	for _, container := range pod.Spec.Containers {
		totalMilliCPU += container.Resources.Limits.Cpu().MilliValue()
		totalMemory += container.Resources.Limits.Memory().Value()
	}

	capacityMilliCPU := node.Status.Capacity.Cpu().MilliValue()
	capacityMemory := node.Status.Capacity.Memory().Value()

	cpuFraction := fractionOfCapacity(totalMilliCPU, capacityMilliCPU, node.Name)
	memoryFraction := fractionOfCapacity(totalMemory, capacityMemory, node.Name)
	if cpuFraction >= 1 || memoryFraction >= 1 {
		// if requested >= capacity, the corresponding host should never be preferrred.
		score = 0
	} else {
		// Upper and lower boundary of difference between cpuFraction and memoryFraction are -1 and 1
		// respectively. Multilying the absolute value of the difference by 10 scales the value to
		// 0-10 with 0 representing well balanced allocation and 10 poorly balanced. Subtracting it from
		// 10 leads to the score which also scales from 0 to 10 while 10 representing well balanced.
		diff := math.Abs(cpuFraction - memoryFraction)
		score = int(10 - diff*10)
	}
//	glog.V(10).Infof(
	glog.Infof(
		"%v -> %v: Balanced Resource Allocation, Absolute/Requested: (%d, %d) / (%d, %d) Score: (%d)",
		pod.Name, node.Name,
		totalMilliCPU, totalMemory,
		capacityMilliCPU, capacityMemory,
		score,
	)

	return algorithm.HostPriority{
		Host:  node.Name,
		Score: score,
	}
}

func fractionOfCapacity(requested, capacity int64, node string) float64 {
	if capacity == 0 {
		return 1
	}
	return float64(requested) / float64(capacity)
}
