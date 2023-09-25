package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jarcoal/httpmock"
	"go.elara.ws/go-lemmy"
	"go.elara.ws/go-lemmy/types"
	"go.elara.ws/lemmy-reply-bot/internal/store"
)

func TestCreate(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	// register list API endpoints
	registerListComments(t)
	registerListPosts(t)

	var commentReplies []string
	var postReplies []string

	// Whenver the create comment endpoint is called, if the comment is replying to a post,
	// append it to the postReplies slice. If it's replying to another comment, append it to
	// the commentReplies slice.
	httpmock.RegisterResponder("POST", "https://lemmy.example.com/api/v3/comment", func(r *http.Request) (*http.Response, error) {
		var cc types.CreateComment
		if err := json.NewDecoder(r.Body).Decode(&cc); err != nil {
			t.Fatal("Error decoding CreateComment request:", err)
		}

		// Check whether the comment is replying to a post or another comment
		if cc.PostID != 0 {
			// If the comment is a reply to a post, append it to postReplies
			postReplies = append(postReplies, cc.Content)
		} else {
			// If the comment is a reply to another comment, append it to commentReplies
			commentReplies = append(commentReplies, cc.Content)
		}

		// Return a successful response
		return httpmock.NewJsonResponse(200, types.CommentResponse{})
	})

	// Open a new in-memory reply database
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal("Error opening in-memory database:", err)
	}
	defer db.Close()

	// Create a context that will get canceled in 5 seconds
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Run the workers concurrently
	wg := initWorkers(t, ctx, db)
	// Wait for the workers to stop due to context cancellation
	wg.Wait()

	expectedCommentReplies := []string{"pong", "Lemmy Comment!"}
	expectedPostReplies := []string{"pong", "Lemmy Post!"}

	if !reflect.DeepEqual(commentReplies, expectedCommentReplies) {
		t.Errorf("[Comment] Expected %v, got %v", expectedCommentReplies, commentReplies)
	}

	if !reflect.DeepEqual(postReplies, expectedPostReplies) {
		t.Errorf("[Post] Expected %v, got %v", expectedPostReplies, postReplies)
	}
}

func TestEdit(t *testing.T) {
	httpmock.Activate()
	defer httpmock.DeactivateAndReset()

	// register list API endpoints
	registerListComments(t)
	registerListPosts(t)

	// We don't care about new comments in this test case, so we don't do anything in the comment handler
	httpmock.RegisterResponder("POST", "https://lemmy.example.com/api/v3/comment", func(r *http.Request) (*http.Response, error) {
		return httpmock.NewJsonResponse(200, types.CommentResponse{})
	})

	edited := map[float64]string{}

	// Whenever the edit comment endpoint is called, add the edited comment to
	// the edited map, so that it can be checked later
	httpmock.RegisterResponder("PUT", "https://lemmy.example.com/api/v3/comment", func(r *http.Request) (*http.Response, error) {
		var ec types.EditComment
		if err := json.NewDecoder(r.Body).Decode(&ec); err != nil {
			t.Fatal("Error decoding CreateComment request:", err)
		}
		edited[ec.CommentID] = ec.Content.ValueOr("")
		return httpmock.NewJsonResponse(200, types.CommentResponse{})
	})

	// Open a new in-memory reply database
	db, err := openDB(":memory:")
	if err != nil {
		t.Fatal("Error opening in-memory database:", err)
	}
	defer db.Close()
	rs := store.New(db)

	// Add a new comment with id 12 that was updated before the fake one in
	// registerListComments. This will cause the bot to edit that comment.
	rs.AddItem(context.Background(), store.AddItemParams{
		ID:          12,
		ItemType:    store.Comment,
		UpdatedTime: 0,
	})

	// Set the reply ID of comment id 12 to 100 so we know it edited the
	// right comment when it calls the edit API endpoint.
	rs.SetReplyID(context.Background(), store.SetReplyIDParams{
		ID:      12,
		ReplyID: 100,
	})

	// Add a new post with id 3 that was updated before the fake one in
	// registerListPosts. This will cause the bot to edit that comment.
	rs.AddItem(context.Background(), store.AddItemParams{
		ID:          3,
		ItemType:    store.Post,
		UpdatedTime: 0,
	})

	// Set the reply ID of post id 3 to 100 so we know it edited the
	// right comment when it calls the edit API endpoint.
	rs.SetReplyID(context.Background(), store.SetReplyIDParams{
		ID:      3,
		ReplyID: 101,
	})

	// Create a context that will get canceled in 5 seconds
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Run the workers concurrently
	wg := initWorkers(t, ctx, db)
	// Wait for the workers to stop due to context cancellation
	wg.Wait()

	expected := map[float64]string{
		100: "Lemmy Comment!",
		101: "Lemmy Post!",
	}

	if !reflect.DeepEqual(edited, expected) {
		t.Errorf("Expected %v, got %v", expected, edited)
	}
}

