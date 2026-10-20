package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// GroupVersion is the API group and version of the Workspace resource.
var GroupVersion = schema.GroupVersion{Group: "sbx.dev", Version: "v1alpha1"}

var (
	schemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)
	// AddToScheme registers the sbx.dev types with a scheme.
	AddToScheme = schemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(GroupVersion, &Workspace{}, &WorkspaceList{})
	metav1.AddToGroupVersion(s, GroupVersion)
	return nil
}
