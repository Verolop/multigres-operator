package toposerver

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	multigresv1alpha1 "github.com/multigres/multigres-operator/api/v1alpha1"
	"github.com/multigres/multigres-operator/pkg/util/certs"
	"github.com/multigres/multigres-operator/pkg/util/metadata"
)

func certScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = multigresv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(certs.GVK, &unstructured.Unstructured{})
	listGVK := certs.GVK
	listGVK.Kind += "List"
	scheme.AddKnownTypeWithName(listGVK, &unstructured.UnstructuredList{})
	return scheme
}

func certTestTopoServer(tls *multigresv1alpha1.TopoTLSConfig) *multigresv1alpha1.TopoServer {
	return &multigresv1alpha1.TopoServer{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "multigres.com/v1alpha1",
			Kind:       "TopoServer",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster-global-topo",
			Namespace: "supabase",
			UID:       "toposerver-uid",
			Labels:    map[string]string{metadata.LabelMultigresCluster: "test-cluster"},
		},
		Spec: multigresv1alpha1.TopoServerSpec{
			Etcd: &multigresv1alpha1.EtcdSpec{Replicas: ptr.To(int32(3))},
			TLS:  tls,
		},
	}
}

func TestBuildServingCertificate(t *testing.T) {
	scheme := certScheme()
	toposerver := certTestTopoServer(&multigresv1alpha1.TopoTLSConfig{
		Enabled:    ptr.To(true),
		IssuerName: "multigres-infra-issuer",
	})

	got, err := BuildServingCertificate(toposerver, scheme)
	if err != nil {
		t.Fatalf("BuildServingCertificate() error = %v", err)
	}
	if got == nil {
		t.Fatal("BuildServingCertificate() = nil, want a Certificate")
	}

	wantName := "test-cluster-global-topo-topo-server-tls"
	if got.GetName() != wantName {
		t.Errorf("name = %q, want %q", got.GetName(), wantName)
	}
	if got.GetNamespace() != "supabase" {
		t.Errorf("namespace = %q, want supabase", got.GetNamespace())
	}
	ownerRefs := got.GetOwnerReferences()
	if len(ownerRefs) != 1 || ownerRefs[0].Kind != "TopoServer" {
		t.Fatalf("ownerReferences = %+v, want one TopoServer ref", ownerRefs)
	}

	spec, ok := got.Object["spec"].(map[string]any)
	if !ok {
		t.Fatal("spec is not a map")
	}

	// Both Services the controller creates have to verify: the client Service
	// (BuildClientService) and the headless peer Service (BuildHeadlessService).
	wantDNSNames := []any{
		"test-cluster-global-topo",
		"test-cluster-global-topo.supabase",
		"test-cluster-global-topo.supabase.svc",
		"test-cluster-global-topo.supabase.svc.cluster.local",
		"test-cluster-global-topo-headless",
		"test-cluster-global-topo-headless.supabase",
		"test-cluster-global-topo-headless.supabase.svc",
		"test-cluster-global-topo-headless.supabase.svc.cluster.local",
		"*.test-cluster-global-topo-headless.supabase.svc.cluster.local",
	}
	if diff := cmp.Diff(wantDNSNames, spec["dnsNames"]); diff != "" {
		t.Errorf("dnsNames mismatch (-want +got):\n%s", diff)
	}

	wantSubject := fmt.Sprintf(
		certs.LiteralSubjectTemplate,
		"test-cluster-global-topo.supabase.svc.cluster.local",
	)
	if diff := cmp.Diff(wantSubject, spec["literalSubject"]); diff != "" {
		t.Errorf("literalSubject mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(wantName, spec["secretName"]); diff != "" {
		t.Errorf("secretName mismatch (-want +got):\n%s", diff)
	}

	// The topology server is shared infrastructure, so it takes the issuer from
	// the topology TLS config rather than any single cluster's issuer.
	wantIssuerRef := map[string]any{
		"name":  "multigres-infra-issuer",
		"kind":  "ClusterIssuer",
		"group": "cert-manager.io",
	}
	if diff := cmp.Diff(wantIssuerRef, spec["issuerRef"]); diff != "" {
		t.Errorf("issuerRef mismatch (-want +got):\n%s", diff)
	}
}

func TestBuildServingCertificateSANsCoverBothServices(t *testing.T) {
	scheme := certScheme()
	toposerver := certTestTopoServer(&multigresv1alpha1.TopoTLSConfig{Enabled: ptr.To(true)})

	clientSvc, err := BuildClientService(toposerver, scheme)
	if err != nil {
		t.Fatalf("BuildClientService() error = %v", err)
	}
	headlessSvc, err := BuildHeadlessService(toposerver, scheme)
	if err != nil {
		t.Fatalf("BuildHeadlessService() error = %v", err)
	}

	cert, err := BuildServingCertificate(toposerver, scheme)
	if err != nil {
		t.Fatalf("BuildServingCertificate() error = %v", err)
	}
	sans, _, err := unstructured.NestedSlice(cert.Object, "spec", "dnsNames")
	if err != nil {
		t.Fatalf("NestedSlice(dnsNames) error = %v", err)
	}
	covered := make(map[string]struct{}, len(sans))
	for _, s := range sans {
		covered[s.(string)] = struct{}{}
	}

	for _, svc := range []*corev1.Service{clientSvc, headlessSvc} {
		fqdn := fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, svc.Namespace)
		if _, ok := covered[fqdn]; !ok {
			t.Errorf("Service %q FQDN %q is not a SAN; SANs = %v", svc.Name, fqdn, sans)
		}
	}
}