// testConfig returns a new Config for testing purposes.
func testConfig(t *testing.T) Config {
	t.Helper()

	cfgFile := &ConfigFile{
		Replies: []Reply{
			{
				Regex: "ping",
				Msg:   "pong",
			},
			{
				Regex: "Hello, (.+)",
				Msg:   "{{.Match 0 1}}!",
			},
		},
	}

	compiledRegexes, compiledTmpls, err := compileReplies(cfgFile.Replies)
	if err != nil {
		t.Fatal("Error compiling replies:", err)
	}

	return Config{
		ConfigFile:   cfgFile,
		Regexes:      compiledRegexes,
		Tmpls:        compiledTmpls,
		PollInterval: time.Second,
	}
}

// initWorkers does some setup and then starts the bot workers in separate goroutines.
// It returns a WaitGroup that's released when both of the workers return
func initWorkers(t *testing.T, ctx context.Context, db *sql.DB) *sync.WaitGroup {
	t.Helper()

	// Register a login endpoint that always returns test_token
	httpmock.RegisterResponder("POST", "https://lemmy.example.com/api/v3/user/login", func(r *http.Request) (*http.Response, error) {
		return httpmock.NewJsonResponse(200, types.LoginResponse{JWT: types.NewOptional("test_token")})
	})

	// Create a new lemmy client using the mocked instance
	c, err := lemmy.New("https://lemmy.example.com")
	if err != nil {
		t.Fatal("Error creating lemmy client:", err)
	}

	// Log in to the fake instance
	err = c.ClientLogin(ctx, types.Login{
		UsernameOrEmail: "test_username",
		Password:        "test_password",
	})
	if err != nil {
		t.Fatal("Error logging in to mocked client:", err)
	}

	// Create a config for testing
	cfg := testConfig(t)
	rs := store.New(db)

	replyCh := make(chan replyJob, 200)

	wg := &sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		commentWorker(ctx, c, cfg, rs, replyCh)
	}()

	go func() {
		defer wg.Done()
		commentReplyWorker(ctx, c, rs, replyCh)
	}()

	return wg
}

// registerListComments registers an HTTP mock for the /comment/list API endpoint
func registerListComments(t *testing.T) {
	t.Helper()
	httpmock.RegisterResponder("GET", `=~^https://lemmy\.example\.com/api/v3/comment/list\?.*`, func(r *http.Request) (*http.Response, error) {
		return httpmock.NewJsonResponse(200, types.GetCommentsResponse{
			Comments: []types.CommentView{
				{
					Comment: types.Comment{ // Should match reply index 0
						ID:        10,
						Published: types.LemmyTime{Time: time.Unix(1550164620, 0)},
						Content:   "ping",
					},
					Community: types.Community{
						Local: true,
					},
				},
				{
					Comment: types.Comment{ // Should be skipped due to non-local community
						ID:        11,
						Published: types.LemmyTime{Time: time.Unix(1550164620, 0)},
						Content:   "ping",
					},
					Community: types.Community{
						Local: false,
					},
				},
				{
					Comment: types.Comment{ // Should match reply index 1
						ID:        12,
						Published: types.LemmyTime{Time: time.Unix(1550164620, 0)},
						Updated:   types.NewOptional(types.LemmyTime{Time: time.Unix(1581700620, 0)}),
						Content:   "Hello, Lemmy Comment",
					},
					Community: types.Community{
						Local: true,
					},
				},
				{
					Comment: types.Comment{ // Shouldn't match
						ID:        13,
						Published: types.LemmyTime{Time: time.Unix(1550164620, 0)},
						Content:   "This comment doesn't match any replies",
					},
					Community: types.Community{
						Local: true,
					},
				},
			},
		})
	})
}

// registerListPosts registers an HTTP mock for the /post/list API endpoint
func registerListPosts(t *testing.T) {
	t.Helper()
	httpmock.RegisterResponder("GET", `=~^https://lemmy\.example\.com/api/v3/post/list\?.*`, func(r *http.Request) (*http.Response, error) {
		return httpmock.NewJsonResponse(200, types.GetPostsResponse{
			Posts: []types.PostView{
				{
					Post: types.Post{ // Should match reply index 0
						ID:        1,
						Published: types.LemmyTime{Time: time.Unix(1550164620, 0)},
						Body:      types.NewOptional("ping"),
					},
					Community: types.Community{
						Local: true,
					},
				},
				{
					Post: types.Post{ // Should be skipped due to non-local community
						ID:        2,
						Published: types.LemmyTime{Time: time.Unix(1550164620, 0)},
						Body:      types.NewOptional("ping"),
					},
					Community: types.Community{
						Local: false,
					},
				},
				{
					Post: types.Post{ // Should match reply index 1
						ID:        3,
						Published: types.LemmyTime{Time: time.Unix(1550164620, 0)},
						Updated:   types.NewOptional(types.LemmyTime{Time: time.Unix(1581700620, 0)}),
						Body:      types.NewOptional("Hello, Lemmy Post"),
					},
					Community: types.Community{
						Local: true,
					},
				},
				{
					Post: types.Post{ // Shouldn't match
						ID:        4,
						Published: types.LemmyTime{Time: time.Unix(1550164620, 0)},
						Body:      types.NewOptional("This comment doesn't match any replies"),
					},
					Community: types.Community{
						Local: true,
					},
				},
			},
		})
	})
}
