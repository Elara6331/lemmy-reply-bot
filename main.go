package main

import (
	"context"
	"database/sql"
	_ "embed"
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

//go:embed sql/schema.sql
var schema string

func init() {
	db, err := sql.Open("sqlite", "replied.db")
	if err != nil {
		log.Fatal("Error opening database during init").Err(err).Send()
	}
	_, err = db.Exec(schema)
	if err != nil {
		log.Fatal("Error initializing database").Err(err).Send()
	}
	err = db.Close()
	if err != nil {
		log.Fatal("Error closing database after init").Err(err).Send()
	}
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
		go commentReplyWorker(ctx, c, replyCh)
	}

	commentWorker(ctx, c, cfg, replyCh)
}

func commentWorker(ctx context.Context, c *lemmy.Client, cfg Config, replyCh chan<- replyJob) {
	db, err := sql.Open("sqlite", "replied.db")
	if err != nil {
		log.Fatal("Error opening reply database").Err(err).Send()
	}
	rs := store.New(db)

	for {
		select {
		case <-time.After(15 * time.Second):
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

				// If the item we're checking for already exists, we've already replied, so skip it
				if c, err := rs.ItemExists(ctx, store.ItemExistsParams{
					ID:          int64(c.Comment.ID),
					ItemType:    store.Comment,
					UpdatedTime: c.Comment.Updated.Unix(),
				}); c > 0 && err == nil {
					continue
				} else if err != nil {
					log.Warn("Error checking if item exists").Err(err).Send()
					continue
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

				// If the item we're checking for already exists, we've already replied, so skip it
				if c, err := rs.ItemExists(ctx, store.ItemExistsParams{
					ID:          int64(p.Post.ID),
					ItemType:    store.Post,
					UpdatedTime: p.Post.Updated.Unix(),
				}); c > 0 && err == nil {
					continue
				} else if err != nil {
					log.Warn("Error checking if item exists").Err(err).Send()
					continue
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
			err = db.Close()
			if err != nil {
				log.Warn("Error closing database").Err(err).Send()
				continue
			}
			return
		}
	}
}

type replyJob struct {
	Content   string
	CommentID types.Optional[int]
	PostID    int
}

func commentReplyWorker(ctx context.Context, c *lemmy.Client, ch <-chan replyJob) {
	for {
		select {
		case reply := <-ch:
			cr, err := c.CreateComment(ctx, types.CreateComment{
				PostID:   reply.PostID,
				ParentID: reply.CommentID,
				Content:  reply.Content,
			})
			if err != nil {
				log.Warn("Error while trying to create new comment").Err(err).Send()
			}

			log.Info("Created new comment").
				Int("post-id", reply.PostID).
				Int("parent-id", reply.CommentID.ValueOr(-1)).
				Int("comment-id", cr.CommentView.Comment.ID).
				Send()
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
