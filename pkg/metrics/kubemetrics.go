package metrics

import (
	"fmt"
	"strings"
	"sync"

	"github.com/kubecost/cost-model/pkg/clustercache"
	"github.com/kubecost/cost-model/pkg/prom"

	"github.com/prometheus/client_golang/prometheus"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
)

//--------------------------------------------------------------------------
//  Kube Metric Registration
//--------------------------------------------------------------------------

// initializer
var kubeMetricInit sync.Once

// KubeMetricsOpts represents our Kubernetes metrics emission options.
type KubeMetricsOpts struct {
	EmitKubecostControllerMetrics bool
	EmitNamespaceAnnotations      bool
	EmitPodAnnotations            bool
	EmitKubeStateMetrics          bool
}

// DefaultKubeMetricsOpts returns KubeMetricsOpts with default values set
func DefaultKubeMetricsOpts() *KubeMetricsOpts {
	return &KubeMetricsOpts{
		EmitKubecostControllerMetrics: true,
		EmitNamespaceAnnotations:      false,
		EmitPodAnnotations:            false,
		EmitKubeStateMetrics:          true,
	}
}

// InitKubeMetrics initializes kubernetes metric emission using the provided options.
func InitKubeMetrics(clusterCache clustercache.ClusterCache, opts *KubeMetricsOpts) {
	if opts == nil {
		opts = DefaultKubeMetricsOpts()
	}

	kubeMetricInit.Do(func() {
		if opts.EmitKubecostControllerMetrics {
			prometheus.MustRegister(KubecostServiceCollector{
				KubeClusterCache: clusterCache,
			})
			prometheus.MustRegister(KubecostDeploymentCollector{
				KubeClusterCache: clusterCache,
			})
			prometheus.MustRegister(KubecostStatefulsetCollector{
				KubeClusterCache: clusterCache,
			})
		}

		if opts.EmitPodAnnotations {
			prometheus.MustRegister(KubecostPodCollector{
				KubeClusterCache: clusterCache,
			})
		}

		if opts.EmitNamespaceAnnotations {
			prometheus.MustRegister(KubecostNamespaceCollector{
				KubeClusterCache: clusterCache,
			})
		}

		if opts.EmitKubeStateMetrics {
			prometheus.MustRegister(KubeNodeCollector{
				KubeClusterCache: clusterCache,
			})
			prometheus.MustRegister(KubeNamespaceCollector{
				KubeClusterCache: clusterCache,
			})
			prometheus.MustRegister(KubeDeploymentCollector{
				KubeClusterCache: clusterCache,
			})
			prometheus.MustRegister(KubePodCollector{
				KubeClusterCache: clusterCache,
			})
			prometheus.MustRegister(KubePVCollector{
				KubeClusterCache: clusterCache,
			})
			prometheus.MustRegister(KubePVCCollector{
				KubeClusterCache: clusterCache,
			})
			prometheus.MustRegister(KubeJobCollector{
				KubeClusterCache: clusterCache,
			})
		}
	})
}

//--------------------------------------------------------------------------
//  Kube Metric Helpers
//--------------------------------------------------------------------------

// getPersistentVolumeClaimClass returns StorageClassName. If no storage class was
// requested, it returns "".
func getPersistentVolumeClaimClass(claim *v1.PersistentVolumeClaim) string {
	// Use beta annotation first
	if class, found := claim.Annotations[v1.BetaStorageClassAnnotation]; found {
		return class
	}

	if claim.Spec.StorageClassName != nil {
		return *claim.Spec.StorageClassName
	}

	// Special non-empty string to indicate absence of storage class.
	return "<none>"
}

// toResourceUnitValue accepts a resource name and quantity and returns the sanitized resource, the unit, and the value in the units.
// Returns an empty string for resource and unit if there was a failure.
func toResourceUnitValue(resourceName v1.ResourceName, quantity resource.Quantity) (resource string, unit string, value float64) {
	resource = prom.SanitizeLabelName(string(resourceName))

	switch resourceName {
	case v1.ResourceCPU:
		unit = "core"
		value = float64(quantity.MilliValue()) / 1000
		return

	case v1.ResourceStorage:
		fallthrough
	case v1.ResourceEphemeralStorage:
		fallthrough
	case v1.ResourceMemory:
		unit = "byte"
		value = float64(quantity.Value())
		return
	case v1.ResourcePods:
		unit = "integer"
		value = float64(quantity.Value())
		return
	default:
		if isHugePageResourceName(resourceName) || isAttachableVolumeResourceName(resourceName) {
			unit = "byte"
			value = float64(quantity.Value())
			return
		}

		if isExtendedResourceName(resourceName) {
			unit = "integer"
			value = float64(quantity.Value())
			return
		}
	}

	resource = ""
	unit = ""
	value = 0.0
	return
}

// isHugePageResourceName checks for a huge page container resource name
func isHugePageResourceName(name v1.ResourceName) bool {
	return strings.HasPrefix(string(name), v1.ResourceHugePagesPrefix)
}

// isAttachableVolumeResourceName checks for attached volume container resource name
func isAttachableVolumeResourceName(name v1.ResourceName) bool {
	return strings.HasPrefix(string(name), v1.ResourceAttachableVolumesPrefix)
}

// isExtendedResourceName checks for extended container resource name
func isExtendedResourceName(name v1.ResourceName) bool {
	if isNativeResource(name) || strings.HasPrefix(string(name), v1.DefaultResourceRequestsPrefix) {
		return false
	}
	// Ensure it satisfies the rules in IsQualifiedName() after converted into quota resource name
	nameForQuota := fmt.Sprintf("%s%s", v1.DefaultResourceRequestsPrefix, string(name))
	if errs := validation.IsQualifiedName(nameForQuota); len(errs) != 0 {
		return false
	}
	return true
}

// isNativeResource checks for a kubernetes.io/ prefixed resource name
func isNativeResource(name v1.ResourceName) bool {
	return !strings.Contains(string(name), "/") || isPrefixedNativeResource(name)
}

func isPrefixedNativeResource(name v1.ResourceName) bool {
	return strings.Contains(string(name), v1.ResourceDefaultNamespacePrefix)
}

func failureReason(jc *batchv1.JobCondition, reason string) bool {
	if jc == nil {
		return false
	}
	return jc.Reason == reason
}

// boolFloat64 converts a boolean input into a 1 or 0
func boolFloat64(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// toStringPtr is used to create a new string pointer from iteration vars
func toStringPtr(s string) *string { return &s }
