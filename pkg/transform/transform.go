package transform

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/registry/customresource/tableconvertor"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	registryrest "k8s.io/apiserver/pkg/registry/rest"
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

func NewTableTransformMap(log *zap.Logger, crds map[string]*apiextensionsv1.CustomResourceDefinition) func(TransformEntryKey, string) TransformFunc {
	inTreeHandler := newInTreeHandler(log)
	defaultConvertor := registryrest.NewDefaultTableConvertor(schema.GroupResource{})
	return func(key TransformEntryKey, tableVersion string) TransformFunc {
		fallBackHandler := func(r runtime.Object) (*metav1.Table, error) {
			var convertor func(ctx context.Context, object runtime.Object, tableOptions runtime.Object) (*metav1.Table, error)

			additionalPrinterColums := additionalPrinterColumsForCRD(key, crds)
			if len(additionalPrinterColums) == 0 {
				convertor = defaultConvertor.ConvertToTable
			} else {
				// TODO: Should we cache these?
				converter, err := tableconvertor.New(additionalPrinterColums)
				if err != nil {
					return nil, fmt.Errorf("failed to construct tableconvertor: %w", err)
				}
				convertor = converter.ConvertToTable
			}
			table, err := convertor(context.Background(), r, &metav1.TableOptions{})
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

		return inTreeHandler.transformFunc(tableVersion, fallBackHandler)
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
