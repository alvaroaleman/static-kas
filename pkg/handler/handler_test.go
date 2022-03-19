package handler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/alvaroaleman/static-kas/pkg/handler"
)

func TestServer(t *testing.T) {
	handler, err := handler.New(zaptest.NewLogger(t), "./testdata")
	if err != nil {
		t.Fatalf("failed to construct server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	serverDone := make(chan struct{})
	server := &http.Server{Addr: "127.0.0.1:8080", Handler: handler}
	t.Cleanup(func() {
		cancel()
		server.Shutdown(ctx)
		<-serverDone
	})
	go func() {
		defer close(serverDone)
		server.ListenAndServe()
		cancel()
	}()

	startTimer := time.NewTicker(5 * time.Second)
	defer startTimer.Stop()
	for {
		time.Sleep(25 * time.Millisecond)
		select {
		case <-startTimer.C:
			t.Fatal("timed out waiting for server to be up")
		default:
			resp, err := http.Get("http://127.0.0.1:8080/version")
			if err != nil {
				t.Logf("encountered error when checking if server is up: %v", err)
				continue
			}
			if resp.StatusCode != 200 {
				t.Logf("Got a non-200 statuscode of %d when checking if server is up", resp.StatusCode)
				continue
			}
			startTimer.Stop()
		}
		break
	}

	c, err := client.New(&rest.Config{Host: "http://127.0.0.1:8080"}, client.Options{})
	if err != nil {
		t.Fatalf("failed to construct client: %v", err)
	}

	tcs := []struct {
		name string
		run  func(*testing.T)
	}{
		{
			name: "List cluster-scoped core resource",
			run: func(t *testing.T) {
				list := unstructuredListFor("v1", "Node")
				if err := c.List(ctx, list); err != nil {
					t.Fatalf("failed to list nodes: %v", err)
				}
				if n := len(list.Items); n != 1 {
					t.Errorf("expected to get a nodelist with one item, got %d items", n)
				}
			},
		},
		{
			name: "List namespaced core resource from namespace",
			run: func(t *testing.T) {
				list := unstructuredListFor("v1", "Pod")
				if err := c.List(ctx, list, client.InNamespace("openshift-network-operator")); err != nil {
					t.Fatalf("failed to list pods from namespace openshift-network-operator: %v", err)
				}
				if n := len(list.Items); n != 1 {
					t.Errorf("expected to get exactly one pod back, got %d", n)
				}
			},
		},
		{
			name: "List namespaced core object from all namespaces",
			run: func(t *testing.T) {
				list := unstructuredListFor("v1", "Pod")
				if err := c.List(ctx, list); err != nil {
					t.Fatalf("failed to list pods: %v", err)
				}
				if n := len(list.Items); n != 2 {
					t.Errorf("expected to get exactly two pods back, got %d", n)
				}
			},
		},
		{
			name: "List cluster-scoped non-core resource",
			run: func(t *testing.T) {
				list := unstructuredListFor("rbac.authorization.k8s.io/v1", "ClusterRoleBinding")
				if err := c.List(ctx, list); err != nil {
					t.Fatalf("failed to list clusterrolebindings: %v", err)
				}
				if n := len(list.Items); n != 1 {
					t.Errorf("expected exactly one item, got %d", n)
				}
			},
		},
		{
			name: "List namespaced non-core resource from namespace",
			run: func(t *testing.T) {
				list := unstructuredListFor("apps/v1", "Deployment")
				if err := c.List(ctx, list, client.InNamespace("openshift-network-operator")); err != nil {
					t.Fatalf("failed to list deployments in namespace openshift-network-operator: %v", err)
				}
				if n := len(list.Items); n != 1 {
					t.Errorf("expected to get exactly one item, got %d", n)
				}
			},
		},
		{
			name: "List namespaced non-core resource from all namespaces",
			run: func(t *testing.T) {
				list := unstructuredListFor("apps/v1", "Deployment")
				if err := c.List(ctx, list); err != nil {
					t.Fatalf("failed to list deployments: %v", err)
				}
				if n := len(list.Items); n != 2 {
					t.Errorf("expected to get exactly two items, got %d", n)
				}
			},
		},
		{
			// These are special because they are not in the dump
			name: "Listing namespaces",
			run: func(t *testing.T) {
				list := unstructuredListFor("v1", "Namespace")
				if err := c.List(ctx, list); err != nil {
					t.Fatalf("failed to list namespaces: %v", err)
				}
				if n := len(list.Items); n != 2 {
					t.Errorf("expected to get exactly two namespacs back, got %d", n)
				}
			},
		},
		{
			name: "List pods table printing",
			run: func(t *testing.T) {
				table, err := requestTableOnPath(ctx, "/api/v1/pods")
				if err != nil {
					t.Fatalf("failed to get table for pods: %v", err)
				}
				if n := len(table.ColumnDefinitions); n != 9 {
					t.Errorf("expected nine columns, got %d", n)
				}
				if n := len(table.Rows); n != 2 {
					t.Errorf("expected to get two pods back, got %d", n)
				}
			},
		},
		{
			name: "Get pod table printing",
			run: func(t *testing.T) {
				table, err := requestTableOnPath(ctx, "/api/v1/namespaces/openshift-network-operator/pods/network-operator-7887564c4-mjg9d")
				if err != nil {
					t.Fatalf("failed to get table for replicaset: %v", err)
				}
				if n := len(table.ColumnDefinitions); n != 9 {
					t.Errorf("expected nine columns, got %d", n)
				}
				if n := len(table.Rows); n != 1 {
					t.Errorf("expected to get one replicaset back, got %d", n)
				}
			},
		},
		{
			name: "List replicasets table printing",
			run: func(t *testing.T) {
				table, err := requestTableOnPath(ctx, "/apis/apps/v1/replicasets")
				if err != nil {
					t.Fatalf("failed to get table for replicaset: %v", err)
				}
				if n := len(table.ColumnDefinitions); n != 8 {
					t.Errorf("expected eight columns, got %d", n)
				}
				if n := len(table.Rows); n != 2 {
					t.Errorf("expected to get two replicasets back, got %d", n)
				}
			},
		},
		{
			name: "Get replicaset table printing",
			run: func(t *testing.T) {
				table, err := requestTableOnPath(ctx, "/apis/apps/v1/namespaces/openshift-network-operator/replicasets/network-operator-7887564c4")
				if err != nil {
					t.Fatalf("failed to get table for replicaset: %v", err)
				}
				if n := len(table.ColumnDefinitions); n != 8 {
					t.Errorf("expected eight columns, got %d", n)
				}
				if n := len(table.Rows); n != 1 {
					t.Errorf("expected to get one replicaset back, got %d", n)
				}
			},
		},
	}

	for _, tc := range tcs {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.run(t)
		})
	}
}

func unstructuredListFor(apiVersion, kind string) *unstructured.UnstructuredList {
	u := &unstructured.UnstructuredList{}
	u.SetAPIVersion(apiVersion)
	u.SetKind(kind)

	return u
}

func requestTableOnPath(ctx context.Context, path string) (*metav1.Table, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:8080"+path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to construct request: %w", err)
	}
	req.Header.Set("Accept", "application/json;as=Table;v=v1;g=meta.k8s.io")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to do http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("got a non-200 status code of %d back", resp.StatusCode)
	}

	table := &metav1.Table{}
	if err := json.NewDecoder(resp.Body).Decode(table); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response into metav1.Table: %w", err)
	}

	return table, nil
}
