# Reusing a GOWA chatstorage database

The installed Mac binary is public upstream commit
`07c20ff1c682398cda9b6b5c4fc8be99b3b30fa8`. It prints `v9.9.0`. It is not this
fork. This fork's base is `4c4c4ed66d5130f1ef15ea2f92e331d1be8a8fbd`
(28 October 2026), its `AppVersion` is still `v9.7.0`, and its migration list
ends at 50. Do not treat this checkout's schema as the Mac file's schema.

`07c20ff` was read from public GitHub on 8 November 2026. Its parent is
`be8e3a252f3d091ec86effb23adc0e0c99c9a561`, which is tag `v9.9.0`. The only
file changed after that tag is `.agents/skills/update-release-note/SKILL.md`.
`AppVersion` is `v9.9.0`. `getMigrations` ends at 54, the same statements as
the tag. `go.mod` requires `go.mau.fi/whatsmeow v0.0.0-20261105212532-307a2edf117d`.
Public `main` compared equal to `07c20ff` on that date. A later main commit is
not this binary.

The database does not store the git revision. Schema 54 is also the v9.8.0
fingerprint. Preflight has to read the snapshot. A newer or unknown schema
blocks reuse. These tools do not guess.

## What was reported, and not copied here

At 02:25 IST the Mac `chatstorage.db` was 57,929,728 bytes. Its WAL was
4,260,112 bytes and its latest time was 02:04 IST. The session database was
5,746,688 bytes with a 4,124,152 byte WAL. Eight history JSON files totaled
38,606,270 bytes and were preserved, not parsed. The target history database
was 23,818,240 bytes and had an active WAL; that WAL's size was not recorded.
`DB_KEYS_URI` was absent from the LaunchAgent environment and arguments only.
Other runtime configuration was not verified, so the keys location is unknown,
not absent. No live file was copied or modified for this report.

## Feasibility limits

Real cutover stays blocked until the checks below are true on the files, not
on this note.

- Freeze three writers before the snapshot that will be installed: Mac GOWA,
  the Mac periodic merge writer, and target GOWA. A SQLite backup is
  transactionally consistent and can still fall in the middle of a
  multi-transaction merge. No `-shm` does not prove the periodic writer is
  stopped; it can open the database and exit. A freeze attestation while
  `-shm` exists is refused.
- Copy with `snapshot`, not by copying the `.db` and leaving the WAL behind.
  The WAL sizes above are large enough that a raw copy drops the newest commits.
- `preflight` must accept the snapshot. Versions outside 49–54, extra or
  missing history columns, a `whatsapp.db` basename, or a symlink path are
  refused. Schema 49–53 can be upgraded by official v9.9.0, but `gate` blocks
  cutover for the verified revision unless the file is schema 54. A running
  `07c20ff` process applies migrations through 54 at startup. Schema 54 still
  does not prove the binary.
- `gate` exits non-zero even when those mechanical checks pass. It does not
  open the session. A whatsmeow database newer than the v9.9.0 upgrade table
  makes official v9.9.0 refuse `whatsapp.db` at startup. These tools cannot
  see that compat version. They also cannot see a keys database when
  `DB_KEYS_URI` is unknown.
- `reconcile` does not merge. Target-only messages stay in the target
  snapshot. `overlay` writes a new file. `--base source` copies the source
  registry and adds mapped target rows. `--base target` copies the target
  registry, so an account that exists only on the target stays, and mapped
  source rows are added onto the target storage JID. It does not edit
  `devices`, `scheduled_sends`, or Chatwoot tables, and it does not rewrite
  `chat_jid` or `sender`. Genuine content conflicts, edit divergence,
  ambiguous chats, and unmapped incoming device ids are reported and the new
  file is not published. Provenance timestamps and empty-metadata fills do
  not block. The kept row is the base row.
- Account history identity is `devices.jid` when set, otherwise the tool
  refuses the row. A slot UUID is not a storage JID. Each side must confirm
  the declared phone from `ad_jid` independently. Equal JID strings are not
  enough.
