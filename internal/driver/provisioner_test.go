package driver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	cosi "sigs.k8s.io/container-object-storage-interface/proto"
)

const (
	testDriverName = "cloudscale-objectstorage.helmetica.io"
	// What the COSI controller names a dynamically provisioned Bucket.
	testBucketName = "bc-8f2a1c4e-1b3d-4e5f-9a7b-2c6d8e0f1a3b"
	testBucketID   = "rma/" + testBucketName
)

func newTestProvisioner() (*Provisioner, *fakeUsers, *fakeBucketStore) {
	users := newFakeUsers()
	store := newFakeBucketStore()
	return &Provisioner{Name: testDriverName, Users: users, Buckets: store}, users, store
}

func testParams() map[string]string {
	return map[string]string{ParamRegion: "rma"}
}

func s3Protocols() []*cosi.ObjectProtocol {
	return []*cosi.ObjectProtocol{{Type: cosi.ObjectProtocol_S3}}
}

func mode(m cosi.AccessMode_Mode) *cosi.AccessMode {
	return &cosi.AccessMode{Mode: m}
}

func generateBucketID(t *testing.T, p *Provisioner, name string, params map[string]string) string {
	t.Helper()
	resp, err := p.DriverGenerateBucketId(context.Background(), &cosi.DriverGenerateBucketIdRequest{
		Name:       name,
		Protocols:  s3Protocols(),
		Parameters: params,
	})
	if err != nil {
		t.Fatalf("DriverGenerateBucketId(%q): %v", name, err)
	}
	return resp.GetBucketId()
}

func createBucketRequest(bucketID string, params map[string]string) *cosi.DriverCreateBucketRequest {
	return &cosi.DriverCreateBucketRequest{
		BucketId:   bucketID,
		Protocols:  s3Protocols(),
		Parameters: params,
	}
}

// createBucket runs both provisioning phases the way the sidecar does and
// returns the bucket ID.
func createBucket(t *testing.T, p *Provisioner, name string, params map[string]string) (string, *cosi.DriverCreateBucketResponse) {
	t.Helper()
	bucketID := generateBucketID(t, p, name, params)
	resp, err := p.DriverCreateBucket(context.Background(), createBucketRequest(bucketID, params))
	if err != nil {
		t.Fatalf("DriverCreateBucket(%q): %v", bucketID, err)
	}
	return bucketID, resp
}

// grantRequest is a well-formed single-bucket Key grant that leaves every
// access mode at ANY, which is what the sidecar sends when a BucketAccess sets
// none of them.
func grantRequest(bucketID string) *cosi.DriverGrantBucketAccessRequest {
	return &cosi.DriverGrantBucketAccessRequest{
		AccountName:        "ba-my-access",
		Protocol:           &cosi.ObjectProtocol{Type: cosi.ObjectProtocol_S3},
		AuthenticationType: &cosi.AuthenticationType{Type: cosi.AuthenticationType_KEY},
		Buckets: []*cosi.DriverGrantBucketAccessRequest_AccessedBucket{{
			BucketId:                 bucketID,
			ObjectDataAccessMode:     mode(cosi.AccessMode_ANY),
			ObjectMetadataAccessMode: mode(cosi.AccessMode_ANY),
			BucketMetadataAccessMode: mode(cosi.AccessMode_ANY),
		}},
	}
}

