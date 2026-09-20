//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// OpenShift/CRC Integration Tests
//
// These tests run against a real OpenShift cluster (CRC or full OpenShift) with
// a real TrueNAS instance to verify end-to-end volume provisioning.
//
// Required environment variables:
//   - TRUENAS_IP: TrueNAS IP address
//   - TRUENAS_API_KEY: API key for authentication
//
// Optional environment variables:
//   - TRUENAS_POOL: Pool to use for tests (default: "tank")
//   - TRUENAS_INSECURE: Skip TLS verification (default: "true")
//   - OPERATOR_IMAGE: Operator image to deploy (default: ghcr.io/kenmoini/truenas-csi-operator:latest)
//   - DRIVER_IMAGE: CSI driver image (default: uses operator default)
//   - SKIP_OPERATOR_DEPLOY: Skip operator deployment if already deployed (default: "false")
//
// Prerequisites:
//   - CRC or OpenShift cluster running and accessible via `oc`
//   - Logged in with cluster-admin privileges: `oc login -u kubeadmin ...`
//   - TrueNAS accessible from the cluster nodes
//
// Run with:
//   TRUENAS_IP=192.168.1.100 TRUENAS_API_KEY=your-key go test -v -tags=integration ./test/integration/...

const (
	operatorNamespace = "operator-system"
	csiNamespace      = "truenas-csi"
	testNamespace     = "truenas-csi-test"
	secretName        = "truenas-api-credentials"
	crName            = "truenas-integration-test"
)

var (
	truenasIP     string
	truenasAPIKey string
	truenasPool   string
	truenasURL    string
	operatorImage string
	driverImage   string
	skipDeploy    bool
)

func TestIntegration(t *testing.T) {
	RegisterFailHandler(Fail)

	// Check required environment variables
	truenasIP = os.Getenv("TRUENAS_IP")
	truenasAPIKey = os.Getenv("TRUENAS_API_KEY")

	if truenasIP == "" || truenasAPIKey == "" {
		t.Skip("Skipping integration tests: TRUENAS_IP and TRUENAS_API_KEY must be set")
	}

	// Verify we're logged into an OpenShift cluster
	if !isOpenShiftCluster() {
		t.Skip("Skipping integration tests: Not connected to an OpenShift cluster. Run 'oc login' first.")
	}

	truenasPool = os.Getenv("TRUENAS_POOL")
	if truenasPool == "" {
		truenasPool = "tank"
	}

	truenasURL = fmt.Sprintf("wss://%s/api/current", truenasIP)

	operatorImage = os.Getenv("OPERATOR_IMAGE")
	if operatorImage == "" {
		operatorImage = "ghcr.io/kenmoini/truenas-csi-operator:latest"
	}

	driverImage = os.Getenv("DRIVER_IMAGE")
	skipDeploy = os.Getenv("SKIP_OPERATOR_DEPLOY") == "true"

	fmt.Printf("Running integration tests against OpenShift cluster\n")
	fmt.Printf("  TrueNAS: %s\n", truenasIP)
	fmt.Printf("  Pool: %s\n", truenasPool)
	fmt.Printf("  Operator image: %s\n", operatorImage)

	RunSpecs(t, "OpenShift Integration Test Suite")
}

// isOpenShiftCluster checks if we're connected to an OpenShift cluster
func isOpenShiftCluster() bool {
	// Check if oc is available and we're logged in
	cmd := exec.Command("oc", "whoami")
	if err := cmd.Run(); err != nil {
		return false
	}

	// Verify it's OpenShift by checking for OpenShift-specific API
	cmd = exec.Command("oc", "api-resources", "--api-group=config.openshift.io")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(output), "clusterversions")
}

