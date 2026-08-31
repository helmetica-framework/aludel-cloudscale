package driver

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	cosi "sigs.k8s.io/container-object-storage-interface/proto"
)

const testDriverName = "cloudscale-objectstorage.helmetica.io"

func newTestProvisioner() (*Provisioner, *fakeUsers, *fakeBucketStore) {
	users := newFakeUsers()
	store := newFakeBucketStore()
	return &Provisioner{Name: testDriverName, Users: users, Buckets: store}, users, store
}

func testParams() map[string]string {
	return map[string]string{ParamRegion: "rma"}
}

func createBucket(t *testing.T, p *Provisioner, name string, params map[string]string) *cosi.DriverCreateBucketResponse {
	t.Helper()
	resp, err := p.DriverCreateBucket(context.Background(), &cosi.DriverCreateBucketRequest{
		Name:       name,
		Parameters: params,
	})
	if err != nil {
		t.Fatalf("DriverCreateBucket(%q): %v", name, err)
	}
	return resp
}

func requireCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with code %v, got nil", want)
	}
	if got := status.Code(err); got != want {
		t.Fatalf("expected code %v, got %v (%v)", want, got, err)
	}
}

func TestCreateBucketProvisionsUserAndBucket(t *testing.T) {
	p, users, store := newTestProvisioner()

	resp := createBucket(t, p, "my-bucket", testParams())

	id, err := ParseBucketID(resp.GetBucketId())
	if err != nil {
		t.Fatalf("returned an unparsable bucket id: %v", err)
	}
	if id.Region != "rma" || id.Bucket != "my-bucket" {
		t.Errorf("unexpected bucket id %+v", id)
	}
	if _, ok := users.users[id.UserID]; !ok {
		t.Errorf("objects user %q was not created", id.UserID)
	}
	if _, ok := store.buckets["my-bucket"]; !ok {
		t.Error("bucket was not created")
	}
	if got := resp.GetProtocols().GetS3().GetRegion(); got != "rma" {
		t.Errorf("bucket info region = %q, want %q", got, "rma")
	}
}

// A retried DriverCreateBucket must adopt the objects user from the previous
// attempt instead of leaking a second one.
func TestCreateBucketIsIdempotent(t *testing.T) {
	p, users, _ := newTestProvisioner()

	first := createBucket(t, p, "my-bucket", testParams())
	second := createBucket(t, p, "my-bucket", testParams())

	if first.GetBucketId() != second.GetBucketId() {
		t.Errorf("bucket id changed across retries: %q then %q", first.GetBucketId(), second.GetBucketId())
	}
	if len(users.users) != 1 {
		t.Errorf("expected 1 objects user after retry, got %d", len(users.users))
	}
}

// If the S3 call fails, the tagged user survives so the next retry adopts it.
func TestCreateBucketRetriesAdoptUserAfterS3Failure(t *testing.T) {
	p, users, store := newTestProvisioner()
	store.createErr = errors.New("boom")

	_, err := p.DriverCreateBucket(context.Background(), &cosi.DriverCreateBucketRequest{
		Name:       "my-bucket",
		Parameters: testParams(),
	})
	requireCode(t, err, codes.Internal)
	if len(users.users) != 1 {
		t.Fatalf("expected the objects user to be kept for the retry, got %d users", len(users.users))
	}

	store.createErr = nil
	createBucket(t, p, "my-bucket", testParams())

	if len(users.users) != 1 {
		t.Errorf("retry created a duplicate objects user: %d total", len(users.users))
	}
}

func TestCreateBucketRejectsBucketOwnedByAnother(t *testing.T) {
	p, _, store := newTestProvisioner()
	store.put("taken", "someone-elses-key", 0)

	_, err := p.DriverCreateBucket(context.Background(), &cosi.DriverCreateBucketRequest{
		Name:       "taken",
		Parameters: testParams(),
	})
	requireCode(t, err, codes.AlreadyExists)
}

func TestCreateBucketRequiresRegion(t *testing.T) {
	p, _, _ := newTestProvisioner()

	_, err := p.DriverCreateBucket(context.Background(), &cosi.DriverCreateBucketRequest{
		Name:       "my-bucket",
		Parameters: map[string]string{},
	})
	requireCode(t, err, codes.InvalidArgument)
}