// ownerOf returns the single objects user tagged for a bucket.
func ownerOf(t *testing.T, users *fakeUsers, bucket string) *ObjectsUser {
	t.Helper()
	var found []*ObjectsUser
	for _, u := range users.users {
		if u.Tags[tagBucket] == bucket {
			found = append(found, u)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly 1 objects user for bucket %q, got %d", bucket, len(found))
	}
	return found[0]
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

func TestDriverGetInfo(t *testing.T) {
	p, _, _ := newTestProvisioner()

	resp, err := p.DriverGetInfo(context.Background(), &cosi.DriverGetInfoRequest{})
	if err != nil {
		t.Fatalf("DriverGetInfo: %v", err)
	}
	if resp.GetName() != testDriverName {
		t.Errorf("name = %q, want %q", resp.GetName(), testDriverName)
	}
	protos := resp.GetSupportedProtocols()
	if len(protos) != 1 || protos[0].GetType() != cosi.ObjectProtocol_S3 {
		t.Errorf("supported protocols = %v, want exactly S3", protos)
	}
}

func TestDriverGetInfoRequiresName(t *testing.T) {
	p, _, _ := newTestProvisioner()
	p.Name = ""

	_, err := p.DriverGetInfo(context.Background(), &cosi.DriverGetInfoRequest{})
	requireCode(t, err, codes.Internal)
}

func TestGenerateBucketIDEncodesRegionAndName(t *testing.T) {
	p, _, _ := newTestProvisioner()

	got := generateBucketID(t, p, testBucketName, testParams())

	if got != testBucketID {
		t.Errorf("bucket id = %q, want %q", got, testBucketID)
	}
}

// Phase one must not touch the backend: COSI may drop the Bucket after it
// without ever calling DriverDeleteBucket.
func TestGenerateBucketIDCreatesNothing(t *testing.T) {
	p, users, store := newTestProvisioner()

	generateBucketID(t, p, testBucketName, testParams())

	if len(users.users) != 0 || len(store.buckets) != 0 {
		t.Errorf("DriverGenerateBucketId created %d users and %d buckets, want none",
			len(users.users), len(store.buckets))
	}
}

// COSI retries with the same name if it fails to persist the ID, and must get
// the same ID back.
func TestGenerateBucketIDIsDeterministic(t *testing.T) {
	p, _, _ := newTestProvisioner()

	first := generateBucketID(t, p, testBucketName, testParams())
	second := generateBucketID(t, p, testBucketName, testParams())

	if first != second {
		t.Errorf("bucket id changed across calls: %q then %q", first, second)
	}
}

func TestGenerateBucketIDRejectsInvalidRequests(t *testing.T) {
	cases := map[string]*cosi.DriverGenerateBucketIdRequest{
		"missing name": {
			Parameters: testParams(),
		},
		"name too long for S3": {
			Name:       "bc-" + strings.Repeat("a", maxBucketNameLength),
			Parameters: testParams(),
		},
		"missing region": {
			Name:       testBucketName,
			Parameters: map[string]string{},
		},
		"unknown deletion policy": {
			Name:       testBucketName,
			Parameters: map[string]string{ParamRegion: "rma", ParamDeletionPolicy: "Nuke"},
		},
		"non-S3 protocol": {
			Name:       testBucketName,
			Protocols:  []*cosi.ObjectProtocol{{Type: cosi.ObjectProtocol_AZURE}},
			Parameters: testParams(),
		},
	}

	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			p, _, _ := newTestProvisioner()
			_, err := p.DriverGenerateBucketId(context.Background(), req)
			requireCode(t, err, codes.InvalidArgument)
		})
	}
}

func TestCreateBucketProvisionsUserAndBucket(t *testing.T) {
	p, users, store := newTestProvisioner()

	_, resp := createBucket(t, p, testBucketName, testParams())

	ownerOf(t, users, testBucketName)
	if _, ok := store.buckets[testBucketName]; !ok {
		t.Error("bucket was not created")
	}

	info := resp.GetProtocols().GetS3()
	if info == nil {
		t.Fatal("no S3 bucket info in response")
	}
	if info.GetBucketId() != testBucketName {
		t.Errorf("bucket info bucket = %q, want %q", info.GetBucketId(), testBucketName)
	}
	if info.GetRegion() != "rma" {
		t.Errorf("bucket info region = %q, want %q", info.GetRegion(), "rma")
	}
	if got, want := info.GetEndpoint(), DefaultEndpointURL("rma"); got != want {
		t.Errorf("bucket info endpoint = %q, want %q", got, want)
	}
	if got := info.GetAddressingStyle().GetStyle(); got != cosi.S3AddressingStyle_PATH {
		t.Errorf("addressing style = %v, want PATH", got)
	}
	if resp.GetProtocols().GetAzure() != nil || resp.GetProtocols().GetGcs() != nil {
		t.Error("must not report support for protocols other than S3")
	}
}

func TestCreateBucketReportsVirtualHostedStyle(t *testing.T) {
	p, _, _ := newTestProvisioner()
	params := testParams()
	params[ParamPathStyle] = "false"

	_, resp := createBucket(t, p, testBucketName, params)

	if got := resp.GetProtocols().GetS3().GetAddressingStyle().GetStyle(); got != cosi.S3AddressingStyle_VIRTUAL {
		t.Errorf("addressing style = %v, want VIRTUAL", got)
	}
}

