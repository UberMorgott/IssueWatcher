-- Mod pages (CurseForge, Nexus, Steam projects) linked to the code project
-- (a GitHub repo with a mapped folder) whose code they ship: fix jobs of a mod
-- page's items run in the code project's folder. One code project per mod page.
CREATE TABLE project_links (
    mod_project_id  INTEGER PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    code_project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    created_at      TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    CHECK (mod_project_id <> code_project_id)
);
CREATE INDEX project_links_code ON project_links(code_project_id);
