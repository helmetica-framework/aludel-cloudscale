# aludel-cloudscale

> An aludel is the sealed vessel an alchemist sublimes into — a container you
> put things in and close.

`aludel-cloudscale` is a [COSI](https://container-object-storage-interface.sigs.k8s.io/)
driver for [cloudscale.ch](https://www.cloudscale.ch) object storage. It lets a
Kubernetes workload ask for an S3 bucket with a `BucketClaim` and receive
credentials in a Secret, without anybody touching the cloudscale.ch control
panel.

## The one design constraint

cloudscale.ch has **no IAM, no roles, and no bucket policies**. The API offers
exactly two primitives: objects users (`/v1/objects-users`) and their S3 key
pairs. `PutBucketPolicy` is not available on the S3 endpoint either.

That means a bucket is reachable by exactly one identity: the objects user that
created it. `aludel-cloudscale` therefore runs a strict **one objects user per bucket**
model:

| COSI RPC | What aludel-cloudscale does |
| --- | --- |
| `DriverCreateBucket` | Creates a tagged objects user, then creates the bucket over S3 **as that user** |
| `DriverGrantBucketAccess` | Reads the owning user's key pair back out of the cloudscale.ch API |
| `DriverRevokeBucketAccess` | **No-op** |
| `DriverDeleteBucket` | Deletes the bucket, then the objects user |

### Consequences you should know before adopting this

- **`authenticationType: IAM` is unsupported.** `aludel-cloudscale` returns
  `Unimplemented`. Use `KEY`.
- **Every `BucketAccess` on the same `Bucket` gets identical credentials.**
  There is no mechanism to admit a second identity to an existing bucket, so
  per-workload isolation within one bucket is not possible. Give each workload
  its own `BucketClaim`.
- **Revoke does nothing.** Deleting or rotating the only key pair would not
  revoke one grant, it would sever every grant and lock the driver out of its
  own bucket. Access ends when the `Bucket` is deleted.
- **Brownfield buckets are not adoptable.** A bucket created outside `aludel-cloudscale`
  has an owning user the driver does not know about.

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

`BucketAccessClass` needs no parameters beyond `authenticationType: KEY`.

See `config/samples/bucketclass.yaml` for a full worked example.

## Running it

```console
just build        # build the binary
just test         # unit tests
just build-docker # container image
just install      # apply config/rbac and config/manager
```

The driver serves gRPC on a unix socket shared with the upstream COSI sidecar,
which is what actually watches the Kubernetes resources. `config/manager`
deploys both containers together.

## Debugging

`aludel-cloudscale selftest` runs `DriverCreateBucket` → `DriverGrantBucketAccess` →
`DriverDeleteBucket` in-process against the real cloudscale.ch API, with no
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
`BucketClaim`. From `controller/pkg/bucketclaim/bucketclaim.go`:

```go
bucketName = fmt.Sprintf("bucket-%s", bucketClaim.ObjectMeta.UID)
```

So the real bucket and its objects user are called `bucket-<uuid>`. Looking for
the claim's name in the cloudscale.ch control panel will not find anything.

## Trying it on the athanor kind cluster

> **This talks to the real cloudscale.ch API.** There is no emulator: every
> `BucketClaim` you create here provisions a real objects user and a real,
> billed bucket in your cloudscale.ch project. Use a throwaway project, and
> delete your claims when you are done.

From the athanor devcontainer, with the cluster already up (`just ignite`):

```console
export KUBECONFIG=/workspaces/athanor/.kind/kind-config
cd /workspaces/athanor/aludel-cloudscale
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

A successful run leaves a `Bucket` whose `spec.bucketID` looks like
`rma/my-bucket-<uuid>/<objectsUserID>`, and a Secret with the credentials:

```console
kubectl get secret my-bucket-credentials \
  -o jsonpath='{.data.BucketInfo}' | base64 -d | jq
```

**6. Prove the credentials work** from inside the cluster:

```console
kubectl run s3 --rm -it --restart=Never --image=amazon/aws-cli:latest \
  --env=AWS_ACCESS_KEY_ID=<accessKeyID from the secret> \
  --env=AWS_SECRET_ACCESS_KEY=<accessSecretKey from the secret> \
  -- --endpoint-url https://objects.rma.cloudscale.ch s3 ls
```

**7. Clean up** — deleting the `BucketClaim` removes the bucket *and* its
objects user, because the sample `BucketClass` sets `deletionPolicy: Delete`:

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
NAME            READY   BUCKET ID                          CLASS            AGE
bucket-8f2a..   true    lpg/bucket-8f2a…/1228191ff5215a…   cloudscale-lpg   2m
```

The `Bucket ID` column is doing real work here: aludel-cloudscale encodes
`<region>/<bucket>/<objectsUserID>` into it, so the owning objects user is
visible without going to the cloudscale.ch panel. `Driver` is `priority: 1`,
so it only shows under `kubectl get -o wide`.

**Status fields: not yours.** `BucketStatus` is exactly `bucketReady` and
`bucketID`; `BucketClaimStatus` is `bucketReady` and `bucketName`;
`BucketAccessStatus` is `accessGranted` and `accountID`. There are no
conditions, no message field, and no extension point. The driver cannot
contribute status either — `DriverCreateBucketResponse` returns only a bucket
ID and a protocol, and the sidecar decides what to write. Adding a field means
forking the CRD *and* the sidecar, and the fork would be overwritten by the
next upstream install.

When you need to surface more than a boolean, use events instead — the sidecar
copies the driver's gRPC error message verbatim onto the `BucketClaim`:

```console
kubectl describe bucketclaim <name>
kubectl get events --field-selector involvedObject.kind=BucketClaim
```

That is why aludel-cloudscale's error strings name the bucket, the region and the failing
operation: the event is the only place a human sees them.

## Idempotency and orphans

COSI retries RPCs, so `DriverCreateBucket` must not create a second objects user
on a second attempt. Every user `aludel-cloudscale` creates is tagged:

```
aludel_managed_by=<driver name>
aludel_bucket=<bucket name>
```

`DriverCreateBucket` does a server-side tag-filtered list first and adopts an
existing user if it finds one. If the S3 call failed on a previous attempt the
user survives and gets reused.

A user is only genuinely orphaned if the `BucketClaim` is abandoned between the
two steps. There is no garbage collector for that yet — the tags are there so
one can be written.

## Status

Pre-alpha, and so is COSI itself. This targets COSI **v1alpha1** as released in
`sigs.k8s.io/container-object-storage-interface` v0.2.2. Upstream `main` is
already on pre-alpha v1alpha2 with explicitly breaking API changes ahead; see
the [v1alpha2 KEP](https://github.com/kubernetes/enhancements/pull/4599).

## Prior art

[`vshn/provider-cloudscale`](https://github.com/vshn/provider-cloudscale) solves
the same provisioning problem as a Crossplane provider. Its split between an
`ObjectsUser` managed resource and a `Bucket` that consumes that user's
credentials is the same boundary `aludel-cloudscale` draws inside `DriverCreateBucket`.
