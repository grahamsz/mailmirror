// File overview: Durable asynchronous SMTP outbox state and optimistic Sent-message ownership.

package store

func userOutboxMigrationSet() migrationSet {
	return migrationSet{
		Scope:   "user",
		Version: UserSchemaVersion029,
		Label:   "user schema 029 asynchronous outbox",
		Statements: []string{
			`ALTER TABLE messages ADD COLUMN outbox_job_id INTEGER NOT NULL DEFAULT 0`,
			`CREATE TABLE IF NOT EXISTS outbox_jobs (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				submission_key TEXT NOT NULL,
				smtp_account_id INTEGER NOT NULL,
				imap_account_id INTEGER NOT NULL REFERENCES mail_accounts(id) ON DELETE RESTRICT,
				sent_mailbox_id INTEGER NOT NULL REFERENCES mailboxes(id) ON DELETE RESTRICT,
				optimistic_message_id INTEGER NOT NULL DEFAULT 0,
				envelope_from TEXT NOT NULL,
				recipients_json TEXT NOT NULL,
				message_id_header TEXT NOT NULL,
				blob_id INTEGER NOT NULL REFERENCES blobs(id) ON DELETE RESTRICT,
				blob_path TEXT NOT NULL,
				raw_sha256 TEXT NOT NULL,
				raw_size INTEGER NOT NULL,
				delivery_state TEXT NOT NULL DEFAULT 'preparing',
				filing_state TEXT NOT NULL DEFAULT 'not_started',
				attempt_count INTEGER NOT NULL DEFAULT 0,
				filing_attempt_count INTEGER NOT NULL DEFAULT 0,
				next_attempt_at INTEGER NOT NULL DEFAULT 0,
				lease_owner TEXT NOT NULL DEFAULT '',
				lease_expires_at INTEGER NOT NULL DEFAULT 0,
				append_uid_validity INTEGER NOT NULL DEFAULT 0,
				append_uid_next INTEGER NOT NULL DEFAULT 0,
				appended_uid INTEGER NOT NULL DEFAULT 0,
				appended_uid_validity INTEGER NOT NULL DEFAULT 0,
				last_error_kind TEXT NOT NULL DEFAULT '',
				last_error TEXT NOT NULL DEFAULT '',
				attention_at INTEGER NOT NULL DEFAULT 0,
				acknowledged_at INTEGER NOT NULL DEFAULT 0,
				smtp_accepted_at INTEGER NOT NULL DEFAULT 0,
				completed_at INTEGER NOT NULL DEFAULT 0,
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL,
				UNIQUE(user_id, submission_key)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_outbox_jobs_user_due
				ON outbox_jobs(user_id, next_attempt_at, id)`,
			`CREATE INDEX IF NOT EXISTS idx_outbox_jobs_user_state
				ON outbox_jobs(user_id, delivery_state, filing_state, updated_at DESC)`,
			`CREATE INDEX IF NOT EXISTS idx_outbox_jobs_user_message
				ON outbox_jobs(user_id, optimistic_message_id)`,
			`CREATE INDEX IF NOT EXISTS idx_outbox_jobs_user_completed
				ON outbox_jobs(user_id, completed_at) WHERE completed_at > 0`,
			`CREATE INDEX IF NOT EXISTS idx_messages_user_outbox
				ON messages(user_id, outbox_job_id) WHERE outbox_job_id > 0`,
			`CREATE TABLE IF NOT EXISTS outbox_attempts (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
				outbox_job_id INTEGER NOT NULL REFERENCES outbox_jobs(id) ON DELETE CASCADE,
				attempt INTEGER NOT NULL,
				phase TEXT NOT NULL,
				outcome TEXT NOT NULL,
				error_kind TEXT NOT NULL DEFAULT '',
				error_text TEXT NOT NULL DEFAULT '',
				started_at INTEGER NOT NULL,
				finished_at INTEGER NOT NULL,
				UNIQUE(user_id, outbox_job_id, attempt, phase)
			)`,
			`CREATE INDEX IF NOT EXISTS idx_outbox_attempts_user_job
				ON outbox_attempts(user_id, outbox_job_id, id DESC)`,
		},
	}
}
