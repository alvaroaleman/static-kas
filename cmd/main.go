package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"path"

	"github.com/gorilla/mux"
	"go.uber.org/zap"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
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
	l.Info("Finished discovering api resources")

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
		serverPath(path, l, w)
	})
	router.HandleFunc("/api/v1/{resource}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		path := path.Join(o.baseDir, "cluster-scoped-resources", "core", vars["resource"]+".yaml")
		serverPath(path, l, w)
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
		serverPath(path, l, w)
	})
	router.HandleFunc("/apis/{group}/{version}/{resource}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		path := path.Join(o.baseDir, "cluster-scoped-resources", vars["group"], vars["resource"]+".yaml")
		serverPath(path, l, w)
	})

	if err := http.ListenAndServe(":8080", router); err != nil {
		l.Error("server ended", zap.Error(err))
	}
}

func serverPath(path string, l *zap.Logger, w http.ResponseWriter) {
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

	result := map[string]interface{}{}
	if err := yaml.Unmarshal(raw, &result); err != nil {
		http.Error(w, fmt.Sprintf("failed to deserialize contents of %s: %v", path, err), http.StatusInternalServerError)
		return
	}
	serializeAndWite(l, w, result)
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
