package multigrescluster

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	multigresv1alpha1 "github.com/multigres/multigres-operator/api/v1alpha1"
	"github.com/multigres/multigres-operator/pkg/images"
	"github.com/multigres/multigres-operator/pkg/util/metadata"
)

// imagesConfig returns the reconciler's image configuration, falling back to
// the compiled-in defaults with the immediate strategy when unset.
func (r *MultigresClusterReconciler) imagesConfig() images.Config {
	cfg := r.Images
	if cfg.Defaults == (multigresv1alpha1.ComponentImages{}) {
		cfg.Defaults = images.CompiledDefaults()
	}
	if cfg.Strategy == "" {
		cfg.Strategy = images.UpdateImmediate
	}
	return cfg
}

// resolveImages determines the effective component images for this reconcile
// pass and fills unset spec.images fields in-memory. Explicit spec.images
// values are user pins and always win; everything else comes from the
// operator's default image set. Nothing is written into the spec.
//
// Under the lazy update strategy, a cluster already running an older default
// set keeps it until spec.imageUpdatePolicy.acknowledgedRevision names the
// current set's revision. The control plane pacing a fleet upgrade moves only
// that one field, cluster by cluster. Clusters with no applied set recorded
// (newly created ones) adopt the current defaults.
//
// The set the operator commits to resolving is recorded in two places:
// status.images for observers, and the multigres.com/applied-images
// annotation as the durable copy — status can be lost on backup/restore or
// kubectl replace, and losing it must not roll the cluster. The record is
// written before child resources converge, so it reflects the resolution
// decision, not observed pod state. A pending rollout is surfaced through the
// ImageRolloutPending condition.
func (r *MultigresClusterReconciler) resolveImages(
	ctx context.Context,
	cluster *multigresv1alpha1.MultigresCluster,
) error {
	cfg := r.imagesConfig()
	if p := cluster.Spec.ImageUpdatePolicy; p != nil && p.Strategy != "" {
		cfg.Strategy = images.UpdateStrategy(p.Strategy)
	}
	available := cfg.Defaults
	availableRevision := images.Revision(available)

	recorded, recordInvalid := r.recordedImages(ctx, cluster)
	if recordInvalid && recorded == nil {
		// The durable record existed but is unusable and status offers no
		// fallback. Adopting current defaults is the only option left, but it
		// must be loud: this is the silent-roll path the record exists to
		// prevent.
		r.Recorder.Eventf(cluster, "Warning", "ImagesRecordInvalid",
			"The %s annotation is invalid and status.images is unavailable; "+
				"adopting the current default image set %s",
			metadata.AnnotationAppliedImages, availableRevision)
	}

	acknowledged := ""
	if cluster.Spec.ImageUpdatePolicy != nil {
		acknowledged = cluster.Spec.ImageUpdatePolicy.AcknowledgedRevision
	}

	if cfg.Strategy == images.UpdateImmediate && acknowledged != "" &&
		acknowledged != availableRevision &&
		(recorded == nil || acknowledged != images.Revision(*recorded)) {
		r.Recorder.Eventf(cluster, "Warning", "ImagesAcknowledgementIgnored",
			"spec.imageUpdatePolicy.acknowledgedRevision has no effect "+
				"under the immediate update strategy")
	}

	applied := available
	pendingReason := ""
	if cfg.Strategy == images.UpdateLazy && recorded != nil &&
		images.Revision(*recorded) != availableRevision {
		switch acknowledged {
		case availableRevision:
			// Acknowledged; fall through with applied = available.
		case "", images.Revision(*recorded):
			// Nothing acknowledged yet, or a previous acknowledgment already
			// consumed by adoption: the normal state between rollouts.
			applied = *recorded
			pendingReason = multigresv1alpha1.ReasonAwaitingAcknowledgement
		default:
			applied = *recorded
			pendingReason = multigresv1alpha1.ReasonRevisionMismatch
		}
	}

	appliedRevision := images.Revision(applied)
	if recorded != nil && images.Revision(*recorded) != appliedRevision {
		log.FromContext(ctx).Info("switching to new default image set",
			"revision", appliedRevision,
			"previousRevision", images.Revision(*recorded),
			"changes", diffComponentImages(*recorded, applied))
		r.Recorder.Eventf(cluster, "Normal", "ImagesUpdated",
			"Switching to default image set %s (was %s): %s",
			appliedRevision, images.Revision(*recorded),
			diffComponentImages(*recorded, applied))
	}

	if err := r.recordAppliedImages(ctx, cluster, applied); err != nil {
		return err
	}

	cluster.Status.Images = &multigresv1alpha1.ImagesStatus{
		UpdateStrategy:    string(cfg.Strategy),
		Applied:           applied,
		AppliedRevision:   appliedRevision,
		AvailableRevision: availableRevision,
	}

	cond := metav1.Condition{
		Type:               multigresv1alpha1.ConditionImageRolloutPending,
		Status:             metav1.ConditionFalse,
		Reason:             multigresv1alpha1.ReasonImagesUpToDate,
		Message:            fmt.Sprintf("Running default image set %s", appliedRevision),
		ObservedGeneration: cluster.Generation,
		LastTransitionTime: metav1.Now(),
	}
	if pendingReason != "" {
		cond.Status = metav1.ConditionTrue
		cond.Reason = pendingReason
		cond.Message = fmt.Sprintf(
			"Default image set %s is available but not adopted; holding %s "+
				"until spec.imageUpdatePolicy.acknowledgedRevision is set to %q",
			availableRevision, appliedRevision, availableRevision,
		)
		if pendingReason == multigresv1alpha1.ReasonRevisionMismatch {
			r.Recorder.Eventf(cluster, "Warning", "ImagesRevisionMismatch",
				"Acknowledged revision %q does not match the available revision %q; "+
					"it has no effect",
				acknowledged, availableRevision)
		}
	}
	meta.SetStatusCondition(&cluster.Status.Conditions, cond)

	images.Complete(&cluster.Spec.Images, applied)
	return nil
}

