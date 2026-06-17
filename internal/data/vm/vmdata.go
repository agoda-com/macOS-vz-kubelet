package vm

import (
	"github.com/puzpuzpuz/xsync/v4"
	"k8s.io/apimachinery/pkg/types"
)

// VirtualMachineData stores the information about macOS virtual machines.
// A VirtualMachineData must be created with NewVirtualMachineData; the zero value is not usable.
type VirtualMachineData struct {
	data *xsync.Map[types.NamespacedName, VirtualMachineInfo]
}

// NewVirtualMachineData creates a new VirtualMachineData instance.
func NewVirtualMachineData() VirtualMachineData {
	return VirtualMachineData{
		data: xsync.NewMap[types.NamespacedName, VirtualMachineInfo](),
	}
}

// Load retrieves the VirtualMachineInfo for a specific pod.
// It returns the VirtualMachineInfo and a boolean indicating whether the virtual machine information was found.
func (d *VirtualMachineData) Load(podNamespace, podName string) (VirtualMachineInfo, bool) {
	key := types.NamespacedName{Namespace: podNamespace, Name: podName}
	return d.data.Load(key)
}

// Update atomically updates the VirtualMachineInfo for a specific pod.
// It returns the updated VirtualMachineInfo and a boolean indicating whether the virtual machine information was found.
func (d *VirtualMachineData) Update(podNamespace, podName string, updateFunc func(VirtualMachineInfo) VirtualMachineInfo) (VirtualMachineInfo, bool) {
	key := types.NamespacedName{Namespace: podNamespace, Name: podName}
	newVal, ok := d.data.Compute(key, func(oldValue VirtualMachineInfo, loaded bool) (VirtualMachineInfo, xsync.ComputeOp) {
		if !loaded {
			return VirtualMachineInfo{}, xsync.CancelOp
		}
		return updateFunc(oldValue), xsync.UpdateOp
	})
	if !ok {
		return VirtualMachineInfo{}, false
	}
	return newVal, true
}

// LoadOrStore retrieves the VirtualMachineInfo for a specific pod,
// or creates and stores the provided VirtualMachineInfo if it doesn't already exist.
// It returns the VirtualMachineInfo and a boolean indicating whether the virtual machine information was already present.
func (d *VirtualMachineData) LoadOrStore(podNamespace, podName string, info VirtualMachineInfo) (VirtualMachineInfo, bool) {
	key := types.NamespacedName{Namespace: podNamespace, Name: podName}
	return d.data.LoadOrStore(key, info)
}

// Delete removes the VirtualMachineInfo for a specific pod.
func (d *VirtualMachineData) Delete(podNamespace, podName string) {
	key := types.NamespacedName{Namespace: podNamespace, Name: podName}
	d.data.Delete(key)
}

// All returns a map of all virtual machines stored.
func (d *VirtualMachineData) All() map[types.NamespacedName]VirtualMachineInfo {
	vmMap := make(map[types.NamespacedName]VirtualMachineInfo)
	d.data.Range(func(key types.NamespacedName, value VirtualMachineInfo) bool {
		vmMap[key] = value
		return true
	})
	return vmMap
}
