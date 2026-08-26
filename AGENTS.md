# AGENTS.md

## Project Notes

Rolltop V1 is a Go, SQLite, Bleve, and local-blob email mirror. Keep all user-owned data scoped by `user_id` at every layer: SQLite rows, blob paths, search documents, sync runs, and HTTP reads.

## Rules For Future Agents

- Do not add SMTP, remote delete, move, or archive behavior.
- Read-state sync is intentionally allowed to update only the IMAP `\Seen` flag.
- Do not accept `user_id` from normal browser routes.
- Admin routes may manage local users, but must not expose other users' mail.
- Do not log app passwords, IMAP passwords, session tokens, or raw message bodies.
- Keep IMAP credentials encrypted with `ROLLTOP_MASTER_KEY`.
- Keep tests for tenant isolation current when changing sync, search, message, attachment, blob, or route behavior.
- Keep sync incremental: fetch by UID after each mailbox's last stored UID, stream messages into storage, and update `sync_runs` progress during long runs.
- New attachment bodies should be indexed from raw `.eml` data and then discarded, not saved as separate attachment blobs.

## Checks

Run before handing off:

```sh
go test ./...
npm test
```

`npm test` covers the frontend offline modules (search ranking, IndexedDB
caches, outbox policy) via vitest; keep those suites current when changing
anything under `frontend/src/lib/offline*`.
