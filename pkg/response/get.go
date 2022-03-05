package response

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/alvaroaleman/static-kas/pkg/transform"
)

func NewGetResponse(
	w http.ResponseWriter,
	parentDir string,
	resourceName string,
	objectName string,
	transform transform.TransformFunc,
) error {
	return (&getResponse{
		w:            w,
		parentDir:    parentDir,
		resourceName: resourceName,
		objectName:   objectName,
		transform:    transform,
	}).run()
}

type getResponse struct {
	w            http.ResponseWriter
	parentDir    string
	resourceName string
	objectName   string
	transform    transform.TransformFunc
}

func (g *getResponse) run() error {
	object, found, err := g.read()
	if err != nil {
		err = fmt.Errorf("failed to read: %w", err)
		http.Error(g.w, err.Error(), http.StatusInternalServerError)
		return err
	}
	if !found {
		g.w.WriteHeader(404)
		return nil
	}

	transformed, err := transformIfNeeded(object, g.transform)
	if err != nil {
		err = fmt.Errorf("transform failed: %w", err)
		http.Error(g.w, err.Error(), http.StatusInternalServerError)
		return err
	}

	return writeJSON(transformed, g.w)
}

func (g *getResponse) read() (*unstructured.Unstructured, bool, error) {
	data, err := ioutil.ReadFile(filepath.Join(g.parentDir, g.resourceName, g.objectName+".yaml"))
	if err != nil {
		if os.IsNotExist(err) {
			return g.readFromList()
		}
		return nil, false, err
	}

	result := &unstructured.Unstructured{}
	return result, true, yaml.Unmarshal(data, result)
}

func (g *getResponse) readFromList() (*unstructured.Unstructured, bool, error) {
	data, err := ioutil.ReadFile(filepath.Join(g.parentDir, g.resourceName+".yaml"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}

	list := &unstructured.UnstructuredList{}
	if err := yaml.Unmarshal(data, list); err != nil {
		return nil, false, err
	}
	for _, item := range list.Items {
		if item.GetName() == g.objectName {
			return &item, true, nil
		}
	}

	return nil, false, nil
}