var _ = BeforeSuite(func() {
	By("Verifying OpenShift cluster connection")
	output, err := exec.Command("oc", "whoami").CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "Must be logged into OpenShift cluster")
	fmt.Printf("Logged in as: %s\n", strings.TrimSpace(string(output)))

	output, err = exec.Command("oc", "whoami", "--show-server").CombinedOutput()
	Expect(err).NotTo(HaveOccurred())
	fmt.Printf("Cluster: %s\n", strings.TrimSpace(string(output)))

	if !skipDeploy {
		By("Installing VolumeSnapshot CRDs")
		installSnapshotCRDs()

		By("Installing snapshot controller")
		installSnapshotController()

		By("Applying SecurityContextConstraints")
		applySCC()

		By("Deploying the operator")
		deployOperator()
	}

	By("Creating test namespace")
	runOC("create", "namespace", testNamespace, "--dry-run=client", "-o", "yaml")
	runOCPipe("create namespace "+testNamespace+" --dry-run=client -o yaml", "oc apply -f -")

	By("Creating CSI namespace")
	runOCPipe("create namespace "+csiNamespace+" --dry-run=client -o yaml", "oc apply -f -")

	By("Creating credentials secret")
	createCredentialsSecret()

	By("Waiting for CRD to be registered")
	waitForCRD()

	By("Creating TrueNASCSI resource")
	createTrueNASCSI()

	By("Waiting for CSI driver to be ready")
	waitForCSIDriver()

	By("Creating storage classes")
	createStorageClasses()
})

var _ = AfterSuite(func() {
	By("Cleaning up test resources")

	// Delete test PVCs first
	runOCIgnoreError("delete", "pvc", "--all", "-n", testNamespace)

	// Wait for volumes to be deleted
	time.Sleep(10 * time.Second)

	// Delete storage classes
	runOCIgnoreError("delete", "storageclass", "truenas-nfs-test", "truenas-iscsi-test")

	// Delete VolumeSnapshotClass
	runOCIgnoreError("delete", "volumesnapshotclass", "truenas-snapshot-test")

	// Delete TrueNASCSI CR
	runOCIgnoreError("delete", "truenascsi", crName)

	// Wait for CSI driver cleanup
	time.Sleep(10 * time.Second)

	// Delete namespaces
	runOCIgnoreError("delete", "namespace", testNamespace)

	if !skipDeploy {
		By("Undeploying the operator")
		undeployOperator()
	}
})

func applySCC() {
	// Apply the SCC from the deploy directory
	sccPath := "../../../deploy/openshift/scc.yaml"
	if _, err := os.Stat(sccPath); err == nil {
		cmd := exec.Command("oc", "apply", "-f", sccPath)
		output, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Printf("Warning: Failed to apply SCC: %v\nOutput: %s\n", err, output)
		}
	}
}

func installSnapshotCRDs() {
	// Install VolumeSnapshot CRDs from kubernetes-csi/external-snapshotter
	// These are required for snapshot functionality
	baseURL := "https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/master/client/config/crd"
	crds := []string{
		"snapshot.storage.k8s.io_volumesnapshotclasses.yaml",
		"snapshot.storage.k8s.io_volumesnapshotcontents.yaml",
		"snapshot.storage.k8s.io_volumesnapshots.yaml",
	}

	for _, crd := range crds {
		url := fmt.Sprintf("%s/%s", baseURL, crd)
		cmd := exec.Command("oc", "apply", "-f", url)
		output, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Printf("Warning: Failed to install snapshot CRD %s: %v\nOutput: %s\n", crd, err, output)
		}
	}

	// Wait for CRDs to be established
	Eventually(func() bool {
		cmd := exec.Command("oc", "get", "crd", "volumesnapshots.snapshot.storage.k8s.io", "-o", "name")
		output, _ := cmd.CombinedOutput()
		return strings.Contains(string(output), "volumesnapshots.snapshot.storage.k8s.io")
	}, 30*time.Second, 2*time.Second).Should(BeTrue(), "VolumeSnapshot CRDs should be installed")
}

