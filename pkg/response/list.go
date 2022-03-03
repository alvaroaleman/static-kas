package response

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/alvaroaleman/static-kas/pkg/filter"
)

func NewListResponse(
	w http.ResponseWriter,
	path string,
	transform func([]byte) (interface{}, error),
	filter ...filter.Filter,
) error {
	return (&listResponse{
		w:         w,
		path:      path,
		transform: transform,
		filter:    filter,
	}).run()
}

type listResponse struct {
	w         http.ResponseWriter
	path      string
	filter    []filter.Filter
	transform func([]byte) (interface{}, error)
}

func (l *listResponse) run() error {
	list, err := l.readAndFilter()
	if err != nil {
		err = fmt.Errorf("failed to read and filter: %w", err)
		http.Error(l.w, err.Error(), http.StatusInternalServerError)
		return err
	}

	transformed, err := transformIfNeeded(list, l.transform)
	if err != nil {
		err = fmt.Errorf("failed to transform: %w", err)
		http.Error(l.w, err.Error(), http.StatusInternalServerError)
		return err
	}

	return writeJSON(transformed, l.w)
}

func (l *listResponse) readAndFilter() (*unstructured.UnstructuredList, error) {
	result := &unstructured.UnstructuredList{}
	raw, err := ioutil.ReadFile(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return nil, err
	}
	if err := yaml.Unmarshal(raw, result); err != nil {
		return nil, fmt.Errorf("failed to deserialize %s: %w", l.path, err)
	}

	for _, filter := range l.filter {
		result, err = filter(result)
		if err != nil {
			return nil, fmt.Errorf("filter failed: %w", err)
		}
	}

	return result, nil
}