// A retried DriverCreateBucket must adopt the objects user from the previous
// attempt instead of leaking a second one.
func TestCreateBucketIsIdempotent(t *testing.T) {
	p, users, _ := newTestProvisioner()
	bucketID, _ := createBucket(t, p, testBucketName, testParams())

	if _, err := p.DriverCreateBucket(context.Background(), createBucketRequest(bucketID, testParams())); err != nil {
		t.Fatalf("second DriverCreateBucket: %v", err)
	}

	if len(users.users) != 1 {
		t.Errorf("expected 1 objects user after retry, got %d", len(users.users))
	}
}

// If the S3 call fails, the tagged user survives so the next retry adopts it.
func TestCreateBucketRetriesAdoptUserAfterS3Failure(t *testing.T) {
	p, users, store := newTestProvisioner()
	store.createErr = errors.New("boom")

	_, err := p.DriverCreateBucket(context.Background(), createBucketRequest(testBucketID, testParams()))
	requireCode(t, err, codes.Internal)
	if len(users.users) != 1 {
		t.Fatalf("expected the objects user to be kept for the retry, got %d users", len(users.users))
	}

	store.createErr = nil
	if _, err := p.DriverCreateBucket(context.Background(), createBucketRequest(testBucketID, testParams())); err != nil {
		t.Fatalf("retried DriverCreateBucket: %v", err)
	}

	if len(users.users) != 1 {
		t.Errorf("retry created a duplicate objects user: %d total", len(users.users))
	}
}

func TestCreateBucketRejectsBucketOwnedByAnother(t *testing.T) {
	p, _, store := newTestProvisioner()
	store.put(testBucketName, "someone-elses-key", 0)

	_, err := p.DriverCreateBucket(context.Background(), createBucketRequest(testBucketID, testParams()))
	requireCode(t, err, codes.AlreadyExists)
}

func TestCreateBucketRejectsInvalidRequests(t *testing.T) {
	cases := map[string]*cosi.DriverCreateBucketRequest{
		"malformed bucket id": createBucketRequest("not-a-bucket-id", testParams()),
		"v1alpha1 bucket id":  createBucketRequest(testBucketID+"/user-1", testParams()),
		"region differs from bucket id": createBucketRequest(testBucketID,
			map[string]string{ParamRegion: "lpg"}),
		"non-S3 protocol": {
			BucketId:   testBucketID,
			Protocols:  []*cosi.ObjectProtocol{{Type: cosi.ObjectProtocol_GCS}},
			Parameters: testParams(),
		},
	}

	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			p, users, _ := newTestProvisioner()
			_, err := p.DriverCreateBucket(context.Background(), req)
			requireCode(t, err, codes.InvalidArgument)
			if len(users.users) != 0 {
				t.Error("a rejected request must not create an objects user")
			}
		})
	}
}

// The region is in the bucket ID, so DriverCreateBucket works without it in
// the parameters.
func TestCreateBucketTakesRegionFromBucketID(t *testing.T) {
	p, _, store := newTestProvisioner()

	if _, err := p.DriverCreateBucket(context.Background(), createBucketRequest(testBucketID, nil)); err != nil {
		t.Fatalf("DriverCreateBucket: %v", err)
	}
	if _, ok := store.buckets[testBucketName]; !ok {
		t.Error("bucket was not created")
	}
}

// The bucket on cloudscale.ch must be named exactly what COSI called the
// Bucket resource, so that one name identifies it everywhere.
func TestCreateBucketUsesTheRequestedNameVerbatim(t *testing.T) {
	p, users, store := newTestProvisioner()

	createBucket(t, p, testBucketName, testParams())

	if _, ok := store.buckets[testBucketName]; !ok {
		t.Errorf("bucket %q was not created; store has %v", testBucketName, store.buckets)
	}
	owner := ownerOf(t, users, testBucketName)
	if owner.DisplayName != testBucketName {
		t.Errorf("objects user named %q, want %q", owner.DisplayName, testBucketName)
	}
	if owner.Tags[tagManagedBy] != testDriverName {
		t.Errorf("objects user managed-by tag = %q, want %q", owner.Tags[tagManagedBy], testDriverName)
	}
}

// Two users tagged for one bucket mean something went wrong outside the
// driver. Handing out either key pair could be the wrong one.
func TestCreateBucketRefusesAmbiguousOwner(t *testing.T) {
	p, users, _ := newTestProvisioner()
	for range 2 {
		if _, err := users.Create(context.Background(), testBucketName, p.bucketTags(testBucketName)); err != nil {
			t.Fatal(err)
		}
	}

	_, err := p.DriverCreateBucket(context.Background(), createBucketRequest(testBucketID, testParams()))
	requireCode(t, err, codes.Internal)
}

