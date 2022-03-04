package main

var shortNameMapping = map[string][]string{
	"pods":              {"po"},
	"statefulsets.apps": {"sts"},
	"customresourcedefinitions.apiextensions.k8s.io": {"crd", "crds"},
}
