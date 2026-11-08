package auth

import (
	"context"
	"testing"

	"github.com/robertjndw/gotero/internal/repo"
)

// blockingKeys parks GetByHash after reading the row, so a test can run an
// invalidation between the DB read and the cache write.
type blockingKeys struct {
	fakeKeys
	entered chan struct{}
	release chan struct{}
}

func (b *blockingKeys) GetByHash(ctx context.Context, h []byte) (repo.APIKey, error) {
	k, err := b.fakeKeys.GetByHash(ctx, h)
	b.entered <- struct{}{}
	<-b.release
	return k, err
}

func TestAuthenticatorSkipsCacheWhenInvalidatedMidLookup(t *testing.T) {
	for name, invalidate := range map[string]func(a *Authenticator){
		"key":  func(a *Authenticator) { a.Invalidate(HashToken("good")) },
		"user": func(a *Authenticator) { a.InvalidateUser(7) },
	} {
		t.Run(name, func(t *testing.T) {
			keys := &blockingKeys{
				fakeKeys: fakeKeys{keys: map[string]repo.APIKey{string(HashToken("good")): {ID: 1, UserID: 7}}},
				entered:  make(chan struct{}),
				release:  make(chan struct{}),
			}
			users := &fakeUsers{users: map[int64]repo.User{7: {ID: 7}}}
			a := NewAuthenticator(keys, users)

			done := make(chan error, 1)
			go func() {
				_, err := a.Authenticate(context.Background(), "good")
				done <- err
			}()
			<-keys.entered
			invalidate(a)
			keys.release <- struct{}{}
			if err := <-done; err != nil {
				t.Fatalf("in-flight request still gets its principal: %v", err)
			}

			go func() { <-keys.entered; keys.release <- struct{}{} }()
			if _, err := a.Authenticate(context.Background(), "good"); err != nil {
				t.Fatal(err)
			}
			if keys.calls != 2 {
				t.Fatalf("stale principal was cached: %d DB lookups", keys.calls)
			}
		})
	}
}
