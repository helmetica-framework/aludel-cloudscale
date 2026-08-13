package driver

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	cosi "sigs.k8s.io/container-object-storage-interface/proto"
)

// Keys of the credential map handed back to the sidecar. They must match
// sigs.k8s.io/container-object-storage-interface/client/apis/objectstorage/consts,
// which is where the sidecar reads them from when it assembles the BucketInfo
// secret. Duplicated here so the driver does not pull in client-go.
const (
	protocolS3          = "s3"
	credAccessKeyID     = "accessKeyID"
	credAccessSecretKey = "accessSecretKey"
	credEndpoint        = "endpoint"
	credRegion          = "region"
)

// Tag keys set on every objects user aludel-cloudscale creates. They are what
// makes DriverCreateBucket idempotent: on retry the driver finds the user it
// created on the previous attempt instead of creating a second one.
const (
	tagManagedBy = "aludel_managed_by"
	tagBucket    = "aludel_bucket"
)

// Provisioner implements the COSI Provisioner and Identity services against
// cloudscale.ch object storage.
//
// cloudscale.ch has no IAM, no roles and no bucket policies: the only access
// control primitive is the objects user, and a bucket is reachable exactly by
// the user that created it. aludel-cloudscale therefore runs a strict one-objects-user-
// per-bucket model. The user is created in DriverCreateBucket (it has to exist
// before the bucket, to own it) and destroyed in DriverDeleteBucket;
// DriverGrantBucketAccess only reads that user's existing keys back out.
type Provisioner struct {
	cosi.UnimplementedProvisionerServer
	cosi.UnimplementedIdentityServer

	// Name is the driver name reported by DriverGetInfo and matched against a
	// BucketClass's driverName.
	Name string

	Users   ObjectsUsers
	Buckets BucketsFactory
	Log     *slog.Logger

	// Clock and Sleep are injection points for the propagation retry, so tests
	// do not have to spend real seconds. Both default to the wall clock.
	Clock func() time.Time
	Sleep func(context.Context, time.Duration) error
}

func (p *Provisioner) now() time.Time {
	if p.Clock != nil {
		return p.Clock()
	}
	return time.Now()
}

func (p *Provisioner) sleep(ctx context.Context, d time.Duration) error {
	if p.Sleep != nil {
		return p.Sleep(ctx, d)
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var (
	_ cosi.ProvisionerServer = (*Provisioner)(nil)
	_ cosi.IdentityServer    = (*Provisioner)(nil)
)

// DriverGetInfo implements the COSI Identity service.
func (p *Provisioner) DriverGetInfo(_ context.Context, _ *cosi.DriverGetInfoRequest) (*cosi.DriverGetInfoResponse, error) {
	if p.Name == "" {
		return nil, status.Error(codes.Internal, "driver name is not configured")
	}
	return &cosi.DriverGetInfoResponse{Name: p.Name}, nil
}

// DriverCreateBucket creates the objects user that will own the bucket, then
// creates the bucket over S3 using that user's own credentials.
//
// The two steps are not atomic. If the S3 call fails the objects user is left
// behind, tagged with the bucket name; the next retry adopts it rather than
// creating a duplicate. A user is only truly orphaned if the BucketClaim is
// abandoned mid-flight, which `aludel-cloudscale gc` (not yet implemented) is meant to
// clean up.
func (p *Provisioner) DriverCreateBucket(ctx context.Context, req *cosi.DriverCreateBucketRequest) (*cosi.DriverCreateBucketResponse, error) {
	// The name is used verbatim: the bucket on cloudscale.ch is called exactly
	// what the Bucket resource is called, so one name identifies it in kubectl,
	// in the logs and in the cloudscale.ch control panel.
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "bucket name is required")
	}

	params, err := ParseBucketClassParameters(req.GetParameters())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid BucketClass parameters: %v", err)
	}

	log := p.log().With("bucket", name, "region", params.Region)

	user, err := p.ensureUser(ctx, name, log)
	if err != nil {
		return nil, err
	}

	s3, err := p.Buckets.New(params.EndpointURL, params.Region, user.AccessKey, user.SecretKey, params.PathStyle)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "building S3 client: %v", err)
	}

	if err := p.createBucketWithRetry(ctx, s3, name, log); err != nil {
		switch {
		case errors.Is(err, ErrBucketNameExists):
			return nil, status.Errorf(codes.AlreadyExists,
				"the name %q is already taken; bucket names are unique across all of cloudscale.ch, not just this project", name)
		case errors.Is(err, ErrCredentialsNotReady):
			// Surface as Unavailable so the sidecar retries the whole RPC
			// rather than treating it as a permanent failure.
			return nil, status.Errorf(codes.Unavailable,
				"S3 endpoint still does not recognise the new objects user's key after %s: %v",
				credentialPropagationBudget, err)
		default:
			return nil, status.Errorf(codes.Internal, "creating bucket %q: %v", name, err)
		}
	}

	id := BucketID{Region: params.Region, Bucket: name, UserID: user.ID}
	log.Info("bucket provisioned", "bucketID", id.String(), "objectsUser", user.ID)

	return &cosi.DriverCreateBucketResponse{
		BucketId: id.String(),
		BucketInfo: &cosi.Protocol{
			Type: &cosi.Protocol_S3{
				S3: &cosi.S3{
					Region:           params.Region,
					SignatureVersion: cosi.S3SignatureVersion_S3V4,
				},
			},
		},
	}, nil
}

