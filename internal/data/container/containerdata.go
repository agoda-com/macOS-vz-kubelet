package container

import (
	"github.com/puzpuzpuz/xsync/v4"
	"k8s.io/apimachinery/pkg/types"
)

// ContainerData is a thread-safe map to store container information, indexed by NamespacedName and container name.
// A ContainerData must be created with NewContainerData; the zero value is not usable.
type ContainerData struct {
	data *xsync.Map[types.NamespacedName, *xsync.Map[string, ContainerInfo]]
}

// NewContainerData creates a new ContainerData instance.
func NewContainerData() ContainerData {
	return ContainerData{
		data: xsync.NewMap[types.NamespacedName, *xsync.Map[string, ContainerInfo]](),
	}
}

// Load retrieves the ContainerInfo for a specific container within a specific pod.
// It returns the ContainerInfo and a boolean indicating whether the container information was found.
func (d *ContainerData) Load(podNamespace, podName, containerName string) (ContainerInfo, bool) {
	key := types.NamespacedName{Namespace: podNamespace, Name: podName}
	innerMap, ok := d.data.Load(key)
	if !ok {
		return ContainerInfo{}, false
	}
	return innerMap.Load(containerName)
}

// LoadOrStore retrieves the ContainerInfo for a specific container within a specific pod,
// or creates and stores the provided ContainerInfo if it doesn't already exist.
// It returns the ContainerInfo and a boolean indicating whether the container information was already present.
func (d *ContainerData) LoadOrStore(podNamespace, podName, containerName string, info ContainerInfo) (ContainerInfo, bool) {
	key := types.NamespacedName{Namespace: podNamespace, Name: podName}
	innerMap, _ := d.data.LoadOrCompute(key, func() (*xsync.Map[string, ContainerInfo], bool) {
		return xsync.NewMap[string, ContainerInfo](), false
	})
	return innerMap.LoadOrStore(containerName, info)
}

// Store sets the ContainerInfo for a specific container within a specific pod.
func (d *ContainerData) Store(podNamespace, podName, containerName string, info ContainerInfo) {
	key := types.NamespacedName{Namespace: podNamespace, Name: podName}
	innerMap, _ := d.data.LoadOrCompute(key, func() (*xsync.Map[string, ContainerInfo], bool) {
		return xsync.NewMap[string, ContainerInfo](), false
	})
	innerMap.Store(containerName, info)
}

// LoadAndDelete removes all container information for a specific pod.
// It returns a map of container names to ContainerInfo and a boolean indicating whether the pod information was found.
func (d *ContainerData) LoadAndDelete(podNamespace, podName string) (map[string]ContainerInfo, bool) {
	key := types.NamespacedName{Namespace: podNamespace, Name: podName}
	innerMap, ok := d.data.LoadAndDelete(key)
	if !ok {
		return nil, false
	}
	containerInfoMap := make(map[string]ContainerInfo)
	innerMap.Range(func(key string, value ContainerInfo) bool {
		containerInfoMap[key] = value
		return true
	})
	return containerInfoMap, true
}

// LoadAll retrieves all container information for a specific pod.
// It returns a map of container names to ContainerInfo and a boolean indicating whether the pod information was found.
func (d *ContainerData) LoadAll(podNamespace, podName string) (map[string]ContainerInfo, bool) {
	key := types.NamespacedName{Namespace: podNamespace, Name: podName}
	innerMap, ok := d.data.Load(key)
	if !ok {
		return nil, false
	}
	containerInfoMap := make(map[string]ContainerInfo)
	innerMap.Range(func(key string, value ContainerInfo) bool {
		containerInfoMap[key] = value
		return true
	})
	return containerInfoMap, true
}

// All retrieves all container information for all pods.
// It returns a map of NamespacedName to a map of container names to ContainerInfo.
func (d *ContainerData) All() map[types.NamespacedName]map[string]ContainerInfo {
	containerData := make(map[types.NamespacedName]map[string]ContainerInfo)
	d.data.Range(func(key types.NamespacedName, innerMap *xsync.Map[string, ContainerInfo]) bool {
		containerInfoMap := make(map[string]ContainerInfo)
		innerMap.Range(func(name string, info ContainerInfo) bool {
			containerInfoMap[name] = info
			return true
		})
		containerData[key] = containerInfoMap
		return true
	})
	return containerData
}
