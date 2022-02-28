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

	"github.com/gorilla/mux"
	"go.uber.org/zap"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

	l, err := zap.NewProduction()
	if err != nil {
		fmt.Printf("failed to construct logger: %v\n", err)
		os.Exit(1)
	}
	defer l.Sync()

	if o.baseDir == "" {
		l.Fatal("--base-dir is mandatory")
	}

	l.Info("Discovering api resources")
	groupResourceListMap, err := discover(o.baseDir)
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
	router.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) {
		d := metav1.APIVersions{TypeMeta: metav1.TypeMeta{Kind: "APIVersions"}, Versions: []string{"v1"}}
		serializeAndWite(l, w, d)
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
		servePath(path, l, w, transformFunc)
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
			serializeAndWite(l, w, allNamespaces)
			return
		}
		path := path.Join(o.baseDir, "cluster-scoped-resources", "core", vars["resource"]+".yaml")
		servePath(path, l, w, nil)
	})
	router.HandleFunc("/api/v1/{resource}/{name}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
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
		path := path.Join(o.baseDir, "namespaces", vars["namespace"], vars["group"], vars["resource"]+".yaml")
		servePath(path, l, w, nil)
	})
	router.HandleFunc("/apis/{group}/{version}/namespaces/{namespace}/{resource}/{name}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		path := path.Join(o.baseDir, "namespaces", vars["namespace"], vars["group"], vars["resource"]+".yaml")
		serveNamedObjectFromPath(path, l, w, vars["name"], nil)
	})
	router.HandleFunc("/apis/{group}/{version}/{resource}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		path := path.Join(o.baseDir, "cluster-scoped-resources", vars["group"], vars["resource"]+".yaml")
		servePath(path, l, w, nil)
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

func servePath(path string, l *zap.Logger, w http.ResponseWriter, transform transform.TransformFunc) {
	raw, err := ioutil.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			l.Info("file not found", zap.String("path", path))
			w.WriteHeader(404)
			serializeAndWite(l, w, unstructured.UnstructuredList{})
			return
		}
		http.Error(w, fmt.Sprintf("failed to read %s: %v", path, err), http.StatusInternalServerError)
		return
	}

	if transform == nil {
		transform = defaultTransform
	}

	result, err := transform(raw)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to transform: %v", err), http.StatusInternalServerError)
		return
	}

	serializeAndWite(l, w, result)
}

func serveNamedObjectFromPath(path string, l *zap.Logger, w http.ResponseWriter, name string, transform transform.TransformFunc) {
	raw, err := ioutil.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			l.Info("file not found", zap.String("path", path))
			w.WriteHeader(404)
			serializeAndWite(l, w, unstructured.UnstructuredList{})
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
		serializeAndWite(l, w, obj)
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

func serializeAndWite(l *zap.Logger, w http.ResponseWriter, data interface{}) {
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