// grantRequest is a well-formed v1alpha2 grant for one bucket, which tests then bend.
func grantRequest(bucketID, account string) *cosi.DriverGrantBucketAccessRequest {
	return &cosi.DriverGrantBucketAccessRequest{
		AccountName:        account,
		Protocol:           &cosi.ObjectProtocol{Type: cosi.ObjectProtocol_S3},
		AuthenticationType: &cosi.AuthenticationType{Type: cosi.AuthenticationType_KEY},
		Parameters:         testParams(),
		Buckets: []*cosi.DriverGrantBucketAccessRequest_AccessedBucket{{
			BucketId:   bucketID,
			AccessMode: &cosi.AccessMode{Mode: cosi.AccessMode_READ_WRITE},
		}},
	}
}

func TestGrantBucketAccessReturnsOwnerCredentials(t *testing.T) {
	p, users, _ := newTestProvisioner()
	created := createBucket(t, p, "my-bucket", testParams())
	id, _ := ParseBucketID(created.GetBucketId())

	resp, err := p.DriverGrantBucketAccess(context.Background(), grantRequest(created.GetBucketId(), "my-access"))
	if err != nil {
		t.Fatalf("DriverGrantBucketAccess: %v", err)
	}

	if resp.GetAccountId() != id.UserID {
		t.Errorf("account id = %q, want the owning user %q", resp.GetAccountId(), id.UserID)
	}

	creds := resp.GetCredentials().GetS3()
	want := users.users[id.UserID]
	if creds.GetAccessKeyId() != want.AccessKey {
		t.Errorf("access key = %q, want %q", creds.GetAccessKeyId(), want.AccessKey)
	}
	if creds.GetAccessSecretKey() != want.SecretKey {
		t.Errorf("secret key = %q, want %q", creds.GetAccessSecretKey(), want.SecretKey)
	}

	// The bucket info travels beside the credentials now, one field per value, rather
	// than as extra keys in a credentials map.
	if len(resp.GetBuckets()) != 1 {
		t.Fatalf("got %d buckets in the response, want 1", len(resp.GetBuckets()))
	}
	info := resp.GetBuckets()[0].GetBucketInfo().GetS3()
	if got, want := info.GetEndpoint(), DefaultEndpointURL("rma"); got != want {
		t.Errorf("endpoint = %q, want %q", got, want)
	}
	if info.GetRegion() != "rma" {
		t.Errorf("region = %q, want %q", info.GetRegion(), "rma")
	}
	if info.GetBucketId() != id.Bucket {
		t.Errorf("s3 bucket = %q, want %q", info.GetBucketId(), id.Bucket)
	}
}

// Two BucketAccesses on the same Bucket necessarily resolve to the same key
// pair. This is a deliberate limitation, not a bug: without bucket policies
// there is no way to admit a second user.
func TestGrantBucketAccessIsSharedAcrossGrants(t *testing.T) {
	p, _, _ := newTestProvisioner()
	created := createBucket(t, p, "my-bucket", testParams())

	grant := func(name string) *cosi.DriverGrantBucketAccessResponse {
		resp, err := p.DriverGrantBucketAccess(context.Background(), grantRequest(created.GetBucketId(), name))
		if err != nil {
			t.Fatalf("DriverGrantBucketAccess(%q): %v", name, err)
		}
		return resp
	}

	a, b := grant("access-a"), grant("access-b")
	if a.GetAccountId() != b.GetAccountId() {
		t.Errorf("expected both grants to share an account, got %q and %q", a.GetAccountId(), b.GetAccountId())
	}
}

func TestGrantBucketAccessRejectsServiceAccountAuth(t *testing.T) {
	p, _, _ := newTestProvisioner()
	created := createBucket(t, p, "my-bucket", testParams())

	req := grantRequest(created.GetBucketId(), "my-access")
	req.AuthenticationType = &cosi.AuthenticationType{Type: cosi.AuthenticationType_SERVICE_ACCOUNT}

	_, err := p.DriverGrantBucketAccess(context.Background(), req)
	requireCode(t, err, codes.Unimplemented)
}

