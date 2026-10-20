# aludel-cloudscale

> An aludel is the sealed vessel an alchemist sublimes into — a container you
> put things in and close.

`aludel-cloudscale` is a [COSI](https://container-object-storage-interface.sigs.k8s.io/)
driver for [cloudscale.ch](https://www.cloudscale.ch) object storage. It lets a
Kubernetes workload ask for an S3 bucket with a `BucketClaim` and receive
credentials in a Secret, without anybody touching the cloudscale.ch control
panel.

It implements COSI **v1alpha2**, pinned to upstream commit
[`274540c`](https://github.com/kubernetes-sigs/container-object-storage-interface/commit/274540c69400dc1e13278b149a4e073339cefde2)
(2026-10-19). See [Status](#status) and [Upgrading COSI](#upgrading-cosi).

## Configuration

The driver needs a **read-write** cloudscale.ch API token in
`CLOUDSCALE_API_TOKEN`. Read-only tokens are rejected in effect: cloudscale.ch
omits `secret_key` from objects user responses for them, and `aludel-cloudscale` reads the
secret back on every access grant.

### Namespaces

Two different secrets are involved, and they live in different places:

| Secret | Namespace | Who creates it |
| --- | --- | --- |
| `aludel-cloudscale-credentials` (the API token) | `aludel-cloudscale-system`, with the Deployment | you, once per cluster |
| `<credentialsSecretName>` (per-bucket S3 keys) | the **workload's** namespace, next to its `BucketAccess` | the COSI sidecar, per grant |

The token is a cluster-wide credential with full access to every bucket in the
cloudscale.ch project, so it stays in the driver's namespace and is never
exposed to consumers. Workloads only ever see the key pair of their own
bucket's objects user.

```console
kubectl -n aludel-cloudscale-system create secret generic aludel-cloudscale-credentials \
  --from-literal=token="$CLOUDSCALE_API_TOKEN"
```

Deploying into a different namespace means overriding it in both places:
`namespace:` in `config/manager/kustomization.yaml` and `NAMESPACE` in
`Justfile.vars.just`.

### BucketClass parameters

| Parameter | Required | Default | Meaning |
| --- | --- | --- | --- |
| `region` | yes | — | cloudscale.ch region slug, e.g. `rma` or `lpg` |
| `endpointURL` | no | `https://objects.<region>.cloudscale.ch` | S3 endpoint override |
| `bucketDeletionPolicy` | no | `DeleteIfEmpty` | `DeleteIfEmpty` refuses to drop a non-empty bucket; `DeleteAll` purges every object and version first |
| `s3PathStyle` | no | `true` | Use path-style addressing instead of virtual-hosted |

`BucketAccessClass` needs nothing beyond `authenticationType: Key` and
`multiBucketAccess: SingleBucket` (the default). Its parameters, if any, are
parsed the same way, so `endpointURL` and `s3PathStyle` have to be repeated
there if the BucketClass overrides them: they are what ends up in the
credentials secret.

### What a BucketAccess can ask for

cloudscale.ch has no IAM and no bucket policies, so the only credential that
can reach a bucket is the key pair of the objects user that created it. That
shapes what aludel-cloudscale accepts:

| BucketAccess / class field | Accepted | Otherwise |
| --- | --- | --- |
| `authenticationType` | `Key` | `ServiceAccount` → `Unimplemented` |
| `protocol` | `S3` | `InvalidArgument` |
| `bucketClaims` | exactly one | more than one → `OutOfRange`, which the sidecar reports as "driver does not support multi-bucket access" |
| `accessModes.objectData`, `.objectMetadata`, `.bucketMetadata` | `ReadWrite`, or unset | `ReadOnly` / `WriteOnly` → `InvalidArgument`: the owner's key pair is always full access, and handing it out for a narrower request would over-grant |

Every `BucketAccess` on the same bucket gets the same key pair, and revoking
access is a no-op: rotating the owner's key would cut off every other
`BucketAccess` too. Access ends when the bucket is deleted.

The sidecar writes the credentials into the `accessSecretName` Secret in the
workload's namespace, one key per value:

| Key | Value |
| --- | --- |
| `COSI_S3_BUCKET_ID` | bucket name, `bc-<BucketClaim UID>` |
| `COSI_S3_ENDPOINT` | `https://objects.<region>.cloudscale.ch`, or `endpointURL` |
| `COSI_S3_REGION` | the region slug |
| `COSI_S3_ADDRESSING_STYLE` | `Path`, or `Virtual` with `s3PathStyle: "false"` |
| `COSI_S3_ACCESS_KEY_ID` | the owning objects user's access key |
| `COSI_S3_ACCESS_SECRET_KEY` | its secret key |

### Deleting buckets

With `deletionPolicy: Delete` on the BucketClass, deleting a `BucketClaim`
deletes its `Bucket`, and the driver then deletes the S3 bucket followed by
its objects user. `bucketDeletionPolicy` decides what happens to a bucket that
still holds objects:

- `DeleteAll` empties it (every object and version) and deletes it.
- `DeleteIfEmpty` refuses with `FailedPrecondition`. The sidecar treats that
  as retryable, so the `BucketClaim` stays in deletion, with the reason in the
  `Bucket`'s `status.error`, until somebody empties the bucket. The next retry
  then deletes the bucket and the objects user, and the claim goes away. A
  Bucket's parameters are immutable, so emptying it is the only way forward.

`deletionPolicy: Retain` leaves both the bucket and the objects user alone.

See `config/samples/bucketclass.yaml` for a full worked example.

## Running it

```console
just build        # build the binary
just test         # unit tests
just build-docker # container image
just install      # apply config/rbac and config/manager
```

Every pull request from this repository also gets an image on ghcr.io,
`ghcr.io/helmetica-framework/aludel-cloudscale:pr-<number>` (latest push) and
`:pr-<number>-<sha>` (one per commit), for trying a change on a test cluster
before it is released:

```console
cd config/manager
kustomize edit set image ghcr.io/helmetica-framework/aludel-cloudscale=ghcr.io/helmetica-framework/aludel-cloudscale:pr-<number>-<sha>
```

The driver serves gRPC on a unix socket shared with the upstream COSI sidecar,
which is what actually watches the Kubernetes resources. `config/manager`
deploys both containers together.

## Debugging

`aludel-cloudscale selftest` runs `DriverGenerateBucketId` → `DriverCreateBucket` →
`DriverGetBucket` → `DriverGrantBucketAccess` → `DriverDeleteBucket` in-process against the real cloudscale.ch API, with no
Kubernetes, sidecar or gRPC involved:

```console
export CLOUDSCALE_API_TOKEN=<read-write token>
./aludel-cloudscale selftest --region lpg
```

It splits a failure in two. If selftest passes, the cloudscale.ch and S3 paths
are healthy and the problem is in the COSI wiring — driver name mismatch,
sidecar, RBAC or CRDs. If it fails, the output names the failing RPC and the
underlying API error.

`--keep` leaves the bucket and objects user behind for inspection;
`--path-style=false` switches to virtual-hosted addressing.

Note that the COSI controller does **not** name the bucket after your
`BucketClaim`. From `controller/pkg/reconciler/bucketclaim.go`:

```go
name = "bc-" + string(claim.UID) // DO NOT CHANGE UNLESS ABSOLUTELY NECESSARY
```

The driver uses that name verbatim, so the bucket and its objects user are also
called `bc-<uuid>`: one name identifies them in `kubectl`, in the logs and in
the cloudscale.ch control panel. Looking for the claim's name in the panel
will not find anything.

## How provisioning works

COSI v1alpha2 provisions a bucket in two phases, and records the bucket ID on
the `Bucket` before anything exists in the backend:

1. **`DriverGenerateBucketId`** turns the suggested name and the BucketClass
   `region` into the ID `<region>/<bucket>`, e.g. `lpg/bc-6819c4e4-…`. It
   creates nothing, as the spec requires: COSI may drop the `Bucket` after
   this phase without ever calling `DriverDeleteBucket`.
2. **`DriverCreateBucket`** creates the objects user, tagged with the bucket
   name, and then the bucket with that user's own credentials.

Because the ID is fixed before the objects user exists, it cannot contain the
user's ID (v1alpha1 IDs were `<region>/<bucket>/<objectsUserID>`). Every later
call finds the owner through its tags instead (see
[Idempotency and orphans](#idempotency-and-orphans)), and the user's ID shows
up as the `BucketAccess`'s `status.accountID`.

`DriverGetBucket` serves statically provisioned Buckets. Set
`spec.existingBucketID` to `<region>/<bucket>`; the call succeeds only if an
objects user tagged by this driver owns that bucket and can see it, since
without one the driver could neither grant access nor delete it.

## Trying it on the athanor kind cluster

> **This talks to the real cloudscale.ch API.** There is no emulator: every
> `BucketClaim` you create here provisions a real objects user and a real,
> billed bucket in your cloudscale.ch project. Use a throwaway project, and
> delete your claims when you are done.

From the athanor devcontainer, with the cluster already up (`just ignite`):

```console
export KUBECONFIG=/workspaces/athanor/.kind/kind-config
mkdir /workspaces/athanor/reagents
cd /workspaces/athanor/reagents
git clone git@github.com:helmetica-framework/aludel-cloudscale.git 
cd /workspaces/athanor/reagents/aludel-cloudscale
```

**1. Install COSI itself** — the CRDs and the central controller, which is a
separate component from this driver:

```console
just install-cosi
```

**2. Give the driver a cloudscale.ch token.** It must be read-write, and it
must live in `aludel-cloudscale-system` alongside the Deployment — `secretKeyRef` only
resolves within the pod's own namespace:

```console
kubectl create namespace aludel-cloudscale-system
kubectl -n aludel-cloudscale-system create secret generic aludel-cloudscale-credentials \
  --from-literal=token="$CLOUDSCALE_API_TOKEN"
```

**3. Build and side-load the driver:**

```console
just deploy-kind
```

This builds the image, `kind load`s it into the `athanor` cluster (no registry
needed) and rolls out the Deployment. Re-run it after every code change.

**4. Provision a bucket:**

```console
kubectl apply -f config/samples/bucketclass.yaml
```

**5. Watch it work:**

```console
kubectl get bucketclaims,buckets
kubectl -n aludel-cloudscale-system logs deployment/aludel-cloudscale -c aludel-cloudscale -f
```

A successful run leaves `Bucket` and `BucketClaim` with `READY true`, a bucket
ID like `lpg/bc-<uuid>`, a `BucketAccess` with `READY true` and the objects
user's ID as `ACCOUNT ID`, and a Secret with the credentials:

```console
kubectl get secret my-test-bucket-credentials -o json \
  | jq '.data | map_values(@base64d)'
```

If anything is stuck, `kubectl get buckets,bucketclaims,bucketaccesses -A -o wide`
adds an `ERROR` column with the last error from the driver or the controller.

**6. Prove the credentials work** from inside the cluster:

```console
kubectl apply -f config/samples/s3-check.yaml
kubectl logs -f s3-check   # prints "hello"
kubectl delete -f config/samples/s3-check.yaml
```

`s3-check.yaml` mounts the Secret as environment variables and writes, reads
and deletes an object with them.

**7. Clean up** — deleting the `BucketClaim` removes the bucket *and* its
objects user, because the sample `BucketClass` sets `deletionPolicy: Delete`.
It uses `bucketDeletionPolicy: DeleteIfEmpty`, so delete any objects you left
in the bucket first, or the claim waits until you do (see
[Deleting buckets](#deleting-buckets)):

```console
kubectl delete -f config/samples/bucketclass.yaml
```

Then confirm in the cloudscale.ch control panel that no objects user survived.
Until a garbage collector exists, that check is manual.

## cloudscale.ch behaviours worth knowing

Two API characteristics are not obvious from the documentation and both cost a
debugging session:

**At most one `tag:` clause per request.** Filtering a list by two tags returns
`400 At most one tag: clause can be passed`. `FindByTags` therefore pushes a
single tag down to the API and applies the rest client-side. The client-side
pass is not cosmetic: a list narrowed only by `aludel_bucket` can match users
tagged by a different driver, and adopting one would hand out credentials that
cannot read the bucket.

**Key pairs propagate asynchronously.** A key returned by
`POST /v1/objects-users` is not immediately valid at the S3 endpoint; the first
`CreateBucket` can fail with `403 InvalidAccessKeyId` before it settles.
`DriverCreateBucket` retries on exactly that error with exponential backoff
(500ms → 4s, 30s budget) and gives up with `Unavailable` so the sidecar retries
the RPC rather than failing the claim. Every other S3 error still fails on the
first attempt.

**Objects users are not region-scoped.** The same user works against both
`objects.rma.cloudscale.ch` and `objects.lpg.cloudscale.ch`, even though the
two regions are separate storage infrastructures and cross-region requests to
an existing bucket get a `301`. Verified with `aludel-cloudscale selftest` against both.

## What you can and cannot add to the COSI resources

The `Bucket`, `BucketClaim` and `BucketAccess` APIs belong to upstream COSI, not
to this driver, which puts a hard line through the middle of this question.

**Printer columns: yours to change.** `additionalPrinterColumns` lives on the
CRD, not in the API types, so `config/crd` patches them onto the upstream CRDs
during install. Upstream ships none at all, which is why a bare
`kubectl get buckets` shows only NAME and AGE. Every column reads a field
upstream already populates:

```console
$ kubectl get buckets
NAME          READY   BUCKET ID         CLAIM            AGE
bc-8f2a…      true    lpg/bc-8f2a…      my-test-bucket   2m
```

`Driver` and `Error` are `priority: 1`, so they only show under
`kubectl get -o wide`.

**Status fields: not yours.** The status of every COSI resource is written by
the controller and the sidecar, not the driver: `readyToUse`, the IDs, the
protocols and bucket info, and `error`. The driver's only say is the content
of its gRPC responses, and the message of any gRPC error it returns, which the
sidecar copies verbatim into `status.error.message`:

```console
kubectl get bucket <name> -o jsonpath='{.status.error.message}'
```

That is why aludel-cloudscale's error strings name the bucket, the region and
the failing operation: that field is the only place a human sees them. The
v1alpha2 sidecar no longer emits events for them.

## Idempotency and orphans

COSI retries RPCs, so `DriverCreateBucket` must not create a second objects user
on a second attempt. Every user `aludel-cloudscale` creates is tagged:

```
aludel_managed_by=<driver name>
aludel_bucket=<bucket name>
```

`DriverCreateBucket` does a server-side tag-filtered list first and adopts an
existing user if it finds one. If the S3 call failed on a previous attempt the
user survives and gets reused. The same lookup is how `DriverGetBucket`,
`DriverGrantBucketAccess` and `DriverDeleteBucket` find a bucket's owner, and
if two users carry the tags of one bucket they all refuse to guess.

A user is only genuinely orphaned if the `BucketClaim` is abandoned between the
two steps. There is no garbage collector for that yet — the tags are there so
one can be written.

## Status

Pre-alpha, and so is COSI itself. This targets COSI **v1alpha2**, which
upstream serves only from `main`, with no release tag. Everything COSI is
therefore pinned to one upstream commit:

**[`274540c69400dc1e13278b149a4e073339cefde2`](https://github.com/kubernetes-sigs/container-object-storage-interface/commit/274540c69400dc1e13278b149a4e073339cefde2)**
(merge of PR #394, 2026-10-19)

| What | Where | Pin |
| --- | --- | --- |
| CRDs and controller manifests | `config/crd/kustomization.yaml` | `?ref=274540c…` |
| Controller image | `config/crd/kustomization.yaml` | `objectstorage-controller:v20261019-controllerv0.2.0-rc1-354-g274540c` |
| Sidecar image | `config/manager/deployment.yaml` | `objectstorage-sidecar:v20261019-controllerv0.2.0-rc1-354-g274540c` |
| Proto Go module | `go.mod` | `sigs.k8s.io/container-object-storage-interface/proto v0.0.0-20261019183510-274540c69400` |

All four have to come from the same commit: the CRDs, the sidecar and the gRPC
API change together, and upstream `main` still makes breaking changes to all
of them. v1alpha1 (COSI v0.2.2) is not supported any more, and existing
v1alpha1 resources are not converted.

## Upgrading COSI

Never bump one of the four pins on its own. Instead:

```console
just bump-cosi            # upstream main
just bump-cosi <commit>   # or a specific commit
```

`hack/cosi-pins.sh` resolves the commit, looks up the controller and sidecar
images built from it in `gcr.io/k8s-staging-sig-storage` (upstream builds them
after merge, so a brand new commit may not have any yet), and moves all four
pins. Then:

1. `go build ./... && just test`. Upstream changes the proto freely: read the
   diff of `proto/cosi.proto` between the old and the new commit and adapt
   `internal/driver`. `TestEveryRPCIsImplemented` fails if a new RPC appeared
   that the driver does not implement yet.
2. Check `sidecar/pkg/reconciler` for new `+kubebuilder:rbac` markers and
   mirror them in `config/rbac/role.yaml`, and the CRDs for renamed fields
   that the printer columns in `config/crd/kustomization.yaml` read.
3. Update `config/samples`, and the commit and table in [Status](#status).
4. Run the athanor kind walkthrough above end to end.

`just check-cosi-pins` (part of `just lint`, so CI runs it) fails whenever the
four pins disagree.

Renovate is told to leave all four alone. Instead it tracks upstream `main`
as a single `cosi-upstream-commit` dependency and opens a PR at most weekly
that moves only the `?ref=`. That PR fails `just check-cosi-pins` on purpose:
it is the reminder. Check out its branch, run `just bump-cosi <commit>`, and
carry on with the steps above.

## Prior art

[`vshn/provider-cloudscale`](https://github.com/vshn/provider-cloudscale) solves
the same provisioning problem as a Crossplane provider. Its split between an
`ObjectsUser` managed resource and a `Bucket` that consumes that user's
credentials is the same boundary `aludel-cloudscale` draws inside `DriverCreateBucket`.
