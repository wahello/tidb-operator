// Copyright 2018 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package member

import (
	"fmt"
	"strconv"
	"time"

	"github.com/pingcap/advanced-statefulset/client/apis/apps/v1/helper"
	"github.com/pingcap/tidb-operator/pkg/apis/pingcap/v1alpha1"
	"github.com/pingcap/tidb-operator/pkg/controller"
	"github.com/pingcap/tidb-operator/pkg/label"
	"github.com/pingcap/tidb-operator/pkg/util"
	apps "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
)

type tikvScaler struct {
	generalScaler
}

// NewTiKVScaler returns a tikv Scaler
func NewTiKVScaler(deps *controller.Dependencies) Scaler {
	return &tikvScaler{generalScaler: generalScaler{deps: deps}}
}

func (s *tikvScaler) Scale(tc *v1alpha1.TidbCluster, oldSet *apps.StatefulSet, newSet *apps.StatefulSet) error {
	scaling, _, _, _ := scaleOne(oldSet, newSet)
	if scaling > 0 {
		return s.ScaleOut(tc, oldSet, newSet)
	} else if scaling < 0 {
		return s.ScaleIn(tc, oldSet, newSet)
	}
	// we only sync auto scaler annotations when we are finishing syncing scaling
	return s.SyncAutoScalerAnn(tc, oldSet)
}

func (s *tikvScaler) ScaleOut(tc *v1alpha1.TidbCluster, oldSet *apps.StatefulSet, newSet *apps.StatefulSet) error {
	_, ordinal, replicas, deleteSlots := scaleOne(oldSet, newSet)
	resetReplicas(newSet, oldSet)

	klog.Infof("scaling out tikv statefulset %s/%s, ordinal: %d (replicas: %d, delete slots: %v)", oldSet.Namespace, oldSet.Name, ordinal, replicas, deleteSlots.List())
	_, err := s.deleteDeferDeletingPVC(tc, oldSet.GetName(), v1alpha1.TiKVMemberType, ordinal)
	if err != nil {
		return err
	}

	setReplicasAndDeleteSlots(newSet, replicas, deleteSlots)
	return nil
}

