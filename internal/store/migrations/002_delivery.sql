ALTER TABLE publications ADD COLUMN pause_reason TEXT NOT NULL DEFAULT '';

CREATE TABLE delivery_pairs (
    publication_id TEXT NOT NULL REFERENCES publications (id) ON DELETE CASCADE,
    chat_id TEXT NOT NULL,
    first_due_ns INTEGER NOT NULL,
    confirmed_ns INTEGER,
    confirmed_variant TEXT,
    confirmed_attempt INTEGER,
    PRIMARY KEY (publication_id, chat_id)
) STRICT;

CREATE TABLE delivery_attempts (
    id INTEGER PRIMARY KEY,
    random_id INTEGER NOT NULL UNIQUE CHECK (random_id <> 0),
    publication_id TEXT NOT NULL,
    chat_id TEXT NOT NULL,
    variant_id TEXT NOT NULL,
    submitted_ns INTEGER NOT NULL,
    outcome TEXT NOT NULL CHECK (outcome IN ('pending', 'success', 'failure', 'server_wait', 'unknown')),
    completed_ns INTEGER,
    telegram_message_id INTEGER,
    reason TEXT NOT NULL DEFAULT '',
    FOREIGN KEY (publication_id, chat_id) REFERENCES delivery_pairs (publication_id, chat_id) ON DELETE CASCADE
) STRICT;

CREATE INDEX delivery_attempts_pair ON delivery_attempts (publication_id, chat_id, id DESC);

CREATE TABLE delivery_waits (
    chat_id TEXT PRIMARY KEY,
    until_ns INTEGER NOT NULL
) STRICT;
