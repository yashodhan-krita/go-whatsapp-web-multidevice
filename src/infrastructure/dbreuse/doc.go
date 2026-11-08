// Package dbreuse snapshots and checks a GOWA chatstorage database so another
// host can open that file with the official v9.9.0 binary.
//
// It does not import rows into a second schema, open whatsapp.db, connect to
// WhatsApp, or delete history. Snapshot uses the SQLite online backup API so an
// active WAL is folded into one file. Preflight checks the schema version and
// that history rows use the registry storage id. Reconcile reports rows that
// exist on only one side. Media materialize copies dereferenced symlink bytes.
package dbreuse
