bin_filename := "aludel-cloudscale"

# Container image
IMG_TAG := "latest"
GHCR_IMG := "ghcr.io/helmetica-framework/aludel-cloudscale:" + IMG_TAG

# Namespace the driver is deployed into
NAMESPACE := "aludel-cloudscale-system"

# COSI upstream release providing the CRDs and central controller
# renovate: datasource=github-releases depName=kubernetes-sigs/container-object-storage-interface
COSI_VERSION := "v0.2.2"

# Local test cluster (athanor's kind cluster)
# renovate: datasource=github-releases depName=kubernetes-sigs/kind
KIND_VERSION := "v0.32.0"
KIND_CMD := "go run sigs.k8s.io/kind@" + KIND_VERSION
KIND_CLUSTER := "athanor"
