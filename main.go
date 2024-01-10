package main

import (
	"context"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/pflag"
	"go.elara.ws/go-lemmy"
	"go.elara.ws/lemmy-reply-bot/internal/db"
	"go.elara.ws/logger"
	"go.elara.ws/logger/log"
	"go.elara.ws/salix"
)

func init() {
	log.Logger = logger.NewPretty(os.Stderr)
}

func main() {
	cfgPath := pflag.StringP("config-path", "c", "/etc/lemmy-reply-bot/config.toml", "Path to the config file")
	dbPath := pflag.StringP("db-path", "d", "/etc/lemmy-reply-bot/replies", "Path to the ChaiSQL database")
	pflag.Parse()

	ctx := context.Background()
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	err := db.Init(*dbPath)
	if err != nil {
		log.Fatal("Error initializing database").Err(err).Send()
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatal("Error loading config").Err(err).Send()
	}

	c, err := lemmy.New(cfg.File.Lemmy.InstanceURL)
	if err != nil {
		log.Fatal("Error creating new lemmy client").Err(err).Send()
	}

	err = c.ClientLogin(ctx, lemmy.Login{
		UsernameOrEmail: cfg.File.Lemmy.Account.UserOrEmail,
		Password:        cfg.File.Lemmy.Account.Password,
	})
	if err != nil {
		log.Fatal("Error logging into lemmy").Err(err).Send()
	}

	log.Info("Successfully logged in!").Send()

	go poll(ctx, cfg, c)

	<-ctx.Done()
	_ = db.Close()
}

func poll(ctx context.Context, cfg Config, c *lemmy.Client) {
	for {
		select {
		case <-time.After(cfg.PollInterval):
			// Get 20 of the newest comments from Lemmy
			comments, err := c.Comments(ctx, lemmy.GetComments{
				Type:  lemmy.NewOptional(lemmy.ListingTypeLocal),
				Sort:  lemmy.NewOptional(lemmy.CommentSortTypeNew),
				Limit: lemmy.NewOptional[int64](20),
			})
			if err != nil {
				log.Warn("Error getting comments").Err(err).Send()
				continue
			}

			handleComments(ctx, comments.Comments, cfg, c)

			// Get 20 of the newest comments from Lemmy
			posts, err := c.Posts(ctx, lemmy.GetPosts{
				Type:  lemmy.NewOptional(lemmy.ListingTypeLocal),
				Sort:  lemmy.NewOptional(lemmy.SortTypeNew),
				Limit: lemmy.NewOptional[int64](20),
			})
			if err != nil {
				log.Warn("Error getting posts").Err(err).Send()
				continue
			}

			handlePosts(ctx, posts.Posts, cfg, c)
		case <-ctx.Done():
			return
		}
	}
}

func handleComments(ctx context.Context, comments []lemmy.CommentView, cfg Config, c *lemmy.Client) {
	for _, comment := range comments {
		if !comment.Community.Local {
			continue
		}

		item, err := db.GetItem(comment.Comment.ID, db.Comment)
		if err != nil {
			log.Warn("Error getting comment from db").Err(err).Send()
			continue
		}

		edit := false
		if item == nil {
			// If the item is nil, it doesn't exist, which means we need to
			// create a new reply, so we don't set edit to true in this case.
		} else if item.Updated.Equal(comment.Comment.Updated) {
			// If the item exists but hasn't been edited since we've last seen it,
			// we can skip it since we've already replied to it.
			continue
		} else if item.Updated.Before(comment.Comment.Updated) {
			// If the item exists and has been edited since we've last seen it,
			// we need to edit it, so we set edit to true.
			edit = true
		}

		for i, reply := range cfg.File.Replies {
			re := cfg.Regexes[reply.Regex]
			if !re.MatchString(comment.Comment.Content) {
				continue
			}

			log.Info("Matched comment body").
				Int("reply-index", i).
				Int64("comment-id", comment.Comment.ID).
				Send()

			matches := re.FindAllStringSubmatch(comment.Comment.Content, -1)
			content, err := executeTmpl(cfg.Tmpls, reply.Regex, map[string]any{
				"id":      comment.Comment.ID,
				"type":    db.Comment,
				"matches": matches,
			})
			if err != nil {
				log.Warn("Error executing template").Int("index", i).Err(err).Send()
				continue
			}

			if edit {
				_, err = c.EditComment(ctx, lemmy.EditComment{
					CommentID: item.ReplyID,
					Content:   lemmy.NewOptional(content),
				})
				if err != nil {
					log.Warn("Error editing comment").Int64("id", item.ReplyID).Err(err).Send()
					continue
				}
				
				log.Info("Edited comment").Int64("parent-id", item.ID).Int64("reply-id", item.ReplyID).Send()				

				err = db.SetUpdatedTime(comment.Comment.ID, db.Comment, comment.Comment.Updated)
				if err != nil {
					log.Warn("Error setting new updated time").Int64("id", item.ReplyID).Err(err).Send()
					continue
				}
			} else {
				cr, err := c.CreateComment(ctx, lemmy.CreateComment{
					PostID:   comment.Comment.PostID,
					Content:  content,
					ParentID: lemmy.NewOptional(comment.Comment.ID),
				})
				if err != nil {
					log.Warn("Error creating reply").Int64("comment-id", comment.Comment.ID).Err(err).Send()
					continue
				}
				
				log.Info("Created comment").Int64("parent-id", comment.Comment.ID).Int64("reply-id", cr.CommentView.Comment.ID).Send()
				
				err = db.AddItem(db.Item{
					ID:       comment.Comment.ID,
					ReplyID:  cr.CommentView.Comment.ID,
					ItemType: db.Comment,
					Updated:  comment.Comment.Updated,
				})
				if err != nil {
					log.Warn("Error adding reply to database").Int64("id", item.ReplyID).Err(err).Send()
					continue
				}
			}
		}
	}
}

