package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gorilla/mux"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/felixge/httpsnoop"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"sigs.k8s.io/yaml"

	"github.com/alvaroaleman/static-kas/pkg/transform"
)

type options struct {
	baseDir string
}

func main() {

	o := options{}
	flag.StringVar(&o.baseDir, "base-dir", "", "The basedir of the cluster dump")
	flag.Parse()

	lCfg := zap.NewProductionConfig()
	lCfg.EncoderConfig.EncodeTime = zapcore.RFC3339TimeEncoder
	l, err := lCfg.Build()
	if err != nil {
		fmt.Printf("failed to construct logger: %v\n", err)
		os.Exit(1)
	}
	defer l.Sync()

	if o.baseDir == "" {
		l.Fatal("--base-dir is mandatory")
	}

	l.Info("Discovering api resources")
	groupResourceListMap, groupResourceMap, err := discover(o.baseDir)
	if err != nil {
		l.Fatal("failed to discover apis", zap.Error(err))
	}
	groupSerializedResourceListMap, err := serializeAPIResourceList(groupResourceListMap)
	if err != nil {
		l.Fatal("failed to serialize apiresources", zap.Error(err))
	}
	groupList, err := apiGroupList(groupResourceListMap)
	if err != nil {
		l.Fatal("failed to construct api group list", zap.Error(err))
	}
	serializedGroupList, err := json.Marshal(groupList)
	if err != nil {
		l.Fatal("failed to serialize api group list", zap.Error(err))
	}
	allNamespaces := corev1.NamespaceList{TypeMeta: metav1.TypeMeta{Kind: "List"}}
	namespacePath := filepath.Join(o.baseDir, "namespaces")
	namespacesDirEntries, err := os.ReadDir(namespacePath)
	if err != nil {
		l.Fatal("failed to read namespaces folder", zap.String("path", namespacePath), zap.Error(err))
	}
	for _, entry := range namespacesDirEntries {
		allNamespaces.Items = append(allNamespaces.Items, corev1.Namespace{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
			ObjectMeta: metav1.ObjectMeta{Name: entry.Name()},
		})
	}
	l.Info("Finished discovering api resources")

	tableTransformMap := transform.NewTableTransformMap()

	router := mux.NewRouter()
	router.Use(loggingMiddleware(l))
	// Re-Define the not found handler so it goes through the middleware
	router.NotFoundHandler = router.NewRoute().BuildOnly().HandlerFunc(http.NotFound).GetHandler()
	router.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{}`))
	})
	router.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) {
		d := metav1.APIVersions{TypeMeta: metav1.TypeMeta{Kind: "APIVersions"}, Versions: []string{"v1"}}
		serializeAndWrite(l, w, d)
	})
	router.HandleFunc("/api/v1", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(groupSerializedResourceListMap["v1"])
	})
	router.HandleFunc("/api/v1/namespaces/{namespace}/{resource}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		path := path.Join(o.baseDir, "namespaces", vars["namespace"], "core", vars["resource"]+".yaml")
		var transformFunc func([]byte) (interface{}, error)
		if acceptsTable(r) {
			transformFunc = tableTransformMap[transform.TransformEntryKey{ResourceName: vars["resource"], Verb: transform.VerbList}]
		}
		servePath(path, l, w, transformFunc, filterForFieldSelector(r.URL.Query()["fieldSelector"]))
	})
	router.HandleFunc("/api/v1/namespaces/{namespace}/{resource}/{name}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		var transformFunc func([]byte) (interface{}, error)
		if acceptsTable(r) {
			transformFunc = tableTransformMap[transform.TransformEntryKey{ResourceName: vars["resource"], Verb: transform.VerbGet}]
		}
		path := path.Join(o.baseDir, "namespaces", vars["namespace"], "core", vars["resource"]+".yaml")
		serveNamedObjectFromPath(path, l, w, vars["name"], transformFunc)
	})
	router.HandleFunc("/api/v1/namespaces/{namespace}/pods/{name}/log", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		containerName := r.URL.Query().Get("container")
		fileName := "current.log"
		if r.URL.Query().Get("previous") == "true" {
			fileName = "previous.log"
		}
		path := path.Join(o.baseDir, "namespaces", vars["namespace"], "pods", vars["name"], containerName, containerName, "logs", fileName)
		f, err := os.Open(path)
		if err != nil {
			w.WriteHeader(404)
			w.Write([]byte(fmt.Sprintf("failed to open %s: %v", path, err)))
			return
		}
		defer f.Close()
		io.Copy(w, f)
	})
	router.HandleFunc("/api/v1/{resource}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		// Special snowflake, they are not being dumped by must-gather
		if vars["resource"] == "namespaces" {
			serializeAndWrite(l, w, allNamespaces)
			return
		}
		var transformFunc func([]byte) (interface{}, error)
		if acceptsTable(r) {
			transformFunc = tableTransformMap[transform.TransformEntryKey{ResourceName: vars["resource"], Verb: transform.VerbList}]
		}
		if groupResourceMap[groupVersionResource{groupVersion: "v1", resource: vars["resource"]}].Namespaced {
			basePath := filepath.Join(o.baseDir, "namespaces")
			suffix := filepath.Join("core", vars["resource"]+".yaml")
			namespacedResourceForAllNamespaces(basePath, &allNamespaces, suffix, l, w, transformFunc)
		} else {
			path := path.Join(o.baseDir, "cluster-scoped-resources", "core", vars["resource"]+".yaml")
			servePath(path, l, w, transformFunc, filterForFieldSelector(r.URL.Query()["fieldSelector"]))
		}
	})
	router.HandleFunc("/api/v1/{resource}/{name}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		if vars["resource"] == "namespaces" {
			serveNamespace(l, w, &allNamespaces, vars["name"])
			return
		}
		path := path.Join(o.baseDir, "cluster-scoped-resources", "core", vars["resource"]+".yaml")
		serveNamedObjectFromPath(path, l, w, vars["name"], nil)
	})
	router.HandleFunc("/apis", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(serializedGroupList)
	})
	for groupVersion := range groupSerializedResourceListMap {
		groupVersion := groupVersion
		router.HandleFunc("/apis/"+groupVersion, func(w http.ResponseWriter, _ *http.Request) {
			w.Write(groupSerializedResourceListMap[groupVersion])
		})
	}
	router.HandleFunc("/apis/{group}/{version}/namespaces/{namespace}/{resource}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		var transformFunc func([]byte) (interface{}, error)
		if acceptsTable(r) {
			transformFunc = tableTransformMap[transform.TransformEntryKey{GroupName: vars["group"], ResourceName: vars["resource"], Verb: transform.VerbList}]
		}
		path := path.Join(o.baseDir, "namespaces", vars["namespace"], vars["group"], vars["resource"]+".yaml")
		servePath(path, l, w, transformFunc, filterForFieldSelector(r.URL.Query()["fieldSelector"]))
	})
	router.HandleFunc("/apis/{group}/{version}/namespaces/{namespace}/{resource}/{name}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		var transformFunc func([]byte) (interface{}, error)
		if acceptsTable(r) {
			transformFunc = tableTransformMap[transform.TransformEntryKey{GroupName: vars["group"], ResourceName: vars["resource"], Verb: transform.VerbGet}]
		}
		path := path.Join(o.baseDir, "namespaces", vars["namespace"], vars["group"], vars["resource"]+".yaml")
		serveNamedObjectFromPath(path, l, w, vars["name"], transformFunc)
	})
	router.HandleFunc("/apis/{group}/{version}/{resource}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		if vars["group"] == "authorization.k8s.io" && vars["resource"] == "selfsubjectaccessreviews" {
			handleSSAR(l, w, r)
			return
		}
		var transformFunc func([]byte) (interface{}, error)
		if acceptsTable(r) {
			transformFunc = tableTransformMap[transform.TransformEntryKey{GroupName: vars["group"], ResourceName: vars["resource"], Verb: transform.VerbList}]
		}
		if groupResourceMap[groupVersionResource{groupVersion: vars["group"] + "/" + vars["version"], resource: vars["resource"]}].Namespaced {
			basePath := filepath.Join(o.baseDir, "namespaces")
			suffix := filepath.Join(vars["group"], vars["resource"]+".yaml")
			namespacedResourceForAllNamespaces(basePath, &allNamespaces, suffix, l, w, transformFunc)
		} else {
			path := path.Join(o.baseDir, "cluster-scoped-resources", vars["group"], vars["resource"]+".yaml")
			servePath(path, l, w, transformFunc)
		}
	})
	router.HandleFunc("/apis/{group}/{version}/{resource}/{name}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		path := path.Join(o.baseDir, "cluster-scoped-resources", vars["group"], vars["resource"]+".yaml")
		serveNamedObjectFromPath(path, l, w, vars["name"], nil)
	})

	if err := http.ListenAndServe(":8080", router); err != nil {
		l.Error("server ended", zap.Error(err))
	}
}

func defaultTransform(in []byte) (interface{}, error) {
	result := map[string]interface{}{}
	if err := yaml.Unmarshal(in, &result); err != nil {
		return nil, fmt.Errorf("failed to deserialize: %w", err)
	}
	return result, nil
}

func servePath(path string, l *zap.Logger, w http.ResponseWriter, transform transform.TransformFunc, filter ...filter) {
	raw, err := ioutil.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			l.Info("file not found", zap.String("path", path))
			w.WriteHeader(404)
			serializeAndWrite(l, w, unstructured.UnstructuredList{})
			return
		}
		http.Error(w, fmt.Sprintf("failed to read %s: %v", path, err), http.StatusInternalServerError)
		return
	}

	for _, filter := range filter {
		raw, err = filter(raw)
		if err != nil {
			http.Error(w, fmt.Sprintf("filter failed: %v", err), http.StatusInternalServerError)
			return
		}
	}

	if transform == nil {
		transform = defaultTransform
	}

	result, err := transform(raw)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to transform: %v", err), http.StatusInternalServerError)
		return
	}

	serializeAndWrite(l, w, result)
}

type filter func([]byte) ([]byte, error)

func filterForFieldSelector(value []string) func([]byte) ([]byte, error) {
	selectorMap := make(map[string]string, len(value))
	return func(list []byte) ([]byte, error) {
		if len(value) == 0 {
			return list, nil
		}
		var sanitizedValues []string
		for _, item := range value {
			sanitizedValues = append(strings.Split(item, ","))
		}
		for _, entry := range sanitizedValues {
			split := strings.Split(entry, "=")
			if len(split) != 2 {
				return nil, fmt.Errorf("field selector expression %s split by = doesn't yield exactly two results", entry)
			}
			selectorMap[split[0]] = split[1]
		}

		l := &unstructured.UnstructuredList{}
		if err := yaml.Unmarshal(list, l); err != nil {
			return nil, fmt.Errorf("failed to unmarshal data into list: %w", err)
		}
		result := &unstructured.UnstructuredList{}
		result.SetGroupVersionKind(l.GroupVersionKind())

		for _, item := range l.Items {
			matches := true
			for k, v := range selectorMap {
				if value, _, _ := unstructured.NestedString(item.Object, strings.Split(k, ".")...); value != v {
					matches = false
					break
				}
			}
			if matches {
				result.Items = append(result.Items, *item.DeepCopy())
			}
		}

		return json.Marshal(result)
	}
}

func serveNamedObjectFromPath(path string, l *zap.Logger, w http.ResponseWriter, name string, transform transform.TransformFunc) {
	raw, err := ioutil.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			l.Info("file not found", zap.String("path", path))
			w.WriteHeader(404)
			serializeAndWrite(l, w, unstructured.UnstructuredList{})
			return
		}
		http.Error(w, fmt.Sprintf("failed to read %s: %v", path, err), http.StatusInternalServerError)
		return
	}

	result := unstructured.UnstructuredList{}
	if err := yaml.Unmarshal(raw, &result); err != nil {
		http.Error(w, fmt.Sprintf("failed to deserialize contents of %s: %v", path, err), http.StatusInternalServerError)
		return
	}
	for _, item := range result.Items {
		if item.GetName() != name {
			continue
		}
		obj, err := transformIfNeeded(item.Object, transform)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to transform object: %v", err), http.StatusInternalServerError)
			return
		}
		serializeAndWrite(l, w, obj)
		return
	}
	w.WriteHeader(404)
}

func transformIfNeeded(object interface{}, transform transform.TransformFunc) (interface{}, error) {
	if transform == nil {
		return object, nil
	}
	serialized, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("failed to serialize to json before transforming: %v", err)
	}
	return transform(serialized)
}

func serializeAndWrite(l *zap.Logger, w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	serialized, err := json.Marshal(data)
	if err != nil {
		l.Error("failed to serialize object", zap.String("type", fmt.Sprintf("%T", data)), zap.Error(err))
		return
	}
	if _, err := w.Write(serialized); err != nil {
		l.Error("failed to write object", zap.Error(err))
	}
}

func acceptsTable(r *http.Request) bool {
	return len(r.Header["Accept"]) > 0 && strings.Contains(r.Header["Accept"][0], "as=Table")
}

func handleSSAR(l *zap.Logger, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(400)
		w.Write([]byte(fmt.Sprintf("method %s is not supported, method %s must be used", r.Method, http.MethodPost)))
		return
	}

	var ssar authorizationv1.SelfSubjectAccessReview
	if err := json.NewDecoder(r.Body).Decode(&ssar); err != nil {
		w.WriteHeader(400)
		w.Write([]byte(fmt.Sprintf("failed to decode request body: %v", err)))
		return
	}
	ssar.Status.Allowed = true
	if err := json.NewEncoder(w).Encode(ssar); err != nil {
		l.Error("failed to encode response", zap.Error(err))
	}
}

func serveNamespace(l *zap.Logger, w http.ResponseWriter, namespaceList *corev1.NamespaceList, name string) {
	for _, item := range namespaceList.Items {
		if item.Name == name {
			serializeAndWrite(l, w, item)
			return
		}
	}

	w.WriteHeader(404)
}

func namespacedResourceForAllNamespaces(
	basePath string,
	namespaces *corev1.NamespaceList,
	suffix string,
	l *zap.Logger,
	w http.ResponseWriter,
	transform transform.TransformFunc,
) {

	errs := errorGroup{}
	var lists []*unstructured.UnstructuredList
	lock := sync.Mutex{}
	wg := sync.WaitGroup{}
	wg.Add(len(namespaces.Items))

	for _, ns := range namespaces.Items {
		ns := ns
		go func() {
			defer wg.Done()
			path := filepath.Join(basePath, ns.Name, suffix)
			raw, err := ioutil.ReadFile(path)
			if err != nil {
				if !os.IsNotExist(err) {
					errs.add(fmt.Errorf("failed to read %s: %v", path, err))
				}
				return
			}
			list := &unstructured.UnstructuredList{}
			if err := yaml.Unmarshal(raw, list); err != nil {
				errs.add(fmt.Errorf("failed to deserialize %s into a list: %w", path, err))
				return
			}

			lock.Lock()
			defer lock.Unlock()
			lists = append(lists, list)
		}()
	}
	wg.Wait()

	if err := utilerrors.NewAggregate(errs.errs); err != nil {
		// If there is no result, bail out. If we have both errors and results, just
		// log the error but continue.
		if len(lists) == 0 {
			http.Error(w, fmt.Sprintf("failed to get data: %v", err), http.StatusInternalServerError)
			return
		} else {
			l.Error("Encountered errors when reading data", zap.Error(err))
		}
	}

	if len(lists) == 0 {
		return
	}

	result := &unstructured.UnstructuredList{}
	result.SetGroupVersionKind(lists[0].GroupVersionKind())
	for _, list := range lists {
		result.Items = append(result.Items, list.Items...)
	}

	transformed, err := transformIfNeeded(result, transform)
	if err != nil {
		http.Error(w, fmt.Sprintf("transform failed: %v", err), http.StatusInternalServerError)
		return
	}

	serializeAndWrite(l, w, transformed)
}

func loggingMiddleware(l *zap.Logger) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			m := httpsnoop.CaptureMetrics(next, w, r)
			l.Info("Processed request",
				zap.String("method", r.Method),
				zap.String("url", r.URL.String()),
				zap.Int("status", m.Code),
				zap.String("duration", m.Duration.String()),
			)
		})
	}
}