func TestBuildServingCertificateDefaultIssuer(t *testing.T) {
	scheme := certScheme()
	toposerver := certTestTopoServer(&multigresv1alpha1.TopoTLSConfig{Enabled: ptr.To(true)})

	cert, err := BuildServingCertificate(toposerver, scheme)
	if err != nil {
		t.Fatalf("BuildServingCertificate() error = %v", err)
	}
	issuer, _, _ := unstructured.NestedString(cert.Object, "spec", "issuerRef", "name")
	if issuer != certs.DefaultIssuerName {
		t.Errorf("issuerRef.name = %q, want %q", issuer, certs.DefaultIssuerName)
	}
}

func TestBuildServingCertificateDisabled(t *testing.T) {
	scheme := certScheme()

	for name, tls := range map[string]*multigresv1alpha1.TopoTLSConfig{
		"unset":    nil,
		"disabled": {Enabled: ptr.To(false)},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := BuildServingCertificate(certTestTopoServer(tls), scheme)
			if err != nil {
				t.Fatalf("BuildServingCertificate() error = %v", err)
			}
			if got != nil {
				t.Errorf("BuildServingCertificate() = %v, want nil", got)
			}
		})
	}
}

func TestReconcileCertificate(t *testing.T) {
	certName := "test-cluster-global-topo-topo-server-tls"

	t.Run("applies the certificate when enabled", func(t *testing.T) {
		scheme := certScheme()
		toposerver := certTestTopoServer(&multigresv1alpha1.TopoTLSConfig{Enabled: ptr.To(true)})
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(toposerver).Build()
		r := &TopoServerReconciler{
			Client:   c,
			Scheme:   scheme,
			Recorder: record.NewFakeRecorder(10),
		}

		if err := r.reconcileCertificate(context.Background(), toposerver); err != nil {
			t.Fatalf("reconcileCertificate() error = %v", err)
		}

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(certs.GVK)
		if err := c.Get(
			context.Background(),
			client.ObjectKey{Namespace: "supabase", Name: certName},
			got,
		); err != nil {
			t.Fatalf("expected serving Certificate, got error %v", err)
		}
	})

	t.Run("prunes the certificate and its secret when disabled", func(t *testing.T) {
		scheme := certScheme()
		enabled := certTestTopoServer(&multigresv1alpha1.TopoTLSConfig{Enabled: ptr.To(true)})
		existing, err := BuildServingCertificate(enabled, scheme)
		if err != nil {
			t.Fatalf("BuildServingCertificate() error = %v", err)
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: certName, Namespace: "supabase"},
		}

		toposerver := certTestTopoServer(&multigresv1alpha1.TopoTLSConfig{Enabled: ptr.To(false)})
		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(toposerver, existing, secret).
			Build()
		r := &TopoServerReconciler{
			Client:   c,
			Scheme:   scheme,
			Recorder: record.NewFakeRecorder(10),
		}

		if err := r.reconcileCertificate(context.Background(), toposerver); err != nil {
			t.Fatalf("reconcileCertificate() error = %v", err)
		}

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(certs.GVK)
		if err := c.Get(
			context.Background(),
			client.ObjectKey{Namespace: "supabase", Name: certName},
			got,
		); err == nil {
			t.Error("expected serving Certificate to be deleted")
		}
		if err := c.Get(
			context.Background(),
			client.ObjectKey{Namespace: "supabase", Name: certName},
			&corev1.Secret{},
		); err == nil {
			t.Error("expected generated Secret to be deleted")
		}
	})

	t.Run("issues nothing when TLS is unset", func(t *testing.T) {
		scheme := certScheme()
		toposerver := certTestTopoServer(nil)
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(toposerver).Build()
		r := &TopoServerReconciler{
			Client:   c,
			Scheme:   scheme,
			Recorder: record.NewFakeRecorder(10),
		}

		if err := r.reconcileCertificate(context.Background(), toposerver); err != nil {
			t.Fatalf("reconcileCertificate() error = %v", err)
		}

		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(certs.GVK)
		if err := c.List(context.Background(), list); err != nil {
			t.Fatalf("List() error = %v", err)
		}
		if len(list.Items) != 0 {
			t.Errorf("got %d Certificates, want 0", len(list.Items))
		}
	})

	// Removing the TLS block is as common as setting enabled to false, and it
	// must not leave private key material behind either.
	t.Run("cleans up when the TLS block is removed", func(t *testing.T) {
		scheme := certScheme()
		enabled := certTestTopoServer(&multigresv1alpha1.TopoTLSConfig{Enabled: ptr.To(true)})
		existing, err := BuildServingCertificate(enabled, scheme)
		if err != nil {
			t.Fatalf("BuildServingCertificate() error = %v", err)
		}
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: certName, Namespace: "supabase"},
		}

		toposerver := certTestTopoServer(nil)
		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(toposerver, existing, secret).
			Build()
		r := &TopoServerReconciler{
			Client:   c,
			Scheme:   scheme,
			Recorder: record.NewFakeRecorder(10),
		}

		if err := r.reconcileCertificate(context.Background(), toposerver); err != nil {
			t.Fatalf("reconcileCertificate() error = %v", err)
		}

		key := client.ObjectKey{Namespace: "supabase", Name: certName}
		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(certs.GVK)
		if err := c.Get(context.Background(), key, got); err == nil {
			t.Error("expected orphaned Certificate to be deleted")
		}
		if err := c.Get(context.Background(), key, &corev1.Secret{}); err == nil {
			t.Error("expected orphaned Secret to be deleted")
		}
	})

	// A same-named Certificate owned by something else is not ours to delete.
	t.Run("leaves an unowned certificate alone", func(t *testing.T) {
		scheme := certScheme()
		enabled := certTestTopoServer(&multigresv1alpha1.TopoTLSConfig{Enabled: ptr.To(true)})
		foreign, err := BuildServingCertificate(enabled, scheme)
		if err != nil {
			t.Fatalf("BuildServingCertificate() error = %v", err)
		}
		foreign.SetOwnerReferences(nil)

		toposerver := certTestTopoServer(nil)
		c := fake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(toposerver, foreign).
			Build()
		r := &TopoServerReconciler{
			Client:   c,
			Scheme:   scheme,
			Recorder: record.NewFakeRecorder(10),
		}

		if err := r.reconcileCertificate(context.Background(), toposerver); err != nil {
			t.Fatalf("reconcileCertificate() error = %v", err)
		}

		got := &unstructured.Unstructured{}
		got.SetGroupVersionKind(certs.GVK)
		if err := c.Get(
			context.Background(),
			client.ObjectKey{Namespace: "supabase", Name: certName},
			got,
		); err != nil {
			t.Errorf("unowned Certificate was deleted: %v", err)
		}
	})
}

