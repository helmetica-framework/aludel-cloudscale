package driver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	cosi "sigs.k8s.io/container-object-storage-interface/proto"
)

// Tag keys set on every objects user aludel-cloudscale creates. They are what
// makes DriverCreateBucket idempotent: on retry the driver finds the user it
// created on the previous attempt instead of creating a second one. They are
// also how every later call finds a bucket's owner, since the bucket ID does
// not carry it.
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
// DriverGenerateBucketId creates nothing at all.
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
	// The sidecar checks a BucketClaim's requested protocols against this list
	// before it ever calls the driver, so an Azure claim is refused up front.
	return &cosi.DriverGetInfoResponse{
		Name:               p.Name,
		SupportedProtocols: []*cosi.ObjectProtocol{{Type: cosi.ObjectProtocol_S3}},
	}, nil
}

// DriverGenerateBucketId is phase one of provisioning: it derives the bucket ID
// from the suggested name and the BucketClass region, and touches nothing.
//
// COSI persists the returned ID before calling DriverCreateBucket, and may
// delete the Bucket without ever calling DriverDeleteBucket if this phase
// fails, so creating anything here could leak it.
func (p *Provisioner) DriverGenerateBucketId(_ context.Context, req *cosi.DriverGenerateBucketIdRequest) (*cosi.DriverGenerateBucketIdResponse, error) {
	// The name is used verbatim: the bucket on cloudscale.ch is called exactly
	// what the Bucket resource is called, so one name identifies it in kubectl,
	// in the logs and in the cloudscale.ch control panel.
	name := req.GetName()
	if name == "" {
		return nil, status.Error(codes.InvalidArgument, "bucket name is required")
	}
	if len(name) > maxBucketNameLength {
		return nil, status.Errorf(codes.InvalidArgument,
			"bucket name %q is %d characters long; S3 allows at most %d", name, len(name), maxBucketNameLength)
	}

	if err := requireS3(req.GetProtocols()); err != nil {
		return nil, err
	}

	params, err := ParseBucketClassParameters(req.GetParameters())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid BucketClass parameters: %v", err)
	}

	return &cosi.DriverGenerateBucketIdResponse{
		BucketId: BucketID{Region: params.Region, Bucket: name}.String(),
	}, nil
}

// maxBucketNameLength is the S3 limit. COSI suggests names of up to 253
// characters, but its own dynamically provisioned ones ("bc-<UID>") are 39.
const maxBucketNameLength = 63

// DriverCreateBucket is phase two of provisioning: it creates the objects user
// that will own the bucket, then creates the bucket over S3 using that user's
// own credentials.
//
// The two steps are not atomic. If the S3 call fails the objects user is left
// behind, tagged with the bucket name; the next retry adopts it rather than
// creating a duplicate. A user is only truly orphaned if the BucketClaim is
// abandoned mid-flight, which `aludel-cloudscale gc` (not yet implemented) is meant to
// clean up.
func (p *Provisioner) DriverCreateBucket(ctx context.Context, req *cosi.DriverCreateBucketRequest) (*cosi.DriverCreateBucketResponse, error) {
	id, params, err := parseBucketRequest(req.GetBucketId(), req.GetParameters())
	if err != nil {
		return nil, err
	}
	if err := requireS3(req.GetProtocols()); err != nil {
		return nil, err
	}

	log := p.log().With("bucket", id.Bucket, "region", id.Region)

	user, err := p.ensureUser(ctx, id.Bucket, log)
	if err != nil {
		return nil, err
	}

	s3, err := p.Buckets.New(params.EndpointURL, params.Region, user.AccessKey, user.SecretKey, params.PathStyle)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "building S3 client: %v", err)
	}

	if err := p.createBucketWithRetry(ctx, s3, id.Bucket, log); err != nil {
		switch {
		case errors.Is(err, ErrBucketNameExists):
			return nil, status.Errorf(codes.AlreadyExists,
				"the name %q is already taken; bucket names are unique across all of cloudscale.ch, not just this project", id.Bucket)
		case errors.Is(err, ErrCredentialsNotReady):
			// Surface as Unavailable so the sidecar retries the whole RPC
			// rather than treating it as a permanent failure.
			return nil, status.Errorf(codes.Unavailable,
				"S3 endpoint still does not recognise the new objects user's key after %s: %v",
				credentialPropagationBudget, err)
		default:
			return nil, status.Errorf(codes.Internal, "creating bucket %q: %v", id.Bucket, err)
		}
	}

	log.Info("bucket provisioned", "bucketID", id.String(), "objectsUser", user.ID)

	return &cosi.DriverCreateBucketResponse{
		Protocols: s3BucketInfo(id, params),
	}, nil
}