func handlePosts(ctx context.Context, posts []lemmy.PostView, cfg Config, c *lemmy.Client) {
	for _, post := range posts {
		if !post.Community.Local {
			continue
		}

		item, err := db.GetItem(post.Post.ID, db.Post)
		if err != nil {
			log.Warn("Error getting comment from db").Err(err).Send()
			continue
		}

		edit := false
		if item == nil {
			// If the item is nil, it doesn't exist, which means we need to
			// reply to it, so we don't set edit to true in this case.
		} else if item.Updated.Equal(post.Post.Updated) {
			// If the item exists but hasn't been edited since we've last seen it,
			// we can skip it since we've already replied to it.
			continue
		} else if item.Updated.Before(post.Post.Updated) {
			// If the item exists and has been edited since we've last seen it,
			// we need to edit it, so we set edit to true.
			edit = true
		}

		for i, reply := range cfg.File.Replies {
			re := cfg.Regexes[reply.Regex]
			content := post.Post.URL.ValueOrZero() + "\n\n" + post.Post.Body.ValueOrZero()
			if !re.MatchString(content) {
				continue
			}

			log.Info("Matched post body").
				Int("reply-index", i).
				Int64("post-id", post.Post.ID).
				Send()

			matches := re.FindAllStringSubmatch(content, -1)
			content, err := executeTmpl(cfg.Tmpls, reply.Regex, map[string]any{
				"id":      post.Post.ID,
				"type":    db.Post,
				"matches": matches,
			})
			if err != nil {
				log.Warn("Error executing template").Int("index", i).Err(err).Send()
				continue
			}

			if edit {
				_, err = c.EditComment(ctx, lemmy.EditComment{
					CommentID: item.ReplyID,
					Content:   lemmy.NewOptional(content),
				})
				if err != nil {
					log.Warn("Error editing post").Int64("id", item.ReplyID).Err(err).Send()
					continue
				}
				
				log.Info("Edited comment").Int64("post-id", item.ID).Int64("reply-id", item.ReplyID).Send()

				err = db.SetUpdatedTime(post.Post.ID, db.Post, post.Post.Updated)
				if err != nil {
					log.Warn("Error setting new updated time").Int64("id", item.ReplyID).Err(err).Send()
					continue
				}
			} else {
				cr, err := c.CreateComment(ctx, lemmy.CreateComment{
					PostID:  post.Post.ID,
					Content: content,
				})
				if err != nil {
					log.Warn("Error creating reply").Int64("post-id", post.Post.ID).Err(err).Send()
					continue
				}
				
				log.Info("Created comment").Int64("post-id", post.Post.ID).Int64("reply-id", cr.CommentView.Comment.ID).Send()

				err = db.AddItem(db.Item{
					ID:       post.Post.ID,
					ReplyID:  cr.CommentView.Comment.ID,
					ItemType: db.Post,
					Updated:  post.Post.Updated,
				})
				if err != nil {
					log.Warn("Error adding reply to database").Int64("id", item.ReplyID).Err(err).Send()
					continue
				}
			}
		}
	}
}

func executeTmpl(ns *salix.Namespace, name string, vars map[string]any) (string, error) {
	sb := &strings.Builder{}
	err := ns.ExecuteTemplate(sb, name, vars)
	return sb.String(), err
}
