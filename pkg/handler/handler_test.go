package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"sort"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	appsv1 "k8s.io/api/apps/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	utilpointer "k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/cache"
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
			defer resp.Body.Close()
			bodyBytes, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Logf("encountered error when getting server version: %v", err)
				continue
			}
			var serverVersion map[string]string
			err = json.Unmarshal(bodyBytes, &serverVersion)
			if err != nil {
				t.Logf("encountered error when parsing server version: %v", err)
				continue
			}
			goVersion, ok := serverVersion["goVersion"]
			if !ok {
				t.Logf("encountered error when parsing server version: %v", err)
				continue
			}
			if goVersion != "go1.16.8" {
				t.Errorf("expected goVersion to be %q, was %q", "go1.16.8", goVersion)
			}
			startTimer.Stop()
		}
		break
	}

	cfg := &rest.Config{
		Host: "http://127.0.0.1:8080",
		// Prevent controller-runtime from defaulting to proto
		ContentConfig: rest.ContentConfig{ContentType: "application/json"},
	}

	c, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatalf("failed to construct controller-runtime client: %v", err)
	}
	corev1Client, err := corev1client.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("failed to construct corev1 client: %v", err)
	}
	cache, err := cache.New(cfg, cache.Options{Scheme: c.Scheme(), Mapper: c.RESTMapper()})
	if err != nil {
		t.Fatalf("failed to construct cache: %v", err)
	}
	go func() {
		cache.Start(ctx)
	}()
	if synced := cache.WaitForCacheSync(ctx); !synced {
		t.Fatalf("failed to watch for cache sync: %v", err)
	}

	if err := rbacv1.AddToScheme(c.Scheme()); err != nil {
		t.Fatalf("failed to add rbacv1 to client scheme: %v", err)
	}
	if err := authorizationv1.AddToScheme(c.Scheme()); err != nil {
		t.Fatalf("failed to add authorizationv1 to client scheme: %v", err)
	}

	tcs := []struct {
		name string
		run  func(*testing.T)
	}{
		{
			name: "Get cluster-scoped resource",
			run:  verifyGet(ctx, c, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "ip-10-0-143-10.ec2.internal"}}),
		},
		{
			name: "List cluster-scoped core resource",
			run:  verifyList(ctx, c, &corev1.NodeList{}, 1),
		},
		{
			name: "List cluster-scoped core resource from cache",
			run:  verifyList(ctx, cache, &corev1.NodeList{}, 1),
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
			name: "Get namespaced core resource",
			run:  verifyGet(ctx, c, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "openshift-network-operator", Name: "network-operator-7887564c4-mjg9d"}}),
		},
		{
			name: "List namespaced core resource from namespace",
			run:  verifyList(ctx, c, &corev1.PodList{}, 1, client.InNamespace("openshift-network-operator")),
		},
		{
			name: "List namespaced core resource from cache",
			run:  verifyList(ctx, cache, &corev1.PodList{}, 1, client.InNamespace("openshift-network-operator")),
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
			name: "Get cluster-scoped non-core resource",
			run:  verifyGet(ctx, c, &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "network-diagnostics"}}),
		},
		{
			name: "List cluster-scoped non-core resource",
			run:  verifyList(ctx, c, &rbacv1.ClusterRoleBindingList{}, 1),
		},
		{
			name: "List cluster-scoped non-core resource from cache",
			run:  verifyList(ctx, cache, &rbacv1.ClusterRoleBindingList{}, 1),
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
			name: "Get namespaced non-core resource",
			run:  verifyGet(ctx, c, &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: "openshift-network-operator", Name: "network-operator"}}),
		},
		{
			name: "List namespaced non-core resource from namespace",
			run:  verifyList(ctx, c, &appsv1.DeploymentList{}, 1, client.InNamespace("openshift-network-operator")),
		},
		{
			name: "List namespaced non-core resource from cache",
			run:  verifyList(ctx, cache, &appsv1.DeploymentList{}, 1, client.InNamespace("openshift-network-operator")),
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
			name: "Self-subject access review always gets approved",
			run: func(t *testing.T) {
				ssar := &authorizationv1.SelfSubjectAccessReview{}
				if err := c.Create(ctx, ssar); err != nil {
					t.Fatalf("failed to create self-subject access review: %v", err)
				}
				if !ssar.Status.Allowed {
					t.Error("expected ssar to be allowed, wasn't the case")
				}
			},
		},
		{
			// These are special because they are not in the dump
			name: "Get namespace",
			run:  verifyGet(ctx, c, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "openshift-network-operator"}}),
		},
		{
			// These are special because they are not in the dump
			name: "Listing namespaces",
			run:  verifyList(ctx, c, &corev1.NamespaceList{}, 6),
		},
		{
			name: "List when objects are stored as distinct files",
			run:  verifyList(ctx, c, unstructuredListFor("monitoring.coreos.com/v1", "ServiceMonitor"), 2),
		},
		{
			name: "List nodes table printing",
			run:  verifyTablePrinting(ctx, "/api/v1/nodes", 10, 1),
		},
		{
			name: "Get node table printing",
			run:  verifyTablePrinting(ctx, "/api/v1/nodes/ip-10-0-143-10.ec2.internal", 10, 1),
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
			name: "List deployments in namespace table printing",
			run:  verifyTablePrinting(ctx, "/apis/apps/v1/namespaces/openshift-network-operator/deployments", 8, 1),
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
		{
			name: "List tableprinting uses CRDs additionalPrinterColumns",
			run:  verifyTablePrinting(ctx, "/apis/config.openshift.io/v1/clusteroperators/console", 6, 1),
		},
		{
			name: "Get tableprinting uses CRDs additionalPrinterColumns",
			run:  verifyTablePrinting(ctx, "/apis/config.openshift.io/v1/clusteroperators", 6, 1),
		},
		{
			name: "List for CRD without CRD manifest returns valid table",
			run:  verifyTablePrinting(ctx, "/apis/network.openshift.io/v1/clusternetworks", 1, 1),
		},
		{
			name: "Get for CRD without CRD manifest returns valid table",
			run:  verifyTablePrinting(ctx, "/apis/network.openshift.io/v1/clusternetworks/default", 1, 1),
		},
		{
			name: "List services (Cilium sysdump list format)",
			run:  verifyList(ctx, c, &corev1.ServiceList{}, 2),
		},
		{
			name: "Get pod logs",
			run: verifyGetLogs(ctx,
				corev1Client,
				"openshift-network-operator",
				"network-operator-7887564c4-mjg9d",
				"Current first line\nCurrent second line\n",
				func(o *corev1.PodLogOptions) { o.Container = "network-operator" },
			),
		},
		{
			name: "Get pod logs with follow",
			run: func(t *testing.T) {
				ctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
				defer cancel()

				o := &corev1.PodLogOptions{
					Container: "network-operator",
					Follow:    true,
				}
				logs, err := corev1Client.
					Pods("openshift-network-operator").
					GetLogs("network-operator-7887564c4-mjg9d", o).
					DoRaw(ctx)
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("expected err to be %s, was %s", context.DeadlineExceeded, err)
				}
				if actual, expected := string(logs), "Current first line\nCurrent second line\n"; actual != expected {
					t.Errorf("expected log to be %q, was %q", expected, actual)
				}
			},
		},
		{
			name: "Get pod logs alternate file layout",
			run: verifyGetLogs(ctx,
				corev1Client,
				"openshift-service-ca-operator",
				"service-ca-operator-7496fb6588-2zznl",
				"Current first line\nCurrent second line\n",
				func(o *corev1.PodLogOptions) { o.Container = "service-ca-operator" },
			),
		},
		{
			name: "Get pod logs with tail",
			run: verifyGetLogs(ctx,
				corev1Client,
				"openshift-network-operator",
				"network-operator-7887564c4-mjg9d",
				"Current second line\n",
				func(o *corev1.PodLogOptions) {
					o.Container = "network-operator"
					o.TailLines = utilpointer.Int64(1)
				},
			),
		},
		{
			name: "Get pod logs alternate file layout with tail",
			run: verifyGetLogs(ctx,
				corev1Client,
				"openshift-service-ca-operator",
				"service-ca-operator-7496fb6588-2zznl",
				"Current second line\n",
				func(o *corev1.PodLogOptions) {
					o.Container = "service-ca-operator"
					o.TailLines = utilpointer.Int64(1)
				},
			),
		},
		{
			name: "Get pod logs for previous",
			run: verifyGetLogs(ctx,
				corev1Client,
				"openshift-network-operator",
				"network-operator-7887564c4-mjg9d",
				"Previous first line\nPrevious second line\n",
				func(o *corev1.PodLogOptions) {
					o.Container = "network-operator"
					o.Previous = true
				},
			),
		},
		{
			name: "Get pod logs alternate file layout for previous",
			run: verifyGetLogs(ctx,
				corev1Client,
				"openshift-service-ca-operator",
				"service-ca-operator-7496fb6588-2zznl",
				"Previous first line\nPrevious second line\n",
				func(o *corev1.PodLogOptions) {
					o.Container = "service-ca-operator"
					o.Previous = true
				},
			),
		},
		{
			name: "Get pod logs for previous with tail",
			run: verifyGetLogs(ctx,
				corev1Client,
				"openshift-network-operator",
				"network-operator-7887564c4-mjg9d",
				"Previous second line\n",
				func(o *corev1.PodLogOptions) {
					o.Container = "network-operator"
					o.Previous = true
					o.TailLines = utilpointer.Int64(1)
				},
			),
		},
		{
			name: "Get pod logs alternate file layout for previous with tail",
			run: verifyGetLogs(ctx,
				corev1Client,
				"openshift-service-ca-operator",
				"service-ca-operator-7496fb6588-2zznl",
				"Previous second line\n",
				func(o *corev1.PodLogOptions) {
					o.Container = "service-ca-operator"
					o.Previous = true
					o.TailLines = utilpointer.Int64(1)
				},
			),
		},
		{
			name: "List response is sorted",
			run: func(t *testing.T) {
				podList := &corev1.PodList{}
				if err := c.List(ctx, podList); err != nil {
					t.Fatalf("failed to list pods: %v", err)
				}
				isSorted := sort.SliceIsSorted(podList.Items, func(a, b int) bool {
					return podList.Items[a].CreationTimestamp.Before(&podList.Items[b].CreationTimestamp)
				})
				if !isSorted {
					t.Error("resulting pod list is not sorted by creationTimestamp")
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

func requestTableOnPath(ctx context.Context, path string, version string) (*metav1.Table, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:8080"+path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to construct request: %w", err)
	}
	req.Header.Set("Accept", fmt.Sprintf("application/json;as=Table;v=%s;g=meta.k8s.io", version))
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
		for _, version := range []string{"v1", "v1beta1"} {
			t.Run("version "+version, func(t *testing.T) {
				version := version
				t.Parallel()
				table, err := requestTableOnPath(ctx, path, version)
				if err != nil {
					t.Fatalf("failed to get table for %s: %v", path, err)
				}
				if n := len(table.ColumnDefinitions); n != expectNumColumns {
					t.Errorf("expected %d columns, got %d", expectNumColumns, n)
				}
				if n := len(table.Rows); n != expectNumRows {
					t.Errorf("expected to get %d rows back, got %d", expectNumRows, n)
				}
				if expected := "meta.k8s.io/" + version; table.APIVersion != expected {
					t.Errorf("expected to get table back in requested version %s, got %s", expected, table.APIVersion)
				}
			})
		}
	}
}

func verifyList(ctx context.Context, c client.Reader, list client.ObjectList, numExpected int, listOpts ...client.ListOption) func(t *testing.T) {
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

func verifyGet(ctx context.Context, c client.Client, obj client.Object) func(t *testing.T) {
	return func(t *testing.T) {
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Errorf("failed to get %T %s: %v", obj, client.ObjectKeyFromObject(obj), err)
		}
	}
}

func verifyGetLogs(ctx context.Context, c corev1client.CoreV1Interface, namespace, podName, expectedLog string, opts ...func(*corev1.PodLogOptions)) func(*testing.T) {
	return func(t *testing.T) {
		o := &corev1.PodLogOptions{}
		for _, opt := range opts {
			opt(o)
		}

		logs, err := c.
			Pods(namespace).
			GetLogs(podName, o).
			DoRaw(ctx)
		if err != nil {
			t.Fatalf("failed to get logs: %v", err)
		}
		if actual := string(logs); actual != expectedLog {
			t.Errorf("expected to get %q as log, got %q", expectedLog, actual)
		}
	}
}
