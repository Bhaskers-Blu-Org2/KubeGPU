package gpuschedulerplugin

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/Microsoft/KubeDevice-API/pkg/resource"
	types "github.com/Microsoft/KubeDevice-API/pkg/types"
	"github.com/Microsoft/KubeDevice-API/pkg/utils"
	gputypes "github.com/Microsoft/KubeGPU/gpuplugintypes"
	sctypes "github.com/Microsoft/KubeGPU/gpuplugintypes"
)

// TranslateGPUResources translates GPU resources to max level advertised by the node
func TranslateGPUResources(neededGPUs int64, nodeResources types.ResourceList, containerRequests types.ResourceList) types.ResourceList {
	// First stage translation, translate # of cards to simple GPU resources - extra stage
	re := regexp.MustCompile(types.DeviceGroupPrefix + `.*/gpu/(.*?)/cards`)

	needTranslation := false
	for res := range nodeResources {
		matches := re.FindStringSubmatch(string(res))
		if len(matches) >= 2 {
			needTranslation = true
			break
		}
	}
	if !needTranslation {
		return containerRequests
	}

	haveGPUs := 0
	maxGPUIndex := -1
	for res := range containerRequests {
		matches := re.FindStringSubmatch(string(res))
		if len(matches) >= 2 {
			haveGPUs++
			gpuIndex, err := strconv.Atoi(matches[1])
			if err == nil {
				if gpuIndex > maxGPUIndex {
					maxGPUIndex = gpuIndex
				}
			}
		}
	}
	resourceModified := false
	diffGPU := int(neededGPUs - int64(haveGPUs))
	for i := 0; i < diffGPU; i++ {
		gpuIndex := maxGPUIndex + i + 1
		types.AddGroupResource(containerRequests, "gpu/"+strconv.Itoa(gpuIndex)+"/cards", 1)
		resourceModified = true
	}

	// perform 2nd stage translation if needed
	resourceModified1, containerRequests := resource.TranslateResource(nodeResources, containerRequests, "gpugrp0", "gpu")
	resourceModified = resourceModified || resourceModified1
	// perform 3rd stage translation if needed
	resourceModified1, containerRequests = resource.TranslateResource(nodeResources, containerRequests, "gpugrp1", "gpugrp0")
	resourceModified = resourceModified || resourceModified1

	if resourceModified {
		utils.Logf(3, "New Resources: %v", containerRequests)
	}

	return containerRequests
}

func max(x, y int64) int64 {
	if x > y {
		return x
	}
	return y
}

func TranslateGPUContainerResources(alloc types.ResourceList, cont types.ContainerInfo) types.ResourceList {
	numGPUs, _ := cont.Requests[gputypes.ResourceGPU]
	return TranslateGPUResources(numGPUs, alloc, cont.DevRequests)
}

func SetGPUReqs(cont *types.ContainerInfo) {
	numGPUs, ok := cont.Requests[gputypes.ResourceGPU]
	numKGPUs, okK := cont.KubeRequests[gputypes.ResourceGPU]
	if ok && okK {
		cont.Requests[gputypes.ResourceGPU] = max(numGPUs, numKGPUs)
	} else if ok {
		// numGPUs = numGPUs
	} else if okK {
		cont.Requests[gputypes.ResourceGPU] = numKGPUs
	} else {
		cont.Requests[gputypes.ResourceGPU] = 0
	}
}

func TranslatePodGPUResources(nodeInfo *types.NodeInfo, podInfo *types.PodInfo) (error, bool) {
	for _, contCopy := range podInfo.InitContainers {
		SetGPUReqs(&contCopy)
	}
	for _, contCopy := range podInfo.RunningContainers {
		SetGPUReqs(&contCopy)
	}

	req, ok := podInfo.Requests[GPUTopologyGeneration]
	found := true
	if !ok || req == int64(1) { // auto generate best topology if no explicit request given
		found = ConvertToBestGPURequests(podInfo) // found a tree
		if found {
			utils.Logf(4, "Auto-generated topology using best tree: %+v", podInfo)
			return nil, found
		}
	}

	if !found || req == int64(0) { // zero implies no topology
		for contName, contCopy := range podInfo.InitContainers {
			contCopy.DevRequests = TranslateGPUContainerResources(nodeInfo.Allocatable, contCopy)
			podInfo.InitContainers[contName] = contCopy
		}
		for contName, contCopy := range podInfo.RunningContainers {
			contCopy.DevRequests = TranslateGPUContainerResources(nodeInfo.Allocatable, contCopy)
			podInfo.RunningContainers[contName] = contCopy
		}
		utils.Logf(4, "Auto-generated topology using no topology: %+v", podInfo)
		return nil, true
	}

	utils.Errorf("Invalid topology generation request %v", podInfo.Requests[GPUTopologyGeneration])
	return fmt.Errorf("Invalid topology generation request"), false
}

