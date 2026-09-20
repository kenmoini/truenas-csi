package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	"github.com/operator-framework/operator-lib/proxy"
	csiv1alpha1 "github.com/truenas/truenas-csi/operator/api/v1alpha1"
)

// buildTrueNASEnvVars creates the environment variables for TrueNAS CSI containers
//
//nolint:prealloc
func buildTrueNASEnvVars(csi *csiv1alpha1.TrueNASCSI) []corev1.EnvVar {
	baseEnvVars := []corev1.EnvVar{
		{Name: "CSI_ENDPOINT", Value: CSISocketPath},
		fieldRefEnvVar("NODE_ID", "spec.nodeName"),
		configMapEnvVar("TRUENAS_URL", ConfigMapName, "truenasURL", false),
		secretEnvVar("TRUENAS_API_KEY", csi.Spec.CredentialsSecret, "api-key"),
		configMapEnvVar("TRUENAS_DEFAULT_POOL", ConfigMapName, "defaultPool", false),
		configMapEnvVar("TRUENAS_NFS_SERVER", ConfigMapName, "nfsServer", true),
		configMapEnvVar("TRUENAS_ISCSI_PORTAL", ConfigMapName, "iscsiPortal", true),
		configMapEnvVar("TRUENAS_NVMEOF_PORTAL", ConfigMapName, "nvmeofPortal", true),
		configMapEnvVar("TRUENAS_ISCSI_IQN_BASE", ConfigMapName, "iscsiIQNBase", true),
		configMapEnvVar("TRUENAS_INSECURE_SKIP_VERIFY", ConfigMapName, "truenasInsecure", true),
	}

	// This appends proxy environmental variables if they are set in the operator's environment.
	// The CSI driver can use these to configure its HTTP clients.
	// https://sdk.operatorframework.io/docs/building-operators/golang/references/proxy-vars/
	baseEnvVars = append(baseEnvVars, proxy.ReadProxyVarsFromEnv()...)

	return baseEnvVars
}

// buildConfigMapData returns the driver configuration the CSI workloads read
// through the ConfigMap. The workload reconcilers hash the same map to decide
// when the pods need a new revision, so this must stay the single source.
func buildConfigMapData(csi *csiv1alpha1.TrueNASCSI) map[string]string {
	return map[string]string{
		"truenasURL":      csi.Spec.TrueNASURL,
		"defaultPool":     csi.Spec.DefaultPool,
		"nfsServer":       csi.Spec.NFSServer,
		"iscsiPortal":     csi.Spec.ISCSIPortal,
		"nvmeofPortal":    csi.Spec.NVMeOFPortal,
		"iscsiIQNBase":    csi.Spec.ISCSIIQNBase,
		"truenasInsecure": fmt.Sprintf("%t", csi.Spec.InsecureSkipTLS),
	}
}

// configHash returns a stable SHA-256 over the driver configuration. The
// workload reconcilers stamp it on the pod template, so a configuration change
// rolls the pods. The value depends only on the content, so repeated reconciles
// with an unchanged configuration produce an identical annotation and no write.
func configHash(csi *csiv1alpha1.TrueNASCSI) string {
	data := buildConfigMapData(csi)
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s=%s\n", k, data[k]) //nolint:errcheck
	}
	return hex.EncodeToString(h.Sum(nil))
}

// podTemplateAnnotations returns the annotations to stamp on a workload's pod
// template. The map is never empty: it always carries the configuration hash,
// which is what rolls the pods when the configuration changes.
func podTemplateAnnotations(csi *csiv1alpha1.TrueNASCSI) map[string]string {
	return map[string]string{ConfigHashAnnotation: configHash(csi)}
}

// fieldRefEnvVar creates an environment variable from a field reference
func fieldRefEnvVar(name, fieldPath string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: fieldPath},
		},
	}
}

// configMapEnvVar creates an environment variable from a ConfigMap key
//
//nolint:unparam
func configMapEnvVar(name, configMapName, key string, optional bool) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
				Key:                  key,
				Optional:             ptr.To(optional),
			},
		},
	}
}

// secretEnvVar creates an environment variable from a Secret key
func secretEnvVar(name, secretName, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  key,
			},
		},
	}
}

// SidecarConfig defines the configuration for building a sidecar container
type SidecarConfig struct {
	Name         string
	ImageEnvVar  string
	Args         []string
	VolumeMounts []corev1.VolumeMount
}

// buildSidecarContainer creates a sidecar container from configuration
func buildSidecarContainer(config SidecarConfig) corev1.Container {
	return corev1.Container{
		Name:            config.Name,
		Image:           getSidecarImage(config.ImageEnvVar),
		ImagePullPolicy: corev1.PullIfNotPresent,
		Args:            config.Args,
		VolumeMounts:    config.VolumeMounts,
	}
}

// getSidecarImage returns the sidecar image from environment variable.
// Unlike the old implementation, this does NOT fall back to hardcoded defaults.
// Sidecar images must be configured via environment variables.
func getSidecarImage(envVar string) string {
	return os.Getenv(envVar)
}

// mustParseQuantity parses a resource quantity, panicking on invalid input.
// Use only with compile-time constant strings.
func mustParseQuantity(s string) resource.Quantity {
	return resource.MustParse(s)
}

// socketDirVolumeMount returns the standard socket directory volume mount
func socketDirVolumeMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: VolumeSocketDir, MountPath: HostPathSocketDir}
}

// getDriverImage returns the driver image, using the default if not specified
func getDriverImage(csi *csiv1alpha1.TrueNASCSI) string {
	if csi.Spec.DriverImage != "" {
		return csi.Spec.DriverImage
	}
	return DefaultDriverImage
}

// getLogLevel returns the log level, using the default if not specified
func getLogLevel(csi *csiv1alpha1.TrueNASCSI) int32 {
	if csi.Spec.LogLevel > 0 {
		return csi.Spec.LogLevel
	}
	return DefaultLogLevel
}

// getControllerReplicas returns the controller replicas, using the default if not specified
func getControllerReplicas(csi *csiv1alpha1.TrueNASCSI) int32 {
	if csi.Spec.ControllerReplicas > 0 {
		return csi.Spec.ControllerReplicas
	}
	return DefaultControllerReplicas
}

// getNamespace returns the namespace for CSI components
func getNamespace(csi *csiv1alpha1.TrueNASCSI) string {
	if csi.Spec.Namespace != "" {
		return csi.Spec.Namespace
	}
	return CSINamespace
}

// extractImageTag extracts the tag from an image reference
func extractImageTag(image string) string {
	if idx := strings.LastIndex(image, ":"); idx != -1 {
		return image[idx+1:]
	}
	return "latest"
}
