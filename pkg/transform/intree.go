package transform

import (
	"bytes"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	api "k8s.io/api/core/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/duration"
	utilpointer "k8s.io/utils/pointer"
	"sigs.k8s.io/yaml"
)

func printPodList(podListRaw []byte) ([]metav1.TableRow, error) {
	podList := corev1.PodList{}
	if err := yaml.Unmarshal(podListRaw, &podList); err != nil {
		return nil, fmt.Errorf("failed to unmarshal into podlist: %w", err)
	}
	rows := make([]metav1.TableRow, 0, len(podList.Items))
	for i := range podList.Items {
		r, err := printPod(&podList.Items[i])
		if err != nil {
			return nil, err
		}
		rows = append(rows, r...)
	}
	return rows, nil
}

// This is passed throgh within the upstream code but there is no request option for this, so do the simple thing and always return it.
var options = TablePrinterOptions{Wide: true}

func printPodFromRaw(podRaw []byte) ([]metav1.TableRow, error) {
	pod := corev1.Pod{}
	if err := yaml.Unmarshal(podRaw, &pod); err != nil {
		return nil, err
	}
	return printPod(&pod)
}

func printPod(pod *corev1.Pod) ([]metav1.TableRow, error) {
	restarts := 0
	totalContainers := len(pod.Spec.Containers)
	readyContainers := 0
	lastRestartDate := metav1.NewTime(time.Time{})

	reason := string(pod.Status.Phase)
	if pod.Status.Reason != "" {
		reason = pod.Status.Reason
	}

	row := metav1.TableRow{
		Object: runtime.RawExtension{Object: pod},
	}

	switch pod.Status.Phase {
	case api.PodSucceeded:
		row.Conditions = podSuccessConditions
	case api.PodFailed:
		row.Conditions = podFailedConditions
	}

	initializing := false
	for i := range pod.Status.InitContainerStatuses {
		container := pod.Status.InitContainerStatuses[i]
		restarts += int(container.RestartCount)
		if container.LastTerminationState.Terminated != nil {
			terminatedDate := container.LastTerminationState.Terminated.FinishedAt
			if lastRestartDate.Before(&terminatedDate) {
				lastRestartDate = terminatedDate
			}
		}
		switch {
		case container.State.Terminated != nil && container.State.Terminated.ExitCode == 0:
			continue
		case container.State.Terminated != nil:
			// initialization is failed
			if len(container.State.Terminated.Reason) == 0 {
				if container.State.Terminated.Signal != 0 {
					reason = fmt.Sprintf("Init:Signal:%d", container.State.Terminated.Signal)
				} else {
					reason = fmt.Sprintf("Init:ExitCode:%d", container.State.Terminated.ExitCode)
				}
			} else {
				reason = "Init:" + container.State.Terminated.Reason
			}
			initializing = true
		case container.State.Waiting != nil && len(container.State.Waiting.Reason) > 0 && container.State.Waiting.Reason != "PodInitializing":
			reason = "Init:" + container.State.Waiting.Reason
			initializing = true
		default:
			reason = fmt.Sprintf("Init:%d/%d", i, len(pod.Spec.InitContainers))
			initializing = true
		}
		break
	}
	if !initializing {
		restarts = 0
		hasRunning := false
		for i := len(pod.Status.ContainerStatuses) - 1; i >= 0; i-- {
			container := pod.Status.ContainerStatuses[i]

			restarts += int(container.RestartCount)
			if container.LastTerminationState.Terminated != nil {
				terminatedDate := container.LastTerminationState.Terminated.FinishedAt
				if lastRestartDate.Before(&terminatedDate) {
					lastRestartDate = terminatedDate
				}
			}
			if container.State.Waiting != nil && container.State.Waiting.Reason != "" {
				reason = container.State.Waiting.Reason
			} else if container.State.Terminated != nil && container.State.Terminated.Reason != "" {
				reason = container.State.Terminated.Reason
			} else if container.State.Terminated != nil && container.State.Terminated.Reason == "" {
				if container.State.Terminated.Signal != 0 {
					reason = fmt.Sprintf("Signal:%d", container.State.Terminated.Signal)
				} else {
					reason = fmt.Sprintf("ExitCode:%d", container.State.Terminated.ExitCode)
				}
			} else if container.Ready && container.State.Running != nil {
				hasRunning = true
				readyContainers++
			}
		}

		// change pod status back to "Running" if there is at least one container still reporting as "Running" status
		if reason == "Completed" && hasRunning {
			if hasPodReadyCondition(pod.Status.Conditions) {
				reason = "Running"
			} else {
				reason = "NotReady"
			}
		}
	}

	if pod.DeletionTimestamp != nil && pod.Status.Reason == "NodeLost" {
		reason = "Unknown"
	} else if pod.DeletionTimestamp != nil {
		reason = "Terminating"
	}

	restartsStr := strconv.Itoa(restarts)
	if !lastRestartDate.IsZero() {
		restartsStr = fmt.Sprintf("%d (%s ago)", restarts, translateTimestampSince(lastRestartDate))
	}

	row.Cells = append(row.Cells, pod.Name, fmt.Sprintf("%d/%d", readyContainers, totalContainers), reason, restartsStr, translateTimestampSince(pod.CreationTimestamp))
	if options.Wide {
		nodeName := pod.Spec.NodeName
		nominatedNodeName := pod.Status.NominatedNodeName
		podIP := ""
		if len(pod.Status.PodIPs) > 0 {
			podIP = pod.Status.PodIPs[0].IP
		}

		if podIP == "" {
			podIP = "<none>"
		}
		if nodeName == "" {
			nodeName = "<none>"
		}
		if nominatedNodeName == "" {
			nominatedNodeName = "<none>"
		}

		readinessGates := "<none>"
		if len(pod.Spec.ReadinessGates) > 0 {
			trueConditions := 0
			for _, readinessGate := range pod.Spec.ReadinessGates {
				conditionType := readinessGate.ConditionType
				for _, condition := range pod.Status.Conditions {
					if condition.Type == conditionType {
						if condition.Status == api.ConditionTrue {
							trueConditions++
						}
						break
					}
				}
			}
			readinessGates = fmt.Sprintf("%d/%d", trueConditions, len(pod.Spec.ReadinessGates))
		}
		row.Cells = append(row.Cells, podIP, nodeName, nominatedNodeName, readinessGates)
	}

	return []metav1.TableRow{row}, nil
}

