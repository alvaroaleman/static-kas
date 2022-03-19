# Static KAS

A fake kube-apiserver that serves static data from an Openshift must-gather. Dynamically discovers resources and supports logs. Requires golang >= 1.17.
While there is no explicit documentation for the directory layout, a sample is included for testing in [./pkg/handler/testdata](./pkg/handler/testdata).

Usage:

1. Start the static-kas in a distinct terminal: `go run ./cmd/ --base-dir ../must-gather/quay-io-openshift-release-dev-ocp-v4-0-art-dev-sha256-ec058cf120ee79c97fa385205ae5b4ab7745e4064716cadd1e319652f5999ffd/`
2. Create a Kubeconfig:
```bash
cat <<EOF >/tmp/kk
apiVersion: v1
clusters:
- cluster:
    server: http://localhost:8080
  name: static-kas
contexts:
- context:
    cluster: static-kas
    namespace: default
  name: static-kas
current-context: static-kas
kind: Config
EOF
```
3. Use `kubectl` or any other standard client to interact with the static kas: `kubectl --kubeconfig=/tmp/kk get pod`
