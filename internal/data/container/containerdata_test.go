package container_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/agoda-com/macOS-vz-kubelet/internal/data/container"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/types"
)

func TestLoad(t *testing.T) {
	data := container.NewContainerData()
	info := container.ContainerInfo{ID: "123", Error: nil}
	data.Store("default", "pod1", "container1", info)

	result, found := data.Load("default", "pod1", "container1")
	assert.True(t, found)
	assert.Equal(t, info, result)

	result, found = data.Load("default", "pod1", "container2")
	assert.False(t, found)
	assert.Equal(t, container.ContainerInfo{}, result)

	result, found = data.Load("default", "pod2", "container1")
	assert.False(t, found)
	assert.Equal(t, container.ContainerInfo{}, result)
}

func TestLoadOrStore(t *testing.T) {
	data := container.NewContainerData()
	info := container.ContainerInfo{ID: "123", Error: nil}

	result, loaded := data.LoadOrStore("default", "pod1", "container1", info)
	assert.False(t, loaded)
	assert.Equal(t, info, result)

	// Test retrieving the same container info
	result, loaded = data.LoadOrStore("default", "pod1", "container1", info)
	assert.True(t, loaded)
	assert.Equal(t, info, result)
}

func TestStore(t *testing.T) {
	data := container.NewContainerData()
	info := container.ContainerInfo{ID: "123", Error: nil}

	data.Store("default", "pod1", "container1", info)

	result, found := data.Load("default", "pod1", "container1")
	assert.True(t, found)
	assert.Equal(t, info, result)
}

func TestLoadAndDelete(t *testing.T) {
	data := container.NewContainerData()
	info1 := container.ContainerInfo{ID: "123", Error: nil}
	info2 := container.ContainerInfo{ID: "456", Error: nil}

	data.Store("default", "pod1", "container1", info1)
	data.Store("default", "pod1", "container2", info2)

	containerInfoMap, found := data.LoadAndDelete("default", "pod1")
	assert.True(t, found)
	assert.Len(t, containerInfoMap, 2)
	assert.Equal(t, info1, containerInfoMap["container1"])
	assert.Equal(t, info2, containerInfoMap["container2"])

	// Ensure data is removed
	containerInfoMap, found = data.LoadAndDelete("default", "pod1")
	assert.False(t, found)
	assert.Nil(t, containerInfoMap)
}

func TestLoadAll(t *testing.T) {
	data := container.NewContainerData()
	info1 := container.ContainerInfo{ID: "123", Error: nil}
	info2 := container.ContainerInfo{ID: "456", Error: nil}

	data.Store("default", "pod1", "container1", info1)
	data.Store("default", "pod1", "container2", info2)

	containerInfoMap, found := data.LoadAll("default", "pod1")
	assert.True(t, found)
	assert.Len(t, containerInfoMap, 2)
	assert.Equal(t, info1, containerInfoMap["container1"])
	assert.Equal(t, info2, containerInfoMap["container2"])

	containerInfoMap, found = data.LoadAll("default", "pod2")
	assert.False(t, found)
	assert.Nil(t, containerInfoMap)
}

func TestAll(t *testing.T) {
	data := container.NewContainerData()
	info1 := container.ContainerInfo{ID: "123", Error: nil}
	info2 := container.ContainerInfo{ID: "456", Error: nil}
	info3 := container.ContainerInfo{ID: "789", Error: nil}

	data.Store("default", "pod1", "container1", info1)
	data.Store("default", "pod1", "container2", info2)
	data.Store("default", "pod2", "container3", info3)

	allData := data.All()
	assert.Len(t, allData, 2)
	assert.Len(t, allData[types.NamespacedName{Namespace: "default", Name: "pod1"}], 2)
	assert.Len(t, allData[types.NamespacedName{Namespace: "default", Name: "pod2"}], 1)

	assert.Equal(t, info1, allData[types.NamespacedName{Namespace: "default", Name: "pod1"}]["container1"])
	assert.Equal(t, info2, allData[types.NamespacedName{Namespace: "default", Name: "pod1"}]["container2"])
	assert.Equal(t, info3, allData[types.NamespacedName{Namespace: "default", Name: "pod2"}]["container3"])
}

func TestConcurrentAccess(t *testing.T) {
	data := container.NewContainerData()
	namespace := "default"
	numRoutines := 100
	numPods := 10

	// Phase 1: Concurrent writes - populate pods with containers
	var wg sync.WaitGroup
	for i := range numRoutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			podName := fmt.Sprintf("pod%d", i%numPods)
			containerName := fmt.Sprintf("container%d", i)
			info := container.ContainerInfo{ID: fmt.Sprintf("id%d", i), Error: nil}
			data.Store(namespace, podName, containerName, info)
		}(i)
	}
	wg.Wait()

	// Phase 2: Concurrent mixed operations
	// Split pods into two groups:
	// - Pods 0-4: subjected to LoadAndDelete (deletion-heavy)
	// - Pods 5-9: subjected to Store (write-heavy)
	for i := range numRoutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			podIdx := i % numPods
			podName := fmt.Sprintf("pod%d", podIdx)

			if podIdx < 5 {
				// Deletion-heavy group: concurrent LoadAndDelete
				_, _ = data.LoadAndDelete(namespace, podName)
			} else {
				// Write-heavy group: concurrent Store
				containerName := fmt.Sprintf("new-container%d", i)
				info := container.ContainerInfo{ID: fmt.Sprintf("new-id%d", i), Error: nil}
				data.Store(namespace, podName, containerName, info)
			}

			// All goroutines exercise read operations
			_, _ = data.LoadAll(namespace, podName)
			_, _ = data.Load(namespace, podName, fmt.Sprintf("container%d", i))
			_ = data.All()
		}(i)
	}
	wg.Wait()

	// Phase 3: Assertions
	allData := data.All()

	// Pods 0-4 may or may not exist (concurrent delete + store race)
	// Pods 5-9 should exist with containers
	for i := 5; i < numPods; i++ {
		podName := fmt.Sprintf("pod%d", i)
		podKey := types.NamespacedName{Namespace: namespace, Name: podName}
		containers, found := allData[podKey]
		assert.True(t, found, "pod%d should exist", i)
		assert.NotEmpty(t, containers, "pod%d should have containers", i)
	}

	// Total data should not be empty
	assert.NotEmpty(t, allData, "overall data should not be empty")
}
