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
	// The sidecar checks a BucketClaim's requested protocols against this list before it
	// ever calls the driver, so an Azure claim is refused rather than reaching us.
	return &cosi.DriverGetInfoResponse{
		Name:               p.Name,
		SupportedProtocols: []*cosi.ObjectProtocol{s3Protocol()},
	}, nil
}

// s3Protocol is the only protocol cloudscale.ch object storage speaks.
func s3Protocol() *cosi.ObjectProtocol {
	return &cosi.ObjectProtocol{Type: cosi.ObjectProtocol_S3}
}

// s3BucketInfo is what a client needs to reach the bucket: the sidecar writes each field
// into its own key of the BucketAccess secret.
func s3BucketInfo(id BucketID, params *BucketClassParameters) *cosi.ObjectProtocolAndBucketInfo {
	style := cosi.S3AddressingStyle_VIRTUAL
	if params.PathStyle {
		style = cosi.S3AddressingStyle_PATH
	}
	return &cosi.ObjectProtocolAndBucketInfo{
		S3: &cosi.S3BucketInfo{
			BucketId:        id.Bucket,
			Endpoint:        params.EndpointURL,
			Region:          params.Region,
			AddressingStyle: &cosi.S3AddressingStyle{Style: style},
		},
	}
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
		BucketId:  id.String(),
		Protocols: s3BucketInfo(id, params),
	}, nil
}

// DriverGetBucket reports what a bucket the driver already provisioned looks like.
//
// COSI calls it to adopt a Bucket whose BucketClaim it has lost track of, so it must not
// create anything: an id whose objects user is gone is reported as NotFound rather than
// re-provisioned.
func (p *Provisioner) DriverGetBucket(ctx context.Context, req *cosi.DriverGetBucketRequest) (*cosi.DriverGetBucketResponse, error) {
	id, err := ParseBucketID(req.GetBucketId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	params, err := ParseBucketClassParameters(withRegion(req.GetParameters(), id.Region))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid BucketClass parameters: %v", err)
	}

	// The objects user owns the bucket, so its absence is the bucket's absence as far as
	// this driver is concerned: nothing could reach the bucket without it.
	if _, err := p.Users.Get(ctx, id.UserID); errors.Is(err, ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "objects user %q for bucket %q no longer exists", id.UserID, id.Bucket)
	} else if err != nil {
		return nil, status.Errorf(codes.Internal, "fetching objects user %q: %v", id.UserID, err)
	}

	return &cosi.DriverGetBucketResponse{
		BucketId:  req.GetBucketId(),
		Protocols: s3BucketInfo(id, params),
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

	// parameters carries the BucketClass the bucket was created with. Fall back to
	// defaults derived from the bucket ID if it is empty.
	params, err := ParseBucketClassParameters(withRegion(req.GetParameters(), id.Region))
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid BucketClass parameters: %v", err)
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
	// One account, one key pair, one bucket. The key pair handed back is the bucket
	// owner's, and every bucket has an owner of its own, so a grant spanning several
	// buckets has no single credential that could satisfy it.
	buckets := req.GetBuckets()
	if len(buckets) != 1 {
		return nil, status.Errorf(codes.InvalidArgument,
			"this driver grants access to exactly one bucket per BucketAccess, got %d; set multiBucketAccess: SingleBucket on BucketAccessClass %q",
			len(buckets), req.GetAccountName())
	}
	accessed := buckets[0]

	id, err := ParseBucketID(accessed.GetBucketId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	switch req.GetAuthenticationType().GetType() {
	case cosi.AuthenticationType_KEY:
		// the only thing cloudscale.ch can do
	case cosi.AuthenticationType_SERVICE_ACCOUNT:
		return nil, status.Error(codes.Unimplemented,
			"cloudscale.ch has no IAM; use authenticationType: Key on the BucketAccessClass")
	default:
		return nil, status.Errorf(codes.InvalidArgument,
			"unsupported authentication type %q; set authenticationType to exactly %q on the BucketAccessClass",
			req.GetAuthenticationType().GetType(), "Key")
	}

	if protocol := req.GetProtocol().GetType(); protocol != cosi.ObjectProtocol_S3 {
		return nil, status.Errorf(codes.InvalidArgument,
			"cloudscale.ch object storage speaks S3, not %q", protocol)
	}

	// The credentials are the bucket owner's, which is full access. Handing them out for a
	// narrower request would grant more than was asked for, so say so instead.
	if mode := accessed.GetAccessMode().GetMode(); mode != cosi.AccessMode_READ_WRITE {
		return nil, status.Errorf(codes.InvalidArgument,
			"cloudscale.ch has no bucket policies, so access is the bucket owner's key pair and always read-write; accessMode %q cannot be honoured", mode)
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
		"bucket", id.Bucket, "objectsUser", user.ID, "account", req.GetAccountName())

	return &cosi.DriverGrantBucketAccessResponse{
		AccountId: user.ID,
		Buckets: []*cosi.DriverGrantBucketAccessResponse_BucketInfo{{
			BucketId:   accessed.GetBucketId(),
			BucketInfo: s3BucketInfo(id, params),
		}},
		Credentials: &cosi.CredentialInfo{
			S3: &cosi.S3CredentialInfo{
				AccessKeyId:     user.AccessKey,
				AccessSecretKey: user.SecretKey,
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
	ids := make([]string, 0, len(req.GetBuckets()))
	for _, bucket := range req.GetBuckets() {
		ids = append(ids, bucket.GetBucketId())
	}
	p.log().Info("revoke is a no-op on cloudscale.ch; credentials live and die with the bucket",
		"bucketIDs", ids, "accountID", req.GetAccountId())
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