func TestGetBucketReportsProvisionedBucket(t *testing.T) {
	p, users, store := newTestProvisioner()
	bucketID, _ := createBucket(t, p, testBucketName, testParams())

	resp, err := p.DriverGetBucket(context.Background(), &cosi.DriverGetBucketRequest{
		BucketId:   bucketID,
		Protocols:  s3Protocols(),
		Parameters: testParams(),
	})
	if err != nil {
		t.Fatalf("DriverGetBucket: %v", err)
	}

	info := resp.GetProtocols().GetS3()
	if info.GetBucketId() != testBucketName || info.GetRegion() != "rma" {
		t.Errorf("unexpected bucket info %v", info)
	}
	if len(users.users) != 1 || len(store.buckets) != 1 {
		t.Error("DriverGetBucket must not create anything")
	}
}

func TestGetBucketNotFound(t *testing.T) {
	t.Run("no owning objects user", func(t *testing.T) {
		p, users, _ := newTestProvisioner()

		_, err := p.DriverGetBucket(context.Background(), &cosi.DriverGetBucketRequest{BucketId: testBucketID})
		requireCode(t, err, codes.NotFound)
		if len(users.users) != 0 {
			t.Error("DriverGetBucket must not create an objects user")
		}
	})

	t.Run("owner exists but bucket is gone", func(t *testing.T) {
		p, _, store := newTestProvisioner()
		bucketID, _ := createBucket(t, p, testBucketName, testParams())
		delete(store.buckets, testBucketName)

		_, err := p.DriverGetBucket(context.Background(), &cosi.DriverGetBucketRequest{BucketId: bucketID})
		requireCode(t, err, codes.NotFound)
	})
}

func TestGetBucketRejectsInvalidRequests(t *testing.T) {
	cases := map[string]*cosi.DriverGetBucketRequest{
		"malformed bucket id": {BucketId: "not-a-bucket-id"},
		"non-S3 protocol": {
			BucketId:  testBucketID,
			Protocols: []*cosi.ObjectProtocol{{Type: cosi.ObjectProtocol_AZURE}},
		},
	}

	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			p, _, _ := newTestProvisioner()
			_, err := p.DriverGetBucket(context.Background(), req)
			requireCode(t, err, codes.InvalidArgument)
		})
	}
}

func TestGrantBucketAccessReturnsOwnerCredentials(t *testing.T) {
	p, users, _ := newTestProvisioner()
	bucketID, _ := createBucket(t, p, testBucketName, testParams())
	owner := ownerOf(t, users, testBucketName)

	resp, err := p.DriverGrantBucketAccess(context.Background(), grantRequest(bucketID))
	if err != nil {
		t.Fatalf("DriverGrantBucketAccess: %v", err)
	}

	if resp.GetAccountId() != owner.ID {
		t.Errorf("account id = %q, want the owning user %q", resp.GetAccountId(), owner.ID)
	}

	creds := resp.GetCredentials().GetS3()
	if creds == nil {
		t.Fatal("no S3 credentials in response")
	}
	if creds.GetAccessKeyId() != owner.AccessKey {
		t.Errorf("access key = %q, want %q", creds.GetAccessKeyId(), owner.AccessKey)
	}
	if creds.GetAccessSecretKey() != owner.SecretKey {
		t.Errorf("secret key = %q, want %q", creds.GetAccessSecretKey(), owner.SecretKey)
	}

	if len(resp.GetBuckets()) != 1 {
		t.Fatalf("expected info for exactly 1 bucket, got %d", len(resp.GetBuckets()))
	}
	bucket := resp.GetBuckets()[0]
	if bucket.GetBucketId() != bucketID {
		t.Errorf("bucket info for %q, want %q", bucket.GetBucketId(), bucketID)
	}
	info := bucket.GetBucketInfo().GetS3()
	if info.GetBucketId() != testBucketName {
		t.Errorf("S3 bucket = %q, want %q", info.GetBucketId(), testBucketName)
	}
	if got, want := info.GetEndpoint(), DefaultEndpointURL("rma"); got != want {
		t.Errorf("endpoint = %q, want %q", got, want)
	}
	if info.GetRegion() != "rma" {
		t.Errorf("region = %q, want %q", info.GetRegion(), "rma")
	}
}

