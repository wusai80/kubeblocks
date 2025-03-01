/*
Copyright (C) 2022-2024 ApeCloud Co., Ltd

This file is part of KubeBlocks project

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package rsm

import (
	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	workloads "github.com/apecloud/kubeblocks/apis/workloads/v1alpha1"
	"github.com/apecloud/kubeblocks/pkg/controller/graph"
	"github.com/apecloud/kubeblocks/pkg/controller/model"
	intctrlutil "github.com/apecloud/kubeblocks/pkg/controllerutil"
)

type UpdateStrategyTransformer struct{}

var _ graph.Transformer = &UpdateStrategyTransformer{}

func (t *UpdateStrategyTransformer) Transform(ctx graph.TransformContext, dag *graph.DAG) error {
	transCtx, _ := ctx.(*rsmTransformContext)
	rsm := transCtx.rsm
	rsmOrig := transCtx.rsmOrig
	if !model.IsObjectStatusUpdating(rsmOrig) {
		return nil
	}

	var pods []corev1.Pod

	if rsm.Spec.RsmTransformPolicy == workloads.ToPod {
		podList := &corev1.PodList{}
		ml := GetPodsLabels(rsm.Labels)
		if err := transCtx.Client.List(transCtx, podList, client.InNamespace(rsm.Namespace), ml); err != nil {
			return err
		}
		pods = podList.Items
	} else {
		var err error
		// read the underlying sts
		stsObj := &apps.StatefulSet{}
		if err = transCtx.Client.Get(transCtx.Context, client.ObjectKeyFromObject(rsm), stsObj); err != nil {
			return err
		}
		// read all pods belong to the sts, hence belong to the rsm
		pods, err = getPodsOfStatefulSet(transCtx.Context, transCtx.Client, stsObj)
		if err != nil {
			return err
		}

		// prepare to do pods Deletion, that's the only thing we should do,
		// the stateful_set reconciler will do the others.
		// to simplify the process, we do pods Deletion after stateful_set reconcile done,
		// that is stsObj.Generation == stsObj.Status.ObservedGeneration
		if stsObj.Generation != stsObj.Status.ObservedGeneration {
			return nil
		}

		// then we wait all pods' presence, that is len(pods) == stsObj.Spec.Replicas
		// only then, we have enough info about the previous pods before delete the current one
		if len(pods) != int(*stsObj.Spec.Replicas) {
			return nil
		}
	}

	// we don't check whether pod role label present: prefer stateful_set's Update done than role probing ready
	// TODO(free6om): maybe should wait rsm ready for high availability:
	// 1. after some pods updated
	// 2. before switchover
	// 3. after switchover done

	// generate the pods Deletion plan
	plan := newUpdatePlan(*rsm, pods)
	podsToBeUpdated, err := plan.execute()
	if err != nil {
		return err
	}

	// do switchover if leader in pods to be updated
	switch shouldWaitNextLoop, err := doSwitchoverIfNeeded(transCtx, dag, pods, podsToBeUpdated); {
	case err != nil:
		return err
	case shouldWaitNextLoop:
		return nil
	}

	graphCli, _ := transCtx.Client.(model.GraphClient)
	for _, pod := range podsToBeUpdated {
		graphCli.Delete(dag, pod)
	}

	return nil
}

// return true means action created or in progress, should wait it to the termination state
func doSwitchoverIfNeeded(transCtx *rsmTransformContext, dag *graph.DAG, pods []corev1.Pod, podsToBeUpdated []*corev1.Pod) (bool, error) {
	if len(podsToBeUpdated) == 0 {
		return false, nil
	}

	rsm := transCtx.rsm
	if !shouldSwitchover(rsm, podsToBeUpdated, pods) {
		return false, nil
	}

	graphCli, _ := transCtx.Client.(model.GraphClient)
	actionList, err := getActionList(transCtx, jobScenarioUpdate)
	if err != nil {
		return true, err
	}
	if len(actionList) == 0 {
		return true, createSwitchoverAction(dag, graphCli, rsm, pods)
	}

	// switch status if found:
	// 1. succeed means action executed successfully,
	//    but some kind of cluster may have false positive(apecloud-mysql only?),
	//    we can't wait forever, update is more important.
	//    do the next pod update stage
	// 2. failed means action executed failed,
	//    but this doesn't mean the cluster didn't switchover(again, apecloud-mysql only?)
	//    we can't do anything either in this situation, emit failed event and
	//    do the next pod update state
	// 3. in progress means action still running,
	//    return and wait it reaches termination state.
	action := actionList[0]
	switch {
	case action.Status.Succeeded == 0 && action.Status.Failed == 0:
		// action in progress, wait
		return true, nil
	case action.Status.Failed > 0:
		emitActionFailedEvent(transCtx, jobTypeSwitchover, action.Name)
		fallthrough
	case action.Status.Succeeded > 0:
		// clean up the action
		doActionCleanup(dag, graphCli, action)
	}
	return false, nil
}

func createSwitchoverAction(dag *graph.DAG, cli model.GraphClient, rsm *workloads.ReplicatedStateMachine, pods []corev1.Pod) error {
	leader := getLeaderPodName(rsm.Status.MembersStatus)
	targetOrdinal := selectSwitchoverTarget(rsm, pods)
	target := getPodName(rsm.Name, targetOrdinal)
	actionType := jobTypeSwitchover
	ordinal, _ := getPodOrdinal(leader)
	actionName := getActionName(rsm.Name, int(rsm.Generation), ordinal, actionType)
	action := buildAction(rsm, actionName, actionType, jobScenarioUpdate, leader, target)

	// don't do cluster abnormal status analysis, prefer faster update process
	return createAction(dag, cli, rsm, action)
}

func selectSwitchoverTarget(rsm *workloads.ReplicatedStateMachine, pods []corev1.Pod) int {
	var podUpdated, podUpdatedWithLabel string
	for _, pod := range pods {
		if intctrlutil.GetPodRevision(&pod) != rsm.Status.UpdateRevision {
			continue
		}
		if len(podUpdated) == 0 {
			podUpdated = pod.Name
		}
		if _, ok := pod.Labels[roleLabelKey]; !ok {
			continue
		}
		if len(podUpdatedWithLabel) == 0 {
			podUpdatedWithLabel = pod.Name
			break
		}
	}
	var finalPod string
	switch {
	case len(podUpdatedWithLabel) > 0:
		finalPod = podUpdatedWithLabel
	case len(podUpdated) > 0:
		finalPod = podUpdated
	default:
		finalPod = pods[0].Name
	}
	ordinal, _ := getPodOrdinal(finalPod)
	return ordinal
}

func shouldSwitchover(rsm *workloads.ReplicatedStateMachine, podsToBeUpdated []*corev1.Pod, allPods []corev1.Pod) bool {
	if len(allPods) < 2 {
		// replicas is less than 2, no need to switchover
		return false
	}
	reconfiguration := rsm.Spec.MembershipReconfiguration
	if reconfiguration == nil {
		return false
	}
	if reconfiguration.SwitchoverAction == nil {
		return false
	}
	leaderName := getLeaderPodName(rsm.Status.MembersStatus)
	for _, pod := range podsToBeUpdated {
		if pod.Name == leaderName {
			return true
		}
	}
	return false
}