// A grant spanning several buckets has no single key pair that could satisfy it: every
// bucket is owned by an objects user of its own.
func TestGrantBucketAccessRejectsMultipleBuckets(t *testing.T) {
	p, _, _ := newTestProvisioner()
	a := createBucket(t, p, "bucket-a", testParams())
	b := createBucket(t, p, "bucket-b", testParams())

	req := grantRequest(a.GetBucketId(), "my-access")
	req.Buckets = append(req.Buckets, &cosi.DriverGrantBucketAccessRequest_AccessedBucket{
		BucketId:   b.GetBucketId(),
		AccessMode: &cosi.AccessMode{Mode: cosi.AccessMode_READ_WRITE},
	})

	_, err := p.DriverGrantBucketAccess(context.Background(), req)
	requireCode(t, err, codes.InvalidArgument)
}

// The credentials handed back are the bucket owner's, so a narrower request would be
// answered with more access than it asked for.
func TestGrantBucketAccessRejectsNarrowerAccessModes(t *testing.T) {
	p, _, _ := newTestProvisioner()
	created := createBucket(t, p, "my-bucket", testParams())

	for _, mode := range []cosi.AccessMode_Mode{cosi.AccessMode_READ_ONLY, cosi.AccessMode_WRITE_ONLY} {
		req := grantRequest(created.GetBucketId(), "my-access")
		req.Buckets[0].AccessMode = &cosi.AccessMode{Mode: mode}

		_, err := p.DriverGrantBucketAccess(context.Background(), req)
		requireCode(t, err, codes.InvalidArgument)
	}
}

// A read-only cloudscale.ch API token yields users without secret keys. That
// must fail loudly rather than emit a half-empty credentials secret.
func TestGrantBucketAccessFailsWithoutSecretKey(t *testing.T) {
	p, users, _ := newTestProvisioner()
	created := createBucket(t, p, "my-bucket", testParams())
	id, _ := ParseBucketID(created.GetBucketId())
	users.users[id.UserID].SecretKey = ""

	_, err := p.DriverGrantBucketAccess(context.Background(), grantRequest(created.GetBucketId(), "my-access"))
	requireCode(t, err, codes.Internal)
}

func TestDeleteBucketRemovesBucketAndUser(t *testing.T) {
	p, users, store := newTestProvisioner()
	created := createBucket(t, p, "my-bucket", testParams())

	if _, err := p.DriverDeleteBucket(context.Background(), &cosi.DriverDeleteBucketRequest{
		BucketId:   created.GetBucketId(),
		Parameters: testParams(),
	}); err != nil {
		t.Fatalf("DriverDeleteBucket: %v", err)
	}

	if len(store.buckets) != 0 {
		t.Error("bucket was not deleted")
	}
	if len(users.users) != 0 {
		t.Error("objects user was not deleted")
	}
}

func TestDeleteBucketRefusesNonEmptyByDefault(t *testing.T) {
	p, users, store := newTestProvisioner()
	created := createBucket(t, p, "my-bucket", testParams())
	store.buckets["my-bucket"].objects = 3

	_, err := p.DriverDeleteBucket(context.Background(), &cosi.DriverDeleteBucketRequest{
		BucketId:   created.GetBucketId(),
		Parameters: testParams(),
	})
	requireCode(t, err, codes.FailedPrecondition)

	if len(users.users) != 1 {
		t.Error("objects user must survive a refused deletion")
	}
}

func TestDeleteBucketDeleteAllEmptiesFirst(t *testing.T) {
	p, _, store := newTestProvisioner()
	params := testParams()
	params[ParamDeletionPolicy] = string(DeleteAll)
	created := createBucket(t, p, "my-bucket", params)
	store.buckets["my-bucket"].objects = 3

	if _, err := p.DriverDeleteBucket(context.Background(), &cosi.DriverDeleteBucketRequest{
		BucketId:   created.GetBucketId(),
		Parameters: params,
	}); err != nil {
		t.Fatalf("DriverDeleteBucket: %v", err)
	}
	if len(store.buckets) != 0 {
		t.Error("bucket was not deleted")
	}
}

func TestDeleteBucketIsIdempotent(t *testing.T) {
	p, _, _ := newTestProvisioner()
	created := createBucket(t, p, "my-bucket", testParams())

	for i := range 2 {
		if _, err := p.DriverDeleteBucket(context.Background(), &cosi.DriverDeleteBucketRequest{
			BucketId:   created.GetBucketId(),
			Parameters: testParams(),
		}); err != nil {
			t.Fatalf("DriverDeleteBucket attempt %d: %v", i+1, err)
		}
	}
}

