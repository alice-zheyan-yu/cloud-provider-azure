/*
Copyright 2022 The Kubernetes Authors.

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

package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/compute/mgmt/2022-03-01/compute"

	cloudprovider "k8s.io/cloud-provider"
	"k8s.io/klog/v2"

	azcache "sigs.k8s.io/cloud-provider-azure/pkg/cache"
	"sigs.k8s.io/cloud-provider-azure/pkg/consts"
)

func (fs *FlexScaleSet) newVmssFlexCache() (*azcache.TimedCache, error) {

	getter := func(key string) (interface{}, error) {
		localCache := &sync.Map{}

		allResourceGroups, err := fs.GetResourceGroups()
		if err != nil {
			return nil, err
		}

		for _, resourceGroup := range allResourceGroups.List() {
			allScaleSets, rerr := fs.VirtualMachineScaleSetsClient.List(context.Background(), resourceGroup)
			if rerr != nil {
				if rerr.IsNotFound() {
					klog.Warningf("Skip caching vmss for resource group %s due to error: %v", resourceGroup, rerr.Error())
					continue
				}
				klog.Errorf("VirtualMachineScaleSetsClient.List failed: %v", rerr)
				return nil, rerr.Error()
			}

			for i := range allScaleSets {
				scaleSet := allScaleSets[i]
				if scaleSet.ID == nil || *scaleSet.ID == "" {
					klog.Warning("failed to get the ID of VMSS Flex")
					continue
				}

				if scaleSet.OrchestrationMode == compute.Flexible {
					localCache.Store(*scaleSet.ID, &scaleSet)
				}
			}
		}

		return localCache, nil
	}

	if fs.Config.VmssFlexCacheTTLInSeconds == 0 {
		fs.Config.VmssFlexCacheTTLInSeconds = consts.VmssFlexCacheTTLDefaultInSeconds
	}
	return azcache.NewTimedcache(time.Duration(fs.Config.VmssFlexCacheTTLInSeconds)*time.Second, getter)
}

func (fs *FlexScaleSet) newVmssFlexVMCache() (*azcache.TimedCache, error) {
	getter := func(key string) (interface{}, error) {
		localCache := &sync.Map{}

		ctx, cancel := getContextWithCancel()
		defer cancel()

		vms, rerr := fs.VirtualMachinesClient.ListVmssFlexVMsWithoutInstanceView(ctx, key)
		if rerr != nil {
			klog.Errorf("ListVmssFlexVMsWithoutInstanceView failed: %v", rerr)
			return nil, rerr.Error()
		}

		for i := range vms {
			vm := vms[i]
			if vm.OsProfile != nil && vm.OsProfile.ComputerName != nil {
				localCache.Store(strings.ToLower(*vm.OsProfile.ComputerName), &vm)
				fs.vmssFlexVMNameToVmssID.Store(strings.ToLower(*vm.OsProfile.ComputerName), key)
				fs.vmssFlexVMNameToNodeName.Store(*vm.Name, strings.ToLower(*vm.OsProfile.ComputerName))
			}
		}

		vms, rerr = fs.VirtualMachinesClient.ListVmssFlexVMsWithOnlyInstanceView(ctx, key)
		if rerr != nil {
			klog.Errorf("ListVmssFlexVMsWithOnlyInstanceView failed: %v", rerr)
			return nil, rerr.Error()
		}

		for i := range vms {
			vm := vms[i]
			if vm.Name != nil {
				nodeName, ok := fs.vmssFlexVMNameToNodeName.Load(*vm.Name)
				if !ok {
					continue
				}

				cached, ok := localCache.Load(nodeName)
				if ok {
					cachedVM := cached.(*compute.VirtualMachine)
					cachedVM.VirtualMachineProperties.InstanceView = vm.VirtualMachineProperties.InstanceView
				}
			}
		}

		return localCache, nil
	}

	if fs.Config.VmssFlexVMCacheTTLInSeconds == 0 {
		fs.Config.VmssFlexVMCacheTTLInSeconds = consts.VmssFlexVMCacheTTLDefaultInSeconds
	}
	return azcache.NewTimedcache(time.Duration(fs.Config.VmssFlexVMCacheTTLInSeconds)*time.Second, getter)
}

func (fs *FlexScaleSet) getNodeNameByVMName(vmName string) (string, error) {
	fs.lockMap.LockEntry(consts.GetNodeVmssFlexIDLockKey)
	defer fs.lockMap.UnlockEntry(consts.GetNodeVmssFlexIDLockKey)
	cachedNodeName, isCached := fs.vmssFlexVMNameToNodeName.Load(vmName)
	if isCached {
		return fmt.Sprintf("%v", cachedNodeName), nil
	}

	getter := func(vmName string, crt azcache.AzureCacheReadType) (string, error) {
		cached, err := fs.vmssFlexCache.Get(consts.VmssFlexKey, crt)
		if err != nil {
			return "", err
		}
		vmssFlexes := cached.(*sync.Map)

		vmssFlexes.Range(func(key, value interface{}) bool {
			vmssFlexID := key.(string)
			_, err := fs.vmssFlexVMCache.Get(vmssFlexID, azcache.CacheReadTypeForceRefresh)
			if err != nil {
				klog.Errorf("failed to refresh vmss flex VM cache for vmssFlexID %s", vmssFlexID)
			}
			return true
		})

		cachedNodeName, isCached = fs.vmssFlexVMNameToNodeName.Load(vmName)
		if isCached {
			return fmt.Sprintf("%v", cachedNodeName), nil
		}
		return "", cloudprovider.InstanceNotFound
	}

	nodeName, err := getter(vmName, azcache.CacheReadTypeDefault)
	if err == cloudprovider.InstanceNotFound {
		klog.V(2).Infof("Could not find node (%s) in the existing cache. Forcely freshing the cache to check again...", nodeName)
		return getter(vmName, azcache.CacheReadTypeForceRefresh)
	}
	return nodeName, err

}

func (fs *FlexScaleSet) getNodeVmssFlexID(nodeName string) (string, error) {
	fs.lockMap.LockEntry(consts.GetNodeVmssFlexIDLockKey)
	defer fs.lockMap.UnlockEntry(consts.GetNodeVmssFlexIDLockKey)
	cachedVmssFlexID, isCached := fs.vmssFlexVMNameToVmssID.Load(nodeName)

	if isCached {
		return fmt.Sprintf("%v", cachedVmssFlexID), nil
	}

	getter := func(nodeName string, crt azcache.AzureCacheReadType) (string, error) {
		cached, err := fs.vmssFlexCache.Get(consts.VmssFlexKey, crt)
		if err != nil {
			return "", err
		}
		vmssFlexes := cached.(*sync.Map)

		vmssFlexes.Range(func(key, value interface{}) bool {
			vmssFlexID := key.(string)
			_, err := fs.vmssFlexVMCache.Get(vmssFlexID, azcache.CacheReadTypeForceRefresh)
			if err != nil {
				klog.Errorf("failed to refresh vmss flex VM cache for vmssFlexID %s", vmssFlexID)
			}
			return true
		})

		cachedVmssFlexID, isCached = fs.vmssFlexVMNameToVmssID.Load(nodeName)
		if isCached {
			return fmt.Sprintf("%v", cachedVmssFlexID), nil
		}
		return "", cloudprovider.InstanceNotFound
	}

	vmssFlexID, err := getter(nodeName, azcache.CacheReadTypeDefault)
	if err == cloudprovider.InstanceNotFound {
		klog.V(2).Infof("Could not find node (%s) in the existing cache. Forcely freshing the cache to check again...", nodeName)
		return getter(nodeName, azcache.CacheReadTypeForceRefresh)
	}
	return vmssFlexID, err

}

func (fs *FlexScaleSet) getVmssFlexVM(nodeName string, crt azcache.AzureCacheReadType) (vm compute.VirtualMachine, err error) {
	vmssFlexID, err := fs.getNodeVmssFlexID(nodeName)
	if err != nil {
		return vm, err
	}

	cached, err := fs.vmssFlexVMCache.Get(vmssFlexID, crt)
	if err != nil {
		return vm, err
	}
	vmMap := cached.(*sync.Map)
	cachvmedVM, ok := vmMap.Load(nodeName)
	if !ok {
		klog.V(2).Infof("did not find node (%s) in the existing cache, which means it is deleted...", nodeName)
		return vm, cloudprovider.InstanceNotFound
	}

	return *(cachvmedVM.(*compute.VirtualMachine)), nil
}

func (fs *FlexScaleSet) getVmssFlexByVmssFlexID(vmssFlexID string, crt azcache.AzureCacheReadType) (*compute.VirtualMachineScaleSet, error) {
	cached, err := fs.vmssFlexCache.Get(consts.VmssFlexKey, crt)
	if err != nil {
		return nil, err
	}
	vmssFlexes := cached.(*sync.Map)
	if vmssFlex, ok := vmssFlexes.Load(vmssFlexID); ok {
		result := vmssFlex.(*compute.VirtualMachineScaleSet)
		return result, nil
	}

	klog.V(2).Infof("Couldn't find VMSS Flex with ID %s, refreshing the cache", vmssFlexID)
	cached, err = fs.vmssFlexCache.Get(consts.VmssFlexKey, azcache.CacheReadTypeForceRefresh)
	if err != nil {
		return nil, err
	}
	vmssFlexes = cached.(*sync.Map)
	if vmssFlex, ok := vmssFlexes.Load(vmssFlexID); ok {
		result := vmssFlex.(*compute.VirtualMachineScaleSet)
		return result, nil
	}
	return nil, cloudprovider.InstanceNotFound
}

func (fs *FlexScaleSet) getVmssFlexByNodeName(nodeName string, crt azcache.AzureCacheReadType) (*compute.VirtualMachineScaleSet, error) {
	vmssFlexID, err := fs.getNodeVmssFlexID(nodeName)
	if err != nil {
		return nil, err
	}
	vmssFlex, err := fs.getVmssFlexByVmssFlexID(vmssFlexID, crt)
	if err != nil {
		return nil, err
	}
	return vmssFlex, nil
}

func (fs *FlexScaleSet) getVmssFlexIDByName(vmssFlexName string) (string, error) {
	cached, err := fs.vmssFlexCache.Get(consts.VmssFlexKey, azcache.CacheReadTypeDefault)
	if err != nil {
		return "", err
	}
	var targetVmssFlexID string
	vmssFlexes := cached.(*sync.Map)
	vmssFlexes.Range(func(key, value interface{}) bool {
		vmssFlexID := key.(string)
		name, err := getLastSegment(vmssFlexID, "/")
		if err != nil {
			return true
		}
		if strings.EqualFold(name, vmssFlexName) {
			targetVmssFlexID = vmssFlexID
			return false
		}
		return true
	})
	if targetVmssFlexID != "" {
		return targetVmssFlexID, nil
	}
	return "", cloudprovider.InstanceNotFound
}

func (fs *FlexScaleSet) getVmssFlexByName(vmssFlexName string) (*compute.VirtualMachineScaleSet, error) {
	cached, err := fs.vmssFlexCache.Get(consts.VmssFlexKey, azcache.CacheReadTypeDefault)
	if err != nil {
		return nil, err
	}

	var targetVmssFlex *compute.VirtualMachineScaleSet
	vmssFlexes := cached.(*sync.Map)
	vmssFlexes.Range(func(key, value interface{}) bool {
		vmssFlexID := key.(string)
		vmssFlex := value.(*compute.VirtualMachineScaleSet)
		name, err := getLastSegment(vmssFlexID, "/")
		if err != nil {
			return true
		}
		if strings.EqualFold(name, vmssFlexName) {
			targetVmssFlex = vmssFlex
			return false
		}
		return true
	})
	if targetVmssFlex != nil {
		return targetVmssFlex, nil
	}
	return nil, cloudprovider.InstanceNotFound
}

func (fs *FlexScaleSet) DeleteCacheForNode(nodeName string) error {
	vmssFlexID, err := fs.getNodeVmssFlexID(nodeName)
	if err != nil {
		return err
	}

	cached, err := fs.vmssFlexVMCache.Get(vmssFlexID, azcache.CacheReadTypeDefault)
	if err != nil {
		return err
	}
	vmMap := cached.(*sync.Map)
	vmMap.Delete(nodeName)

	fs.vmssFlexVMNameToVmssID.Delete(nodeName)

	return nil
}
