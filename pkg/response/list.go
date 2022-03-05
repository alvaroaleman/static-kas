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
	"github.com/alvaroaleman/static-kas/pkg/transform"
)

func NewListResponse(
	w http.ResponseWriter,
	parentDir string,
	resourceName string,
	transform transform.TransformFunc,
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
	transform    transform.TransformFunc
}

func (l *listResponse) run() error {
	list, err := l.readAndDeserialize()
	if err != nil {
		err = fmt.Errorf("failed to read and deserialize: %w", err)
		http.Error(l.w, err.Error(), http.StatusInternalServerError)
		return err
	}

	for _, filter := range l.filter {
		list, err = filter(list)
		if err != nil {
			err = fmt.Errorf("filter failed: %w", err)
			http.Error(l.w, err.Error(), http.StatusInternalServerError)
			return err
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

func (l *listResponse) readAndDeserialize() (*unstructured.UnstructuredList, error) {
	return ReadAndDeserializeList(l.parentDir, l.resourceName)
}

func ReadAndDeserializeList(parenDir, resourceName string) (*unstructured.UnstructuredList, error) {
	fileContents, err := readList(parenDir, resourceName)
	if err != nil {
		return nil, err
	}

	result := &unstructured.UnstructuredList{}
	result.SetAPIVersion("v1")
	result.SetKind("List")

	switch len(fileContents) {
	case 0:
		return result, nil
	// Could be a list or a single item
	case 1:
		// Unmarshal into an unstructured first, because that is guaranteed
		// to not cause issues even if we get a list, as it doesn't make any
		// assumptions about structure (a list assumes there is a list under
		// the .items field).
		target := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(fileContents[0], target); err != nil {
			return nil, err
		}
		if !strings.HasSuffix(target.GetKind(), "List") {
			result.Items = []unstructured.Unstructured{*target}
			return result, nil
		}
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

func readList(parentDir, resourceName string) ([][]byte, error) {
	data, err := ioutil.ReadFile(filepath.Join(parentDir, resourceName+".yaml"))
	if err != nil {
		if os.IsNotExist(err) {
			return readIndividualObjects(parentDir, resourceName)
		}
		return nil, err
	}

	return [][]byte{data}, nil
}

func readIndividualObjects(parentDir, resourceName string) ([][]byte, error) {
	dirPath := filepath.Join(parentDir, resourceName)
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
