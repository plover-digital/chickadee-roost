package site

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Historical records belong to the organization scope, including repositories
// that may have been removed from its current runner-group selection.
func TestSelectedOrganizationHistoryRequiresOwnerAndCompleteACL(t *testing.T) {
	account := Account{ID: 10, Type: "Organization"}
	snapshot := AccountUsage{ObservedAt: time.Now(), InstallationID: 8, AccountID: 10, AccountType: "Organization", RepositoryIDs: []int64{99, 100}}
	p := page{User: &User{ID: 7}, Choices: []Choice{
		{Installation: Installation{ID: 8, Account: account}, Repository: Repository{ID: 99}},
		{Installation: Installation{ID: 8, Account: account}, Repository: Repository{ID: 100}},
	}}
	if snapshot.authorized(p) {
		t.Fatal("repository-only administration disclosed historical organization activity")
	}
	p.Choices[0].OrganizationAdmin = true
	p.Choices[1].OrganizationAdmin = true
	if !snapshot.authorized(p) {
		t.Fatal("verified owner with complete selected ACL was denied")
	}
	p.Choices = p.Choices[:1]
	if snapshot.authorized(p) {
		t.Fatal("owner role bypassed incomplete selected repository verification")
	}
}

func TestPersonalUsageHistoryStillRequiresMatchingOwner(t *testing.T) {
	snapshot := AccountUsage{ObservedAt: time.Now(), InstallationID: 8, AccountID: 7, AccountType: "User", RepositoryIDs: []int64{99}}
	p := page{User: &User{ID: 7}, Choices: []Choice{{Installation: Installation{ID: 8, Account: Account{ID: 7, Type: "User"}}, Repository: Repository{ID: 99}}}}
	if !snapshot.authorized(p) {
		t.Fatal("personal owner lost access to their repository usage")
	}
	p.User = &User{ID: 9}
	if snapshot.authorized(p) {
		t.Fatal("another user accessed personal usage")
	}
}

func TestRateLimitedOrganizationMembershipIsUnavailable(t *testing.T) {
	for _, tc := range []struct {
		code   int
		header string
		value  string
	}{
		{403, "X-RateLimit-Remaining", "0"},
		{403, "Retry-After", "60"},
		{403, "", ""},
		{429, "", ""},
	} {
		t.Run(fmt.Sprintf("%d/%s", tc.code, tc.header), func(t *testing.T) {
			s := orgAccessFixture(t)
			original := s.http.Transport
			s.http.Transport = transport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/user/memberships/orgs/example" {
					res := response(`{"message":"API rate limit exceeded"}`)
					res.StatusCode = tc.code
					if tc.header != "" {
						res.Header.Set(tc.header, tc.value)
					}
					return res, nil
				}
				return original.RoundTrip(r)
			})
			if _, err := s.choices(t.Context(), s.sessions["session"]); err == nil {
				t.Fatal("rate-limited owner verification was reported as denied membership")
			}
		})
	}
}
