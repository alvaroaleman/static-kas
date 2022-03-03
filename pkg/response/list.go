package response

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/yaml"

	"github.com/alvaroaleman/static-kas/pkg/filter"
)

func NewListResponse(
	w http.ResponseWriter,
	parentDir string,
	resourceName string,
	transform func([]byte) (interface{}, error),
	filter ...filter.Filter,
) error {
	return (&listResponse{
		w:            w,
		parentDir:    parentDir,
		resourceName: resourceName,
		transform:    transform,
		filter:       filter,
	}).run()
}

type listResponse struct {
	w            http.ResponseWriter
	parentDir    string
	resourceName string
	filter       []filter.Filter
	transform    func([]byte) (interface{}, error)
}

func (l *listResponse) run() error {
	list, err := l.readAndFilter()
	if err != nil {
		err = fmt.Errorf("failed to read and filter: %w", err)
		http.Error(l.w, err.Error(), http.StatusInternalServerError)
		return err
	}

	for _, filter := range l.filter {
		list, err = filter(list)
		if err != nil {
			return fmt.Errorf("filter failed: %w", err)
		}
	}

	transformed, err := transformIfNeeded(list, l.transform)
	if err != nil {
		err = fmt.Errorf("failed to transform: %w", err)
		http.Error(l.w, err.Error(), http.StatusInternalServerError)
		return err
	}

	return writeJSON(transformed, l.w)
}

func (l *listResponse) read() ([][]byte, error) {
	data, err := ioutil.ReadFile(filepath.Join(l.parentDir, l.resourceName, ".yaml"))
	if err != nil {
		if os.IsNotExist(err) {
			return l.readIndividualObjects()
		}
		return nil, err
	}

	return [][]byte{data}, nil
}

func (l *listResponse) readIndividualObjects() ([][]byte, error) {
	dirPath := filepath.Join(l.parentDir, l.resourceName)
	entries, err := ioutil.ReadDir(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var result [][]byte
	var errs []error
	var lock sync.Mutex
	var wg sync.WaitGroup
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yaml") {
			continue
		}
		entry := entry
		wg.Add(1)
		go func() {
			defer wg.Done()
			data, err := ioutil.ReadFile(filepath.Join(dirPath, entry.Name()))
			lock.Lock()
			defer lock.Unlock()
			result = append(result, data)
			errs = append(errs, err)
		}()
	}
	wg.Wait()

	return result, utilerrors.NewAggregate(errs)
}

func (l *listResponse) readAndFilter() (*unstructured.UnstructuredList, error) {
	fileContents, err := l.read()
	if err != nil {
		return nil, err
	}

	result := &unstructured.UnstructuredList{}
	result.SetAPIVersion("v1")
	result.SetKind("List")

	switch len(fileContents) {
	case 0:
		return result, nil
	case 1:
		return result, yaml.Unmarshal(fileContents[0], result)
	default:
		for _, fileContent := range fileContents {
			target := &unstructured.Unstructured{}
			if err := yaml.Unmarshal(fileContent, target); err != nil {
				return nil, err
			}
			result.Items = append(result.Items, *target)
		}

		return result, nil
	}
}
