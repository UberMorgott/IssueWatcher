-- What a mod page tells the auto-linker besides its name: its game when the
-- URL does not name it (Steam "steam:<appid>") and the GitHub repository its
-- own text links to.
ALTER TABLE projects ADD COLUMN game TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN code_url TEXT NOT NULL DEFAULT '';