// Bounds for tolerating key propagation from the cloudscale.ch API to the
// region's RGW cluster. Kept well under the sidecar's own RPC timeout so a
// genuinely bad credential still surfaces promptly.
const (
	credentialPropagationBudget = 30 * time.Second
	credentialRetryInitialDelay = 500 * time.Millisecond
	credentialRetryMaxDelay     = 4 * time.Second
)

// createBucketWithRetry calls Create, retrying only while the S3 endpoint does
// not yet recognise the newly created key pair. Every other error, including
// success, returns immediately.
func (p *Provisioner) createBucketWithRetry(ctx context.Context, buckets Buckets, name string, log *slog.Logger) error {
	deadline := p.now().Add(credentialPropagationBudget)
	delay := credentialRetryInitialDelay

	for attempt := 1; ; attempt++ {
		err := buckets.Create(ctx, name)
		if !errors.Is(err, ErrCredentialsNotReady) {
			return err
		}

		if !p.now().Add(delay).Before(deadline) {
			return err
		}
		log.Info("waiting for the objects user's key to propagate to the S3 endpoint",
			"attempt", attempt, "retryIn", delay)

		if err := p.sleep(ctx, delay); err != nil {
			return err
		}
		delay = min(delay*2, credentialRetryMaxDelay)
	}
}

// ensureUser returns the objects user for a bucket, creating it if this is the
// first attempt.
func (p *Provisioner) ensureUser(ctx context.Context, bucket string, log *slog.Logger) (*ObjectsUser, error) {
	tags := p.bucketTags(bucket)

	existing, err := p.Users.FindByTags(ctx, tags)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing objects users for bucket %q: %v", bucket, err)
	}

	switch len(existing) {
	case 0:
		user, err := p.Users.Create(ctx, bucket, tags)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "creating objects user for bucket %q: %v", bucket, err)
		}
		log.Info("created objects user", "objectsUser", user.ID)
		if err := validateKeys(user); err != nil {
			return nil, err
		}
		return user, nil
	case 1:
		user := &existing[0]
		log.Info("adopting existing objects user", "objectsUser", user.ID)
		if err := validateKeys(user); err != nil {
			return nil, err
		}
		return user, nil
	default:
		// Two users claim the same bucket. Picking one at random would hand out
		// credentials that cannot read the bucket, so refuse and let a human look.
		return nil, status.Errorf(codes.Internal,
			"found %d objects users tagged for bucket %q, expected at most 1; manual cleanup required",
			len(existing), bucket)
	}
}

// DriverDeleteBucket removes the bucket and then the objects user that owns it.
//
// Order matters: deleting the user first would strand the bucket with no
// credentials able to reach it.
func (p *Provisioner) DriverDeleteBucket(ctx context.Context, req *cosi.DriverDeleteBucketRequest) (*cosi.DriverDeleteBucketResponse, error) {
	id, err := ParseBucketID(req.GetBucketId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// delete_context carries the BucketClass parameters the bucket was created
	// with. Fall back to defaults derived from the bucket ID if it is empty.
	params, err := ParseBucketClassParameters(withRegion(req.GetDeleteContext(), id.Region))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid delete context: %v", err)
	}

	log := p.log().With("bucket", id.Bucket, "objectsUser", id.UserID)

	user, err := p.Users.Get(ctx, id.UserID)
	if errors.Is(err, ErrNotFound) {
		// No user means no credentials, so the bucket is unreachable and, per
		// cloudscale.ch's model, already gone with its owner. Nothing to do.
		log.Info("objects user already deleted, nothing to do")
		return &cosi.DriverDeleteBucketResponse{}, nil
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "fetching objects user %q: %v", id.UserID, err)
	}
	if err := validateKeys(user); err != nil {
		return nil, err
	}

	s3, err := p.Buckets.New(params.EndpointURL, params.Region, user.AccessKey, user.SecretKey, params.PathStyle)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "building S3 client: %v", err)
	}

	if params.DeletionPolicy == DeleteAll {
		if err := s3.Empty(ctx, id.Bucket); err != nil && !errors.Is(err, ErrNotFound) {
			return nil, status.Errorf(codes.Internal, "emptying bucket %q: %v", id.Bucket, err)
		}
	}

	if err := s3.Delete(ctx, id.Bucket); err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			log.Info("bucket already deleted")
		case errors.Is(err, ErrBucketNotEmpty):
			return nil, status.Errorf(codes.FailedPrecondition,
				"bucket %q is not empty; set %s=%s on the BucketClass to delete its contents",
				id.Bucket, ParamDeletionPolicy, DeleteAll)
		default:
			return nil, status.Errorf(codes.Internal, "deleting bucket %q: %v", id.Bucket, err)
		}
	}

	if err := p.Users.Delete(ctx, id.UserID); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, status.Errorf(codes.Internal, "deleting objects user %q: %v", id.UserID, err)
	}

	log.Info("bucket and objects user deleted")
	return &cosi.DriverDeleteBucketResponse{}, nil
}

