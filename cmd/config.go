package main

// TODO: Import this somehow
var shortNameMapping = map[string][]string{
	"pods":              {"po"},
	"statefulsets.apps": {"sts"},
	"deployments.apps":  {"dep"},
	"daemonsets.apps":   {"ds"},
	"customresourcedefinitions.apiextensions.k8s.io": {"crd", "crds"},
}