// DriverGetBucket reports on a bucket that should already exist, for a
// statically provisioned Bucket whose spec.existingBucketID an administrator
// set to <region>/<bucket>.
//
// It creates nothing. The bucket only counts as existing if the objects user
// aludel-cloudscale tagged as its owner is still there and can see it, since
// without that user the driver could neither grant access nor delete it.
func (p *Provisioner) DriverGetBucket(ctx context.Context, req *cosi.DriverGetBucketRequest) (*cosi.DriverGetBucketResponse, error) {
	id, params, err := parseBucketRequest(req.GetBucketId(), req.GetParameters())
	if err != nil {
		return nil, err
	}
	if err := requireS3(req.GetProtocols()); err != nil {
		return nil, err
	}

	user, err := p.findOwner(ctx, id.Bucket)
	if errors.Is(err, ErrNotFound) {
		return nil, status.Errorf(codes.NotFound,
			"no objects user tagged %s=%s owns bucket %q", tagBucket, id.Bucket, id.Bucket)
	}
	if err != nil {
		return nil, err
	}

	s3, err := p.Buckets.New(params.EndpointURL, params.Region, user.AccessKey, user.SecretKey, params.PathStyle)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "building S3 client: %v", err)
	}
	exists, err := s3.Exists(ctx, id.Bucket)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "checking bucket %q: %v", id.Bucket, err)
	}
	if !exists {
		return nil, status.Errorf(codes.NotFound,
			"bucket %q does not exist in region %q or is not owned by objects user %q", id.Bucket, id.Region, user.ID)
	}

	return &cosi.DriverGetBucketResponse{
		Protocols: s3BucketInfo(id, params),
	}, nil
}

// s3BucketInfo is what a client needs to reach the bucket. The sidecar writes
// each field into its own COSI_S3_* key of the BucketAccess secret.
func s3BucketInfo(id BucketID, params *BucketClassParameters) *cosi.ObjectProtocolAndBucketInfo {
	style := cosi.S3AddressingStyle_VIRTUAL
	if params.PathStyle {
		style = cosi.S3AddressingStyle_PATH
	}
	return &cosi.ObjectProtocolAndBucketInfo{
		S3: &cosi.S3BucketInfo{
			BucketId:        id.Bucket,
			Endpoint:        params.EndpointURL,
			Region:          id.Region,
			AddressingStyle: &cosi.S3AddressingStyle{Style: style},
		},
	}
}

// requireS3 rejects a request for any protocol other than S3. An empty list
// means no preference, and S3 is what the bucket gets.
func requireS3(protocols []*cosi.ObjectProtocol) error {
	for _, proto := range protocols {
		if proto.GetType() != cosi.ObjectProtocol_S3 {
			return status.Errorf(codes.InvalidArgument,
				"cloudscale.ch object storage speaks S3 only, not %s", proto.GetType())
		}
	}
	return nil
}

// parseBucketRequest parses the bucket ID and the BucketClass parameters
// passed alongside it.
//
// The region is part of the ID, so it is backfilled into the parameters when
// they lack it. If they carry a different one, the BucketClass no longer
// describes the bucket and acting on it would address the wrong endpoint.
func parseBucketRequest(bucketID string, raw map[string]string) (BucketID, *BucketClassParameters, error) {
	id, err := ParseBucketID(bucketID)
	if err != nil {
		return BucketID{}, nil, status.Error(codes.InvalidArgument, err.Error())
	}

	params, err := ParseBucketClassParameters(withRegion(raw, id.Region))
	if err != nil {
		return BucketID{}, nil, status.Errorf(codes.InvalidArgument, "invalid BucketClass parameters: %v", err)
	}
	if params.Region != id.Region {
		return BucketID{}, nil, status.Errorf(codes.InvalidArgument,
			"bucket %q lives in region %q, but the BucketClass parameters say %s=%q",
			id.Bucket, id.Region, ParamRegion, params.Region)
	}

	return id, params, nil
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
	user, err := p.findOwner(ctx, bucket)
	if err == nil {
		log.Info("adopting existing objects user", "objectsUser", user.ID)
		return user, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}

	user, err = p.Users.Create(ctx, bucket, p.bucketTags(bucket))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "creating objects user for bucket %q: %v", bucket, err)
	}
	log.Info("created objects user", "objectsUser", user.ID)
	if err := validateKeys(user); err != nil {
		return nil, err
	}
	return user, nil
}