- `coverage` reports which history-sync JSON message ids are already in the
  snapshot. It does not insert them. A conversation without `messages` is an
  empty conversation. A file whose top-level keys are known history-sync
  fields and which has no `conversations` array is metadata-only:
  `message_audit` is false, and zero absent ids is not coverage. Unknown
  keys, gzip, and SQLite fail closed. There is no old-bridge importer.
  `old_bridge_assessed` stays false. An archive whose ids are all present is
  still not complete history, and it does not authorize a purge.
- `media-refs` counts media columns on messages and message ids in history
  JSON. It does not walk a media tree, and it does not prove which files to
  copy. The completed inventory is 490,698 files, 129.21 GB logical and
  130.48 GB allocated, 33,266 directories, one symlink followed, aliases
  deduped by inode, and nothing missing. The box has 49.21 GB free and the
  Mac has 63.86 GB free. The whole tree is about 80 GB short of the box, and
  about 92 GB short if the allocated size plus a 10 GiB reserve is compared
  with the box. Nothing was copied or deleted. `messages.filename` is a
  display name, not a path. Do not run `materialize-media` on that tree.
- `history_complete` is false in every report these tools write. They cannot
  see messages that were never stored.
- Starting `whatsapp rest` connects saved sessions about two seconds after
  boot. That is the cutover, not a dry run. The offline binaries below have
  no `rest` command. Do not run the Mac and Linux processes on one cloned
  session; WhatsApp replaces the other connection.
- Migration 53, when a pre-54 file is first opened by v9.9.0, marks running
  or already-attempted schedules so they are not replayed. It does not
  rewrite messages or the registry.

## Offline binaries

Use the offline helper, not `go run . rest` and not a server binary, for the
staged checks. The helper is the same Go package built with `-tags purego`,
so it does not need CGO and does not link the WhatsApp client.

```sh
./db-reuse-linux-amd64 snapshot \
  --source /absolute/path/to/chatstorage.db \
  --out /absolute/path/to/work/chatstorage-snapshot.db \
  --allow-live-validation

./db-reuse-linux-amd64 preflight \
  --db /absolute/path/to/work/chatstorage-snapshot.db

./db-reuse-linux-amd64 reconcile \
  --source /absolute/path/to/work/mac-snapshot.db \
  --target /absolute/path/to/work/linux-snapshot.db \
  --work-dir /absolute/path/to/work/reconcile \
  --out /absolute/path/to/work/reconcile.json

./db-reuse-linux-amd64 materialize-media \
  --source /absolute/path/to/mac/statics/media \
  --dest /absolute/path/to/work/media \
  --out /absolute/path/to/work/media.json
```

`--allow-live-validation` is a non-cutover copy. It is refused together with
a freeze attestation. For the snapshot that might be installed, stop the
three writers, confirm no `-shm`, and pass:

```sh
--attest-writers source-gowa,source-periodic-merge,target-gowa
```

`gate` then reads that single-file snapshot:

```sh
./db-reuse-linux-amd64 gate \
  --db /absolute/path/to/work/mac-snapshot.db \
  --expect-revision 07c20ff1c682398cda9b6b5c4fc8be99b3b30fa8 \
  --attest-writers source-gowa,source-periodic-merge,target-gowa \
  --target /absolute/path/to/work/linux-snapshot.db \
  --work-dir /absolute/path/to/work/gate
```

A non-zero exit means do not cut over. `"mechanical_gates_passed": true`
only means the schema, revision, writer attestation, and target comparison
did not add a blocker. `"real_cutover_blocked": true` remains because the
keys location, the session file, and the archives are still open, and
`history_complete` stays false.

## Target-only messages

There is no GOWA command that merges two chatstorage files. `overlay` is a
staged copy for that gap. It snapshots both inputs and checks the schema.
The original files stay in place. `--base source` is the default: the new
file is the source snapshot plus mapped target-only rows. `--base target`
is the direction that keeps a second account which exists only on the
target. The new file is the target snapshot, and mapped source rows are
inserted with `device_id` rewritten to the target storage JID. Target
device ids that are not in the map stay on that copy. They are not an
error, and the map must not invent a source account for them. An incoming
device id that is not in the map still fails the command.