func (s *tikvScaler) ScaleIn(tc *v1alpha1.TidbCluster, oldSet *apps.StatefulSet, newSet *apps.StatefulSet) error {
	ns := tc.GetNamespace()
	tcName := tc.GetName()
	// we can only remove one member at a time when scaling in
	_, ordinal, replicas, deleteSlots := scaleOne(oldSet, newSet)
	resetReplicas(newSet, oldSet)
	setName := oldSet.GetName()

	klog.Infof("scaling in tikv statefulset %s/%s, ordinal: %d (replicas: %d, delete slots: %v)", oldSet.Namespace, oldSet.Name, ordinal, replicas, deleteSlots.List())
	// We need remove member from cluster before reducing statefulset replicas
	podName := ordinalPodName(v1alpha1.TiKVMemberType, tcName, ordinal)
	pod, err := s.deps.PodLister.Pods(ns).Get(podName)
	if err != nil {
		return fmt.Errorf("tikvScaler.ScaleIn: failed to get pods %s for cluster %s/%s, error: %s", podName, ns, tcName, err)
	}

	if pass, err := s.preCheckUpStores(tc, podName); !pass {
		return err
	}

	if s.deps.CLIConfig.PodWebhookEnabled {
		setReplicasAndDeleteSlots(newSet, replicas, deleteSlots)
		return nil
	}

	for _, store := range tc.Status.TiKV.Stores {
		if store.PodName == podName {
			state := store.State
			id, err := strconv.ParseUint(store.ID, 10, 64)
			if err != nil {
				return err
			}
			if state != v1alpha1.TiKVStateOffline {
				if err := controller.GetPDClient(s.deps.PDControl, tc).DeleteStore(id); err != nil {
					klog.Errorf("tikv scale in: failed to delete store %d, %v", id, err)
					return err
				}
				klog.Infof("tikv scale in: delete store %d for tikv %s/%s successfully", id, ns, podName)
			}
			return controller.RequeueErrorf("TiKV %s/%s store %d is still in cluster, state: %s", ns, podName, id, state)
		}
	}
	for id, store := range tc.Status.TiKV.TombstoneStores {
		if store.PodName == podName && pod.Labels[label.StoreIDLabelKey] == id {
			id, err := strconv.ParseUint(store.ID, 10, 64)
			if err != nil {
				return err
			}

			// TODO: double check if store is really not in Up/Offline/Down state
			klog.Infof("TiKV %s/%s store %d becomes tombstone", ns, podName, id)

			pvcName := ordinalPVCName(v1alpha1.TiKVMemberType, setName, ordinal)
			pvc, err := s.deps.PVCLister.PersistentVolumeClaims(ns).Get(pvcName)
			if err != nil {
				return fmt.Errorf("tikvScaler.ScaleIn: failed to get pvc %s for cluster %s/%s, error: %s", pvcName, ns, tcName, err)
			}
			if pvc.Annotations == nil {
				pvc.Annotations = map[string]string{}
			}
			now := time.Now().Format(time.RFC3339)
			pvc.Annotations[label.AnnPVCDeferDeleting] = now
			_, err = s.deps.PVCControl.UpdatePVC(tc, pvc)
			if err != nil {
				klog.Errorf("tikv scale in: failed to set pvc %s/%s annotation: %s to %s",
					ns, pvcName, label.AnnPVCDeferDeleting, now)
				return err
			}
			klog.Infof("tikv scale in: set pvc %s/%s annotation: %s to %s",
				ns, pvcName, label.AnnPVCDeferDeleting, now)

			// endEvictLeader for TombStone stores
			if err = endEvictLeaderbyStoreID(s.deps, tc, id); err != nil {
				return err
			}

			setReplicasAndDeleteSlots(newSet, replicas, deleteSlots)
			return nil
		}
	}

	// When store not found in TidbCluster status, there are two situations as follows:
	// 1. This can happen when TiKV joins cluster but we haven't synced its status.
	//    In this situation return error to wait another round for safety.
	//
	// 2. This can happen when TiKV pod has not been successfully registered in the cluster, such as always pending.
	//    In this situation we should delete this TiKV pod immediately to avoid blocking the subsequent operations.
	if !podutil.IsPodReady(pod) {
		pvcName := ordinalPVCName(v1alpha1.TiKVMemberType, setName, ordinal)
		pvc, err := s.deps.PVCLister.PersistentVolumeClaims(ns).Get(pvcName)
		if err != nil {
			return fmt.Errorf("tikvScaler.ScaleIn: failed to get pvc %s for cluster %s/%s, error: %s", pvcName, ns, tcName, err)
		}
		if tc.TiKVBootStrapped() {
			safeTimeDeadline := pod.CreationTimestamp.Add(5 * s.deps.CLIConfig.ResyncDuration)
			if time.Now().Before(safeTimeDeadline) {
				// Wait for 5 resync periods to ensure that the following situation does not occur:
				//
				// The tikv pod starts for a while, but has not synced its status, and then the pod becomes not ready.
				// Here we wait for 5 resync periods to ensure that the status of this tikv pod has been synced.
				// After this period of time, if there is still no information about this tikv in TidbCluster status,
				// then we can be sure that this tikv has never been added to the tidb cluster.
				// So we can scale in this tikv pod safely.
				resetReplicas(newSet, oldSet)
				return fmt.Errorf("TiKV %s/%s is not ready, wait for some resync periods to synced its status", ns, podName)
			}
		}
		if pvc.Annotations == nil {
			pvc.Annotations = map[string]string{}
		}
		now := time.Now().Format(time.RFC3339)
		pvc.Annotations[label.AnnPVCDeferDeleting] = now
		_, err = s.deps.PVCControl.UpdatePVC(tc, pvc)
		if err != nil {
			klog.Errorf("pod %s not ready, tikv scale in: failed to set pvc %s/%s annotation: %s to %s",
				podName, ns, pvcName, label.AnnPVCDeferDeleting, now)
			return err
		}
		klog.Infof("pod %s not ready, tikv scale in: set pvc %s/%s annotation: %s to %s",
			podName, ns, pvcName, label.AnnPVCDeferDeleting, now)
		setReplicasAndDeleteSlots(newSet, replicas, deleteSlots)
		return nil
	}
	return fmt.Errorf("TiKV %s/%s not found in cluster", ns, podName)
}

