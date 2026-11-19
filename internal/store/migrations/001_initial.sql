CREATE TABLE configuration (
    singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
    account_id INTEGER UNIQUE CHECK (account_id > 0),
    revision INTEGER NOT NULL CHECK (revision >= 0)
) STRICT;

INSERT INTO configuration (singleton, revision) VALUES (1, 0);

CREATE TABLE publications (
    id TEXT PRIMARY KEY CHECK (id <> ''),
    state TEXT NOT NULL CHECK (state IN ('enabled', 'stopped')),
    interval_ns INTEGER NOT NULL CHECK (interval_ns > 0)
) STRICT;

CREATE TABLE publication_destinations (
    publication_id TEXT NOT NULL REFERENCES publications (id) ON DELETE CASCADE,
    chat_id TEXT NOT NULL CHECK (chat_id <> ''),
    position INTEGER NOT NULL CHECK (position >= 0),
    PRIMARY KEY (publication_id, chat_id),
    UNIQUE (publication_id, position)
) STRICT;

CREATE TABLE publication_variants (
    publication_id TEXT NOT NULL REFERENCES publications (id) ON DELETE CASCADE,
    id TEXT NOT NULL CHECK (id <> ''),
    position INTEGER NOT NULL CHECK (position >= 0),
    manual_text TEXT,
    source_account_id INTEGER REFERENCES configuration (account_id),
    source_message_id INTEGER,
    PRIMARY KEY (publication_id, id),
    UNIQUE (publication_id, position),
    CHECK (
        (manual_text IS NOT NULL AND manual_text <> ''
            AND source_account_id IS NULL AND source_message_id IS NULL)
        OR
        (manual_text IS NULL AND source_account_id IS NOT NULL
            AND source_message_id IS NOT NULL AND source_message_id > 0)
    )
) STRICT;

CREATE TABLE disabled_chats (
    chat_id TEXT PRIMARY KEY CHECK (chat_id <> '')
) STRICT;
