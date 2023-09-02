/* name: GetItem :one */
SELECT * FROM replied_items WHERE item_type = ? AND id = ? LIMIT 1;

/* name: AddItem :exec */
INSERT OR REPLACE INTO replied_items (id, reply_id, item_type, updated_time) VALUES (?, -1, ?, ?);

/* name: SetReplyID :exec */
UPDATE replied_items SET reply_id = ? WHERE id = ?;