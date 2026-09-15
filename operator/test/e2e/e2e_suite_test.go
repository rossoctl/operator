/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/rossoctl/operator/test/utils"
)

var (
	// Optional Environment Variables:
	// - E2E_BUILD_SKIP=true: Skips all image builds, Kind image loading, and infrastructure
	//   installs/teardown. Use when running against a pre-installed operator on an existing cluster.
	// - PROMETHEUS_INSTALL_SKIP=true: Skips Prometheus Operator installation during test setup.
	// - CERT_MANAGER_INSTALL_SKIP=true: Skips CertManager installation during test setup.
	// - SPIRE_INSTALL_SKIP=true: Skips SPIRE installation during test setup.
	// These variables are useful if Prometheus, CertManager, or SPIRE is already installed,
	// avoiding re-installation and conflicts.
	skipBuild              = os.Getenv("E2E_BUILD_SKIP") == "true"
	skipPrometheusInstall  = os.Getenv("PROMETHEUS_INSTALL_SKIP") == "true"
	skipCertManagerInstall = os.Getenv("CERT_MANAGER_INSTALL_SKIP") == "true"
	skipSpireInstall       = os.Getenv("SPIRE_INSTALL_SKIP") == "true"
	// isPrometheusOperatorAlreadyInstalled will be set true when prometheus CRDs be found on the cluster
	isPrometheusOperatorAlreadyInstalled = false
	// isCertManagerAlreadyInstalled will be set true when CertManager CRDs be found on the cluster
	isCertManagerAlreadyInstalled = false
	// isSpireAlreadyInstalled will be set true when SPIRE CRDs are found on the cluster
	isSpireAlreadyInstalled = false

	// projectImage is the name of the image which will be build and loaded
	// with the code source changes to be tested.
	projectImage = "example.com/operator:v0.0.1"

	// signerImage is the agentcard-signer init-container image
	signerImage = "ghcr.io/rossoctl/operator/agentcard-signer:e2e-test"

	// sidecarImages are the AuthBridge sidecar images to pull and load into Kind.
	// cortex ships three combined images plus proxy-init:
	//   * authbridge-envoy: envoy-sidecar mode (Envoy + ext_proc + bundled spiffe-helper)
	//   * authbridge:       proxy-sidecar mode (authbridge-proxy + bundled spiffe-helper)
	//   * authbridge-lite:  lite mode (jwt-validation + token-exchange only)
	//   * proxy-init:       iptables init container, envoy-sidecar mode only
	// Spiffe-helper and client-registration are no longer separate images.
	// Keep tags in sync with charts/operator/values.yaml (defaults.images.*) so
	// e2e exercises the images the chart actually ships, not :latest.
	sidecarImages = []string{
		"ghcr.io/rossoctl/cortex/authbridge-envoy:v0.7.0-rc.1",
		"ghcr.io/rossoctl/cortex/authbridge:v0.7.0-rc.1",
		"ghcr.io/rossoctl/cortex/authbridge-lite:v0.7.0-rc.1",
		"ghcr.io/rossoctl/cortex/proxy-init:v0.7.0-rc.1",
	}
)

// TestE2E runs the end-to-end (e2e) test suite for the project. These tests execute in an isolated,
// temporary environment to validate project changes with the the purposed to be used in CI jobs.
// The default setup requires Kind, builds/loads the Manager Docker image locally, and installs
// CertManager and Prometheus.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting rossoctl-operator integration test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	if skipBuild {
		_, _ = fmt.Fprintf(GinkgoWriter, "E2E_BUILD_SKIP=true — skipping all image builds and infrastructure installs\n")
		return
	}

	By("Ensure that Prometheus is enabled")
	_ = utils.UncommentCode("config/default/kustomization.yaml", "#- ../prometheus", "#")

	containerTool := utils.DetectContainerTool()
	_, _ = fmt.Fprintf(GinkgoWriter, "Using container tool: %s\n", containerTool)

	By("building the manager(Operator) image")
	cmd := exec.Command("make", "docker-build",
		fmt.Sprintf("IMG=%s", projectImage),
		fmt.Sprintf("CONTAINER_TOOL=%s", containerTool))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager(Operator) image")

	By("loading the manager(Operator) image on Kind")
	err = utils.LoadImageToKindClusterWithName(projectImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager(Operator) image into Kind")

	if !skipPrometheusInstall {
		By("checking if prometheus is installed already")
		isPrometheusOperatorAlreadyInstalled = utils.IsPrometheusCRDsInstalled()
		if !isPrometheusOperatorAlreadyInstalled {
			_, _ = fmt.Fprintf(GinkgoWriter, "Installing Prometheus Operator...\n")
			Expect(utils.InstallPrometheusOperator()).To(Succeed(), "Failed to install Prometheus Operator")
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: Prometheus Operator is already installed. Skipping installation...\n")
		}
	}
	if !skipCertManagerInstall {
		By("checking if cert manager is installed already")
		isCertManagerAlreadyInstalled = utils.IsCertManagerCRDsInstalled()
		if !isCertManagerAlreadyInstalled {
			_, _ = fmt.Fprintf(GinkgoWriter, "Installing CertManager...\n")
			Expect(utils.InstallCertManager()).To(Succeed(), "Failed to install CertManager")
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: CertManager is already installed. Skipping installation...\n")
		}
	}

	By("building the agentcard-signer image")
	cmd = exec.Command("make", "build-signer",
		fmt.Sprintf("SIGNER_IMG=%s", signerImage),
		fmt.Sprintf("CONTAINER_TOOL=%s", containerTool))
	_, err = utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the agentcard-signer image")

	By("loading the agentcard-signer image on Kind")
	err = utils.LoadImageToKindClusterWithName(signerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the agentcard-signer image into Kind")

	By("pulling and loading AuthBridge sidecar images into Kind")
	Expect(utils.PullAndLoadSidecarImages(sidecarImages)).To(Succeed(),
		"Failed to pull and load AuthBridge sidecar images")

	if !skipSpireInstall {
		By("checking if SPIRE is installed already")
		isSpireAlreadyInstalled = utils.IsSpireCRDsInstalled()
		if !isSpireAlreadyInstalled {
			_, _ = fmt.Fprintf(GinkgoWriter, "Installing SPIRE...\n")
			Expect(utils.InstallSpire("example.org")).To(Succeed(), "Failed to install SPIRE")
			Expect(utils.WaitForSpireReady(10*time.Minute)).To(Succeed(), "SPIRE pods not ready in time")
		} else {
			_, _ = fmt.Fprintf(GinkgoWriter, "WARNING: SPIRE is already installed. Skipping installation...\n")
		}
	}
})

var _ = AfterSuite(func() {
	if skipBuild {
		_, _ = fmt.Fprintf(GinkgoWriter, "E2E_BUILD_SKIP=true — skipping infrastructure teardown\n")
		return
	}

	// Teardown Prometheus, CertManager, and SPIRE after the suite if not skipped
	// and if they were not already installed
	if !skipPrometheusInstall && !isPrometheusOperatorAlreadyInstalled {
		_, _ = fmt.Fprintf(GinkgoWriter, "Uninstalling Prometheus Operator...\n")
		utils.UninstallPrometheusOperator()
	}
	if !skipCertManagerInstall && !isCertManagerAlreadyInstalled {
		_, _ = fmt.Fprintf(GinkgoWriter, "Uninstalling CertManager...\n")
		utils.UninstallCertManager()
	}
	if !skipSpireInstall && !isSpireAlreadyInstalled {
		_, _ = fmt.Fprintf(GinkgoWriter, "Uninstalling SPIRE...\n")
		utils.UninstallSpire()
	}
})
