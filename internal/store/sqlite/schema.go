package sqlite

const schemaSQL = `
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS kv (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS memory (
  name TEXT PRIMARY KEY,
  content TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS skills (
  name TEXT PRIMARY KEY,
  description TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS chats (
  id TEXT PRIMARY KEY,
  meta_json TEXT NOT NULL,
  updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS messages (
  chat_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  turn_json TEXT NOT NULL,
  PRIMARY KEY (chat_id, seq)
);

CREATE TABLE IF NOT EXISTS blobs (
  kind TEXT NOT NULL,
  key TEXT NOT NULL,
  file_id TEXT NOT NULL DEFAULT '',
  message_id INTEGER NOT NULL DEFAULT 0,
  hash TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0,
  meta_json TEXT NOT NULL DEFAULT '{}',
  updated_at TEXT NOT NULL,
  PRIMARY KEY (kind, key)
);

CREATE TABLE IF NOT EXISTS cloud_meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`
