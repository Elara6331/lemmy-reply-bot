/* name: ItemExists :one */
SELECT COUNT(1) FROM replied_items WHERE item_type = ? AND id = ? AND updated_time = ?;

/* name: AddItem :exec */
INSERT OR REPLACE INTO replied_items (id, item_type, updated_time) VALUES (?, ?, ?);