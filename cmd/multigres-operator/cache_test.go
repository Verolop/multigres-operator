package main

import (
	"reflect"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
)

func TestOperatorCacheOptionsNamespaceScoped(t *testing.T) {
	t.Parallel()

	const namespace = "multigres-mats"
	opts := operatorCacheOptions(namespace, namespace)

	if len(opts.DefaultNamespaces) != 1 {
		t.Fatalf("DefaultNamespaces has %d entries, want 1", len(opts.DefaultNamespaces))
	}
	if _, ok := opts.DefaultNamespaces[namespace]; !ok {
		t.Fatalf("DefaultNamespaces does not contain %q", namespace)
	}
	if _, ok := opts.DefaultNamespaces[cache.AllNamespaces]; ok {
		t.Fatal("namespace-scoped cache contains the all-namespaces key")
	}

	for name, byObject := range map[string]map[string]cache.Config{
		"Secret":      namespacesFor(t, opts, &corev1.Secret{}),
		"StatefulSet": namespacesFor(t, opts, &appsv1.StatefulSet{}),
		"Service":     namespacesFor(t, opts, &corev1.Service{}),
		"Pod":         namespacesFor(t, opts, &corev1.Pod{}),
	} {
		if len(byObject) != 1 {
			t.Errorf("%s namespace cache has %d entries, want 1", name, len(byObject))
		}
		if _, ok := byObject[namespace]; !ok {
			t.Errorf("%s namespace cache does not contain %q", name, namespace)
		}
		if _, ok := byObject[cache.AllNamespaces]; ok {
			t.Errorf("%s namespace cache contains the all-namespaces key", name)
		}
	}
}

func TestOperatorCacheOptionsClusterWide(t *testing.T) {
	t.Parallel()

	const operatorNamespace = "multigres-operator"
	opts := operatorCacheOptions(operatorNamespace, "")

	if opts.DefaultNamespaces != nil {
		t.Fatalf("DefaultNamespaces = %#v, want nil", opts.DefaultNamespaces)
	}
	for name, byObject := range map[string]map[string]cache.Config{
		"Secret":      namespacesFor(t, opts, &corev1.Secret{}),
		"StatefulSet": namespacesFor(t, opts, &appsv1.StatefulSet{}),
		"Service":     namespacesFor(t, opts, &corev1.Service{}),
		"Pod":         namespacesFor(t, opts, &corev1.Pod{}),
	} {
		if _, ok := byObject[operatorNamespace]; !ok {
			t.Errorf(
				"%s namespace cache does not contain operator namespace %q",
				name,
				operatorNamespace,
			)
		}
		if _, ok := byObject[cache.AllNamespaces]; !ok {
			t.Errorf("%s namespace cache does not contain the all-namespaces key", name)
		}
	}
}

func namespacesFor(t *testing.T, opts cache.Options, object any) map[string]cache.Config {
	t.Helper()
	wantType := reflect.TypeOf(object)
	for cachedObject, byObject := range opts.ByObject {
		if reflect.TypeOf(cachedObject) == wantType {
			return byObject.Namespaces
		}
	}
	t.Fatalf("cache options do not contain %v", wantType)
	return nil
}