// findOwner returns the objects user tagged as the owner of a bucket, or an
// error wrapping ErrNotFound if there is none.
func (p *Provisioner) findOwner(ctx context.Context, bucket string) (*ObjectsUser, error) {
	existing, err := p.Users.FindByTags(ctx, p.bucketTags(bucket))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "listing objects users for bucket %q: %v", bucket, err)
	}

	switch len(existing) {
	case 0:
		return nil, fmt.Errorf("owner of bucket %q: %w", bucket, ErrNotFound)
	case 1:
		user := &existing[0]
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
	// parameters carries the BucketClass the bucket was created with. Fall back
	// to defaults derived from the bucket ID if it is empty.
	id, params, err := parseBucketRequest(req.GetBucketId(), req.GetParameters())
	if err != nil {
		return nil, err
	}

	log := p.log().With("bucket", id.Bucket)

	user, err := p.findOwner(ctx, id.Bucket)
	if errors.Is(err, ErrNotFound) {
		// No user means no credentials, so the bucket is unreachable and, per
		// cloudscale.ch's model, already gone with its owner. This is also the
		// case when COSI deletes a Bucket whose DriverCreateBucket never ran.
		log.Info("objects user already deleted, nothing to do")
		return &cosi.DriverDeleteBucketResponse{}, nil
	}
	if err != nil {
		return nil, err
	}
	log = log.With("objectsUser", user.ID)

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
			// FailedPrecondition is retryable to the sidecar, so the deletion
			// goes through by itself once somebody empties the bucket. The
			// Bucket's parameters are immutable, so that is the only way out.
			return nil, status.Errorf(codes.FailedPrecondition,
				"bucket %q is not empty and %s is %s; empty it and the deletion will be retried",
				id.Bucket, ParamDeletionPolicy, DeleteIfEmpty)
		default:
			return nil, status.Errorf(codes.Internal, "deleting bucket %q: %v", id.Bucket, err)
		}
	}

	if err := p.Users.Delete(ctx, user.ID); err != nil && !errors.Is(err, ErrNotFound) {
		return nil, status.Errorf(codes.Internal, "deleting objects user %q: %v", user.ID, err)
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
	// One account, one key pair, one bucket. Every bucket has an owner of its
	// own, so a grant spanning several has no single credential that could
	// satisfy it. OutOfRange is what tells the sidecar exactly that.
	buckets := req.GetBuckets()
	switch {
	case len(buckets) == 0:
		return nil, status.Error(codes.InvalidArgument, "no bucket to grant access to")
	case len(buckets) > 1:
		return nil, status.Errorf(codes.OutOfRange,
			"cloudscale.ch grants access to exactly one bucket per BucketAccess, got %d; set multiBucketAccess: SingleBucket on the BucketAccessClass",
			len(buckets))
	}
	accessed := buckets[0]

	switch req.GetAuthenticationType().GetType() {
	case cosi.AuthenticationType_KEY:
		// the only thing cloudscale.ch can do
	case cosi.AuthenticationType_SERVICE_ACCOUNT:
		return nil, status.Error(codes.Unimplemented,
			"cloudscale.ch has no IAM; use authenticationType: Key on the BucketAccessClass")
	default:
		return nil, status.Errorf(codes.InvalidArgument,
			"unsupported authentication type %s; set authenticationType: Key on the BucketAccessClass",
			req.GetAuthenticationType().GetType())
	}

	if proto := req.GetProtocol().GetType(); proto != cosi.ObjectProtocol_S3 {
		return nil, status.Errorf(codes.InvalidArgument,
			"cloudscale.ch object storage speaks S3 only, not %s", proto)
	}

	if err := requireReadWrite(accessed); err != nil {
		return nil, err
	}

	// BucketAccessClass parameters, not BucketClass ones, so endpointURL and
	// s3PathStyle default here unless set on the BucketAccessClass too.
	id, params, err := parseBucketRequest(accessed.GetBucketId(), req.GetParameters())
	if err != nil {
		return nil, err
	}

	user, err := p.findOwner(ctx, id.Bucket)
	if errors.Is(err, ErrNotFound) {
		return nil, status.Errorf(codes.NotFound, "the objects user owning bucket %q no longer exists", id.Bucket)
	}
	if err != nil {
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

// requireReadWrite refuses any access mode narrower than read-write.
//
// The credentials handed out are the bucket owner's, which can do everything
// to the bucket and its objects. Honouring ReadOnly or WriteOnly would need a
// bucket policy, which cloudscale.ch does not have, so handing the owner's key
// pair out anyway would grant more than was asked for. ANY means the
// BucketAccess left the category unset and accepts whatever the driver gives.
func requireReadWrite(bucket *cosi.DriverGrantBucketAccessRequest_AccessedBucket) error {
	for _, m := range []struct {
		field string
		mode  cosi.AccessMode_Mode
	}{
		{"objectData", bucket.GetObjectDataAccessMode().GetMode()},
		{"objectMetadata", bucket.GetObjectMetadataAccessMode().GetMode()},
		{"bucketMetadata", bucket.GetBucketMetadataAccessMode().GetMode()},
	} {
		switch m.mode {
		case cosi.AccessMode_READ_WRITE, cosi.AccessMode_ANY:
		default:
			return status.Errorf(codes.InvalidArgument,
				"cloudscale.ch has no bucket policies, so access is always the bucket owner's read-write key pair; accessModes.%s %s cannot be honoured, use ReadWrite or leave it unset",
				m.field, m.mode)
		}
	}
	return nil
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

// withRegion backfills the region parameter from the bucket ID so that every
// call after DriverGenerateBucketId works even with no parameters at all.
func withRegion(params map[string]string, region string) map[string]string {
	out := make(map[string]string, len(params)+1)
	maps.Copy(out, params)
	if out[ParamRegion] == "" {
		out[ParamRegion] = region
	}
	return out
}
