// File overview: Database records for user-scoped blob metadata.

package store

import "context"

// CreateBlob records blob metadata in the user database after the file has been written to the user blob directory.
func (s *Store) CreateBlob(ctx context.Context, b BlobRecord) (BlobRecord, error) {
	ts := nowUnix()
	_, err := s.mustDataDB(ctx, b.UserID).ExecContext(ctx, `INSERT INTO blobs (user_id, kind, path, sha256, size, created_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(user_id, path) DO UPDATE SET
			kind = excluded.kind,
			sha256 = excluded.sha256,
			size = excluded.size,
			created_at = excluded.created_at`,
		b.UserID, b.Kind, b.Path, b.SHA256, b.Size, ts)
	if err != nil {
		return BlobRecord{}, err
	}
	return s.GetBlobByPathForUser(ctx, b.UserID, b.Path)
}

// GetBlobForUser loads blob metadata by ID only when it belongs to the requested user.
func (s *Store) GetBlobForUser(ctx context.Context, userID, id int64) (BlobRecord, error) {
	var b BlobRecord
	var created int64
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT id, user_id, kind, path, sha256, size, created_at FROM blobs WHERE user_id = ? AND id = ?`, userID, id).
		Scan(&b.ID, &b.UserID, &b.Kind, &b.Path, &b.SHA256, &b.Size, &created)
	b.CreatedAt = unixTime(created)
	return b, err
}

// GetBlobByPathForUser loads blob metadata by path only inside the requested user scope.
func (s *Store) GetBlobByPathForUser(ctx context.Context, userID int64, blobPath string) (BlobRecord, error) {
	var b BlobRecord
	var created int64
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT id, user_id, kind, path, sha256, size, created_at FROM blobs WHERE user_id = ? AND path = ?`, userID, blobPath).
		Scan(&b.ID, &b.UserID, &b.Kind, &b.Path, &b.SHA256, &b.Size, &created)
	b.CreatedAt = unixTime(created)
	return b, err
}

// DeleteBlobsForUser removes multiple blob metadata rows for one user in one transaction.
func (s *Store) DeleteBlobsForUser(ctx context.Context, userID int64, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	tx, err := s.mustDataDB(ctx, userID).BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `DELETE FROM blobs WHERE user_id = ? AND id = ?`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, err := stmt.ExecContext(ctx, userID, id); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return err
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}

// DeleteBlobForUser removes blob metadata for one user; filesystem deletion is handled by the blob store.
func (s *Store) DeleteBlobForUser(ctx context.Context, userID, id int64) error {
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `DELETE FROM blobs WHERE user_id = ? AND id = ?`, userID, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteBlobIfUnreferencedForUser removes blob metadata only when no tenant-owned
// row still uses it. Callers may delete the corresponding filesystem path only
// after this returns deleted=true.
func (s *Store) DeleteBlobIfUnreferencedForUser(ctx context.Context, userID, id int64) (deleted bool, err error) {
	if userID <= 0 || id <= 0 {
		return false, nil
	}
	res, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `DELETE FROM blobs
		WHERE user_id = ? AND id = ?
			AND NOT EXISTS (
				SELECT 1 FROM messages
				WHERE messages.user_id = blobs.user_id AND messages.blob_id = blobs.id
			)
			AND NOT EXISTS (
				SELECT 1 FROM attachments
				WHERE attachments.user_id = blobs.user_id AND attachments.blob_id = blobs.id
			)
			AND NOT EXISTS (
				SELECT 1 FROM contact_icons
				WHERE contact_icons.user_id = blobs.user_id AND contact_icons.blob_id = blobs.id
			)
			AND NOT EXISTS (
				SELECT 1 FROM remote_image_cache
				WHERE remote_image_cache.user_id = blobs.user_id AND remote_image_cache.blob_id = blobs.id
			)
			AND NOT EXISTS (
				SELECT 1 FROM outbox_jobs
				WHERE outbox_jobs.user_id = blobs.user_id AND outbox_jobs.blob_id = blobs.id
			)`, userID, id)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows == 1, nil
}

