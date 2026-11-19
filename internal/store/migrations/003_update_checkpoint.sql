CREATE TABLE update_checkpoint (
    account_id INTEGER PRIMARY KEY REFERENCES configuration (account_id),
    pts INTEGER NOT NULL,
    qts INTEGER NOT NULL,
    date INTEGER NOT NULL CHECK (date > 0),
    seq INTEGER NOT NULL
) STRICT;

CREATE INDEX delivery_attempts_unresolved ON delivery_attempts (publication_id, chat_id)
    WHERE outcome IN ('pending', 'unknown');
