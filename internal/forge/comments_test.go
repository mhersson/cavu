package forge

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mhersson/cavu/internal/testkit/forgefake"
)

const changes = "CHANGES_REQUESTED"

// reviewBots names a bot the way GitHub's REST API and page show it, in
// another case, and lint-bot without its [bot], which names a user.
var reviewBots = []string{"Review-Bot[bot]", "lint-bot"}

// newCommentsFake returns a fake with pull request 1 open, where alice may
// write, ada administers, rita may read and nobody else is a collaborator.
func newCommentsFake(t *testing.T, bare string) (*forgefake.Fake, *Client) {
	t.Helper()

	f, c := newFake(t, bare, nil)
	openPR(t, c)
	f.SetPermission("alice", "write")
	f.SetPermission("ada", "admin")
	f.SetPermission("rita", "read")

	return f, c
}

func TestObserveComments(t *testing.T) {
	bare, _ := newBareRepo(t)

	tests := []struct {
		name string
		// script scripts pull request 1 and returns the requests Observe gives.
		script func(f *forgefake.Fake) []Comment
	}{
		{"a review that requests changes, by write", func(f *forgefake.Fake) []Comment {
			id := f.Review(1, "alice", changes, "Handle the error.")

			return []Comment{{ID: id, Author: "alice", Body: "Handle the error."}}
		}},
		{"a review that requests changes, by admin", func(f *forgefake.Fake) []Comment {
			id := f.Review(1, "ada", changes, "Handle the error.")

			return []Comment{{ID: id, Author: "ada", Body: "Handle the error."}}
		}},
		{"a review by read", func(f *forgefake.Fake) []Comment {
			f.Review(1, "rita", changes, "Handle the error.")

			return nil
		}},
		{"a review by an account that is no collaborator", func(f *forgefake.Fake) []Comment {
			f.Review(1, "mallory", changes, "Handle the error.")

			return nil
		}},
		{"a review by a deleted account", func(f *forgefake.Fake) []Comment {
			f.Review(1, "", changes, "Handle the error.")

			return nil
		}},
		{"a review by a configured bot", func(f *forgefake.Fake) []Comment {
			id := f.Review(1, "review-bot[bot]", changes, "Handle the error.")

			return []Comment{{ID: id, Author: "review-bot[bot]", Body: "Handle the error."}}
		}},
		{"a review by a user who carries a configured bot's name", func(f *forgefake.Fake) []Comment {
			f.Review(1, "review-bot", changes, "Handle the error.")

			return nil
		}},
		{"a review by a bot configured without its suffix", func(f *forgefake.Fake) []Comment {
			f.Review(1, "lint-bot[bot]", changes, "Handle the error.")

			return nil
		}},
		{"a review by a configured user", func(f *forgefake.Fake) []Comment {
			id := f.Review(1, "Lint-Bot", changes, "Handle the error.")

			return []Comment{{ID: id, Author: "Lint-Bot", Body: "Handle the error."}}
		}},
		{"a review without a body", func(f *forgefake.Fake) []Comment {
			f.Review(1, "alice", changes, "")

			return nil
		}},
		{"an approval", func(f *forgefake.Fake) []Comment {
			f.Review(1, "alice", "APPROVED", "Looks good.")

			return nil
		}},
		{"a long body is cut at 4 KiB on a character", func(f *forgefake.Fake) []Comment {
			id := f.Review(1, "alice", changes, strings.Repeat("€", 2000))

			return []Comment{{ID: id, Author: "alice", Body: strings.Repeat("€", 4096/len("€"))}}
		}},
		{"a thread", func(f *forgefake.Fake) []Comment {
			th := f.Thread(1, "alice", "main.go", 12, "Rename this.")

			return []Comment{{ID: th.ID, Author: "alice", Path: "main.go", Line: 12, Body: "Rename this."}}
		}},
		{"a thread's long body is cut at 4 KiB on a character", func(f *forgefake.Fake) []Comment {
			// "€" takes the bytes 4095 to 4097, so a cut at 4096 bytes would split it.
			th := f.Thread(1, "alice", "main.go", 12, strings.Repeat("a", 4095)+"€ and more")
			reply := th.Reply("alice", "Short.")

			return []Comment{{ID: reply, Author: "alice", Path: "main.go", Line: 12, Body: strings.Repeat("a", 4095) + "\n\nShort."}}
		}},
		{"a resolved thread", func(f *forgefake.Fake) []Comment {
			f.Thread(1, "alice", "main.go", 12, "Rename this.").Resolve()

			return nil
		}},
		{"a thread by read", func(f *forgefake.Fake) []Comment {
			f.Thread(1, "rita", "main.go", 12, "Rename this.")

			return nil
		}},
		{"an outdated thread, which counts", func(f *forgefake.Fake) []Comment {
			th := f.Thread(1, "alice", "main.go", 12, "Rename this.")
			th.Move(0)

			return []Comment{{ID: th.ID, Author: "alice", Path: "main.go", Line: 12, Body: "Rename this."}}
		}},
		{"a thread whose line moved", func(f *forgefake.Fake) []Comment {
			th := f.Thread(1, "alice", "main.go", 12, "Rename this.")
			th.Move(20)

			return []Comment{{ID: th.ID, Author: "alice", Path: "main.go", Line: 20, Body: "Rename this."}}
		}},
		{"a pending comment by write", func(f *forgefake.Fake) []Comment {
			f.Thread(1, "alice", "main.go", 12, "Rename this.").MarkPending()

			return nil
		}},
		{"a thread whose newest comment by write is pending", func(f *forgefake.Fake) []Comment {
			th := f.Thread(1, "alice", "main.go", 12, "Rename this.")
			th.Reply("alice", "And this, once I submit.")
			th.MarkPending()

			return []Comment{{ID: th.ID, Author: "alice", Path: "main.go", Line: 12, Body: "Rename this."}}
		}},
		{"a thread with a reply by a read-only account", func(f *forgefake.Fake) []Comment {
			th := f.Thread(1, "alice", "main.go", 12, "Rename this.")
			th.Reply("rita", "Rename it to what?")

			return []Comment{{ID: th.ID, Author: "alice", Path: "main.go", Line: 12, Body: "Rename this."}}
		}},
		{"a thread with a later reply by its reviewer", func(f *forgefake.Fake) []Comment {
			th := f.Thread(1, "alice", "main.go", 12, "Rename this.")
			reply := th.Reply("alice", "Still wrong.")

			return []Comment{{ID: reply, Author: "alice", Path: "main.go", Line: 12, Body: "Rename this.\n\nStill wrong."}}
		}},
		{"a thread a read-only account started and a writer took up", func(f *forgefake.Fake) []Comment {
			th := f.Thread(1, "rita", "main.go", 12, "Rename this?")
			reply := th.Reply("alice", "Yes, to parse.")

			return []Comment{{ID: reply, Author: "alice", Path: "main.go", Line: 12, Body: "Yes, to parse."}}
		}},
		{"a review and a thread", func(f *forgefake.Fake) []Comment {
			th := f.Thread(1, "alice", "main.go", 12, "Rename this.")
			id := f.Review(1, "alice", changes, "See the comments.")

			return []Comment{
				{ID: id, Author: "alice", Body: "See the comments."},
				{ID: th.ID, Author: "alice", Path: "main.go", Line: 12, Body: "Rename this."},
			}
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, c := newCommentsFake(t, bare)
			want := tc.script(f)

			o, err := c.Observe(t.Context(), fakeRepo, fakeAccess, Query{Number: 1, ReviewBots: reviewBots})
			require.NoError(t, err)
			assert.Equal(t, want, o.Comments)
		})
	}
}

