# Reusing a GOWA chatstorage database on official v9.9.0

`storages/chatstorage.db` can be opened by official Linux GOWA v9.9.0. Copy a
SQLite snapshot of that file. Do not import its rows into a second database.
This repository has no chat-history import command; startup schema upgrade is
the supported restore path.

The commands below only read or copy files. They do not connect to WhatsApp,
send, mark chats read, open `whatsapp.db`, delete rows, or replace a live
database.

## What the files are

| File | Role |
| --- | --- |
| `storages/chatstorage.db` | Chats, messages, reactions, edits, polls, the device registry, Chatwoot rows, and scheduled sends. WAL mode is the default. |
| `storages/chatstorage.db-wal` and `-shm` | Uncheckpointed commits and the live-connection index. A copy of the `.db` file alone drops those commits. |
| `storages/whatsapp.db` | WhatsApp session: identity keys, contacts, LID map, and app state. Default `DB_URI`. |
| `DB_KEYS_URI` | Optional second session database. Empty means the keys stay in `whatsapp.db`. |
| `statics/media/` and `storages/<timestamp>-<uuid>*` | Downloaded media. Message rows store a display filename and the WhatsApp media URL, not this local path. |
| `storages/history-*.json` | A dump written while a live history-sync event is handled. Startup does not read these files back. |

`InitializeSchema` applies only migrations newer than `schema_info`. On this
fork that list ends at migration 50. Upstream v9.9.0 has the same migrations
1–50 and appends 51–54, all of which alter `scheduled_sends` only. Migration 53
marks schedules that are `running` or already have attempts so they are not
replayed. It does not rewrite messages, chats, reactions, edits, polls, or
`devices`.

A database at schema 54 is already the v9.8.0 and v9.9.0 shape, so v9.9.0
opens it without further changes. Schema 54 does not by itself prove the
binary was v9.9.0 rather than v9.8.0. `AppVersion` is not stored in the
database.

History is partitioned by the registry storage id: `devices.jid` when that
column is set, otherwise `devices.device_id`. Message rows use that same id
(`Store.ID` with the companion suffix removed). A Mac database whose message
`device_id` values match those storage ids does not need its message keys
rewritten. `scheduled_sends.device_id` is the slot name (`devices.device_id`),
not the history id.

## Which session to keep

Adopting the Mac `chatstorage.db` does not require adopting the Mac session.

**Adopt the Mac session.** Stop the Mac process first. Copy `whatsapp.db` and,
if `DB_KEYS_URI` is set, that file too. The registry `ad_jid`
(`number:device@s.whatsapp.net`) matches the session, so boot attaches them.
Do not start the Mac process again. Two processes with the same session
replace each other (`STREAM_REPLACED`). `whatsapp rest` connects saved
sessions about two seconds after boot, so starting it is the cutover, not a
dry run.

The whatsmeow upgrade scripts in this fork (15 October 2026) and in upstream
v9.9.0 (5 November 2026) are the same set. Either of those builds can open the
other's `whatsapp.db`. A newer custom build can make v9.9.0 refuse the file
with an unsupported-database-version error. That check happens inside
whatsmeow at startup; these commands do not open the session file.

**Keep the Linux session.** This keeps the Linux pairing, and it is a
reasonable choice. It is not automatic. On boot the Mac slot already claims
the phone number with a different `ad_jid`, and the loader leaves the Linux
companion unreferenced instead of attaching it. History remains on the Mac
slot. The existing backfill runs only when `ad_jid` is empty. Clearing it is a
registry edit, not a message import, and it is the wrong edit when the Linux
storage JID differs from `devices.jid` (for example a LID versus a phone JID):
the slot would then query a different history partition. Compare the two JIDs
before changing anything. This tool does not clear `ad_jid`.

## Target-only rows and older archives

Replacing the Linux file with the Mac snapshot removes Linux-only rows from
the running database. Snapshot the Linux file first and keep it. `reconcile`
lists message ids present on only one side, including newer Linux rows, and
does not merge or delete them. They stay in the Linux snapshot.

`history-*.json` archives are not part of either database. Reusing
`chatstorage.db` does not load them. Nothing here can claim they are included.

Contact addresses and group participant lists live in `whatsapp.db`. Chat
names and group message rows live in `chatstorage.db` and come across with
that file. Media bytes do not. Copy them with `materialize-media`, which
follows symlinks such as `media/_mac` and writes regular files. A dangling
symlink is reported and the command exits non-zero. Destination files with
different bytes are left untouched.

## Commands

Run these from `src/` with this fork's binary. They skip WhatsApp startup.

```sh
go run . db-reuse snapshot \
  --source /absolute/path/to/mac/storages/chatstorage.db \
  --out /absolute/path/to/work/mac-chatstorage-snapshot.db

go run . db-reuse preflight \
  --db /absolute/path/to/work/mac-chatstorage-snapshot.db

go run . db-reuse snapshot \
  --source /absolute/path/to/linux/storages/chatstorage.db \
  --out /absolute/path/to/work/linux-chatstorage-backup.db

go run . db-reuse reconcile \
  --source /absolute/path/to/work/mac-chatstorage-snapshot.db \
  --target /absolute/path/to/work/linux-chatstorage-backup.db \
  --work-dir /absolute/path/to/work/reconcile \
  --out /absolute/path/to/work/reconcile.json

go run . db-reuse materialize-media \
  --source /absolute/path/to/mac/statics/media \
  --dest /absolute/path/to/work/media \
  --out /absolute/path/to/work/media.json
```

Leave the Mac `-wal` beside `chatstorage.db` for `snapshot`. Do not copy the
`.db` and `-wal` yourself. `preflight` must report `"reusable": true`. Schema
50 reports `"mode": "official-v9.9.0-upgrade"`. Schema 54 reports
`"mode": "unchanged"`. `"history_complete"` is false on every successful
report.

Stop the Mac process before installing files on Linux. If `-shm` exists, a
process still has the database open. Install the snapshot as
`storages/chatstorage.db` with no `-wal` or `-shm` next to it. Install the
materialized media under the Linux `statics/media` tree. Then choose one
session, as above, and start official v9.9.0 only after the Mac process is
stopped.

## Metadata still required from the Mac build

This fork is upstream commit `4c4c4ed` (28 October 2026). Its `AppVersion` is
`v9.7.0` and its schema stops at 50. It is not the v9.9.0 tag `be8e3a2`
(6 November 2026). No Mac build manifest is in this tree. Before the cutover,
record:

- `whatsapp version`
- `go version -m ./whatsapp`, in particular `vcs.revision` and the whatsmeow
  version
- the preflight JSON (`schema_version`, `mode`, registry `jid` and `ad_jid`)
- sizes of `chatstorage.db`, `-wal`, and `-shm`
- whether `DB_KEYS_URI` is set
- sizes and SHA-256 of any `history-*.json` archives

Do not record session keys, webhook secrets, or API tokens. Preflight does not
select those columns.
