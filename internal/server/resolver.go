package server

import (
	"context"
	"errors"

	"github.com/robertjndw/gotero/internal/auth"
	"github.com/robertjndw/gotero/internal/repo"
	"github.com/robertjndw/gotero/internal/stream"
	zs "github.com/robertjndw/gotero/zotero/stream"
)

// topicResolver maps API keys to the library topics the stream may push.
type topicResolver struct {
	auth   *auth.Authenticator
	groups repo.GroupRepo
}

func (r topicResolver) KeyTopics(ctx context.Context, apiKey string) (int64, []string, error) {
	p, err := r.auth.Authenticate(ctx, apiKey)
	if errors.Is(err, auth.ErrInvalidKey) || errors.Is(err, auth.ErrNoCredentials) {
		return 0, nil, stream.ErrInvalidKey
	}
	if err != nil {
		return 0, nil, err
	}
	var topics []string
	if auth.UserLibraryAccess(p, p.User.ID).Read {
		topics = append(topics, zs.UserTopic(p.User.ID))
	}
	groups, err := r.groups.ListForUser(ctx, p.User.ID)
	if err != nil {
		return 0, nil, err
	}
	for _, g := range groups {
		// ListForUser only returns groups the user belongs to, so any role
		// suffices here; the key's group permission still applies.
		if auth.GroupLibraryAccess(p, g, "member").Read {
			topics = append(topics, zs.GroupTopic(g.ID))
		}
	}
	return p.User.ID, topics, nil
}
