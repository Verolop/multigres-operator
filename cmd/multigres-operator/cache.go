package main

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/multigres/multigres-operator/pkg/util/metadata"
)

// operatorCacheOptions builds the manager cache configuration. When watchNamespace
// is set, every namespaced informer is restricted to that namespace. This allows
// independent operator deployments to reconcile different namespaces without
// observing or competing over each other's resources.
func operatorCacheOptions(operatorNamespace, watchNamespace string) cache.Options {
	if watchNamespace != "" {
		namespaceConfig := map[string]cache.Config{
			watchNamespace: {},
		}
		return cache.Options{
			DefaultNamespaces: namespaceConfig,
			ByObject: map[client.Object]cache.ByObject{
				&corev1.Secret{}:      {Namespaces: namespaceConfig},
				&appsv1.StatefulSet{}: {Namespaces: namespaceConfig},
				&corev1.Service{}:     {Namespaces: namespaceConfig},
				&corev1.Pod{}:         {Namespaces: namespaceConfig},
			},
		}
	}

	labelReq, _ := labels.NewRequirement(
		metadata.LabelAppManagedBy,
		selection.Equals,
		[]string{metadata.ManagedByMultigres},
	)
	selector := labels.NewSelector().Add(*labelReq)
	filteredConfig := cache.Config{LabelSelector: selector}
	unfilteredConfig := cache.Config{}

	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {
				Namespaces: map[string]cache.Config{
					operatorNamespace:   unfilteredConfig,
					cache.AllNamespaces: filteredConfig,
				},
			},
			&appsv1.StatefulSet{}: {
				Namespaces: map[string]cache.Config{
					operatorNamespace:   unfilteredConfig,
					cache.AllNamespaces: filteredConfig,
				},
			},
			&corev1.Service{}: {
				Namespaces: map[string]cache.Config{
					operatorNamespace:   unfilteredConfig,
					cache.AllNamespaces: filteredConfig,
				},
			},
			&corev1.Pod{}: {
				Namespaces: map[string]cache.Config{
					operatorNamespace:   unfilteredConfig,
					cache.AllNamespaces: filteredConfig,
				},
			},
		},
	}
}