// recordedImages returns the default image set the operator last committed to
// for this cluster: the applied-images annotation first (durable), then
// status (informational). A record is only trusted when it parses and names
// every component. A partial set would resolve some components from the
// record and silently drop the rest to compiled-in fallbacks.
//
// Nil with invalid=false means no record: a new cluster. Nil with
// invalid=true means a record existed but was unusable; the caller decides
// the failure posture.
func (r *MultigresClusterReconciler) recordedImages(
	ctx context.Context,
	cluster *multigresv1alpha1.MultigresCluster,
) (set *multigresv1alpha1.ComponentImages, invalid bool) {
	if raw := cluster.Annotations[metadata.AnnotationAppliedImages]; raw != "" {
		var recorded multigresv1alpha1.ComponentImages
		err := json.Unmarshal([]byte(raw), &recorded)
		if err == nil && images.IsComplete(recorded) {
			return &recorded, false
		}
		invalid = true
		log.FromContext(ctx).Error(err, "applied-images annotation is invalid or incomplete",
			"annotation", metadata.AnnotationAppliedImages, "value", raw)
		r.Recorder.Eventf(cluster, "Warning", "ImagesRecordInvalid",
			"The %s annotation is invalid or incomplete and was ignored",
			metadata.AnnotationAppliedImages)
	}
	if cluster.Status.Images != nil && images.IsComplete(cluster.Status.Images.Applied) {
		return cluster.Status.Images.Applied.DeepCopy(), invalid
	}
	return nil, invalid
}

// diffComponentImages renders the per-component changes between two image
// sets, for events and logs.
func diffComponentImages(from, to multigresv1alpha1.ComponentImages) string {
	var changes []string
	add := func(name string, f, t multigresv1alpha1.ImageRef) {
		if f != t {
			changes = append(changes, fmt.Sprintf("%s: %s -> %s", name, f, t))
		}
	}
	add("postgres", from.Postgres, to.Postgres)
	add("multiadmin", from.Multiadmin, to.Multiadmin)
	add("multiadminWeb", from.MultiadminWeb, to.MultiadminWeb)
	add("multiorch", from.Multiorch, to.Multiorch)
	add("multipooler", from.Multipooler, to.Multipooler)
	add("multigateway", from.Multigateway, to.Multigateway)
	if len(changes) == 0 {
		return "no component changes"
	}
	return strings.Join(changes, ", ")
}

// recordAppliedImages persists the set the operator has committed to
// resolving into the applied-images annotation when it changed. Children may
// not have converged to it yet. The patch is applied to a copy: patching
// refreshes the object from the server, which would discard the in-memory
// defaults applied earlier in the reconcile pass.
func (r *MultigresClusterReconciler) recordAppliedImages(
	ctx context.Context,
	cluster *multigresv1alpha1.MultigresCluster,
	applied multigresv1alpha1.ComponentImages,
) error {
	raw, err := json.Marshal(applied)
	if err != nil {
		return fmt.Errorf("failed to encode applied images: %w", err)
	}
	want := string(raw)
	if cluster.Annotations[metadata.AnnotationAppliedImages] == want {
		return nil
	}

	obj := cluster.DeepCopy()
	base := obj.DeepCopy()
	if obj.Annotations == nil {
		obj.Annotations = map[string]string{}
	}
	obj.Annotations[metadata.AnnotationAppliedImages] = want
	if err := r.Patch(ctx, obj, client.MergeFromWithOptions(
		base, client.MergeFromWithOptimisticLock{},
	)); err != nil {
		return fmt.Errorf("failed to record applied images: %w", err)
	}

	if cluster.Annotations == nil {
		cluster.Annotations = map[string]string{}
	}
	cluster.Annotations[metadata.AnnotationAppliedImages] = want
	cluster.ResourceVersion = obj.ResourceVersion
	return nil
}