func TestObserveAsksEachAuthorOnce(t *testing.T) {
	bare, _ := newBareRepo(t)
	f, c := newCommentsFake(t, bare)

	f.Review(1, "alice", changes, "See the comments.")
	f.Thread(1, "alice", "a.go", 1, "One.").Reply("rita", "Why?")
	f.Thread(1, "alice", "b.go", 2, "Two.")
	f.Review(1, "review-bot[bot]", changes, "Lint.")
	f.Review(1, "ci-bot[bot]", changes, "Build.")
	f.Thread(1, "", "c.go", 3, "From a deleted account.")

	for range 2 {
		_, err := c.Observe(t.Context(), fakeRepo, fakeAccess, Query{Number: 1, ReviewBots: reviewBots})
		require.NoError(t, err)
	}

	var asked []string

	for _, r := range f.Requests() {
		if rest, ok := strings.CutPrefix(r.Path, "/api/v3/repos/o/n/collaborators/"); ok {
			asked = append(asked, strings.TrimSuffix(rest, "/permission"))
		}
	}

	// Once per Observe each; never a bot, configured or not, nor a deleted account.
	assert.Equal(t, []string{"alice", "rita", "alice", "rita"}, asked)
}

func TestAuthorName(t *testing.T) {
	for _, tc := range []struct {
		a    author
		want string
	}{
		{author{Typename: "Bot", Login: "review-bot"}, "review-bot[bot]"},
		{author{Typename: "Bot", Login: "review-bot[bot]"}, "review-bot[bot]"},
		{author{Typename: "User", Login: "review-bot"}, "review-bot"},
		{author{}, ""},
	} {
		assert.Equal(t, tc.want, tc.a.name(), "%+v", tc.a)
	}
}
