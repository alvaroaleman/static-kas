package transform

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"

	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/printers"
	"k8s.io/kubernetes/pkg/printers/internalversion"

	_ "k8s.io/kubernetes/pkg/apis/admission/install"
	_ "k8s.io/kubernetes/pkg/apis/admissionregistration/install"
	_ "k8s.io/kubernetes/pkg/apis/apps/install"
	_ "k8s.io/kubernetes/pkg/apis/authentication/install"
	_ "k8s.io/kubernetes/pkg/apis/authorization/install"
	_ "k8s.io/kubernetes/pkg/apis/autoscaling/install"
	_ "k8s.io/kubernetes/pkg/apis/batch/install"
	_ "k8s.io/kubernetes/pkg/apis/certificates/install"
	_ "k8s.io/kubernetes/pkg/apis/coordination/install"
	_ "k8s.io/kubernetes/pkg/apis/events/install"
	_ "k8s.io/kubernetes/pkg/apis/extensions/install"
	_ "k8s.io/kubernetes/pkg/apis/flowcontrol/install"
	_ "k8s.io/kubernetes/pkg/apis/networking/install"
	_ "k8s.io/kubernetes/pkg/apis/node/install"
	_ "k8s.io/kubernetes/pkg/apis/policy/install"
	_ "k8s.io/kubernetes/pkg/apis/rbac/install"
	_ "k8s.io/kubernetes/pkg/apis/scheduling/install"
	_ "k8s.io/kubernetes/pkg/apis/storage/install"
)

func newInTreeHandler(l *zap.Logger) *printHandler {
	ph := &printHandler{log: l}
	internalversion.AddHandlers(ph)
	return ph
}

type handlerEntry struct {
	columnDefinitions []metav1.TableColumnDefinition
	printFunc         reflect.Value
}

type printHandler struct {
	handlers map[reflect.Type]handlerEntry
	log      *zap.Logger
}

func (ph *printHandler) TableHandler(columns []metav1.TableColumnDefinition, printFunc interface{}) error {
	if ph.handlers == nil {
		ph.handlers = map[reflect.Type]handlerEntry{}
	}
	printFuncVal := reflect.ValueOf(printFunc)
	printFuncType := printFuncVal.Type()

	// Key the handlers by the type of the first argument of the printFunc
	ph.handlers[printFuncType.In(0)] = handlerEntry{
		columnDefinitions: columns,
		printFunc:         printFuncVal,
	}
	return nil
}

func (ph *printHandler) transformFunc(tableVersion string, fallback TransformFunc) TransformFunc {
	return func(o runtime.Object) (*metav1.Table, error) {
		res, err := ph.printInternal(tableVersion, o)
		if err != nil {
			if gvk := o.GetObjectKind().GroupVersionKind(); scheme.Scheme.Recognizes(gvk) {
				ph.log.Warn("Internal conversion failed but kubernetes scheme recognizes gvk - missing imports?", zap.String("gvk", gvk.String()), zap.Error(err))
			}
			return fallback(o)
		}

		if res == nil {
			return fallback(o)
		}

		return res, nil
	}
}

func (ph *printHandler) printInternal(tableVersion string, o runtime.Object) (*metav1.Table, error) {
	internalVersion, err := legacyscheme.Scheme.New(schema.GroupVersionKind{Group: o.GetObjectKind().GroupVersionKind().Group, Kind: o.GetObjectKind().GroupVersionKind().Kind, Version: runtime.APIVersionInternal})
	if err != nil {
		return nil, fmt.Errorf("failed to get object from scheme for internal version: %w", err)
	}
	handler, ok := ph.handlers[reflect.TypeOf(internalVersion)]
	if !ok {
		return nil, nil
	}
	externalVersion, err := legacyscheme.Scheme.New(o.GetObjectKind().GroupVersionKind())
	if err != nil {
		return nil, fmt.Errorf("failed ton get object from scheme for external version: %w", err)
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return nil, fmt.Errorf("failed to unmarshal: %w", err)
	}
	if err := json.Unmarshal(raw, externalVersion); err != nil {
		return nil, fmt.Errorf("failed to marshal into external version %T: %w", externalVersion, err)
	}
	if err := legacyscheme.Scheme.Convert(externalVersion, internalVersion, nil); err != nil {
		return nil, fmt.Errorf("failed to convert to internal version: %w", err)
	}

	generateOpts := printers.GenerateOptions{Wide: true}
	result := handler.printFunc.Call([]reflect.Value{
		reflect.ValueOf(internalVersion),
		reflect.ValueOf(generateOpts),
	})
	rowsVal, errVal := result[0], result[1]
	if v := errVal.Interface(); v != nil {
		err := v.(error)
		return nil, fmt.Errorf("printFunc failed: %w", err)
	}
	rows, ok := rowsVal.Interface().([]metav1.TableRow)
	if !ok {
		return nil, fmt.Errorf("printfunc didn't return tablerows, but %T", rowsVal.Interface())
	}
	for idx := range rows {
		// We have to convert the embedded object back to the external version
		gvk := o.GetObjectKind().GroupVersionKind()
		gvk.Kind = strings.TrimSuffix(gvk.Kind, "List")
		externalVersion, _ := legacyscheme.Scheme.New(gvk)
		if err := legacyscheme.Scheme.Convert(rows[idx].Object.Object, externalVersion, nil); err != nil {
			return nil, fmt.Errorf("failed to convert embedded object to external version: %w", err)
		}
		externalVersion.(gvkSetter).SetGroupVersionKind(gvk)
		rows[idx].Object = runtime.RawExtension{Object: externalVersion}
	}
	return &metav1.Table{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "meta.k8s.io/" + tableVersion,
			Kind:       "Table",
		},
		ColumnDefinitions: handler.columnDefinitions,
		Rows:              rows,
	}, nil
}

type gvkSetter interface {
	SetGroupVersionKind(gvk schema.GroupVersionKind)
}
