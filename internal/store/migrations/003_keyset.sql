-- Keyset pagination (infinite scroll): items newest update first with an id
-- tiebreak, comments oldest first per item.

DROP INDEX items_updated;
CREATE INDEX items_updated_id ON items(updated_at, id);
CREATE INDEX comments_item_created ON comments(item_id, created_at, id);
