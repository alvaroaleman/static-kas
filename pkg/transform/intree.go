package transform

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	"go.uber.org/zap"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/scheme"

	"k8s.io/kube-aggregator/pkg/apis/apiregistration"
	apiservicerest "k8s.io/kube-aggregator/pkg/registry/apiservice/etcd"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/printers"
	"k8s.io/kubernetes/pkg/printers/internalversion"

	apiregistrationinstall "k8s.io/kube-aggregator/pkg/apis/apiregistration/install"
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

	apiregistrationinstall.Install(legacyscheme.Scheme)
	apiServiceRest := &apiservicerest.REST{}
	apiServiceTable, err := apiServiceRest.ConvertToTable(context.Background(), &apiregistration.APIService{}, nil)
	if err != nil {
		panic(err)
	}
	apiServiceHandler := handlerEntry{
		columnDefinitions: apiServiceTable.ColumnDefinitions,
		printFunc:         reflect.ValueOf(apiServiceRest).MethodByName("ConvertToTable"),
	}
	ph.handlers[reflect.TypeOf(&apiregistration.APIService{})] = apiServiceHandler
	ph.handlers[reflect.TypeOf(&apiregistration.APIServiceList{})] = apiServiceHandler
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
			ph.log.Error("Internal printer errored", zap.Error(err))
			return fallback(o)
		}

		if res == nil {
			if gvk := o.GetObjectKind().GroupVersionKind(); scheme.Scheme.Recognizes(gvk) {
				ph.log.Warn("No in-tree tableprinter but kubernetes scheme recognizes gvk - missing imports?", zap.String("gvk", gvk.String()), zap.Error(err))
			}
			return fallback(o)
		}

		return res, nil
	}
}

// printInternal prints using an imported table printer. Because the tableprinters act on the internal version, we have to:
// * Convert into the internal version
// * Call the printfunc using reflect (The printfuncs are given to us as a slice of type Any)
// * Convert the object that is part of the row from the internal version to the external version and set the GVK along
//   the way, because:
//    * Kubectl will refuse the entire list if any of the object keys does not have GVK set
//    * Kubectl infers the namespace in case of namespaced objects from the embedded object, so we can not just omit it
func (ph *printHandler) printInternal(tableVersion string, o runtime.Object) (*metav1.Table, error) {
	internalVersion, err := legacyscheme.Scheme.New(schema.GroupVersionKind{Group: o.GetObjectKind().GroupVersionKind().Group, Kind: o.GetObjectKind().GroupVersionKind().Kind, Version: runtime.APIVersionInternal})
	if err != nil {
		return nil, nil
	}
	handler, ok := ph.handlers[reflect.TypeOf(internalVersion)]
	if !ok {
		ph.log.Info("Found no handler for type", zap.String("type", fmt.Sprintf("%T", internalVersion)))
		return nil, nil
	}
	ph.log.Info("Found handler for type", zap.String("type", fmt.Sprintf("%T", internalVersion)))
	if err := legacyscheme.Scheme.Convert(o, internalVersion, nil); err != nil {
		return nil, fmt.Errorf("failed to convert to internal version: %w", err)
	}

	generateOpts := printers.GenerateOptions{Wide: true}
	var result []reflect.Value
	switch argCount := reflect.TypeOf(handler.printFunc.Interface()).NumIn(); argCount {
	case 2:
		result = handler.printFunc.Call([]reflect.Value{
			reflect.ValueOf(internalVersion),
			reflect.ValueOf(generateOpts),
		})
	case 3:
		result = handler.printFunc.Call([]reflect.Value{
			reflect.ValueOf(context.Background()),
			reflect.ValueOf(internalVersion),
			// Can be literally anything, but needs a concrete type, ValueOf(runtime.Object(nil)
			// results in a zero reflect.Value.
			reflect.Zero(reflect.TypeOf(&unstructured.Unstructured{})),
		})
	default:
		ph.log.Error("Unexpected argument count for print func, exepected to or three", zap.Int("arg_count", argCount))
		return nil, nil

	}
	rowsVal, errVal := result[0], result[1]
	if v := errVal.Interface(); v != nil {
		err := v.(error)
		return nil, fmt.Errorf("printFunc failed: %w", err)
	}
	var rows []metav1.TableRow
	switch v := rowsVal.Interface().(type) {
	case []metav1.TableRow:
		rows = v
	case *metav1.Table:
		rows = v.Rows
	default:
		return nil, fmt.Errorf("printFunc returned unexepcted type %T", v)
	}
	for idx := range rows {
		// We have to convert the embedded object back to the external version
		gvk := o.GetObjectKind().GroupVersionKind()
		gvk.Kind = strings.TrimSuffix(gvk.Kind, "List")
		externalVersion, err := legacyscheme.Scheme.New(gvk)
		if err != nil {
			return nil, fmt.Errorf("failed to get an object from scheme for %s: %w", gvk, err)
		}
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
