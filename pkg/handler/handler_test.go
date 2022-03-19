package handler_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
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
