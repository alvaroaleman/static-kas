package discovery

// TODO: Import this somehow
var shortNameMapping = map[string][]string{
	"pods":              {"po"},
	"services":          {"svc"},
	"statefulsets.apps": {"sts"},
	"deployments.apps":  {"dep"},
	"daemonsets.apps":   {"ds"},
	"replicasets.apps":  {"rs"},
	"customresourcedefinitions.apiextensions.k8s.io": {"crd", "crds"},
}
