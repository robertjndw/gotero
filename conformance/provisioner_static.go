package conformance

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// ErrNotEnoughUsers means a StaticProvisioner ran out of pre-made users.
var ErrNotEnoughUsers = errors.New("conformance: static provisioner has no unused users left")

// StaticProvisioner hands out pre-made users, each at most once. It cannot
// create groups or change server state, so tests needing those are skipped.
type StaticProvisioner struct {
	mu    sync.Mutex
	users []User
	next  int
}

func NewStaticProvisioner(users []User) *StaticProvisioner {
	return &StaticProvisioner{users: users}
}

// ParseStaticUsers parses "id:username:password:apikey" entries separated by
// commas or newlines. Username and password may be empty.
func ParseStaticUsers(s string) ([]User, error) {
	var out []User
	for _, e := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '\n' }) {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		p := strings.Split(e, ":")
		if len(p) != 4 {
			return nil, fmt.Errorf("conformance: bad user entry %q, want id:username:password:apikey", e)
		}
		id, err := strconv.ParseInt(p[0], 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("conformance: bad user id in %q", e)
		}
		if p[3] == "" {
			return nil, fmt.Errorf("conformance: empty api key in %q", e)
		}
		out = append(out, User{ID: id, Username: p[1], Password: p[2], APIKey: p[3]})
	}
	return out, nil
}

func (s *StaticProvisioner) NewUser(context.Context) (User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next >= len(s.users) {
		return User{}, ErrNotEnoughUsers
	}
	u := s.users[s.next]
	s.next++
	return u, nil
}

func (s *StaticProvisioner) NewGroup(context.Context, User, []User, []User) (Group, error) {
	return Group{}, ErrUnsupported
}