var (
	podSuccessConditions = []metav1.TableRowCondition{{Type: metav1.RowCompleted, Status: metav1.ConditionTrue, Reason: string(api.PodSucceeded), Message: "The pod has completed successfully."}}
	podFailedConditions  = []metav1.TableRowCondition{{Type: metav1.RowCompleted, Status: metav1.ConditionTrue, Reason: string(api.PodFailed), Message: "The pod failed."}}
)

func hasPodReadyCondition(conditions []api.PodCondition) bool {
	for _, condition := range conditions {
		if condition.Type == api.PodReady && condition.Status == api.ConditionTrue {
			return true
		}
	}
	return false
}

// translateTimestampSince returns the elapsed time since timestamp in
// human-readable approximation.
func translateTimestampSince(timestamp metav1.Time) string {
	if timestamp.IsZero() {
		return "<unknown>"
	}

	return duration.HumanDuration(time.Since(timestamp.Time))
}

type TablePrinterOptions struct {
	NoHeaders bool
	Wide      bool
}

func printDeploymentFromRaw(raw []byte) ([]metav1.TableRow, error) {
	var dep appsv1.Deployment
	if err := yaml.Unmarshal(raw, &dep); err != nil {
		return nil, err
	}
	return printDeployment(&dep)
}

func printDeployment(obj *appsv1.Deployment) ([]metav1.TableRow, error) {
	row := metav1.TableRow{
		Object: runtime.RawExtension{Object: obj},
	}
	desiredReplicas := utilpointer.Int32Deref(obj.Spec.Replicas, 0)
	updatedReplicas := obj.Status.UpdatedReplicas
	readyReplicas := obj.Status.ReadyReplicas
	availableReplicas := obj.Status.AvailableReplicas
	age := translateTimestampSince(obj.CreationTimestamp)
	containers := obj.Spec.Template.Spec.Containers
	selector, err := metav1.LabelSelectorAsSelector(obj.Spec.Selector)
	selectorString := ""
	if err != nil {
		selectorString = "<invalid>"
	} else {
		selectorString = selector.String()
	}
	row.Cells = append(row.Cells, obj.Name, fmt.Sprintf("%d/%d", int64(readyReplicas), int64(desiredReplicas)), int64(updatedReplicas), int64(availableReplicas), age)
	if options.Wide {
		containers, images := layoutContainerCells(containers)
		row.Cells = append(row.Cells, containers, images, selectorString)
	}
	return []metav1.TableRow{row}, nil
}

func printDeploymentList(listRaw []byte) ([]metav1.TableRow, error) {
	var list appsv1.DeploymentList
	if err := yaml.Unmarshal(listRaw, &list); err != nil {
		return nil, err
	}
	rows := make([]metav1.TableRow, 0, len(list.Items))
	for i := range list.Items {
		r, err := printDeployment(&list.Items[i])
		if err != nil {
			return nil, err
		}
		rows = append(rows, r...)
	}
	return rows, nil
}

// Lay out all the containers on one line if use wide output.
func layoutContainerCells(containers []api.Container) (names string, images string) {
	var namesBuffer bytes.Buffer
	var imagesBuffer bytes.Buffer

	for i, container := range containers {
		namesBuffer.WriteString(container.Name)
		imagesBuffer.WriteString(container.Image)
		if i != len(containers)-1 {
			namesBuffer.WriteString(",")
			imagesBuffer.WriteString(",")
		}
	}
	return namesBuffer.String(), imagesBuffer.String()
}

