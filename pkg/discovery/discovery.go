package discovery

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"io/ioutil"
	"path/filepath"
	"strings"
	"sync"

	"go.uber.org/zap"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/yaml"

	"github.com/alvaroaleman/static-kas/pkg/response"
)

func Discover(l *zap.Logger, basePath string) (map[string]*metav1.APIResourceList, map[GroupVersionResource]metav1.APIResource, map[string]*apiextensionsv1.CustomResourceDefinition, error) {
	// explicitly read crds first, so we can insert the shortnames we find there into discovery
	crdMap, err := getCRDs(basePath)
	if err != nil {
		// This shouldn't make us fail
		l.Warn("encountered errors reading crds", zap.Error(err))
	}
	errs := errorGroup{}
	result := map[string]*metav1.APIResourceList{}
	apiResources := map[GroupVersionResource]metav1.APIResource{}
	lock := sync.Mutex{}
	wg := sync.WaitGroup{}

	filepath.WalkDir(basePath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			errs.add(fmt.Errorf("error walking at %s: %w", path, err))
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".yaml") {
			return nil
		}
		wg.Add(1)
		// TODO: Optimize by stopping here if the group and object are already discovered
		// this likely requires to key the map by group and not by groupVersion
		go func() {
			defer wg.Done()
			raw, err := ioutil.ReadFile(path)
			if err != nil {
				errs.add(fmt.Errorf("failed to read file %s: %w", path, err))
				return
			}

			u := &unstructured.Unstructured{}
			if err := yaml.Unmarshal(raw, u); err != nil {
				errs.add(fmt.Errorf("failed to decode %s into an unstructured: %w", path, err))
				return
			}
			items, found, err := unstructured.NestedSlice(u.Object, "items")
			if err != nil {
				// It's a list but has no entries
				if err.Error() == ".items accessor error: <nil> is of the type <nil>, expected []interface{}" {
					return
				}
				errs.add(fmt.Errorf("items field for file %s was not a slice: %w", path, err))
				return
			}

			var name, kind, groupVersion string
			if found {
				if len(items) < 1 {
					return
				}
				// If we find a list, the resouce name is simply the filename without the yaml suffix
				name = strings.TrimSuffix(d.Name(), ".yaml")
				kind, _, _ = unstructured.NestedString(items[0].(map[string]interface{}), "kind")
				groupVersion, _, _ = unstructured.NestedString(items[0].(map[string]interface{}), "apiVersion")
			} else {
				pathElements := strings.Split(path, "/")
				// Should never happen(tm)
				if len(pathElements) < 2 {
					return
				}
				fileNameWithoutSuffix := strings.TrimSuffix(d.Name(), ".yaml")
				// If we find a single object, the resource name is the name of the first parent folder that is not also the name
				// of the object (pods are nested in a pods/$podname/$podname.yaml structure for some reason)
				for i := len(pathElements) - 2; i > 0; i-- {
					if pathElements[i] != fileNameWithoutSuffix {
						name = pathElements[i]
						break
					}
				}
				kind = u.GetKind()
				groupVersion = u.GetAPIVersion()
			}
			namespaced := strings.Contains(path, "namespaces/")

			lock.Lock()
			defer lock.Unlock()

			if _, hasEntry := result[groupVersion]; !hasEntry {
				result[groupVersion] = &metav1.APIResourceList{
					GroupVersion: groupVersion,
				}
				if groupVersion == "v1" {
					result[groupVersion].APIResources = append(result[groupVersion].APIResources, metav1.APIResource{
						Name:       "namespaces",
						Kind:       "Namespace",
						Verbs:      []string{"get", "list"},
						ShortNames: []string{"ns"},
					})
				}
			}
			for _, resource := range result[groupVersion].APIResources {
				// Entry for our resource already exist, nothing to do
				if resource.Name == name {
					return
				}
			}

			resource := metav1.APIResource{
				Name:       name,
				Namespaced: namespaced,
				Kind:       kind,
				Verbs:      []string{"get", "list", "watch"},
				ShortNames: shortNamesFor(name, groupVersion, crdMap),
			}
			result[groupVersion].APIResources = append(result[groupVersion].APIResources, resource)
			apiResources[GroupVersionResource{GroupVersion: groupVersion, Resource: name}] = resource
		}()

		return nil
	})

	wg.Wait()
	return result, apiResources, crdMap, utilerrors.NewAggregate(errs.errs)
}

type errorGroup struct {
	errs []error
	lock sync.Mutex
}

func (e *errorGroup) add(err error) {
	e.lock.Lock()
	defer e.lock.Unlock()
	e.errs = append(e.errs, err)
}

type GroupVersionResource struct {
	GroupVersion string
	Resource     string
}

func getCRDs(basePath string) (map[string]*apiextensionsv1.CustomResourceDefinition, error) {
	raw, err := response.ReadAndDeserializeList(filepath.Join(basePath, "cluster-scoped-resources", "apiextensions.k8s.io"), "customresourcedefinitions")
	if err != nil {
		return nil, fmt.Errorf("failed to read crds: %w", err)
	}

	var errs []error
	result := make(map[string]*apiextensionsv1.CustomResourceDefinition, len(raw.Items))
	for _, item := range raw.Items {
		serialized, err := json.Marshal(&item)
		if err != nil {
			errs = append(errs, fmt.Errorf("failed to serialize crd %s from unstructured: %w", item.GetName(), err))
			continue
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := json.Unmarshal(serialized, crd); err != nil {
			errs = append(errs, fmt.Errorf("failed to deserialize crd %s into a %T: %w", item.GetName(), crd, err))
			continue
		}
		result[crd.Name] = crd
	}

	return result, utilerrors.NewAggregate(errs)
}

func shortNamesFor(resource string, groupVersion string, crds map[string]*apiextensionsv1.CustomResourceDefinition) []string {
	var group string
	if split := strings.Split(groupVersion, "/"); len(split) == 2 {
		group = split[0]
	}
	resourceGroup := resource
	if group != "" {
		resourceGroup += "." + group
	}

	// TODO: We should try to import this from k/k
	if staticMappingVal, found := shortNameMapping[resourceGroup]; found {
		return staticMappingVal
	}

	if crd, found := crds[resourceGroup]; found {
		return crd.Spec.Names.ShortNames
	}

	return nil
}