// DriverGrantBucketAccess hands back the credentials of the bucket's owning
// objects user.
//
// It deliberately creates nothing. Without bucket policies there is no way to
// let a second user into an existing bucket, so every BucketAccess on a given
// Bucket resolves to the same key pair.
func (p *Provisioner) DriverGrantBucketAccess(ctx context.Context, req *cosi.DriverGrantBucketAccessRequest) (*cosi.DriverGrantBucketAccessResponse, error) {
	id, err := ParseBucketID(req.GetBucketId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	switch req.GetAuthenticationType() {
	case cosi.AuthenticationType_Key:
		// the only thing cloudscale.ch can do
	case cosi.AuthenticationType_IAM:
		return nil, status.Error(codes.Unimplemented,
			"cloudscale.ch has no IAM; use authenticationType: KEY on the BucketAccessClass")
	default:
		return nil, status.Errorf(codes.InvalidArgument,
			"unsupported authentication type %q", req.GetAuthenticationType())
	}

	params, err := ParseBucketClassParameters(withRegion(req.GetParameters(), id.Region))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid BucketAccessClass parameters: %v", err)
	}

	user, err := p.Users.Get(ctx, id.UserID)
	if errors.Is(err, ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "objects user %q for bucket %q no longer exists", id.UserID, id.Bucket)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "fetching objects user %q: %v", id.UserID, err)
	}
	if err := validateKeys(user); err != nil {
		return nil, err
	}

	p.log().Info("granted bucket access",
		"bucket", id.Bucket, "objectsUser", user.ID, "bucketAccess", req.GetName())

	return &cosi.DriverGrantBucketAccessResponse{
		AccountId: user.ID,
		Credentials: map[string]*cosi.CredentialDetails{
			protocolS3: {
				Secrets: map[string]string{
					credAccessKeyID:     user.AccessKey,
					credAccessSecretKey: user.SecretKey,
					credEndpoint:        params.EndpointURL,
					credRegion:          params.Region,
				},
			},
		},
	}, nil
}

// DriverRevokeBucketAccess is a no-op.
//
// The only credentials that exist are the bucket owner's. Deleting or rotating
// them would not revoke one grant, it would sever every grant and lock the
// driver out of its own bucket. Access ends when the Bucket is deleted.
func (p *Provisioner) DriverRevokeBucketAccess(_ context.Context, req *cosi.DriverRevokeBucketAccessRequest) (*cosi.DriverRevokeBucketAccessResponse, error) {
	p.log().Info("revoke is a no-op on cloudscale.ch; credentials live and die with the bucket",
		"bucketID", req.GetBucketId(), "accountID", req.GetAccountId())
	return &cosi.DriverRevokeBucketAccessResponse{}, nil
}

func (p *Provisioner) bucketTags(bucket string) map[string]string {
	return map[string]string{
		tagManagedBy: p.Name,
		tagBucket:    bucket,
	}
}

func (p *Provisioner) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// validateKeys guards against a user that exists but carries no usable key
// pair, which happens if the API token is read-only (cloudscale.ch omits
// secret_key in that case).
func validateKeys(u *ObjectsUser) error {
	if u.AccessKey == "" || u.SecretKey == "" {
		return status.Errorf(codes.Internal,
			"objects user %q has no usable key pair; is the cloudscale.ch API token read-only?", u.ID)
	}
	return nil
}

// withRegion backfills the region parameter from the bucket ID so that delete
// and grant work even when the caller passes no parameters at all.
func withRegion(params map[string]string, region string) map[string]string {
	out := make(map[string]string, len(params)+1)
	for k, v := range params {
		out[k] = v
	}
	if out[ParamRegion] == "" {
		out[ParamRegion] = region
	}
	return out
}
