package driver

import (
	"context"
	"fmt"
	"maps"
	"sync"
)

// fakeUsers is an in-memory driver.ObjectsUsers.
type fakeUsers struct {
	mu     sync.Mutex
	users  map[string]*ObjectsUser
	nextID int

	// createErr, if set, fails the next Create.
	createErr error
	// omitSecret simulates a read-only API token.
	omitSecret bool
}

func newFakeUsers() *fakeUsers {
	return &fakeUsers{users: map[string]*ObjectsUser{}}
}

func (f *fakeUsers) Create(_ context.Context, displayName string, tags map[string]string) (*ObjectsUser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.createErr != nil {
		return nil, f.createErr
	}

	f.nextID++
	u := &ObjectsUser{
		ID:          fmt.Sprintf("user-%d", f.nextID),
		DisplayName: displayName,
		AccessKey:   fmt.Sprintf("AK%d", f.nextID),
		SecretKey:   fmt.Sprintf("SK%d", f.nextID),
		Tags:        maps.Clone(tags),
	}
	if f.omitSecret {
		u.SecretKey = ""
	}
	f.users[u.ID] = u
	return u, nil
}

func (f *fakeUsers) Get(_ context.Context, id string) (*ObjectsUser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	u, ok := f.users[id]
	if !ok {
		return nil, fmt.Errorf("user %q: %w", id, ErrNotFound)
	}
	clone := *u
	return &clone, nil
}

func (f *fakeUsers) FindByTags(_ context.Context, tags map[string]string) ([]ObjectsUser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []ObjectsUser
	for _, u := range f.users {
		match := true
		for k, v := range tags {
			if u.Tags[k] != v {
				match = false
				break
			}
		}
		if match {
			out = append(out, *u)
		}
	}
	return out, nil
}

func (f *fakeUsers) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.users[id]; !ok {
		return fmt.Errorf("user %q: %w", id, ErrNotFound)
	}
	delete(f.users, id)
	return nil
}

// fakeBucketStore is the shared S3 "backend" behind every fakeBuckets client.
// Buckets remember their owning access key, which is how the fake reproduces
// cloudscale.ch's rule that only the creating user can reach a bucket.
type fakeBucketStore struct {
	mu      sync.Mutex
	buckets map[string]*fakeBucket
	// createErr, if set, fails the next Create.
	createErr error
	// notReady counts down Create calls that report ErrCredentialsNotReady.
	notReady int
}

type fakeBucket struct {
	owner   string
	objects int
}

// notReadyFor makes the next n Create calls fail as if the key pair had not
// propagated to the S3 endpoint yet.
func (s *fakeBucketStore) notReadyFor(n int) { s.notReady = n }

func newFakeBucketStore() *fakeBucketStore {
	return &fakeBucketStore{buckets: map[string]*fakeBucket{}}
}

func (s *fakeBucketStore) New(_, _, accessKey, _ string, _ bool) (Buckets, error) {
	return &fakeBuckets{store: s, accessKey: accessKey}, nil
}

func (s *fakeBucketStore) put(name, owner string, objects int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buckets[name] = &fakeBucket{owner: owner, objects: objects}
}

type fakeBuckets struct {
	store     *fakeBucketStore
	accessKey string
}

func (b *fakeBuckets) Create(_ context.Context, name string) error {
	b.store.mu.Lock()
	defer b.store.mu.Unlock()

	if b.store.createErr != nil {
		return b.store.createErr
	}
	if b.store.notReady > 0 {
		b.store.notReady--
		return ErrCredentialsNotReady
	}
	if existing, ok := b.store.buckets[name]; ok {
		if existing.owner == b.accessKey {
			return nil // BucketAlreadyOwnedByYou
		}
		return ErrBucketNameExists
	}
	b.store.buckets[name] = &fakeBucket{owner: b.accessKey}
	return nil
}

func (b *fakeBuckets) Empty(_ context.Context, name string) error {
	b.store.mu.Lock()
	defer b.store.mu.Unlock()

	bucket, ok := b.store.buckets[name]
	if !ok {
		return ErrNotFound
	}
	bucket.objects = 0
	return nil
}

func (b *fakeBuckets) Delete(_ context.Context, name string) error {
	b.store.mu.Lock()
	defer b.store.mu.Unlock()

	bucket, ok := b.store.buckets[name]
	if !ok {
		return ErrNotFound
	}
	if bucket.objects > 0 {
		return ErrBucketNotEmpty
	}
	delete(b.store.buckets, name)
	return nil
}