```sh
./db-reuse-linux-amd64 overlay \
  --base target \
  --source /absolute/path/to/work/mac-snapshot.db \
  --target /absolute/path/to/work/linux-snapshot.db \
  --map /absolute/path/to/work/accounts.json \
  --work-dir /absolute/path/to/work/overlay \
  --out /absolute/path/to/work/staged.db
```

`accounts.json` allows only these fields:

```json
{
  "accounts": [
    {
      "label": "line-a",
      "phone": "15550001000",
      "lid": "999001",
      "source_storage_jid": "999001@lid",
      "target_storage_jid": "15550001000@s.whatsapp.net"
    }
  ]
}
```

`lid` may be omitted. Each side's registry must contain exactly one row whose
storage id is the declared JID, and that row's `ad_jid` must confirm `phone`.
A declared `lid` must be one of the two storage JIDs. The inserted message's
`device_id` becomes the base storage JID. Chat JIDs are not rewritten.

Rows that already exist on both sides are classified by column. `created_at`
and `updated_at` stay as the base wrote them. A different `timestamp` with
the same content stays as the base wrote it, and both times are reported.
An empty base metadata column can be filled from the incoming row. Content,
sender, `is_from_me`, file hashes, and `media_key` differences are genuine
conflicts. If one side's content is an edit row's previous content and the
other side is that row's new content, the class is `edit_divergence`. Either
class is unresolved: the report names the columns and does not include the
text, the hash, or the key, and `--out` is not created. A message id stored
under two chats, or an incoming device id missing from the map, also leaves
`--out` uncreated. Chat name differences are reported and the base chat row
is kept.

`"applied": true` means the staged file was written. It does not mean the
file is ready to install. `cutover_ready` and `history_complete` stay false.

## Archive coverage

```sh
./db-reuse-linux-amd64 coverage \
  --db /absolute/path/to/work/mac-snapshot.db \
  --archive /absolute/path/to/history-one.json \
  --archive /absolute/path/to/history-two.json \
  --out /absolute/path/to/work/coverage.json
```

`present` means the snapshot already has that chat JID and message id.
`absent` means it does not. `other_chat` means the id exists under a
different chat. Conversation ids are read from `ID` or `id`. A conversation
that omits `messages` increments `empty_conversations` and does not fail the
file. `kind` `metadata_only` with `message_audit` false means the file had
no conversations array; status, push-name, and progress files are that kind.
None of those counts set `history_complete`. `old_bridge_assessed` stays
false. Do not purge a bridge, archive, or target snapshot from this report.

## Message-referenced media

```sh
./db-reuse-linux-amd64 media-refs \
  --db /absolute/path/to/work/mac-snapshot.db \
  --archive /absolute/path/to/history-one.json \
  --out /absolute/path/to/work/media-refs.json
```

The report's `subset_proven`, `whole_tree_copy_allowed`, `purge_allowed`,
and `history_complete` fields are false. `bytes_selected` is null. Passing
`--media-root` is an error; the command does not compare display names with
a directory. History staging does not wait on this manifest.

A later adapter can prove a subset only by showing, for each file it selects:
the account storage JID, chat JID, message id, a relative path that is not
`messages.filename`, the byte length, the sha256 of those bytes, the inode
and device, and the root the path is under. Until that evidence exists, an
unreferenced original is not identified and is not disposable.

## Files

| File | Role |
| --- | --- |
| `storages/chatstorage.db` | Chats, messages, reactions, edits, polls, registry, Chatwoot rows, and scheduled sends. |
| `storages/chatstorage.db-wal` and `-shm` | Uncheckpointed commits and the live-connection index. |
| `storages/whatsapp.db` | WhatsApp session. Not opened by these tools. |
| `DB_KEYS_URI` | Optional second session database. Unknown on the Mac, not recorded as empty. |
| `statics/media/` | Downloaded media. Message rows store a display name and a remote URL, not this path. The inventoried tree does not fit on the box. `media-refs` does not select a subset. `materialize-media` follows symlinks and writes regular files; do not point it at the 130 GB tree. |
| `storages/history-*.json` | A dump written during a live history-sync event. Startup does not read it back. |
