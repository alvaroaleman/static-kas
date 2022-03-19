package transform

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	extensionsv1beta1 "k8s.io/api/extensions/v1beta1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/registry/customresource/tableconvertor"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

type TransformEntryKey struct {
	ResourceName string
	GroupName    string
	Version      string
	Verb         string
}

const (
	VerbList = "list"
	VerbGet  = "get"
)

type TransformFunc func(r runtime.Object) (*metav1.Table, error)

func transform(header []metav1.TableColumnDefinition, body func([]byte) ([]metav1.TableRow, error)) func(string) TransformFunc {
	return func(tableVersion string) TransformFunc {
		return func(o runtime.Object) (*metav1.Table, error) {
			serialized, err := json.Marshal(o)
			if err != nil {
				return nil, fmt.Errorf("failed to serialize: %w", err)
			}
			rows, err := body(serialized)
			if err != nil {
				return nil, err
			}

			return &metav1.Table{
				TypeMeta: metav1.TypeMeta{
					Kind:       "Table",
					APIVersion: "meta.k8s.io/" + tableVersion,
				},
				ColumnDefinitions: header,
				Rows:              rows,
			}, nil
		}
	}
}

func NewTableTransformMap(crds map[string]*apiextensionsv1.CustomResourceDefinition) func(TransformEntryKey, string) TransformFunc {
	result := map[TransformEntryKey]func(string) TransformFunc{}

	// Everything below here is copied from https://github.com/kubernetes/kubernetes/blob/ab13c85316015cf9f115e29923ba9740bd1564fd/pkg/printers/internalversion/printers.go#L89
	// with some slight adjustments to work on the external api types
	podColumnDefinitions := []metav1.TableColumnDefinition{
		{Name: "Name", Type: "string", Format: "name", Description: metav1.ObjectMeta{}.SwaggerDoc()["name"]},
		{Name: "Ready", Type: "string", Description: "The aggregate readiness state of this pod for accepting traffic."},
		{Name: "Status", Type: "string", Description: "The aggregate status of the containers in this pod."},
		{Name: "Restarts", Type: "string", Description: "The number of times the containers in this pod have been restarted and when the last container in this pod has restarted."},
		{Name: "Age", Type: "string", Description: metav1.ObjectMeta{}.SwaggerDoc()["creationTimestamp"]},
		{Name: "IP", Type: "string", Priority: 1},
		{Name: "Node", Type: "string", Priority: 1},
		{Name: "Nominated Node", Type: "string", Priority: 1},
		{Name: "Readiness Gates", Type: "string", Priority: 1},
	}
	result[TransformEntryKey{ResourceName: "pods", Verb: VerbList}] = transform(podColumnDefinitions, printPodList)
	result[TransformEntryKey{ResourceName: "pods", Verb: VerbGet}] = transform(podColumnDefinitions, printPodFromRaw)

	replicaSetColumnDefinitions := []metav1.TableColumnDefinition{
		{Name: "Name", Type: "string", Format: "name", Description: metav1.ObjectMeta{}.SwaggerDoc()["name"]},
		{Name: "Desired", Type: "integer", Description: extensionsv1beta1.ReplicaSetSpec{}.SwaggerDoc()["replicas"]},
		{Name: "Current", Type: "integer", Description: extensionsv1beta1.ReplicaSetStatus{}.SwaggerDoc()["replicas"]},
		{Name: "Ready", Type: "integer", Description: extensionsv1beta1.ReplicaSetStatus{}.SwaggerDoc()["readyReplicas"]},
		{Name: "Age", Type: "string", Description: metav1.ObjectMeta{}.SwaggerDoc()["creationTimestamp"]},
		{Name: "Containers", Type: "string", Priority: 1, Description: "Names of each container in the template."},
		{Name: "Images", Type: "string", Priority: 1, Description: "Images referenced by each container in the template."},
		{Name: "Selector", Type: "string", Priority: 1, Description: extensionsv1beta1.ReplicaSetSpec{}.SwaggerDoc()["selector"]},
	}
	result[TransformEntryKey{GroupName: "apps", ResourceName: "replicasets", Version: "v1", Verb: VerbList}] = transform(replicaSetColumnDefinitions, printReplicaSetListFromRaw)
	result[TransformEntryKey{GroupName: "apps", ResourceName: "replicasets", Version: "v1", Verb: VerbGet}] = transform(replicaSetColumnDefinitions, printReplicaSetFromRaw)

	deploymentColumnDefinitions := []metav1.TableColumnDefinition{
		{Name: "Name", Type: "string", Format: "name", Description: metav1.ObjectMeta{}.SwaggerDoc()["name"]},
		{Name: "Ready", Type: "string", Description: "Number of the pod with ready state"},
		{Name: "Up-to-date", Type: "string", Description: extensionsv1beta1.DeploymentStatus{}.SwaggerDoc()["updatedReplicas"]},
		{Name: "Available", Type: "string", Description: extensionsv1beta1.DeploymentStatus{}.SwaggerDoc()["availableReplicas"]},
		{Name: "Age", Type: "string", Description: metav1.ObjectMeta{}.SwaggerDoc()["creationTimestamp"]},
		{Name: "Containers", Type: "string", Priority: 1, Description: "Names of each container in the template."},
		{Name: "Images", Type: "string", Priority: 1, Description: "Images referenced by each container in the template."},
		{Name: "Selector", Type: "string", Priority: 1, Description: extensionsv1beta1.DeploymentSpec{}.SwaggerDoc()["selector"]},
	}
	result[TransformEntryKey{GroupName: "apps", ResourceName: "deployments", Version: "v1", Verb: VerbList}] = transform(deploymentColumnDefinitions, printDeploymentList)
	result[TransformEntryKey{GroupName: "apps", ResourceName: "deployments", Version: "v1", Verb: VerbGet}] = transform(deploymentColumnDefinitions, printDeploymentFromRaw)

	statefulSetColumnDefinitions := []metav1.TableColumnDefinition{
		{Name: "Name", Type: "string", Format: "name", Description: metav1.ObjectMeta{}.SwaggerDoc()["name"]},
		{Name: "Ready", Type: "string", Description: "Number of the pod with ready state"},
		{Name: "Age", Type: "string", Description: metav1.ObjectMeta{}.SwaggerDoc()["creationTimestamp"]},
		{Name: "Containers", Type: "string", Priority: 1, Description: "Names of each container in the template."},
		{Name: "Images", Type: "string", Priority: 1, Description: "Images referenced by each container in the template."},
	}
	result[TransformEntryKey{GroupName: "apps", ResourceName: "statefulsets", Version: "v1", Verb: VerbList}] = transform(statefulSetColumnDefinitions, printStatefulSetList)
	result[TransformEntryKey{GroupName: "apps", ResourceName: "statefulsets", Version: "v1", Verb: VerbGet}] = transform(statefulSetColumnDefinitions, printStatefulSetFromRaw)

	daemonSetColumnDefinitions := []metav1.TableColumnDefinition{
		{Name: "Name", Type: "string", Format: "name", Description: metav1.ObjectMeta{}.SwaggerDoc()["name"]},
		{Name: "Desired", Type: "integer", Description: extensionsv1beta1.DaemonSetStatus{}.SwaggerDoc()["desiredNumberScheduled"]},
		{Name: "Current", Type: "integer", Description: extensionsv1beta1.DaemonSetStatus{}.SwaggerDoc()["currentNumberScheduled"]},
		{Name: "Ready", Type: "integer", Description: extensionsv1beta1.DaemonSetStatus{}.SwaggerDoc()["numberReady"]},
		{Name: "Up-to-date", Type: "integer", Description: extensionsv1beta1.DaemonSetStatus{}.SwaggerDoc()["updatedNumberScheduled"]},
		{Name: "Available", Type: "integer", Description: extensionsv1beta1.DaemonSetStatus{}.SwaggerDoc()["numberAvailable"]},
		{Name: "Node Selector", Type: "string", Description: corev1.PodSpec{}.SwaggerDoc()["nodeSelector"]},
		{Name: "Age", Type: "string", Description: metav1.ObjectMeta{}.SwaggerDoc()["creationTimestamp"]},
		{Name: "Containers", Type: "string", Priority: 1, Description: "Names of each container in the template."},
		{Name: "Images", Type: "string", Priority: 1, Description: "Images referenced by each container in the template."},
		{Name: "Selector", Type: "string", Priority: 1, Description: extensionsv1beta1.DaemonSetSpec{}.SwaggerDoc()["selector"]},
	}
	result[TransformEntryKey{GroupName: "apps", ResourceName: "daemonsets", Version: "v1", Verb: VerbList}] = transform(daemonSetColumnDefinitions, printDaemonSetListFromRaw)
	result[TransformEntryKey{GroupName: "apps", ResourceName: "daemonsets", Version: "v1", Verb: VerbGet}] = transform(daemonSetColumnDefinitions, printDaemonSetFromRaw)

	return func(key TransformEntryKey, tableVersion string) TransformFunc {
		if fn, found := result[key]; found {
			return fn(tableVersion)
		}

		return func(r runtime.Object) (*metav1.Table, error) {
			// TODO: Should we cache these?
			converter, err := tableconvertor.New(additionalPrinterColumsForCRD(key, crds))
			if err != nil {
				return nil, fmt.Errorf("failed to construct tableconvertor: %w", err)
			}

			table, err := converter.ConvertToTable(context.Background(), r, &metav1.TableOptions{})
			if err != nil {
				return nil, err
			}
			if err := makeTableObjectsPartialObjectMetadata(table); err != nil {
				return nil, fmt.Errorf("failed to convert table objects to partialObjectMetadata: %w", err)
			}
			table.Kind = "Table"
			// There is a v1 and a v1beta1 and they both look the same, but clients might be unable to decode
			// if the version doesn't match what they requested.
			table.APIVersion = "meta.k8s.io/" + tableVersion
			return table, nil
		}
	}
}

func additionalPrinterColumsForCRD(key TransformEntryKey, crds map[string]*apiextensionsv1.CustomResourceDefinition) []apiextensionsv1.CustomResourceColumnDefinition {

	crd, found := crds[key.ResourceName+"."+key.GroupName]
	if !found {
		return nil
	}
	for _, version := range crd.Spec.Versions {
		if version.Name != key.Version {
			continue
		}
		return version.AdditionalPrinterColumns
	}

	return nil
}

func makeTableObjectsPartialObjectMetadata(t *metav1.Table) error {
	for idx := range t.Rows {
		serialized, err := json.Marshal(t.Rows[idx].Object)
		if err != nil {
			return err
		}
		m := &metav1.PartialObjectMetadata{}
		if err := json.Unmarshal(serialized, m); err != nil {
			return err
		}
		m.Kind = "PartialObjectMetadata"
		m.APIVersion = "meta.k8s.io/v1beta1"
		t.Rows[idx].Object = runtime.RawExtension{Object: m}
	}

	return nil
}
