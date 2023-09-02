CREATE TABLE IF NOT EXISTS replied_items (
  id           INT  NOT NULL PRIMARY KEY,
  reply_id     INT  NOT NULL,
  item_type    TEXT NOT NULL CHECK( item_type IN ('p', 'c') ),
  updated_time INT  NOT NULL,
  UNIQUE(id, item_type)
);