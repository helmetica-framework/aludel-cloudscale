#!/usr/bin/env bash
# Keeps the four COSI pins on one upstream commit.
#
# COSI v1alpha2 has no release tag, so aludel-cloudscale pins a commit on
# upstream main. Four things have to come from that same commit, because the
# CRDs, the sidecar, the controller and the gRPC API all change together:
#
#   1. the CRDs + controller manifests   config/crd/kustomization.yaml (?ref=<sha>)
#   2. the controller image              config/crd/kustomization.yaml (newTag ...-g<sha7>)
#   3. the sidecar image                 config/manager/deployment.yaml (...-g<sha7>)
#   4. the proto Go module               go.mod (pseudo-version ...-<sha12>)
#
# The ?ref= in (1) is the source of truth.
#
#   cosi-pins.sh check          fail unless all four agree
#   cosi-pins.sh bump [<ref>]   move all four to <ref> (a commit or branch, default main)
set -euo pipefail

repo=https://github.com/kubernetes-sigs/container-object-storage-interface
registry=https://gcr.io/v2/k8s-staging-sig-storage
proto_module=sigs.k8s.io/container-object-storage-interface/proto

kustomization=config/crd/kustomization.yaml
deployment=config/manager/deployment.yaml

die() {
	echo "error: $*" >&2
	exit 1
}

pinned_commit() {
	sed -nE 's|.*container-object-storage-interface\?ref=([0-9a-f]{40}).*|\1|p' "$kustomization"
}

controller_tag() {
	awk '/objectstorage-controller/ { found = 1 } found && /newTag:/ { print $2; exit }' "$kustomization"
}

sidecar_tag() {
	sed -nE 's|.*objectstorage-sidecar:([^ "]+).*|\1|p' "$deployment"
}

proto_version() {
	go list -m -f '{{.Version}}' "$proto_module"
}

# latest_tag_for <image> <sha>: the most recently pushed tag built from <sha>.
# Upstream re-tags the same commit on every scheduled build, so there can be
# several; any of them is the same image, the newest is just the tidiest.
latest_tag_for() {
	curl -fsSL "$registry/$1/tags/list" | python3 -I -c '
import json, re, sys
sha = sys.argv[1]
manifests = json.load(sys.stdin)["manifest"].values()
tags = [(int(m["timeUploadedMs"]), t) for m in manifests for t in m["tag"]
        if (g := re.search(r"-g([0-9a-f]{7,})$", t)) and sha.startswith(g.group(1))]
print(max(tags)[1] if tags else "")
' "$2"
}

check() {
	local sha controller sidecar proto ok=1
	sha=$(pinned_commit)
	[[ -n $sha ]] || die "no ?ref=<commit> found in $kustomization"

	controller=$(controller_tag)
	sidecar=$(sidecar_tag)
	proto=$(proto_version)

	echo "COSI pinned to $sha"
	report() { # <what> <value> <sha suffix>
		if [[ -n $3 && $sha == "$3"* ]]; then
			echo "  ok        $1: $2"
		else
			echo "  MISMATCH  $1: $2"
			ok=0
		fi
	}
	report "controller image ($kustomization)" "$controller" "${controller##*-g}"
	report "sidecar image ($deployment)" "$sidecar" "${sidecar##*-g}"
	report "proto module (go.mod)" "$proto" "${proto##*-}"

	if [[ $ok != 1 ]]; then
		echo >&2
		echo "The COSI pins disagree. Move them together with: just bump-cosi <commit>" >&2
		exit 1
	fi
}

bump() {
	local ref=${1:-main} sha short controller sidecar
	if [[ $ref =~ ^[0-9a-f]{40}$ ]]; then
		sha=$ref
	else
		sha=$(git ls-remote "$repo" "$ref" "refs/heads/$ref" | awk 'NR==1 {print $1}')
		[[ -n $sha ]] || die "cannot resolve $ref in $repo"
	fi
	short=${sha:0:7}
	echo "Bumping COSI to $sha"

	controller=$(latest_tag_for objectstorage-controller "$sha")
	sidecar=$(latest_tag_for objectstorage-sidecar "$sha")
	[[ -n $controller && -n $sidecar ]] ||
		die "no controller/sidecar image tagged *-g$short yet; upstream builds them after merge, try again later or pick an older commit"

	sed -i -E "s|(container-object-storage-interface\?ref=)[0-9a-f]{40}|\1$sha|" "$kustomization"
	sed -i -E "s|^( *newTag: *).*-g[0-9a-f]+$|\1$controller|" "$kustomization"
	sed -i -E "s|(objectstorage-sidecar:)[^ \"]+|\1$sidecar|" "$deployment"
	go get "$proto_module@$sha"
	go mod tidy

	check
	echo
	echo "Now build and test: the proto may have changed under the driver (just test)."
}

cmd=${1:-}
shift || true
case $cmd in
check) check ;;
bump) bump "$@" ;;
*) die "usage: $0 check | bump [<commit or branch>]" ;;
esac