// Two BucketAccesses on the same Bucket necessarily resolve to the same key
// pair. This is a deliberate limitation, not a bug: without bucket policies
// there is no way to admit a second user.
func TestGrantBucketAccessIsSharedAcrossGrants(t *testing.T) {
	p, _, _ := newTestProvisioner()
	bucketID, _ := createBucket(t, p, testBucketName, testParams())

	grant := func(name string) *cosi.DriverGrantBucketAccessResponse {
		req := grantRequest(bucketID)
		req.AccountName = name
		resp, err := p.DriverGrantBucketAccess(context.Background(), req)
		if err != nil {
			t.Fatalf("DriverGrantBucketAccess(%q): %v", name, err)
		}
		return resp
	}

	a, b := grant("ba-access-a"), grant("ba-access-b")
	if a.GetAccountId() != b.GetAccountId() {
		t.Errorf("expected both grants to share an account, got %q and %q", a.GetAccountId(), b.GetAccountId())
	}
}

func TestGrantBucketAccessAcceptsReadWrite(t *testing.T) {
	p, _, _ := newTestProvisioner()
	bucketID, _ := createBucket(t, p, testBucketName, testParams())

	req := grantRequest(bucketID)
	req.Buckets[0].ObjectDataAccessMode = mode(cosi.AccessMode_READ_WRITE)
	req.Buckets[0].ObjectMetadataAccessMode = mode(cosi.AccessMode_READ_WRITE)
	req.Buckets[0].BucketMetadataAccessMode = mode(cosi.AccessMode_READ_WRITE)

	if _, err := p.DriverGrantBucketAccess(context.Background(), req); err != nil {
		t.Fatalf("DriverGrantBucketAccess: %v", err)
	}
}

// The owner's key pair is full access. Handing it out for a narrower request
// would grant more than was asked for, in any of the three categories.
func TestGrantBucketAccessRefusesNarrowerAccessModes(t *testing.T) {
	setters := map[string]func(*cosi.DriverGrantBucketAccessRequest_AccessedBucket, *cosi.AccessMode){
		"objectData": func(b *cosi.DriverGrantBucketAccessRequest_AccessedBucket, m *cosi.AccessMode) {
			b.ObjectDataAccessMode = m
		},
		"objectMetadata": func(b *cosi.DriverGrantBucketAccessRequest_AccessedBucket, m *cosi.AccessMode) {
			b.ObjectMetadataAccessMode = m
		},
		"bucketMetadata": func(b *cosi.DriverGrantBucketAccessRequest_AccessedBucket, m *cosi.AccessMode) {
			b.BucketMetadataAccessMode = m
		},
	}
	modes := []cosi.AccessMode_Mode{
		cosi.AccessMode_READ_ONLY,
		cosi.AccessMode_WRITE_ONLY,
		// COSI never sends UNKNOWN on purpose; the spec says to treat it as an error.
		cosi.AccessMode_UNKNOWN,
	}

	for field, set := range setters {
		for _, m := range modes {
			t.Run(field+"="+m.String(), func(t *testing.T) {
				p, _, _ := newTestProvisioner()
				bucketID, _ := createBucket(t, p, testBucketName, testParams())

				req := grantRequest(bucketID)
				set(req.Buckets[0], mode(m))

				_, err := p.DriverGrantBucketAccess(context.Background(), req)
				requireCode(t, err, codes.InvalidArgument)
				if !strings.Contains(err.Error(), field) {
					t.Errorf("error %q does not name the offending field %q", err, field)
				}
			})
		}
	}
}

// The spec reserves OutOfRange for exactly this, and the sidecar turns it into
// a "driver does not support multi-bucket access" error.
func TestGrantBucketAccessRefusesMultipleBuckets(t *testing.T) {
	p, _, _ := newTestProvisioner()
	first, _ := createBucket(t, p, testBucketName, testParams())
	second, _ := createBucket(t, p, "bc-0b8e3c2d-5f6a-4b7c-8d9e-0f1a2b3c4d5e", testParams())

	req := grantRequest(first)
	other := grantRequest(second).Buckets[0]
	req.Buckets = append(req.Buckets, other)

	_, err := p.DriverGrantBucketAccess(context.Background(), req)
	requireCode(t, err, codes.OutOfRange)
}

