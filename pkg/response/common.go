package response

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/alvaroaleman/static-kas/pkg/transform"
)

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

func writeJSON(data interface{}, w http.ResponseWriter) error {
	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(data)
}
