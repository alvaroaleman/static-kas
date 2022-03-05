package response

import (
	"encoding/json"
	"net/http"

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
