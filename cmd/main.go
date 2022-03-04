package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/gorilla/mux"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/felixge/httpsnoop"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/alvaroaleman/static-kas/pkg/filter"
	"github.com/alvaroaleman/static-kas/pkg/response"
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
	groupResourceListMap, groupResourceMap, err := discover(l, o.baseDir)
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
	router.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{}`))
	})
	router.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) {
		d := metav1.APIVersions{TypeMeta: metav1.TypeMeta{Kind: "APIVersions"}, Versions: []string{"v1"}}
		serializeAndWrite(l, w, d)
	}).Methods(http.MethodGet)
	router.HandleFunc("/api/v1", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(groupSerializedResourceListMap["v1"])
	}).Methods(http.MethodGet)
	router.HandleFunc("/api/v1/namespaces/{namespace}/{resource}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		path := path.Join(o.baseDir, "namespaces", vars["namespace"], "core")
		var transformFunc func([]byte) (interface{}, error)
		if acceptsTable(r) {
			transformFunc = tableTransformMap[transform.TransformEntryKey{ResourceName: vars["resource"], Verb: transform.VerbList}]
		}
		if err := response.NewListResponse(w, path, vars["resource"], transformFunc, filter.FromRequest(r)...); err != nil {
			l.Error("failed to respond", zap.Error(err))
		}
	}).Methods(http.MethodGet)
	router.HandleFunc("/api/v1/namespaces/{namespace}/{resource}/{name}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		var transformFunc func([]byte) (interface{}, error)
		if acceptsTable(r) {
			transformFunc = tableTransformMap[transform.TransformEntryKey{ResourceName: vars["resource"], Verb: transform.VerbGet}]
		}
		path := path.Join(o.baseDir, "namespaces", vars["namespace"], "core")
		if err := response.NewGetResponse(w, path, vars["resource"], vars["name"], transformFunc); err != nil {
			l.Error("failed to respond", zap.Error(err))
		}
	}).Methods(http.MethodGet)
	router.HandleFunc("/api/v1/namespaces/{namespace}/pods/{name}/log", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		containerName := r.URL.Query().Get("container")
		fileName := "current.log"
		hypershiftSuffix := ".log"
		if r.URL.Query().Get("previous") == "true" {
			fileName = "previous.log"
			hypershiftSuffix = "-previous.log"
		}
		paths := []string{
			path.Join(o.baseDir, "namespaces", vars["namespace"], "pods", vars["name"], containerName, containerName, "logs", fileName),
			path.Join(o.baseDir, "namespaces", vars["namespace"], "core", "pods", "logs", vars["name"]+"-"+containerName+hypershiftSuffix),
		}
		f, err := openFirstFound(paths)
		if err != nil {
			w.WriteHeader(404)
			w.Write([]byte(fmt.Sprintf("failed to open one of %v: %v", paths, err)))
			return
		}
		defer f.Close()
		io.Copy(w, f)
	}).Methods(http.MethodGet)
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
			if err := response.NewCrossNamespaceListResponse(w, filepath.Join(o.baseDir, "namespaces"), "core", vars["resource"], transformFunc); err != nil {
				l.Error("failed to respond", zap.Error(err))
			}
		} else {
			path := path.Join(o.baseDir, "cluster-scoped-resources", "core")
			if err := response.NewListResponse(w, path, vars["resource"], transformFunc, filter.FromRequest(r)...); err != nil {
				l.Error("failed to respond", zap.Error(err))
			}
		}
	}).Methods(http.MethodGet)
	router.HandleFunc("/api/v1/{resource}/{name}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		if vars["resource"] == "namespaces" {
			serveNamespace(l, w, &allNamespaces, vars["name"])
			return
		}
		var transformFunc func([]byte) (interface{}, error)
		if acceptsTable(r) {
			transformFunc = tableTransformMap[transform.TransformEntryKey{ResourceName: vars["resource"], Verb: transform.VerbList}]
		}
		path := path.Join(o.baseDir, "cluster-scoped-resources", "core")
		if err := response.NewGetResponse(w, path, vars["resource"], vars["name"], transformFunc); err != nil {
			l.Error("failed to respond", zap.Error(err))
		}
	}).Methods(http.MethodGet)
	router.HandleFunc("/apis", func(w http.ResponseWriter, _ *http.Request) {
		w.Write(serializedGroupList)
	}).Methods(http.MethodGet)
	for groupVersion := range groupSerializedResourceListMap {
		groupVersion := groupVersion
		router.HandleFunc("/apis/"+groupVersion, func(w http.ResponseWriter, _ *http.Request) {
			w.Write(groupSerializedResourceListMap[groupVersion])
		}).Methods(http.MethodGet)
	}
	router.HandleFunc("/apis/{group}/{version}/namespaces/{namespace}/{resource}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		var transformFunc func([]byte) (interface{}, error)
		if acceptsTable(r) {
			transformFunc = tableTransformMap[transform.TransformEntryKey{GroupName: vars["group"], ResourceName: vars["resource"], Verb: transform.VerbList}]
		}
		path := path.Join(o.baseDir, "namespaces", vars["namespace"], vars["group"])
		if err := response.NewListResponse(w, path, vars["resource"], transformFunc, filter.FromRequest(r)...); err != nil {
			l.Error("failed to respond", zap.Error(err))
		}
	}).Methods(http.MethodGet)
	router.HandleFunc("/apis/{group}/{version}/namespaces/{namespace}/{resource}/{name}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		var transformFunc func([]byte) (interface{}, error)
		if acceptsTable(r) {
			transformFunc = tableTransformMap[transform.TransformEntryKey{GroupName: vars["group"], ResourceName: vars["resource"], Verb: transform.VerbGet}]
		}
		path := path.Join(o.baseDir, "namespaces", vars["namespace"], vars["group"])
		if err := response.NewGetResponse(w, path, vars["resource"], vars["name"], transformFunc); err != nil {
			l.Error("failed to respond", zap.Error(err))
		}
	}).Methods(http.MethodGet)
	router.HandleFunc("/apis/authorization.k8s.io/{version}/selfsubjectaccessreviews", func(w http.ResponseWriter, r *http.Request) {
		handleSSAR(l.With(zap.String("path", r.URL.Path)), w, r)
	}).Methods(http.MethodPost)
	router.HandleFunc("/apis/{group}/{version}/{resource}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		if vars["group"] == "authorization.k8s.io" && vars["resource"] == "selfsubjectaccessreviews" {
			http.Error(w, "this endpoint only supports POST", http.StatusMethodNotAllowed)
			return
		}
		var transformFunc func([]byte) (interface{}, error)
		if acceptsTable(r) {
			transformFunc = tableTransformMap[transform.TransformEntryKey{GroupName: vars["group"], ResourceName: vars["resource"], Verb: transform.VerbList}]
		}
		if groupResourceMap[groupVersionResource{groupVersion: vars["group"] + "/" + vars["version"], resource: vars["resource"]}].Namespaced {
			if err := response.NewCrossNamespaceListResponse(w, filepath.Join(o.baseDir, "namespaces"), vars["group"], vars["resource"], transformFunc); err != nil {
				l.Error("failed to respond", zap.Error(err))
			}
		} else {
			path := path.Join(o.baseDir, "cluster-scoped-resources", vars["group"])
			if err := response.NewListResponse(w, path, vars["resource"], transformFunc, filter.FromRequest(r)...); err != nil {
				l.Error("failed to respond", zap.Error(err))
			}
		}
	}).Methods(http.MethodGet)
	router.HandleFunc("/apis/{group}/{version}/{resource}/{name}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		l := l.With(zap.String("path", r.URL.Path))
		path := path.Join(o.baseDir, "cluster-scoped-resources", vars["group"])
		var transformFunc func([]byte) (interface{}, error)
		if acceptsTable(r) {
			transformFunc = tableTransformMap[transform.TransformEntryKey{GroupName: vars["group"], ResourceName: vars["resource"], Verb: transform.VerbList}]
		}
		if err := response.NewGetResponse(w, path, vars["resource"], vars["name"], transformFunc); err != nil {
			l.Error("failed to respond", zap.Error(err))
		}
	}).Methods(http.MethodGet)

	// Re-Define the error handlers so they go through the middleware
	router.NotFoundHandler = router.NewRoute().HandlerFunc(http.NotFound).GetHandler()
	router.MethodNotAllowedHandler = router.NewRoute().HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "", http.StatusMethodNotAllowed) }).GetHandler()

	if err := http.ListenAndServe(":8080", router); err != nil {
		l.Error("server ended", zap.Error(err))
	}
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
	var ssar authorizationv1.SelfSubjectAccessReview
	if err := json.NewDecoder(r.Body).Decode(&ssar); err != nil {
		w.WriteHeader(400)
		w.Write([]byte(fmt.Sprintf("failed to decode request body: %v", err)))
		return
	}
	ssar.Status.Allowed = true
	w.Header().Set("Content-Type", "application/json")
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

func openFirstFound(paths []string) (*os.File, error) {
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		return f, nil
	}

	return nil, os.ErrNotExist
}
