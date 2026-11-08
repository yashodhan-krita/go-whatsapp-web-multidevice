// Package dbreuse snapshots and checks a GOWA chatstorage database offline.
//
// The commands do not connect to WhatsApp, open whatsapp.db, edit the device
// registry, schedules, or Chatwoot rows, or delete history. Snapshot uses the
// SQLite online backup API so an uncheckpointed WAL is folded into one new
// file. Preflight checks the on-disk schema. Gate blocks a real cutover unless
// that schema matches the verified installed revision and the source writers
// were attested frozen. Overlay writes target-only rows into a new copy. With
// --base target the copy is the target registry, and mapped incoming history
// is added onto it. import-archive inserts absent history JSON and can fill
// empty content it previously stored. audit-verify checks that retention
// without printing message bodies and can append a missing source original to
// the private audit. legacy-schema reports a non-session SQLite catalog and
// does not read rows. legacy-coverage and legacy-import read a declared bridge
// file whose hash matches the map; import appends missing messages only.
// media-refs counts message media fields and
// does not select local bytes.
package dbreuse
