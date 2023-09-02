package main

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"os/signal"
	"strings"
	"syscall"
	"text/template"
	"time"
	"unsafe"

	"github.com/spf13/pflag"
	"go.elara.ws/go-lemmy"
	"go.elara.ws/go-lemmy/types"
	"go.elara.ws/lemmy-reply-bot/internal/store"
	"go.elara.ws/logger/log"
	_ "modernc.org/sqlite"
)

//go:generate sqlc generate

//go:embed sql/schema.sql
var schema string

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(schema)
	return db, err
}

func main() {
	configPath := pflag.StringP("config", "c", "./lemmy-reply-bot.toml", "Path to the config file")
	dryRun := pflag.BoolP("dry-run", "D", false, "Don't actually send comments, just check for matches")
	pflag.Parse()

	ctx := context.Background()
	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal("Error loading config file").Err(err).Send()
	}

	db, err := openDB("replied.db")
	if err != nil {
		log.Fatal("Error opening reply database").Err(err).Send()
	}
	defer db.Close()
	rs := store.New(db)

	c, err := lemmy.New(cfg.ConfigFile.Lemmy.InstanceURL)
	if err != nil {
		log.Fatal("Error creating new Lemmy API client").Err(err).Send()
	}

	err = c.ClientLogin(ctx, types.Login{
		UsernameOrEmail: cfg.ConfigFile.Lemmy.Account.UserOrEmail,
		Password:        cfg.ConfigFile.Lemmy.Account.Password,
	})
	if err != nil {
		log.Fatal("Error logging in to Lemmy instance").Err(err).Send()
	}

	log.Info("Successfully logged in to Lemmy instance").Send()

	replyCh := make(chan replyJob, 200)

	if !*dryRun {
		// Start the reply worker in the background
		go commentReplyWorker(ctx, c, rs, replyCh)
	}

	// Start the comment worker
	commentWorker(ctx, c, cfg, rs, replyCh)
}