func printStatefulSetFromRaw(raw []byte) ([]metav1.TableRow, error) {
	var sst appsv1.StatefulSet
	if err := yaml.Unmarshal(raw, &sst); err != nil {
		return nil, err
	}
	return printStatefulSet(&sst)
}

func printStatefulSet(obj *appsv1.StatefulSet) ([]metav1.TableRow, error) {
	row := metav1.TableRow{
		Object: runtime.RawExtension{Object: obj},
	}
	desiredReplicas := utilpointer.Int32Deref(obj.Spec.Replicas, 0)
	readyReplicas := obj.Status.ReadyReplicas
	createTime := translateTimestampSince(obj.CreationTimestamp)
	row.Cells = append(row.Cells, obj.Name, fmt.Sprintf("%d/%d", int64(readyReplicas), int64(desiredReplicas)), createTime)
	if options.Wide {
		names, images := layoutContainerCells(obj.Spec.Template.Spec.Containers)
		row.Cells = append(row.Cells, names, images)
	}
	return []metav1.TableRow{row}, nil
}

func printStatefulSetList(listRaw []byte) ([]metav1.TableRow, error) {
	list := appsv1.StatefulSetList{}
	if err := yaml.Unmarshal(listRaw, &list); err != nil {
		return nil, err
	}
	rows := make([]metav1.TableRow, 0, len(list.Items))
	for i := range list.Items {
		r, err := printStatefulSet(&list.Items[i])
		if err != nil {
			return nil, err
		}
		rows = append(rows, r...)
	}
	return rows, nil
}

func printDaemonSetFromRaw(raw []byte) ([]metav1.TableRow, error) {
	ds := appsv1.DaemonSet{}
	if err := yaml.Unmarshal(raw, &ds); err != nil {
		return nil, err
	}
	return printDaemonSet(&ds)
}

func printDaemonSet(obj *appsv1.DaemonSet) ([]metav1.TableRow, error) {
	row := metav1.TableRow{
		Object: runtime.RawExtension{Object: obj},
	}

	desiredScheduled := obj.Status.DesiredNumberScheduled
	currentScheduled := obj.Status.CurrentNumberScheduled
	numberReady := obj.Status.NumberReady
	numberUpdated := obj.Status.UpdatedNumberScheduled
	numberAvailable := obj.Status.NumberAvailable

	row.Cells = append(row.Cells, obj.Name, int64(desiredScheduled), int64(currentScheduled), int64(numberReady), int64(numberUpdated), int64(numberAvailable), labels.FormatLabels(obj.Spec.Template.Spec.NodeSelector), translateTimestampSince(obj.CreationTimestamp))
	if options.Wide {
		names, images := layoutContainerCells(obj.Spec.Template.Spec.Containers)
		row.Cells = append(row.Cells, names, images, metav1.FormatLabelSelector(obj.Spec.Selector))
	}
	return []metav1.TableRow{row}, nil
}

func printDaemonSetListFromRaw(raw []byte) ([]metav1.TableRow, error) {
	list := appsv1.DaemonSetList{}
	if err := yaml.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	return printDaemonSetList(&list)
}

func printDaemonSetList(list *appsv1.DaemonSetList) ([]metav1.TableRow, error) {
	rows := make([]metav1.TableRow, 0, len(list.Items))
	for i := range list.Items {
		r, err := printDaemonSet(&list.Items[i])
		if err != nil {
			return nil, err
		}
		rows = append(rows, r...)
	}
	return rows, nil
}

func printReplicaSetFromRaw(raw []byte) ([]metav1.TableRow, error) {
	rs := appsv1.ReplicaSet{}
	if err := yaml.Unmarshal(raw, &rs); err != nil {
		return nil, err
	}
	return printReplicaSet(&rs)

}

func printReplicaSetListFromRaw(raw []byte) ([]metav1.TableRow, error) {
	list := appsv1.ReplicaSetList{}
	if err := yaml.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	return printReplicaSetList(&list)

}

func printReplicaSet(obj *appsv1.ReplicaSet) ([]metav1.TableRow, error) {
	row := metav1.TableRow{
		Object: runtime.RawExtension{Object: obj},
	}

	desiredReplicas := *obj.Spec.Replicas
	currentReplicas := obj.Status.Replicas
	readyReplicas := obj.Status.ReadyReplicas

	row.Cells = append(row.Cells, obj.Name, int64(desiredReplicas), int64(currentReplicas), int64(readyReplicas), translateTimestampSince(obj.CreationTimestamp))
	if options.Wide {
		names, images := layoutContainerCells(obj.Spec.Template.Spec.Containers)
		row.Cells = append(row.Cells, names, images, metav1.FormatLabelSelector(obj.Spec.Selector))
	}
	return []metav1.TableRow{row}, nil
}

func printReplicaSetList(list *appsv1.ReplicaSetList) ([]metav1.TableRow, error) {
	rows := make([]metav1.TableRow, 0, len(list.Items))
	for i := range list.Items {
		r, err := printReplicaSet(&list.Items[i])
		if err != nil {
			return nil, err
		}
		rows = append(rows, r...)
	}
	return rows, nil
}