func TestDeleteBucketRejectsMalformedID(t *testing.T) {
	p, _, _ := newTestProvisioner()

	_, err := p.DriverDeleteBucket(context.Background(), &cosi.DriverDeleteBucketRequest{
		BucketId: "not-a-bucket-id",
	})
	requireCode(t, err, codes.InvalidArgument)
}

func TestRevokeBucketAccessIsNoOp(t *testing.T) {
	p, users, store := newTestProvisioner()
	created := createBucket(t, p, "my-bucket", testParams())

	if _, err := p.DriverRevokeBucketAccess(context.Background(), &cosi.DriverRevokeBucketAccessRequest{
		AccountId: "user-1",
		Buckets: []*cosi.DriverRevokeBucketAccessRequest_AccessedBucket{
			{BucketId: created.GetBucketId()},
		},
	}); err != nil {
		t.Fatalf("DriverRevokeBucketAccess: %v", err)
	}

	if len(users.users) != 1 || len(store.buckets) != 1 {
		t.Error("revoke must not touch the bucket or its owning user")
	}
}

func TestDriverGetInfo(t *testing.T) {
	p, _, _ := newTestProvisioner()

	resp, err := p.DriverGetInfo(context.Background(), &cosi.DriverGetInfoRequest{})
	if err != nil {
		t.Fatalf("DriverGetInfo: %v", err)
	}
	if resp.GetName() != testDriverName {
		t.Errorf("name = %q, want %q", resp.GetName(), testDriverName)
	}
}

// A freshly created objects user's key takes a moment to reach the region's
// RGW cluster, so the first CreateBucket calls can fail with InvalidAccessKeyId.
// The driver must ride that out rather than failing the claim.
func TestCreateBucketWaitsForCredentialPropagation(t *testing.T) {
	p, users, store := newTestProvisioner()
	var slept []time.Duration
	withFakeClock(p, &slept)
	store.notReadyFor(3)

	resp := createBucket(t, p, "my-bucket", testParams())

	if _, ok := store.buckets["my-bucket"]; !ok {
		t.Fatal("bucket was not created after the key propagated")
	}
	if len(slept) != 3 {
		t.Errorf("retried %d times, want 3", len(slept))
	}
	if len(users.users) != 1 {
		t.Errorf("retrying must not create extra objects users, got %d", len(users.users))
	}
	if resp.GetBucketId() == "" {
		t.Error("expected a bucket id")
	}
}

func TestCreateBucketBacksOffExponentially(t *testing.T) {
	p, _, store := newTestProvisioner()
	var slept []time.Duration
	withFakeClock(p, &slept)
	store.notReadyFor(4)

	createBucket(t, p, "my-bucket", testParams())

	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second}
	if len(slept) != len(want) {
		t.Fatalf("slept %v, want %v", slept, want)
	}
	for i := range want {
		if slept[i] != want[i] {
			t.Errorf("backoff[%d] = %v, want %v", i, slept[i], want[i])
		}
	}
}

// If the key never shows up the RPC must fail as Unavailable, so the sidecar
// retries instead of marking the claim permanently failed.
func TestCreateBucketGivesUpOnPropagationAsUnavailable(t *testing.T) {
	p, _, store := newTestProvisioner()
	var slept []time.Duration
	withFakeClock(p, &slept)
	store.notReadyFor(1000)

	_, err := p.DriverCreateBucket(context.Background(), &cosi.DriverCreateBucketRequest{
		Name:       "my-bucket",
		Parameters: testParams(),
	})
	requireCode(t, err, codes.Unavailable)

	var total time.Duration
	for _, d := range slept {
		total += d
	}
	if total > credentialPropagationBudget {
		t.Errorf("slept %v in total, exceeding the %v budget", total, credentialPropagationBudget)
	}
}

// withFakeClock makes the retry loop advance virtual time instead of sleeping,
// and records each delay.
func withFakeClock(p *Provisioner, slept *[]time.Duration) {
	now := time.Unix(0, 0)
	p.Clock = func() time.Time { return now }
	p.Sleep = func(_ context.Context, d time.Duration) error {
		*slept = append(*slept, d)
		now = now.Add(d)
		return nil
	}
}

