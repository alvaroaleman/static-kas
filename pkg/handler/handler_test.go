package handler_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/alvaroaleman/static-kas/pkg/handler"
)

func init() {
	klog.InitFlags(flag.CommandLine)
}

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

	c, err := client.New(
		&rest.Config{
			Host: "http://127.0.0.1:8080",
			// Prevent controller-runtime from defaulting to proto
			ContentConfig: rest.ContentConfig{ContentType: "application/json"},
		},
		client.Options{},
	)
	if err != nil {
		t.Fatalf("failed to construct client: %v", err)
	}

	if err := rbacv1.AddToScheme(c.Scheme()); err != nil {
		t.Fatalf("failed to add rbacv1 to client scheme: %v", err)
	}

	tcs := []struct {
		name string
		run  func(*testing.T)
	}{
		{
			name: "List cluster-scoped core resource",
			run:  verifyList(ctx, c, &corev1.NodeList{}, 1),
		},
		{
			name: "List cluster-scoped core resoruce with label selector, match",
			run:  verifyList(ctx, c, &corev1.NodeList{}, 1, client.MatchingLabels{"beta.kubernetes.io/arch": "amd64"}),
		},
		{
			name: "List cluster-scoped core resoruce with label selector, no match",
			run:  verifyList(ctx, c, &corev1.NodeList{}, 0, client.MatchingLabels{"beta.kubernetes.io/arch": "other"}),
		},
		{
			name: "List cluster-scoped core resoruce with field selector, match",
			run:  verifyList(ctx, c, &corev1.NodeList{}, 1, client.MatchingFields{"metadata.name": "ip-10-0-143-10.ec2.internal"}),
		},
		{
			name: "List cluster-scoped core resource with field selector, no match",
			run:  verifyList(ctx, c, &corev1.NodeList{}, 0, client.MatchingFields{"metadata.name": "other"}),
		},
		{
			name: "List namespaced core resource from namespace",
			run:  verifyList(ctx, c, &corev1.PodList{}, 1, client.InNamespace("openshift-network-operator")),
		},
		{
			name: "List namespaced core resource from namespace with matching label selector",
			run:  verifyList(ctx, c, &corev1.PodList{}, 1, client.InNamespace("openshift-network-operator"), client.MatchingLabels{"name": "network-operator"}),
		},
		{
			name: "List namespaced core resource from namespace with non-matching label selector",
			run:  verifyList(ctx, c, &corev1.PodList{}, 0, client.InNamespace("openshift-network-operator"), client.MatchingLabels{"name": "other"}),
		},
		{
			name: "List namespaced core resource from namespace with matching field selector",
			run:  verifyList(ctx, c, &corev1.PodList{}, 1, client.InNamespace("openshift-network-operator"), client.MatchingFields{"metadata.name": "network-operator-7887564c4-mjg9d"}),
		},
		{
			name: "List namespaced core resource from namespace with non-matching field selector",
			run:  verifyList(ctx, c, &corev1.PodList{}, 0, client.InNamespace("openshift-network-operator"), client.MatchingFields{"metadata.name": "other"}),
		},
		{
			name: "List namespaced core object from all namespaces",
			run:  verifyList(ctx, c, &corev1.PodList{}, 2),
		},
		{
			name: "List namespaced core object from all namespaces with label selector matching one",
			run:  verifyList(ctx, c, &corev1.PodList{}, 1, client.MatchingLabels{"name": "network-operator"}),
		},
		{
			name: "List namespaced core object from all namespaces with field selector matching one",
			run:  verifyList(ctx, c, &corev1.PodList{}, 1, client.MatchingFields{"metadata.name": "network-operator-7887564c4-mjg9d"}),
		},
		{
			name: "List cluster-scoped non-core resource",
			run:  verifyList(ctx, c, &rbacv1.ClusterRoleBindingList{}, 1),
		},
		{
			name: "List cluster-scoped non-core resource with matching label selector",
			run:  verifyList(ctx, c, &rbacv1.ClusterRoleBindingList{}, 1, client.MatchingLabels{"foo": "bar"}),
		},
		{
			name: "List cluster-scoped non-core resource with non-matching label selector",
			run:  verifyList(ctx, c, &rbacv1.ClusterRoleBindingList{}, 0, client.MatchingLabels{"foo": "other"}),
		},
		{
			name: "List cluster-scoped non-core resource with matching field selector",
			run:  verifyList(ctx, c, &rbacv1.ClusterRoleBindingList{}, 1, client.MatchingFields{"metadata.name": "network-diagnostics"}),
		},
		{
			name: "List cluster-scoped non-core resource with non-matching field selector",
			run:  verifyList(ctx, c, &rbacv1.ClusterRoleBindingList{}, 0, client.MatchingFields{"metadata.name": "other"}),
		},
		{
			name: "List namespaced non-core resource from namespace",
			run:  verifyList(ctx, c, &appsv1.DeploymentList{}, 1, client.InNamespace("openshift-network-operator")),
		},
		{
			name: "List namespaced non-core resource from namespace with matching label selector",
			run:  verifyList(ctx, c, &appsv1.DeploymentList{}, 1, client.InNamespace("openshift-network-operator"), client.MatchingLabels{"name": "network-operator"}),
		},
		{
			name: "List namespaced non-core resource from namespace with non-matching label selector",
			run:  verifyList(ctx, c, &appsv1.DeploymentList{}, 0, client.InNamespace("openshift-network-operator"), client.MatchingLabels{"name": "other"}),
		},
		{
			name: "List namespaced non-core resource from namespace with matching field selector",
			run:  verifyList(ctx, c, &appsv1.DeploymentList{}, 1, client.InNamespace("openshift-network-operator"), client.MatchingFields{"metadata.name": "network-operator"}),
		},
		{
			name: "List namespaced non-core resource from namespace with non-matching label selector",
			run:  verifyList(ctx, c, &appsv1.DeploymentList{}, 0, client.InNamespace("openshift-network-operator"), client.MatchingFields{"metadata.name": "other"}),
		},
		{
			name: "List namespaced non-core resource from all namespaces",
			run:  verifyList(ctx, c, &appsv1.DeploymentList{}, 2),
		},
		{
			name: "List namespaced non-core resource from all namespaces with label selector matching one object",
			run:  verifyList(ctx, c, &appsv1.DeploymentList{}, 1, client.MatchingLabels{"name": "network-operator"}),
		},
		{
			name: "List namespaced non-core resource from all namespaces with field selector matching one object",
			run:  verifyList(ctx, c, &appsv1.DeploymentList{}, 1, client.MatchingFields{"metadata.name": "network-operator"}),
		},
		{
			// These are special because they are not in the dump
			name: "Listing namespaces",
			run:  verifyList(ctx, c, &corev1.NamespaceList{}, 3),
		},
		{
			name: "List pods table printing",
			run:  verifyTablePrinting(ctx, "/api/v1/pods", 9, 2),
		},
		{
			name: "Get pod table printing",
			run:  verifyTablePrinting(ctx, "/api/v1/namespaces/openshift-network-operator/pods/network-operator-7887564c4-mjg9d", 9, 1),
		},
		{
			name: "List replicasets table printing",
			run:  verifyTablePrinting(ctx, "/apis/apps/v1/replicasets", 8, 2),
		},
		{
			name: "Get replicaset table printing",
			run:  verifyTablePrinting(ctx, "/apis/apps/v1/namespaces/openshift-network-operator/replicasets/network-operator-7887564c4", 8, 1),
		},
		{
			name: "List deployments table printing",
			run:  verifyTablePrinting(ctx, "/apis/apps/v1/deployments", 8, 2),
		},
		{
			name: "Get deployments table printing",
			run:  verifyTablePrinting(ctx, "/apis/apps/v1/namespaces/openshift-network-operator/deployments/network-operator", 8, 1),
		},
		{
			name: "List statefulsets table printing",
			run:  verifyTablePrinting(ctx, "/apis/apps/v1/statefulsets", 5, 2),
		},
		{
			name: "Get statefulsets table printing",
			run:  verifyTablePrinting(ctx, "/apis/apps/v1/namespaces/openshift-monitoring/statefulsets/prometheus-k8s", 5, 1),
		},
		{
			name: "List daemonsets table printing",
			run:  verifyTablePrinting(ctx, "/apis/apps/v1/daemonsets", 11, 1),
		},
		{
			name: "Get daemonsets table printing",
			run:  verifyTablePrinting(ctx, "/apis/apps/v1/namespaces/openshift-monitoring/daemonsets/node-exporter", 11, 1),
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

func verifyTablePrinting(ctx context.Context, path string, expectNumColumns int, expectNumRows int) func(t *testing.T) {
	return func(t *testing.T) {
		table, err := requestTableOnPath(ctx, path)
		if err != nil {
			t.Fatalf("failed to get table for %s: %v", path, err)
		}
		if n := len(table.ColumnDefinitions); n != expectNumColumns {
			t.Errorf("expected %d columns, got %d", expectNumColumns, n)
		}
		if n := len(table.Rows); n != expectNumRows {
			t.Errorf("expected to get %d rows back, got %d", expectNumRows, n)
		}
	}
}

func verifyList(ctx context.Context, c client.Client, list client.ObjectList, numExpected int, listOpts ...client.ListOption) func(t *testing.T) {
	return func(t *testing.T) {
		if err := c.List(ctx, list, listOpts...); err != nil {
			t.Fatalf("failed to list %T: %v", list, err)
		}
		items, err := apimeta.ExtractList(list)
		if err != nil {
			t.Fatalf("failed to extract items from list: %v", err)
		}
		if n := len(items); n != numExpected {
			t.Errorf("expected to get %T with exactly %d items back, got %d", list, numExpected, n)
		}
	}
}
