-- sqlc compiler projection for the optional current-platform social folder
-- tables. The social plugin owns their runtime lifecycle. Main reads them only
-- after CurrentFolderAccessState confirms that all three tables exist
-- in the authorized tenant search_path.

CREATE TABLE entity_folders (
    id INTEGER PRIMARY KEY,
    entity_type VARCHAR(32) NOT NULL
);

CREATE TABLE social_folder_items (
    id INTEGER PRIMARY KEY,
    folder_id INTEGER NOT NULL,
    entity VARCHAR(32) NOT NULL,
    entity_id INTEGER NOT NULL
);

CREATE TABLE folder_access_overrides (
    id INTEGER PRIMARY KEY,
    folder_id INTEGER NOT NULL,
    user_id INTEGER NOT NULL,
    access_level VARCHAR(16) NOT NULL
);
