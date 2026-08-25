bin_filename := "aludel-cloudscale"

# Container image
IMG_TAG := "latest"
GHCR_IMG := "ghcr.io/helmetica-framework/aludel-cloudscale:" + IMG_TAG

# Namespace the driver is deployed into
NAMESPACE := "aludel-cloudscale-system"

# Local test cluster (athanor's kind cluster)
# renovate: datasource=github-releases depName=kubernetes-sigs/kind
KIND_VERSION := "v0.32.0"
KIND_CMD := "go run sigs.k8s.io/kind@" + KIND_VERSION
KIND_CLUSTER := "athanor"
