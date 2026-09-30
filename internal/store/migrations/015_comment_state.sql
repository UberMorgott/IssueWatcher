-- Comment threads (kind 'comment') behave like comments, not issues:
-- status 'open' = waiting for the owner's answer; 'closed' = answered (the last
-- reply is the owner's), resolved, or hidden. resolved is a local «Решено» flag
-- (never sent to the platform); hidden marks a thread only the owner wrote (an
-- own Steam comment), kept for sync but left out of lists and counts.
ALTER TABLE items ADD COLUMN resolved INTEGER NOT NULL DEFAULT 0;
ALTER TABLE items ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0;

UPDATE items SET hidden = 1
WHERE kind = 'comment' AND author <> ''
  AND author = (SELECT account FROM sources WHERE id = items.source_id)
  AND NOT EXISTS (SELECT 1 FROM comments c WHERE c.item_id = items.id AND c.author <> items.author);

UPDATE items SET status = 'closed'
WHERE kind = 'comment' AND status = 'open' AND (hidden = 1 OR
  coalesce((SELECT c.author FROM comments c WHERE c.item_id = items.id ORDER BY c.created_at DESC, c.id DESC LIMIT 1), items.author)
    = (SELECT nullif(account, '') FROM sources WHERE id = items.source_id));

-- A closed item is never unread (012_closed_read).
UPDATE items SET unread = 0 WHERE status = 'closed' AND unread = 1;
