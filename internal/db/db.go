package db

import (
	"time"

	"github.com/chaisql/chai"
)

var db *chai.DB

// Init opens the database and applies migrations
func Init(path string) error {
	g, err := chai.Open(path)
	if err != nil {
		return err
	}
	db = g
	return db.Exec(`
		CREATE TABLE IF NOT EXISTS replied_items (
			id        INT       NOT NULL PRIMARY KEY,
			reply_id  INT       NOT NULL,
			item_type TEXT      NOT NULL CHECK ( item_type IN ['post', 'comment'] ),
			updated   TIMESTAMP NOT NULL,
			UNIQUE(id, item_type)
		)
	`)
}

func Close() error {
	return db.Close()
}

type ItemType string

const (
	Post    ItemType = "post"
	Comment ItemType = "comment"
)

type Item struct {
	ID       int64     `chai:"id"`
	ReplyID  int64     `chai:"reply_id"`
	ItemType ItemType  `chai:"item_type"`
	Updated  time.Time `chai:"updated"`
}

func AddItem(i Item) error {
	return db.Exec(`INSERT INTO replied_items VALUES ?`, &i)
}

func SetUpdatedTime(id int64, itemType ItemType, updated time.Time) error {
	return db.Exec(`UPDATE replied_items SET updated = ? WHERE id = ? AND item_type = ?`, updated, id, itemType)	
}

func GetItem(id int64, itemType ItemType) (*Item, error) {
	row, err := db.QueryRow(`SELECT * FROM replied_items WHERE id = ? AND item_type = ?`, id, itemType)
	if chai.IsNotFoundError(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	} else {
		out := &Item{}
		return out, row.StructScan(out)
	}
}