// The bucket on cloudscale.ch must be named exactly what COSI called the
// Bucket resource, so that one name identifies it everywhere.
func TestCreateBucketUsesTheRequestedNameVerbatim(t *testing.T) {
	p, users, store := newTestProvisioner()
	const cosiName = "bucket-8f2a1c4e-1b3d-4e5f-9a7b-2c6d8e0f1a3b"

	resp := createBucket(t, p, cosiName, testParams())

	id, err := ParseBucketID(resp.GetBucketId())
	if err != nil {
		t.Fatalf("ParseBucketID: %v", err)
	}
	if id.Bucket != cosiName {
		t.Errorf("bucket id carries %q, want the requested %q", id.Bucket, cosiName)
	}
	if _, ok := store.buckets[cosiName]; !ok {
		t.Errorf("bucket %q was not created; store has %v", cosiName, store.buckets)
	}
	if got := users.users[id.UserID].Tags[tagBucket]; got != cosiName {
		t.Errorf("objects user tagged %q, want %q", got, cosiName)
	}
}

// Retries must still land on the same bucket and the same objects user.
func TestCreateBucketIsIdempotentWithPrefixedNames(t *testing.T) {
	p, users, _ := newTestProvisioner()
	const cosiName = "bucket-8f2a1c4e-1b3d-4e5f-9a7b-2c6d8e0f1a3b"

	first := createBucket(t, p, cosiName, testParams())
	second := createBucket(t, p, cosiName, testParams())

	if first.GetBucketId() != second.GetBucketId() {
		t.Errorf("bucket id changed across retries: %q then %q", first.GetBucketId(), second.GetBucketId())
	}
	if len(users.users) != 1 {
		t.Errorf("expected 1 objects user after retry, got %d", len(users.users))
	}
}

func TestGetBucketReportsAProvisionedBucket(t *testing.T) {
	p, _, _ := newTestProvisioner()
	created := createBucket(t, p, "my-bucket", testParams())
	id, _ := ParseBucketID(created.GetBucketId())

	resp, err := p.DriverGetBucket(context.Background(), &cosi.DriverGetBucketRequest{
		BucketId:   created.GetBucketId(),
		Protocols:  []*cosi.ObjectProtocol{{Type: cosi.ObjectProtocol_S3}},
		Parameters: testParams(),
	})
	if err != nil {
		t.Fatalf("DriverGetBucket: %v", err)
	}

	if resp.GetBucketId() != created.GetBucketId() {
		t.Errorf("bucket id = %q, want %q", resp.GetBucketId(), created.GetBucketId())
	}
	if got := resp.GetProtocols().GetS3().GetBucketId(); got != id.Bucket {
		t.Errorf("s3 bucket = %q, want %q", got, id.Bucket)
	}
	if got, want := resp.GetProtocols().GetS3().GetEndpoint(), DefaultEndpointURL("rma"); got != want {
		t.Errorf("endpoint = %q, want %q", got, want)
	}
}

// Adoption must never provision: a bucket whose objects user is gone is gone, and saying
// so is what lets COSI mark the Bucket lost instead of handing out dead credentials.
func TestGetBucketIsNotFoundOnceItsUserIsGone(t *testing.T) {
	p, users, _ := newTestProvisioner()
	created := createBucket(t, p, "my-bucket", testParams())
	id, _ := ParseBucketID(created.GetBucketId())
	delete(users.users, id.UserID)

	_, err := p.DriverGetBucket(context.Background(), &cosi.DriverGetBucketRequest{
		BucketId:   created.GetBucketId(),
		Parameters: testParams(),
	})
	requireCode(t, err, codes.NotFound)
}

func TestGetInfoAdvertisesS3(t *testing.T) {
	p, _, _ := newTestProvisioner()

	resp, err := p.DriverGetInfo(context.Background(), &cosi.DriverGetInfoRequest{})
	if err != nil {
		t.Fatalf("DriverGetInfo: %v", err)
	}

	// The sidecar refuses a claim asking for a protocol that is not in this list, so an
	// empty one would make every claim fail with nothing in the driver's log.
	protocols := resp.GetSupportedProtocols()
	if len(protocols) != 1 || protocols[0].GetType() != cosi.ObjectProtocol_S3 {
		t.Errorf("supported protocols = %v, want exactly [S3]", protocols)
	}
}