func TestGrantBucketAccessRejectsServiceAccount(t *testing.T) {
	p, _, _ := newTestProvisioner()
	bucketID, _ := createBucket(t, p, testBucketName, testParams())

	req := grantRequest(bucketID)
	req.AuthenticationType = &cosi.AuthenticationType{Type: cosi.AuthenticationType_SERVICE_ACCOUNT}
	req.ServiceAccountName = "my-workload"

	_, err := p.DriverGrantBucketAccess(context.Background(), req)
	requireCode(t, err, codes.Unimplemented)
}

func TestGrantBucketAccessRejectsInvalidRequests(t *testing.T) {
	cases := map[string]func(*cosi.DriverGrantBucketAccessRequest){
		"no buckets":             func(r *cosi.DriverGrantBucketAccessRequest) { r.Buckets = nil },
		"unknown authentication": func(r *cosi.DriverGrantBucketAccessRequest) { r.AuthenticationType = nil },
		"non-S3 protocol": func(r *cosi.DriverGrantBucketAccessRequest) {
			r.Protocol = &cosi.ObjectProtocol{Type: cosi.ObjectProtocol_AZURE}
		},
		"malformed bucket id": func(r *cosi.DriverGrantBucketAccessRequest) { r.Buckets[0].BucketId = "nope" },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p, _, _ := newTestProvisioner()
			bucketID, _ := createBucket(t, p, testBucketName, testParams())

			req := grantRequest(bucketID)
			mutate(req)

			_, err := p.DriverGrantBucketAccess(context.Background(), req)
			requireCode(t, err, codes.InvalidArgument)
		})
	}
}

func TestGrantBucketAccessNotFoundWithoutOwner(t *testing.T) {
	p, _, _ := newTestProvisioner()

	_, err := p.DriverGrantBucketAccess(context.Background(), grantRequest(testBucketID))
	requireCode(t, err, codes.NotFound)
}

// A read-only cloudscale.ch API token yields users without secret keys. That
// must fail loudly rather than emit a half-empty credentials secret.
func TestGrantBucketAccessFailsWithoutSecretKey(t *testing.T) {
	p, users, _ := newTestProvisioner()
	bucketID, _ := createBucket(t, p, testBucketName, testParams())
	ownerOf(t, users, testBucketName).SecretKey = ""

	_, err := p.DriverGrantBucketAccess(context.Background(), grantRequest(bucketID))
	requireCode(t, err, codes.Internal)
}