func addToNode(node *sctypes.SortedTreeNode, nodeResources types.ResourceList, partitionPrefix string, suffix string, partitionLevel int) *sctypes.SortedTreeNode {
	childMap := make(map[string]types.ResourceList)
	re := regexp.MustCompile(`.*/` + partitionPrefix + strconv.Itoa(partitionLevel) + `/(.*?)/.*/` + suffix)
	totalLen := 0
	sortedKeys := utils.SortedStringKeys(nodeResources)
	for _, resourceKey := range sortedKeys {
		resourceVal := nodeResources[types.ResourceName(resourceKey)]
		matches := re.FindStringSubmatch(string(resourceKey))
		if len(matches) >= 2 {
			subGrpKey := matches[1]
			if childMap[subGrpKey] == nil {
				childMap[subGrpKey] = make(types.ResourceList)
			}
			childMap[subGrpKey][types.ResourceName(resourceKey)] = resourceVal
			totalLen++
		}
	}
	if node == nil {
		node = &sctypes.SortedTreeNode{Val: totalLen, Child: nil}
	}
	sortedKeys = utils.SortedStringKeys(childMap)
	for _, subMapKey := range sortedKeys {
		subMaps := childMap[subMapKey]
		childNode := &sctypes.SortedTreeNode{Val: len(subMaps), Child: nil}
		if partitionLevel > 0 {
			addToNode(childNode, subMaps, partitionPrefix, suffix, partitionLevel-1)
			childNode.Score = computeTreeScore(childNode)
			//fmt.Printf("Child score = %f\n", childNode.Score)
		}
		sctypes.AddNodeToSortedTreeNode(node, childNode)
	}
	return node
}

type treeInfo struct {
	ListOfNodes map[string]bool
	TreeScore   float64
}

var NodeCacheMap = make(map[*sctypes.SortedTreeNode]treeInfo)
var NodeLocationMap = make(map[string]*sctypes.SortedTreeNode)

func removeNodeFromCache(nodeName string, nodeLocation *sctypes.SortedTreeNode) {
	if nodeLocation != nil {
		delete(NodeCacheMap[nodeLocation].ListOfNodes, nodeName)
		if len(NodeCacheMap[nodeLocation].ListOfNodes) == 0 {
			delete(NodeCacheMap, nodeLocation)
		}
	}
}

func computeTreeScoreAtLevel(node *sctypes.SortedTreeNode, level int, numChild int) float64 {
	score := float64(node.Val*level) / float64(numChild)
	for _, child := range node.Child {
		score += computeTreeScoreAtLevel(child, level+1, len(node.Child))
	}
	return score
}

func computeTreeScore(node *sctypes.SortedTreeNode) float64 {
	return computeTreeScoreAtLevel(node, 0, len(node.Child))
}

func AddResourcesToNodeTreeCache(nodeName string, nodeResources types.ResourceList) {
	if nodeResources == nil || len(nodeResources) == 0 {
		return
	}
	// get tree representation of node gpu resources
	node := addToNode(nil, nodeResources, "gpugrp", "cards", 1) // gpugrp1 and gpugrp0
	// see if resource has changed
	nodeLocation := NodeLocationMap[nodeName]
	if sctypes.CompareTreeNode(node, nodeLocation) {
		return
	}
	// remove node from current location
	removeNodeFromCache(nodeName, nodeLocation)
	// check if matches to some other node in cache
	found := false
	for cacheKey := range NodeCacheMap {
		if sctypes.CompareTreeNode(node, cacheKey) {
			NodeCacheMap[cacheKey].ListOfNodes[nodeName] = true
			nodeLocation = cacheKey
			found = true
			break
		}
	}
	// if not found add new to cache
	if !found {
		treeScore := computeTreeScore(node)
		treeInfo := treeInfo{ListOfNodes: map[string]bool{nodeName: true}, TreeScore: treeScore}
		nodeLocation = node
		NodeCacheMap[node] = treeInfo
	}
	//fmt.Printf("NodeName: %v nodeLocation: %v", nodeName, nodeLocation)
	NodeLocationMap[nodeName] = nodeLocation
}

