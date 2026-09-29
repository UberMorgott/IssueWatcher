-- A closed item is never unread: mark every item closed before this rule read.
UPDATE items SET unread = 0 WHERE status = 'closed' AND unread = 1;