func commentWorker(ctx context.Context, c *lemmy.Client, cfg Config, rs *store.Queries, replyCh chan<- replyJob) {
	for {
		select {
		case <-time.After(cfg.PollInterval):
			// Get 50 of the newest comments from Lemmy
			comments, err := c.Comments(ctx, types.GetComments{
				Type:  types.NewOptional(types.ListingTypeLocal),
				Sort:  types.NewOptional(types.CommentSortTypeNew),
				Limit: types.NewOptional[int64](50),
			})
			if err != nil {
				log.Warn("Error getting comments").Err(err).Send()
				continue
			}

			for _, c := range comments.Comments {
				// Skip all non-local comments
				if !c.Community.Local {
					continue
				}

				edit := false

				// Try to get comment item from the database
				item, err := rs.GetItem(ctx, store.GetItemParams{
					ID:       int64(c.Comment.ID),
					ItemType: store.Comment,
				})
				if errors.Is(err, sql.ErrNoRows) {
					// If the item doesn't exist, we need to reply to it,
					// so don't continue or set edit
				} else if err != nil {
					log.Warn("Error checking if item exists").Err(err).Send()
					continue
				} else if item.UpdatedTime == c.Comment.Updated.Unix() {
					// If the item we're checking for exists and hasn't been edited,
					// we've already replied, so skip it
					continue
				} else if item.UpdatedTime != c.Comment.Updated.Unix() {
					// If the item exists but has been edited since we replied,
					// set edit to true so we know to edit it instead of making
					// a new comment
					edit = true
				}

				for i, reply := range cfg.ConfigFile.Replies {
					re := cfg.Regexes[reply.Regex]
					if !re.MatchString(c.Comment.Content) {
						continue
					}

					log.Info("Matched comment body").
						Int("reply-index", i).
						Int("comment-id", c.Comment.ID).
						Send()

					job := replyJob{
						CommentID: types.NewOptional(c.Comment.ID),
						PostID:    c.Comment.PostID,
					}

					// If edit is set to true, we need to edit the comment,
					// so set the job's EditID so the reply worker knows which
					// comment to edit
					if edit {
						job.EditID = int(item.ReplyID)
					}

					matches := re.FindAllStringSubmatch(c.Comment.Content, -1)
					job.Content, err = executeTmpl(cfg.Tmpls[reply.Regex], TmplContext{
						Matches: toSubmatches(matches),
						Type:    "comment",
					})
					if err != nil {
						log.Warn("Error while executing template").Err(err).Send()
						continue
					}

					replyCh <- job

					// Add the reply to the database so we don't reply to it
					// again if we encounter it again
					err = rs.AddItem(ctx, store.AddItemParams{
						ID:          int64(c.Comment.ID),
						ItemType:    store.Comment,
						UpdatedTime: c.Comment.Updated.Unix(),
					})
					if err != nil {
						log.Warn("Error adding comment to the reply store").Err(err).Send()
						continue
					}
				}
			}

			// Get 20 of the newest posts from Lemmy
			posts, err := c.Posts(ctx, types.GetPosts{
				Type:  types.NewOptional(types.ListingTypeLocal),
				Sort:  types.NewOptional(types.SortTypeNew),
				Limit: types.NewOptional[int64](20),
			})
			if err != nil {
				log.Warn("Error getting comments").Err(err).Send()
				continue
			}

			for _, p := range posts.Posts {
				// Skip all non-local posts
				if !p.Community.Local {
					continue
				}

				edit := false

				// Try to get post item from the database
				item, err := rs.GetItem(ctx, store.GetItemParams{
					ID:       int64(p.Post.ID),
					ItemType: store.Post,
				})
				if errors.Is(err, sql.ErrNoRows) {
					// If the item doesn't exist, we need to reply to it,
					// so don't continue or set edit
				} else if err != nil {
					log.Warn("Error checking if item exists").Err(err).Send()
					continue
				} else if item.UpdatedTime == p.Post.Updated.Unix() {
					// If the item we're checking for exists and hasn't been edited,
					// we've already replied, so skip it
					continue
				} else if item.UpdatedTime != p.Post.Updated.Unix() {
					// If the item exists but has been edited since we replied,
					// set edit to true so we know to edit it instead of making
					// a new comment
					edit = true
				}

				body := p.Post.URL.ValueOr("") + "\n\n" + p.Post.Body.ValueOr("")
				for i, reply := range cfg.ConfigFile.Replies {
					re := cfg.Regexes[reply.Regex]
					if !re.MatchString(body) {
						continue
					}

					log.Info("Matched post body").
						Int("reply-index", i).
						Int("post-id", p.Post.ID).
						Send()

					job := replyJob{PostID: p.Post.ID}

					// If edit is set to true, we need to edit the comment,
					// so set the job's EditID so the reply worker knows which
					// comment to edit
					if edit {
						job.EditID = int(item.ReplyID)
					}

					matches := re.FindAllStringSubmatch(body, -1)
					job.Content, err = executeTmpl(cfg.Tmpls[reply.Regex], TmplContext{
						Matches: toSubmatches(matches),
						Type:    "post",
					})
					if err != nil {
						log.Warn("Error while executing template").Err(err).Send()
						continue
					}

					replyCh <- job

					// Add the reply to the database so we don't reply to it
					// again if we encounter it again
					err = rs.AddItem(ctx, store.AddItemParams{
						ID:          int64(p.Post.ID),
						ItemType:    store.Post,
						UpdatedTime: p.Post.Updated.Unix(),
					})
					if err != nil {
						log.Warn("Error adding post to the reply store").Err(err).Send()
						continue
					}
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

type replyJob struct {
	Content   string
	CommentID types.Optional[int]
	EditID    int
	PostID    int
}

func commentReplyWorker(ctx context.Context, c *lemmy.Client, rs *store.Queries, ch <-chan replyJob) {
	for {
		select {
		case reply := <-ch:
			// If the edit ID is set
			if reply.EditID > 0 {
				// Edit the comment with the specified ID with the new content
				cr, err := c.EditComment(ctx, types.EditComment{
					CommentID: reply.EditID,
					Content:   types.NewOptional(reply.Content),
				})
				if err != nil {
					log.Warn("Error while trying to edit comment").Err(err).Send()
				}

				// Set the reply ID for the post/comment in the database
				// so that we know which comment ID to edit if we need to.
				err = rs.SetReplyID(ctx, store.SetReplyIDParams{
					ID:      int64(reply.CommentID.ValueOr(reply.PostID)),
					ReplyID: int64(cr.CommentView.Comment.ID),
				})
				if err != nil {
					log.Warn("Error setting the reply ID of the new comment").Err(err).Send()
				}

				log.Info("Edited comment").
					Int("comment-id", cr.CommentView.Comment.ID).
					Send()
			} else {
				// Create a new comment replying to a post/comment
				cr, err := c.CreateComment(ctx, types.CreateComment{
					PostID:   reply.PostID,
					ParentID: reply.CommentID,
					Content:  reply.Content,
				})
				if err != nil {
					log.Warn("Error while trying to create new comment").Err(err).Send()
				}

				// Set the reply ID for the post/comment in the database
				// so that we know which comment ID to edit if we need to.
				err = rs.SetReplyID(ctx, store.SetReplyIDParams{
					ID:      int64(reply.CommentID.ValueOr(reply.PostID)),
					ReplyID: int64(cr.CommentView.Comment.ID),
				})
				if err != nil {
					log.Warn("Error setting the reply ID of the new comment").Err(err).Send()
				}

				log.Info("Created new comment").
					Int("post-id", reply.PostID).
					Int("parent-id", reply.CommentID.ValueOr(-1)).
					Int("comment-id", cr.CommentView.Comment.ID).
					Send()
			}
		case <-ctx.Done():
			return
		}
	}
}

func executeTmpl(tmpl *template.Template, tc TmplContext) (string, error) {
	sb := &strings.Builder{}
	err := tmpl.Execute(sb, tc)
	return sb.String(), err
}

// toSubmatches converts matches coming from PCRE2 to a
// submatch array used for the template
func toSubmatches(s [][]string) []Submatches {
	// Unfortunately, Go doesn't allow for this conversion
	// even though the memory layout is identical and it's
	// safe, so it is done using unsafe pointer magic
	return *(*[]Submatches)(unsafe.Pointer(&s))
}