func installSnapshotController() {
	// Install the snapshot controller from kubernetes-csi/external-snapshotter
	// This controller reconciles VolumeSnapshot resources
	baseURL := "https://raw.githubusercontent.com/kubernetes-csi/external-snapshotter/master/deploy/kubernetes/snapshot-controller"
	manifests := []string{
		"rbac-snapshot-controller.yaml",
		"setup-snapshot-controller.yaml",
	}

	for _, manifest := range manifests {
		url := fmt.Sprintf("%s/%s", baseURL, manifest)
		cmd := exec.Command("oc", "apply", "-f", url)
		output, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Printf("Warning: Failed to install snapshot controller %s: %v\nOutput: %s\n", manifest, err, output)
		}
	}

	// Wait for snapshot controller to be ready
	Eventually(func() bool {
		cmd := exec.Command("oc", "get", "deployment", "snapshot-controller", "-n", "kube-system",
			"-o", "jsonpath={.status.availableReplicas}")
		output, err := cmd.CombinedOutput()
		if err != nil {
			return false
		}
		replicas := strings.TrimSpace(string(output))
		return replicas != "" && replicas != "0"
	}, 2*time.Minute, 5*time.Second).Should(BeTrue(), "Snapshot controller should be available")
}

func deployOperator() {
	// Run make deploy from operator directory
	cmd := exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", operatorImage))
	cmd.Dir = "../.." // Go up from test/integration to operator directory
	output, err := cmd.CombinedOutput()
	if err != nil {
		Fail(fmt.Sprintf("Failed to deploy operator: %v\nOutput: %s", err, output))
	}

	// Wait for operator to be ready
	Eventually(func() bool {
		output, err := exec.Command("oc", "get", "deployment",
			"operator-controller-manager",
			"-n", operatorNamespace,
			"-o", "jsonpath={.status.availableReplicas}").CombinedOutput()
		if err != nil {
			return false
		}
		return strings.TrimSpace(string(output)) == "1"
	}, 3*time.Minute, 5*time.Second).Should(BeTrue(), "Operator should be available")
}

func undeployOperator() {
	cmd := exec.Command("make", "undeploy")
	cmd.Dir = "../.."
	cmd.CombinedOutput() // Ignore errors during cleanup
}