// CreateAttachment records attachment metadata for a stored message.
func (s *Store) CreateAttachment(ctx context.Context, a Attachment) (Attachment, error) {
	ts := nowUnix()
	res, err := s.mustDataDB(ctx, a.UserID).ExecContext(ctx, `INSERT INTO attachments (user_id, message_id, blob_id, filename, content_type, content_id, is_inline, size, blob_path, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, a.UserID, a.MessageID, a.BlobID, a.Filename, a.ContentType, a.ContentID, boolInt(a.IsInline), a.Size, a.BlobPath, ts)
	if err != nil {
		return Attachment{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Attachment{}, err
	}
	return s.GetAttachmentForUser(ctx, a.UserID, id)
}

// DeleteAttachmentsForMessage removes attachment rows when a message is replaced or deleted.
func (s *Store) DeleteAttachmentsForMessage(ctx context.Context, userID, messageID int64) error {
	_, err := s.mustDataDB(ctx, userID).ExecContext(ctx, `DELETE FROM attachments WHERE user_id = ? AND message_id = ?`, userID, messageID)
	return err
}

// GetAttachmentForUser loads one attachment through its message ownership boundary.
func (s *Store) GetAttachmentForUser(ctx context.Context, userID, id int64) (Attachment, error) {
	var a Attachment
	var created int64
	var isInline int
	err := s.mustDataDB(ctx, userID).QueryRowContext(ctx, `SELECT id, user_id, message_id, blob_id, filename, content_type, content_id, is_inline, size, blob_path, created_at
		FROM attachments WHERE user_id = ? AND id = ?`, userID, id).
		Scan(&a.ID, &a.UserID, &a.MessageID, &a.BlobID, &a.Filename, &a.ContentType, &a.ContentID, &isInline, &a.Size, &a.BlobPath, &created)
	a.IsInline = isInline != 0
	a.CreatedAt = unixTime(created)
	return a, err
}

// ListAttachmentsForMessage returns attachment metadata for a user-owned message.
func (s *Store) ListAttachmentsForMessage(ctx context.Context, userID, messageID int64) ([]Attachment, error) {
	rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT id, user_id, message_id, blob_id, filename, content_type, content_id, is_inline, size, blob_path, created_at
		FROM attachments WHERE user_id = ? AND message_id = ? ORDER BY id`, userID, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attachment
	for rows.Next() {
		var a Attachment
		var created int64
		var isInline int
		if err := rows.Scan(&a.ID, &a.UserID, &a.MessageID, &a.BlobID, &a.Filename, &a.ContentType, &a.ContentID, &isInline, &a.Size, &a.BlobPath, &created); err != nil {
			return nil, err
		}
		a.IsInline = isInline != 0
		a.CreatedAt = unixTime(created)
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAttachmentsForMessages returns attachment metadata grouped by message ID.
func (s *Store) ListAttachmentsForMessages(ctx context.Context, userID int64, messageIDs []int64) (map[int64][]Attachment, error) {
	ids := make([]int64, 0, len(messageIDs))
	seen := map[int64]bool{}
	for _, id := range messageIDs {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	out := map[int64][]Attachment{}
	if userID <= 0 || len(ids) == 0 {
		return out, nil
	}
	for start := 0; start < len(ids); start += 500 {
		end := start + 500
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		args := make([]any, 0, len(chunk)+1)
		args = append(args, userID)
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.mustDataDB(ctx, userID).QueryContext(ctx, `SELECT id, user_id, message_id, blob_id, filename, content_type, content_id, is_inline, size, blob_path, created_at
			FROM attachments WHERE user_id = ? AND message_id IN (`+sqlPlaceholders(len(chunk))+`) ORDER BY message_id, id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var a Attachment
			var created int64
			var isInline int
			if err := rows.Scan(&a.ID, &a.UserID, &a.MessageID, &a.BlobID, &a.Filename, &a.ContentType, &a.ContentID, &isInline, &a.Size, &a.BlobPath, &created); err != nil {
				_ = rows.Close()
				return nil, err
			}
			a.IsInline = isInline != 0
			a.CreatedAt = unixTime(created)
			out[a.MessageID] = append(out[a.MessageID], a)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return out, nil
}
