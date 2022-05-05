package response

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"path"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/alvaroaleman/static-kas/pkg/filter"
	"github.com/alvaroaleman/static-kas/pkg/transform"
)

func NewCrossNamespaceListResponse(
	r *http.Request,
	w http.ResponseWriter,
	parentDir string,
	group string,
	resource string,
	transform transform.TransformFunc,
	filter ...filter.Filter,
) error {
	result, err := readAndDeserializeForAllNamespaces(parentDir, group, resource)
	if err != nil {
		err = fmt.Errorf("failed to get %s from all namespaces: %w", resource, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}

	for _, filter := range filter {
		result, err = filter(result)
		if err != nil {
			err = fmt.Errorf("filter failed: %w", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return err
		}
	}

	if isWatch(r) {
		return respondToWatch(r, w, unstructuredListItemsToRuntimeObjects(result)...)
	}

	transformed, err := transformIfNeeded(result, transform)
	if err != nil {
		err = fmt.Errorf("failed to transform: %w", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return err
	}

	return writeJSON(transformed, w)
}

func readAndDeserializeForAllNamespaces(parentDir, group, resource string) (*unstructured.UnstructuredList, error) {
	namespaces, err := ioutil.ReadDir(parentDir)
	if err != nil {
		return nil, fmt.Errorf("failed to list namespaces: %w", err)
	}
	result := &unstructured.UnstructuredList{}
	result.SetAPIVersion("v1")
	result.SetKind("List")
	for _, namespace := range namespaces {
		if !namespace.IsDir() {
			continue
		}
		fromNamespace, err := ReadAndDeserializeList(path.Join(parentDir, namespace.Name(), group), resource)
		if err != nil {
			return nil, fmt.Errorf("failed to read from namespace %s: %w", namespace.Name(), err)
		}
		result.Items = append(result.Items, fromNamespace.Items...)
	}
	if len(result.Items) > 0 {
		result.SetAPIVersion(result.Items[0].GetAPIVersion())
		result.SetKind(result.Items[0].GetKind() + "List")
	}

	return result, nil
}
