-- Mod pages whose code-project link was decided — linked automatically, linked
-- or unlinked by hand: the auto-linker (store.AutoLink) leaves them alone and
-- offers no suggestion for them.
CREATE TABLE project_link_decisions (
    mod_project_id INTEGER PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    decided_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
