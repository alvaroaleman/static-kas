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
	// Send a BOOKMARK so clients using WatchList (sendInitialEvents=true)
	// know the initial list is complete.
	if r.URL.Query().Get("sendInitialEvents") == "true" {
		// KEP-3157: client-go >=0.32 collapses List+Watch into a single
		// watch with sendInitialEvents=true. The reflector expects a
		// BOOKMARK with this annotation to mark the end of the initial
		// synthetic list — without it, it waits ~10s then silently gives
		// up showing zero results. Unstructured because the bookmark
		// carries no real data and we don't know the concrete type here.
		// ResourceVersion "0": static-kas serves a snapshot, no real
		// version ordering to preserve.
		bookmark := &unstructured.Unstructured{}
		bookmark.SetResourceVersion("0")
		bookmark.SetAnnotations(map[string]string{"k8s.io/initial-events-end": "true"})
		if err := writeJSON(&metav1.WatchEvent{Type: "BOOKMARK", Object: runtime.RawExtension{Object: bookmark}}, w); err != nil {
			return fmt.Errorf("failed to write bookmark: %w", err)
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