func createCredentialsSecret() {
	yaml := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: %s
type: Opaque
stringData:
  api-key: "%s"
`, secretName, csiNamespace, truenasAPIKey)

	applyYAML(yaml)
}

func waitForCRD() {
	// Wait for TrueNASCSI CRD to be registered
	Eventually(func() bool {
		cmd := exec.Command("oc", "get", "crd", "truenascsis.csi.truenas.io", "-o", "name")
		output, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Printf("CRD check failed: %v, output: %s\n", err, string(output))
			return false
		}
		result := strings.TrimSpace(string(output))
		return strings.Contains(result, "truenascsis.csi.truenas.io")
	}, 2*time.Minute, 5*time.Second).Should(BeTrue(), "TrueNASCSI CRD should be available")
}

func createTrueNASCSI() {
	insecure := os.Getenv("TRUENAS_INSECURE")
	if insecure == "" {
		insecure = "true"
	}

	yaml := fmt.Sprintf(`apiVersion: csi.truenas.io/v1alpha1
kind: TrueNASCSI
metadata:
  name: %s
spec:
  truenasURL: "%s"
  credentialsSecret: "%s"
  defaultPool: "%s"
  nfsServer: "%s"
  iscsiPortal: "%s:3260"
  insecureSkipTLS: %s
  namespace: "%s"
`, crName, truenasURL, secretName, truenasPool, truenasIP, truenasIP, insecure, csiNamespace)

	if driverImage != "" {
		yaml = strings.Replace(yaml, "spec:", fmt.Sprintf("spec:\n  driverImage: \"%s\"", driverImage), 1)
	}

	applyYAML(yaml)
}

func waitForCSIDriver() {
	// Wait for controller deployment
	Eventually(func() bool {
		output, err := exec.Command("oc", "get", "pods",
			"-n", csiNamespace,
			"-l", "app=truenas-csi-controller",
			"-o", "jsonpath={.items[0].status.phase}").CombinedOutput()
		if err != nil {
			return false
		}
		return strings.TrimSpace(string(output)) == "Running"
	}, 2*time.Minute, 5*time.Second).Should(BeTrue(), "Controller pod should be running")

	// Wait for node pods
	Eventually(func() bool {
		output, err := exec.Command("oc", "get", "pods",
			"-n", csiNamespace,
			"-l", "app=truenas-csi-node",
			"-o", "jsonpath={.items[*].status.phase}").CombinedOutput()
		if err != nil {
			return false
		}
		phases := strings.Fields(string(output))
		if len(phases) == 0 {
			return false
		}
		for _, phase := range phases {
			if phase != "Running" {
				return false
			}
		}
		return true
	}, 2*time.Minute, 5*time.Second).Should(BeTrue(), "Node pods should be running")
}

func createStorageClasses() {
	nfsClass := fmt.Sprintf(`apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: truenas-nfs-test
provisioner: csi.truenas.io
parameters:
  protocol: "nfs"
  pool: "%s"
allowVolumeExpansion: true
reclaimPolicy: Delete
volumeBindingMode: Immediate
`, truenasPool)

	iscsiClass := fmt.Sprintf(`apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: truenas-iscsi-test
provisioner: csi.truenas.io
parameters:
  protocol: "iscsi"
  pool: "%s"
allowVolumeExpansion: true
reclaimPolicy: Delete
volumeBindingMode: Immediate
`, truenasPool)

	snapshotClass := `apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshotClass
metadata:
  name: truenas-snapshot-test
driver: csi.truenas.io
deletionPolicy: Delete
`

	applyYAML(nfsClass)
	applyYAML(iscsiClass)
	applyYAML(snapshotClass)
}

// =============================================================================
// Volume Provisioning Tests
// =============================================================================

var _ = Describe("Volume Provisioning", func() {
	Context("NFS Volumes", func() {
		var pvcName string

		BeforeEach(func() {
			pvcName = fmt.Sprintf("test-nfs-%d", time.Now().UnixNano())
		})

		AfterEach(func() {
			runOCIgnoreError("delete", "pvc", pvcName, "-n", testNamespace)
		})

		It("should provision an NFS volume", func() {
			By("Creating a PVC")
			pvc := fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: truenas-nfs-test
  resources:
    requests:
      storage: 1Gi
`, pvcName, testNamespace)

			applyYAML(pvc)

			By("Waiting for PVC to be bound")
			Eventually(func() string {
				output, _ := exec.Command("oc", "get", "pvc", pvcName,
					"-n", testNamespace,
					"-o", "jsonpath={.status.phase}").CombinedOutput()
				return strings.TrimSpace(string(output))
			}, 2*time.Minute, 5*time.Second).Should(Equal("Bound"))

			By("Verifying PV was created")
			output, err := exec.Command("oc", "get", "pvc", pvcName,
				"-n", testNamespace,
				"-o", "jsonpath={.spec.volumeName}").CombinedOutput()
			Expect(err).NotTo(HaveOccurred())
			pvName := strings.TrimSpace(string(output))
			Expect(pvName).NotTo(BeEmpty())

			By("Verifying PV has correct CSI driver")
			output, err = exec.Command("oc", "get", "pv", pvName,
				"-o", "jsonpath={.spec.csi.driver}").CombinedOutput()
			Expect(err).NotTo(HaveOccurred())
			Expect(strings.TrimSpace(string(output))).To(Equal("csi.truenas.io"))
		})

		It("should delete NFS volume when PVC is deleted", func() {
			By("Creating a PVC")
			pvc := fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: truenas-nfs-test
  resources:
    requests:
      storage: 1Gi
`, pvcName, testNamespace)

			applyYAML(pvc)

			By("Waiting for PVC to be bound")
			Eventually(func() string {
				output, _ := exec.Command("oc", "get", "pvc", pvcName,
					"-n", testNamespace,
					"-o", "jsonpath={.status.phase}").CombinedOutput()
				return strings.TrimSpace(string(output))
			}, 2*time.Minute, 5*time.Second).Should(Equal("Bound"))

			// Get PV name before deleting
			output, _ := exec.Command("oc", "get", "pvc", pvcName,
				"-n", testNamespace,
				"-o", "jsonpath={.spec.volumeName}").CombinedOutput()
			pvName := strings.TrimSpace(string(output))

			By("Deleting the PVC")
			runOC("delete", "pvc", pvcName, "-n", testNamespace)

			By("Verifying PV is deleted")
			Eventually(func() bool {
				output, err := exec.Command("oc", "get", "pv", pvName).CombinedOutput()
				return err != nil || strings.Contains(string(output), "not found")
			}, 2*time.Minute, 5*time.Second).Should(BeTrue())
		})
	})

	Context("iSCSI Volumes", func() {
		var pvcName string

		BeforeEach(func() {
			pvcName = fmt.Sprintf("test-iscsi-%d", time.Now().UnixNano())
		})

		AfterEach(func() {
			runOCIgnoreError("delete", "pvc", pvcName, "-n", testNamespace)
		})

		It("should provision an iSCSI volume", func() {
			By("Creating a PVC")
			pvc := fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes:
    - ReadWriteOnce
  storageClassName: truenas-iscsi-test
  resources:
    requests:
      storage: 1Gi
`, pvcName, testNamespace)

			applyYAML(pvc)

			By("Waiting for PVC to be bound")
			Eventually(func() string {
				output, _ := exec.Command("oc", "get", "pvc", pvcName,
					"-n", testNamespace,
					"-o", "jsonpath={.status.phase}").CombinedOutput()
				return strings.TrimSpace(string(output))
			}, 2*time.Minute, 5*time.Second).Should(Equal("Bound"))

			By("Verifying PV was created")
			output, err := exec.Command("oc", "get", "pvc", pvcName,
				"-n", testNamespace,
				"-o", "jsonpath={.spec.volumeName}").CombinedOutput()
			Expect(err).NotTo(HaveOccurred())
			pvName := strings.TrimSpace(string(output))
			Expect(pvName).NotTo(BeEmpty())
		})
	})
})

