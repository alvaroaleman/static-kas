package response

import (
	"encoding/json"
	"fmt"
	"net/http"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/alvaroaleman/static-kas/pkg/transform"
)

func transformIfNeeded(object runtime.Object, transform transform.TransformFunc) (interface{}, error) {
	if transform == nil {
		return object, nil
	}
	return transform(object)
}

func writeJSON(data interface{}, w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(data)
}

func isWatch(r *http.Request) bool {
	return r.URL.Query().Get("watch") == "true"
}

func respondToWatch(r *http.Request, w http.ResponseWriter, objects ...runtime.Object) error {
	for _, item := range objects {
		if err := writeJSON(&metav1.WatchEvent{Type: "ADDED", Object: runtime.RawExtension{Object: item}}, w); err != nil {
			return fmt.Errorf("failed to write watch item: %w", err)
		}
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	<-r.Context().Done()

	return nil
}

func unstructuredListItemsToRuntimeObjects(l *unstructured.UnstructuredList) []runtime.Object {
	result := make([]runtime.Object, 0, len(l.Items))
	for idx := range l.Items {
		result = append(result, &l.Items[idx])
	}

	return result
}