// SyncAutoScalerAnn would reclaim the auto-scaling out slots if the target pod is no longer existed
func (s *tikvScaler) SyncAutoScalerAnn(tc *v1alpha1.TidbCluster, actual *apps.StatefulSet) error {
	currentScalingSlots := util.GetAutoScalingOutSlots(tc, v1alpha1.TiKVMemberType)
	if currentScalingSlots.Len() < 1 {
		return nil
	}
	currentOrdinals := helper.GetPodOrdinals(tc.Spec.TiKV.Replicas, actual)

	// reclaim the auto-scaling out slots if the target pod is no longer existed
	if !currentOrdinals.HasAll(currentScalingSlots.List()...) {
		reclaimedSlots := currentScalingSlots.Difference(currentOrdinals)
		currentScalingSlots = currentScalingSlots.Delete(reclaimedSlots.List()...)
		if currentScalingSlots.Len() < 1 {
			delete(tc.Annotations, label.AnnTiKVAutoScalingOutOrdinals)
			return nil
		}
		v, err := util.Encode(currentScalingSlots.List())
		if err != nil {
			return err
		}
		tc.Annotations[label.AnnTiKVAutoScalingOutOrdinals] = v
		return nil
	}
	return nil
}

func (s *tikvScaler) preCheckUpStores(tc *v1alpha1.TidbCluster, podName string) (bool, error) {
	if !tc.TiKVBootStrapped() {
		klog.Infof("TiKV of Cluster %s/%s is not bootstrapped yet, skip pre check when scale in TiKV", tc.Namespace, tc.Name)
		return true, nil
	}

	pdClient := controller.GetPDClient(s.deps.PDControl, tc)
	// get the number of stores whose state is up
	upNumber := 0

	storesInfo, err := pdClient.GetStores()
	if err != nil {
		return false, fmt.Errorf("failed to get stores info in TidbCluster %s/%s", tc.GetNamespace(), tc.GetName())
	}
	// filter out TiFlash
	for _, store := range storesInfo.Stores {
		if store.Store != nil {
			if store.Store.StateName == v1alpha1.TiKVStateUp && util.MatchLabelFromStoreLabels(store.Store.Labels, label.TiKVLabelVal) {
				upNumber++
			}
		}
	}

	// get the state of the store which is about to be scaled in
	storeState := ""
	for _, store := range tc.Status.TiKV.Stores {
		if store.PodName == podName {
			storeState = store.State
		}
	}

	config, err := pdClient.GetConfig()
	if err != nil {
		return false, err
	}
	maxReplicas := *(config.Replication.MaxReplicas)
	if upNumber < int(maxReplicas) {
		errMsg := fmt.Sprintf("the number of stores in Up state of TidbCluster [%s/%s] is %d, less than MaxReplicas in PD configuration(%d), can't scale in TiKV, podname %s ", tc.GetNamespace(), tc.GetName(), upNumber, maxReplicas, podName)
		klog.Error(errMsg)
		s.deps.Recorder.Event(tc, v1.EventTypeWarning, "FailedScaleIn", errMsg)
		return false, nil
	} else if upNumber == int(maxReplicas) {
		if storeState == v1alpha1.TiKVStateUp {
			errMsg := fmt.Sprintf("can't scale in TiKV of TidbCluster [%s/%s], cause the number of up stores is equal to MaxReplicas in PD configuration(%d), and the store in Pod %s which is going to be deleted is up too", tc.GetNamespace(), tc.GetName(), maxReplicas, podName)
			klog.Error(errMsg)
			s.deps.Recorder.Event(tc, v1.EventTypeWarning, "FailedScaleIn", errMsg)
			return false, nil
		}
	}

	return true, nil
}

type fakeTiKVScaler struct{}

// NewFakeTiKVScaler returns a fake tikv Scaler
func NewFakeTiKVScaler() Scaler {
	return &fakeTiKVScaler{}
}

func (s *fakeTiKVScaler) Scale(tc *v1alpha1.TidbCluster, oldSet *apps.StatefulSet, newSet *apps.StatefulSet) error {
	if *newSet.Spec.Replicas > *oldSet.Spec.Replicas {
		return s.ScaleOut(tc, oldSet, newSet)
	} else if *newSet.Spec.Replicas < *oldSet.Spec.Replicas {
		return s.ScaleIn(tc, oldSet, newSet)
	}
	return nil
}

func (s *fakeTiKVScaler) ScaleOut(_ *v1alpha1.TidbCluster, oldSet *apps.StatefulSet, newSet *apps.StatefulSet) error {
	setReplicasAndDeleteSlots(newSet, *oldSet.Spec.Replicas+1, nil)
	return nil
}

func (s *fakeTiKVScaler) ScaleIn(_ *v1alpha1.TidbCluster, oldSet *apps.StatefulSet, newSet *apps.StatefulSet) error {
	setReplicasAndDeleteSlots(newSet, *oldSet.Spec.Replicas-1, nil)
	return nil
}

func (s *fakeTiKVScaler) SyncAutoScalerAnn(tc *v1alpha1.TidbCluster, actual *apps.StatefulSet) error {
	return nil
}