// Issuing certificates does not change what etcd runs with. Holding the
// StatefulSet pod template and the etcd container environment identical is
// what makes enabling topology TLS safe on a running cluster.
func TestTopoTLSDoesNotChangeRenderedPodSpec(t *testing.T) {
	scheme := certScheme()

	without := certTestTopoServer(nil)
	with := certTestTopoServer(&multigresv1alpha1.TopoTLSConfig{
		Enabled:    ptr.To(true),
		IssuerName: "multigres-infra-issuer",
	})

	baseline, err := BuildStatefulSet(without, scheme)
	if err != nil {
		t.Fatalf("BuildStatefulSet() error = %v", err)
	}
	withTLS, err := BuildStatefulSet(with, scheme)
	if err != nil {
		t.Fatalf("BuildStatefulSet() error = %v", err)
	}

	if diff := cmp.Diff(baseline.Spec.Template, withTLS.Spec.Template); diff != "" {
		t.Errorf(
			"pod template changed when topology TLS was enabled (-without +with):\n%s",
			diff,
		)
	}
	if diff := cmp.Diff(baseline.Spec, withTLS.Spec); diff != "" {
		t.Errorf(
			"StatefulSet spec changed when topology TLS was enabled (-without +with):\n%s",
			diff,
		)
	}
}