func TestDeleteBucketRemovesBucketAndUser(t *testing.T) {
	p, users, store := newTestProvisioner()
	bucketID, _ := createBucket(t, p, testBucketName, testParams())

	if _, err := p.DriverDeleteBucket(context.Background(), &cosi.DriverDeleteBucketRequest{
		BucketId:   bucketID,
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

// DeleteIfEmpty must refuse with a code the sidecar retries, so the claim
// finishes deleting by itself once somebody empties the bucket.
func TestDeleteBucketRefusesNonEmptyByDefaultThenRecovers(t *testing.T) {
	p, users, store := newTestProvisioner()
	bucketID, _ := createBucket(t, p, testBucketName, testParams())
	store.buckets[testBucketName].objects = 3

	req := &cosi.DriverDeleteBucketRequest{BucketId: bucketID, Parameters: testParams()}

	_, err := p.DriverDeleteBucket(context.Background(), req)
	requireCode(t, err, codes.FailedPrecondition)
	if len(users.users) != 1 {
		t.Fatal("objects user must survive a refused deletion")
	}

	store.buckets[testBucketName].objects = 0
	if _, err := p.DriverDeleteBucket(context.Background(), req); err != nil {
		t.Fatalf("DriverDeleteBucket after emptying: %v", err)
	}
	if len(store.buckets) != 0 || len(users.users) != 0 {
		t.Error("bucket and objects user should be gone once the bucket was emptied")
	}
}

func TestDeleteBucketDeleteAllEmptiesFirst(t *testing.T) {
	p, users, store := newTestProvisioner()
	params := testParams()
	params[ParamDeletionPolicy] = string(DeleteAll)
	bucketID, _ := createBucket(t, p, testBucketName, params)
	store.buckets[testBucketName].objects = 3

	if _, err := p.DriverDeleteBucket(context.Background(), &cosi.DriverDeleteBucketRequest{
		BucketId:   bucketID,
		Parameters: params,
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

func TestDeleteBucketIsIdempotent(t *testing.T) {
	p, _, _ := newTestProvisioner()
	bucketID, _ := createBucket(t, p, testBucketName, testParams())

	for i := range 2 {
		if _, err := p.DriverDeleteBucket(context.Background(), &cosi.DriverDeleteBucketRequest{
			BucketId:   bucketID,
			Parameters: testParams(),
		}); err != nil {
			t.Fatalf("DriverDeleteBucket attempt %d: %v", i+1, err)
		}
	}
}

// COSI records the generated ID before DriverCreateBucket runs, so it can ask
// to delete a bucket that was never created. That must succeed, or the claim
// would never finish deleting.
func TestDeleteBucketNeverCreated(t *testing.T) {
	p, _, _ := newTestProvisioner()
	bucketID := generateBucketID(t, p, testBucketName, testParams())

	if _, err := p.DriverDeleteBucket(context.Background(), &cosi.DriverDeleteBucketRequest{
		BucketId:   bucketID,
		Parameters: testParams(),
	}); err != nil {
		t.Fatalf("DriverDeleteBucket: %v", err)
	}
}

// A user created by a DriverCreateBucket whose S3 call never succeeded must
// still be cleaned up.
func TestDeleteBucketRemovesUserWithoutBucket(t *testing.T) {
	p, users, store := newTestProvisioner()
	store.createErr = errors.New("boom")
	_, err := p.DriverCreateBucket(context.Background(), createBucketRequest(testBucketID, testParams()))
	requireCode(t, err, codes.Internal)

	if _, err := p.DriverDeleteBucket(context.Background(), &cosi.DriverDeleteBucketRequest{
		BucketId:   testBucketID,
		Parameters: testParams(),
	}); err != nil {
		t.Fatalf("DriverDeleteBucket: %v", err)
	}
	if len(users.users) != 0 {
		t.Error("orphaned objects user was not deleted")
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
	bucketID, _ := createBucket(t, p, testBucketName, testParams())

	if _, err := p.DriverRevokeBucketAccess(context.Background(), &cosi.DriverRevokeBucketAccessRequest{
		AccountId:          ownerOf(t, users, testBucketName).ID,
		Protocol:           &cosi.ObjectProtocol{Type: cosi.ObjectProtocol_S3},
		AuthenticationType: &cosi.AuthenticationType{Type: cosi.AuthenticationType_KEY},
		Buckets:            []*cosi.DriverRevokeBucketAccessRequest_AccessedBucket{{BucketId: bucketID}},
	}); err != nil {
		t.Fatalf("DriverRevokeBucketAccess: %v", err)
	}

	if len(users.users) != 1 || len(store.buckets) != 1 {
		t.Error("revoke must not touch the bucket or its owning user")
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

	createBucket(t, p, testBucketName, testParams())

	if _, ok := store.buckets[testBucketName]; !ok {
		t.Fatal("bucket was not created after the key propagated")
	}
	if len(slept) != 3 {
		t.Errorf("retried %d times, want 3", len(slept))
	}
	if len(users.users) != 1 {
		t.Errorf("retrying must not create extra objects users, got %d", len(users.users))
	}
}

func TestCreateBucketBacksOffExponentially(t *testing.T) {
	p, _, store := newTestProvisioner()
	var slept []time.Duration
	withFakeClock(p, &slept)
	store.notReadyFor(4)

	createBucket(t, p, testBucketName, testParams())

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

	_, err := p.DriverCreateBucket(context.Background(), createBucketRequest(testBucketID, testParams()))
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

// Provisioner embeds the generated Unimplemented servers, so an RPC added
// upstream compiles fine and only fails at runtime with Unimplemented. Calling
// every method in the service descriptors with an empty request catches that
// on the next proto bump instead.
func TestEveryRPCIsImplemented(t *testing.T) {
	p, _, _ := newTestProvisioner()
	emptyRequest := func(any) error { return nil }

	for _, desc := range []grpc.ServiceDesc{cosi.Identity_ServiceDesc, cosi.Provisioner_ServiceDesc} {
		for _, m := range desc.Methods {
			t.Run(desc.ServiceName+"/"+m.MethodName, func(t *testing.T) {
				_, err := m.Handler(p, context.Background(), emptyRequest, nil)
				if status.Code(err) == codes.Unimplemented {
					t.Errorf("%s is not implemented: %v", m.MethodName, err)
				}
			})
		}
	}
}