// =============================================================================
// Volume Expansion Tests
// =============================================================================

var _ = Describe("Volume Expansion", func() {
	var pvcName string

	BeforeEach(func() {
		pvcName = fmt.Sprintf("test-expand-%d", time.Now().UnixNano())

		// Create initial PVC
		pvc := fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: truenas-nfs-test
  resources:
    requests:
      storage: 1Gi
`, pvcName, testNamespace)

		applyYAML(pvc)

		// Wait for bound
		Eventually(func() string {
			output, _ := exec.Command("oc", "get", "pvc", pvcName,
				"-n", testNamespace,
				"-o", "jsonpath={.status.phase}").CombinedOutput()
			return strings.TrimSpace(string(output))
		}, 2*time.Minute, 5*time.Second).Should(Equal("Bound"))
	})

	AfterEach(func() {
		runOCIgnoreError("delete", "pvc", pvcName, "-n", testNamespace)
	})

	It("should expand a volume", func() {
		By("Patching PVC to request more storage")
		runOC("patch", "pvc", pvcName, "-n", testNamespace,
			"--type=merge", "-p", `{"spec":{"resources":{"requests":{"storage":"2Gi"}}}}`)

		By("Waiting for expansion to complete")
		Eventually(func() string {
			output, _ := exec.Command("oc", "get", "pvc", pvcName,
				"-n", testNamespace,
				"-o", "jsonpath={.status.capacity.storage}").CombinedOutput()
			return strings.TrimSpace(string(output))
		}, 2*time.Minute, 5*time.Second).Should(Equal("2Gi"))
	})
})

// =============================================================================
// Snapshot Tests
// =============================================================================

var _ = Describe("Volume Snapshots", func() {
	var pvcName, snapshotName string

	BeforeEach(func() {
		pvcName = fmt.Sprintf("test-snap-src-%d", time.Now().UnixNano())
		snapshotName = fmt.Sprintf("test-snapshot-%d", time.Now().UnixNano())

		// Create source PVC
		pvc := fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: truenas-nfs-test
  resources:
    requests:
      storage: 1Gi
`, pvcName, testNamespace)

		applyYAML(pvc)

		Eventually(func() string {
			output, _ := exec.Command("oc", "get", "pvc", pvcName,
				"-n", testNamespace,
				"-o", "jsonpath={.status.phase}").CombinedOutput()
			return strings.TrimSpace(string(output))
		}, 2*time.Minute, 5*time.Second).Should(Equal("Bound"))
	})

	AfterEach(func() {
		runOCIgnoreError("delete", "volumesnapshot", snapshotName, "-n", testNamespace)
		runOCIgnoreError("delete", "pvc", pvcName, "-n", testNamespace)
	})

	It("should create a volume snapshot", func() {
		By("Creating a VolumeSnapshot")
		snapshot := fmt.Sprintf(`apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: %s
  namespace: %s
spec:
  volumeSnapshotClassName: truenas-snapshot-test
  source:
    persistentVolumeClaimName: %s
`, snapshotName, testNamespace, pvcName)

		applyYAML(snapshot)

		By("Waiting for snapshot to be ready")
		Eventually(func() string {
			output, _ := exec.Command("oc", "get", "volumesnapshot", snapshotName,
				"-n", testNamespace,
				"-o", "jsonpath={.status.readyToUse}").CombinedOutput()
			return strings.TrimSpace(string(output))
		}, 2*time.Minute, 5*time.Second).Should(Equal("true"))
	})

	It("should restore a volume from snapshot", func() {
		By("Creating a VolumeSnapshot")
		snapshot := fmt.Sprintf(`apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: %s
  namespace: %s
spec:
  volumeSnapshotClassName: truenas-snapshot-test
  source:
    persistentVolumeClaimName: %s
`, snapshotName, testNamespace, pvcName)

		applyYAML(snapshot)

		Eventually(func() string {
			output, _ := exec.Command("oc", "get", "volumesnapshot", snapshotName,
				"-n", testNamespace,
				"-o", "jsonpath={.status.readyToUse}").CombinedOutput()
			return strings.TrimSpace(string(output))
		}, 2*time.Minute, 5*time.Second).Should(Equal("true"))

		By("Creating a PVC from snapshot")
		restoreName := fmt.Sprintf("restored-%d", time.Now().UnixNano())
		restorePVC := fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: truenas-nfs-test
  resources:
    requests:
      storage: 1Gi
  dataSource:
    name: %s
    kind: VolumeSnapshot
    apiGroup: snapshot.storage.k8s.io
`, restoreName, testNamespace, snapshotName)

		applyYAML(restorePVC)

		defer runOCIgnoreError("delete", "pvc", restoreName, "-n", testNamespace)

		By("Waiting for restored PVC to be bound")
		Eventually(func() string {
			output, _ := exec.Command("oc", "get", "pvc", restoreName,
				"-n", testNamespace,
				"-o", "jsonpath={.status.phase}").CombinedOutput()
			return strings.TrimSpace(string(output))
		}, 3*time.Minute, 5*time.Second).Should(Equal("Bound"))
	})
})

// =============================================================================
// Clone Tests
// =============================================================================

var _ = Describe("Volume Cloning", func() {
	var sourcePVCName string

	BeforeEach(func() {
		sourcePVCName = fmt.Sprintf("test-clone-src-%d", time.Now().UnixNano())

		pvc := fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: truenas-nfs-test
  resources:
    requests:
      storage: 1Gi
`, sourcePVCName, testNamespace)

		applyYAML(pvc)

		Eventually(func() string {
			output, _ := exec.Command("oc", "get", "pvc", sourcePVCName,
				"-n", testNamespace,
				"-o", "jsonpath={.status.phase}").CombinedOutput()
			return strings.TrimSpace(string(output))
		}, 2*time.Minute, 5*time.Second).Should(Equal("Bound"))
	})

	AfterEach(func() {
		runOCIgnoreError("delete", "pvc", sourcePVCName, "-n", testNamespace)
	})

	It("should clone a volume", func() {
		cloneName := fmt.Sprintf("test-clone-%d", time.Now().UnixNano())

		By("Creating a clone PVC")
		clonePVC := fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %s
  namespace: %s
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: truenas-nfs-test
  resources:
    requests:
      storage: 1Gi
  dataSource:
    name: %s
    kind: PersistentVolumeClaim
`, cloneName, testNamespace, sourcePVCName)

		applyYAML(clonePVC)

		defer runOCIgnoreError("delete", "pvc", cloneName, "-n", testNamespace)

		By("Waiting for clone PVC to be bound")
		Eventually(func() string {
			output, _ := exec.Command("oc", "get", "pvc", cloneName,
				"-n", testNamespace,
				"-o", "jsonpath={.status.phase}").CombinedOutput()
			return strings.TrimSpace(string(output))
		}, 3*time.Minute, 5*time.Second).Should(Equal("Bound"))

		By("Verifying clone has different PV than source")
		sourceOutput, _ := exec.Command("oc", "get", "pvc", sourcePVCName,
			"-n", testNamespace,
			"-o", "jsonpath={.spec.volumeName}").CombinedOutput()
		cloneOutput, _ := exec.Command("oc", "get", "pvc", cloneName,
			"-n", testNamespace,
			"-o", "jsonpath={.spec.volumeName}").CombinedOutput()

		sourcePV := strings.TrimSpace(string(sourceOutput))
		clonePV := strings.TrimSpace(string(cloneOutput))

		Expect(clonePV).NotTo(Equal(sourcePV))
	})
})

// =============================================================================
// TrueNASCSI Status Tests
// =============================================================================

var _ = Describe("TrueNASCSI Status", func() {
	It("should report Running phase", func() {
		Eventually(func() string {
			output, _ := exec.Command("oc", "get", "truenascsi", crName,
				"-o", "jsonpath={.status.phase}").CombinedOutput()
			return strings.TrimSpace(string(output))
		}, 1*time.Minute, 5*time.Second).Should(Equal("Running"))
	})

	It("should report controller ready", func() {
		output, err := exec.Command("oc", "get", "truenascsi", crName,
			"-o", "jsonpath={.status.controllerReady}").CombinedOutput()
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(string(output))).To(Equal("true"))
	})

	It("should report node daemonset ready", func() {
		output, err := exec.Command("oc", "get", "truenascsi", crName,
			"-o", "jsonpath={.status.nodeDaemonSetReady}").CombinedOutput()
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(string(output))).To(Equal("true"))
	})

	It("should have Ready condition", func() {
		output, err := exec.Command("oc", "get", "truenascsi", crName,
			"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}").CombinedOutput()
		Expect(err).NotTo(HaveOccurred())
		Expect(strings.TrimSpace(string(output))).To(Equal("True"))
	})
})

// =============================================================================
// Helper Functions
// =============================================================================

func runOC(args ...string) {
	cmd := exec.Command("oc", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		Fail(fmt.Sprintf("oc %s failed: %v\nOutput: %s", strings.Join(args, " "), err, output))
	}
}

func runOCIgnoreError(args ...string) {
	cmd := exec.Command("oc", args...)
	cmd.CombinedOutput() // Ignore errors
}

func runOCPipe(cmd1, cmd2 string) {
	// Simple pipe execution for "oc create ... | oc apply -f -" patterns
	shell := exec.Command("sh", "-c", "oc "+cmd1+" | "+cmd2)
	output, err := shell.CombinedOutput()
	if err != nil {
		// Ignore "already exists" errors
		if !strings.Contains(string(output), "already exists") {
			fmt.Printf("Warning: %s | %s: %v\n", cmd1, cmd2, err)
		}
	}
}

func applyYAML(yaml string) {
	cmd := exec.Command("oc", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(yaml)
	output, err := cmd.CombinedOutput()
	if err != nil {
		Fail(fmt.Sprintf("oc apply failed: %v\nYAML:\n%s\nOutput: %s", err, yaml, output))
	}
}

// contextWithTimeout creates a context with timeout for operations
func contextWithTimeout(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}
