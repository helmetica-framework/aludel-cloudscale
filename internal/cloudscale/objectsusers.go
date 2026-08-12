// Package cloudscale adapts the cloudscale.ch Go SDK to the interfaces the
// driver package depends on.
package cloudscale

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/cloudscale-ch/cloudscale-go-sdk/v6"

	"github.com/helmetica-framework/aludel-cloudscale/internal/driver"
)

// ObjectsUsers implements driver.ObjectsUsers on top of the cloudscale.ch API.
type ObjectsUsers struct {
	client *cloudscale.Client
}

var _ driver.ObjectsUsers = (*ObjectsUsers)(nil)

// New builds a cloudscale.ch API client from a read-write API token.
//
// The token must be read-write: cloudscale.ch only returns objects user secret
// keys to read-write tokens, and aludel-cloudscale reads them back on every access grant.
func New(token string) (*ObjectsUsers, error) {
	if token == "" {
		return nil, errors.New("cloudscale.ch API token is empty")
	}
	c := cloudscale.NewClient(http.DefaultClient)
	c.AuthToken = token
	return &ObjectsUsers{client: c}, nil
}

func (o *ObjectsUsers) Create(ctx context.Context, displayName string, tags map[string]string) (*driver.ObjectsUser, error) {
	tagMap := cloudscale.TagMap(tags)
	user, err := o.client.ObjectsUsers.Create(ctx, &cloudscale.ObjectsUserRequest{
		DisplayName:           displayName,
		TaggedResourceRequest: cloudscale.TaggedResourceRequest{Tags: &tagMap},
	})
	if err != nil {
		return nil, err
	}
	return convert(user), nil
}

func (o *ObjectsUsers) Get(ctx context.Context, id string) (*driver.ObjectsUser, error) {
	user, err := o.client.ObjectsUsers.Get(ctx, id)
	if err != nil {
		if isNotFound(err) {
			return nil, fmt.Errorf("objects user %q: %w", id, driver.ErrNotFound)
		}
		return nil, err
	}
	return convert(user), nil
}

// FindByTags returns the objects users carrying all of the given tags.
//
// The filtering happens in two places, because cloudscale.ch rejects a request
// with more than one `tag:` query parameter. One tag therefore goes to the API
// and the remaining ones are matched here, over the response.
//
// Matching the remaining tags locally is not an optimisation, it is what makes
// the result correct: the API alone would also return users that carry the one
// tag it filtered on but belong to a different bucket or driver.
//
// Which tag goes to the API does not affect the result, so the keys are sorted
// and the first one is used, keeping the request reproducible.
func (o *ObjectsUsers) FindByTags(ctx context.Context, tags map[string]string) ([]driver.ObjectsUser, error) {
	var modifiers []cloudscale.ListRequestModifier
	if key, ok := serverSideFilterKey(tags); ok {
		modifiers = append(modifiers, cloudscale.WithTagFilter(cloudscale.TagMap{key: tags[key]}))
	}

	users, err := o.client.ObjectsUsers.List(ctx, modifiers...)
	if err != nil {
		return nil, err
	}

	out := make([]driver.ObjectsUser, 0, len(users))
	for i := range users {
		if !hasAllTags(users[i].Tags, tags) {
			continue
		}
		out = append(out, *convert(&users[i]))
	}
	return out, nil
}

// serverSideFilterKey picks the one tag key to send to the API.
func serverSideFilterKey(tags map[string]string) (string, bool) {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return "", false
	}
	slices.Sort(keys)
	return keys[0], true
}

func hasAllTags(have cloudscale.TagMap, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func (o *ObjectsUsers) Delete(ctx context.Context, id string) error {
	if err := o.client.ObjectsUsers.Delete(ctx, id); err != nil {
		if isNotFound(err) {
			return fmt.Errorf("objects user %q: %w", id, driver.ErrNotFound)
		}
		return err
	}
	return nil
}

// convert maps an objects user from the SDK onto the driver's view of one.
//
// The SDK leaves the key pairs untyped, as plain string maps, so the keys are
// read out by their JSON names. Only the first pair is taken: cloudscale.ch
// issues one at creation and aludel-cloudscale never adds another.
func convert(u *cloudscale.ObjectsUser) *driver.ObjectsUser {
	out := &driver.ObjectsUser{
		ID:          u.ID,
		DisplayName: u.DisplayName,
		Tags:        map[string]string(u.Tags),
	}
	if len(u.Keys) > 0 {
		out.AccessKey = u.Keys[0]["access_key"]
		out.SecretKey = u.Keys[0]["secret_key"]
	}
	return out
}

func isNotFound(err error) bool {
	apiErr, ok := errors.AsType[*cloudscale.ErrorResponse](err)
	return ok && apiErr.StatusCode == http.StatusNotFound
}
