package vm_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/agoda-com/macOS-vz-kubelet/internal/data/vm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func TestLoad(t *testing.T) {
	d := vm.NewVirtualMachineData()
	namespace := "default"
	podName := "pod1"
	info := vm.VirtualMachineInfo{Ref: "vm1"}

	// Initially, the info should not be found
	_, found := d.Load(namespace, podName)
	assert.False(t, found)

	// Set the info and then retrieve it
	_, loaded := d.LoadOrStore(namespace, podName, info)
	assert.False(t, loaded)
	retrievedInfo, found := d.Load(namespace, podName)
	assert.True(t, found)
	assert.Equal(t, info, retrievedInfo)
}

func TestUpdate(t *testing.T) {
	d := vm.NewVirtualMachineData()
	namespace := "default"
	podName := "pod1"
	initialInfo := vm.VirtualMachineInfo{Ref: "vm1"}
	updatedInfo := vm.VirtualMachineInfo{Ref: "vm2"}

	// Initially, the info should not be found
	_, found := d.Update(namespace, podName, func(info vm.VirtualMachineInfo) vm.VirtualMachineInfo {
		return updatedInfo
	})
	assert.False(t, found)

	// Set the info, update it, and then retrieve the updated info
	_, loaded := d.LoadOrStore(namespace, podName, initialInfo)
	assert.False(t, loaded)
	retrievedInfo, found := d.Update(namespace, podName, func(info vm.VirtualMachineInfo) vm.VirtualMachineInfo {
		return updatedInfo
	})
	assert.True(t, found)
	assert.Equal(t, updatedInfo, retrievedInfo)
}

func TestLoadOrStore(t *testing.T) {
	d := vm.NewVirtualMachineData()
	namespace := "default"
	podName := "pod1"
	info := vm.VirtualMachineInfo{Ref: "vm1"}

	// Initially, the info should be created
	retrievedInfo, found := d.LoadOrStore(namespace, podName, info)
	assert.False(t, found)
	assert.Equal(t, info, retrievedInfo)

	// The info should now exist
	retrievedInfo, found = d.LoadOrStore(namespace, podName, vm.VirtualMachineInfo{Ref: "vm2"})
	assert.True(t, found)
	assert.Equal(t, info, retrievedInfo)
}

func TestDelete(t *testing.T) {
	d := vm.NewVirtualMachineData()
	namespace := "default"
	podName := "pod1"
	info := vm.VirtualMachineInfo{Ref: "vm1"}

	_, loaded := d.LoadOrStore(namespace, podName, info)
	assert.False(t, loaded)
	d.Delete(namespace, podName)
	_, found := d.Load(namespace, podName)
	assert.False(t, found)
}

func TestAll(t *testing.T) {
	d := vm.NewVirtualMachineData()
	vm1 := vm.VirtualMachineInfo{Ref: "vm1"}
	vm2 := vm.VirtualMachineInfo{Ref: "vm2"}

	_, loaded := d.LoadOrStore("default", "pod1", vm1)
	assert.False(t, loaded)
	_, loaded = d.LoadOrStore("default", "pod2", vm2)
	assert.False(t, loaded)

	vms := d.All()
	require.Len(t, vms, 2)
	assert.Equal(t, vm1, vms[types.NamespacedName{Namespace: "default", Name: "pod1"}])
	assert.Equal(t, vm2, vms[types.NamespacedName{Namespace: "default", Name: "pod2"}])
}

func TestConcurrentAccess(t *testing.T) {
	d := vm.NewVirtualMachineData()
	var wg sync.WaitGroup
	namespace := "default"
	numRoutines := 100

	// Create and start multiple goroutines to test concurrent access
	for i := range numRoutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			podName := fmt.Sprintf("pod%d", i)
			info := vm.VirtualMachineInfo{Ref: fmt.Sprintf("vm%d", i)}
			_, loaded := d.LoadOrStore(namespace, podName, info)
			assert.False(t, loaded)
		}(i)
	}

	wg.Wait()

	// Verify each virtual machine info was set correctly
	for i := range numRoutines {
		podName := fmt.Sprintf("pod%d", i)
		info, found := d.Load(namespace, podName)
		assert.True(t, found)
		assert.Equal(t, fmt.Sprintf("vm%d", i), info.Ref)
	}
}
