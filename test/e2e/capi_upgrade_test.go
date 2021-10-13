/*
Copyright 2021 The Kubernetes Authors.

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
	"context"

	. "github.com/onsi/ginkgo"

	capi_e2e "sigs.k8s.io/cluster-api/test/e2e"
)

var _ = Context("ClusterAPI Upgrade Tests", func() {
	Describe("Upgrading cluster from v1alpha3 to v1beta1 using clusterctl", func() {
		capi_e2e.ClusterctlUpgradeSpec(context.TODO(), func() capi_e2e.ClusterctlUpgradeSpecInput {
			return capi_e2e.ClusterctlUpgradeSpecInput{
				E2EConfig:                 e2eConfig,
				ClusterctlConfigPath:      clusterctlConfigPath,
				BootstrapClusterProxy:     bootstrapClusterProxy,
				ArtifactFolder:            artifactFolder,
				SkipCleanup:               skipCleanup,
				InitWithBinary:            e2eConfig.GetVariable("INIT_WITH_BINARY_V1ALPHA3"),
				InitWithProvidersContract: "v1alpha3",
				MgmtFlavor:                "remote-management",
			}
		})
	})

	Describe("Upgrading cluster from v1alpha4 to v1beta1 using clusterctl", func() {
		capi_e2e.ClusterctlUpgradeSpec(context.TODO(), func() capi_e2e.ClusterctlUpgradeSpecInput {
			return capi_e2e.ClusterctlUpgradeSpecInput{
				E2EConfig:                 e2eConfig,
				ClusterctlConfigPath:      clusterctlConfigPath,
				BootstrapClusterProxy:     bootstrapClusterProxy,
				ArtifactFolder:            artifactFolder,
				SkipCleanup:               skipCleanup,
				InitWithBinary:            e2eConfig.GetVariable("INIT_WITH_BINARY_V1ALPHA4"),
				InitWithProvidersContract: "v1alpha4",
				MgmtFlavor:                "remote-management",
			}
		})
	})
})