func RemoveNodeFromNodeTreeCache(nodeName string) {
	nodeLocation := NodeLocationMap[nodeName]
	removeNodeFromCache(nodeName, nodeLocation)
	delete(NodeLocationMap, nodeName)
}

func findBestTreeInCache(num int) *sctypes.SortedTreeNode {
	var bestTree *sctypes.SortedTreeNode
	bestScore := 0.0
	for tree, treeInfo := range NodeCacheMap {
		if tree.Val >= num {
			if treeInfo.TreeScore > bestScore {
				bestTree = tree
				bestScore = treeInfo.TreeScore
			}
		}
	}
	//fmt.Printf("Choose best tree with score %f\n", bestScore)
	return bestTree
}

func assignGPUs(node *sctypes.SortedTreeNode, prefix string, resourceGrp string, resource string, suffix string, level int, numLeft *int) types.ResourceList {
	resList := make(types.ResourceList)
	if level == 0 {
		toTake := node.Val
		if *numLeft <= node.Val {
			toTake = *numLeft
		}
		for i := 0; i < toTake; i++ {
			resList[types.ResourceName(prefix+"/"+resource+"/"+strconv.Itoa(i)+"/"+suffix)] = 1
		}
		*numLeft = *numLeft - toTake
	} else {
		for i, child := range node.Child {
			newPrefix := prefix + strconv.Itoa(level-1) + "/" + strconv.Itoa(i)
			if level-1 != 0 {
				newPrefix += "/" + resourceGrp
			}
			resListChild := assignGPUs(child, newPrefix, resourceGrp, resource, suffix, level-1, numLeft)
			for resKey, resVal := range resListChild {
				resList[resKey] = resVal
			}
		}
	}
	return resList
}

func translateToTree(node *sctypes.SortedTreeNode, cont *types.ContainerInfo) {
	// remove all GPU topology requests
	re := regexp.MustCompile(`.*/gpu/.*`)
	newRequests := make(types.ResourceList)
	for reqKey, reqVal := range cont.DevRequests {
		matches := re.FindStringSubmatch(string(reqKey))
		if len(matches) == 0 {
			newRequests[reqKey] = reqVal
		}
	}
	cont.DevRequests = newRequests
	// append requests
	numGPUs := int(cont.Requests[gputypes.ResourceGPU])
	resList := assignGPUs(node, types.DeviceGroupPrefix+"/gpugrp", "gpugrp", "gpu", "cards", 2, &numGPUs)
	//fmt.Printf("ResList: %+v", resList)
	for resKey, resVal := range resList {
		cont.DevRequests[resKey] = resVal
	}
}

// find total GPUs needed
func ConvertToBestGPURequests(podInfo *types.PodInfo) bool {
	numGPUs := int64(0)
	for _, cont := range podInfo.RunningContainers {
		numGPUs += cont.Requests[gputypes.ResourceGPU]
	}
	for _, cont := range podInfo.InitContainers {
		if cont.Requests[gputypes.ResourceGPU] > numGPUs {
			numGPUs = cont.Requests[gputypes.ResourceGPU]
		}
	}
	bestTree := findBestTreeInCache(int(numGPUs))
	if bestTree != nil {
		utils.Logf(5, "Best tree\n")
		gputypes.LogTreeNode(5, bestTree)
		// now translate requests to best tree
		contKeys := utils.SortedStringKeys(podInfo.RunningContainers)
		for _, contKey := range contKeys {
			contCopy := podInfo.RunningContainers[contKey]
			translateToTree(bestTree, &contCopy)
			podInfo.RunningContainers[contKey] = contCopy
		}
		contKeys = utils.SortedStringKeys(podInfo.InitContainers)
		for _, contKey := range contKeys {
			contCopy := podInfo.InitContainers[contKey]
			translateToTree(bestTree, &contCopy)
			podInfo.InitContainers[contKey] = contCopy
		}
		return true
	}
	return false
}
