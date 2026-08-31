// File overview: User-scoped blob path construction, saving, opening, and deletion.

package blob

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Store owns the filesystem root for user-scoped blob files.
type Store struct {
	Root string
}

// Saved is the file metadata returned after a blob body is written.
type Saved struct {
	Path   string
	SHA256 string
	Size   int64
}

// New opens the filesystem blob store rooted at dataDir and creates the directory if needed.
func New(root string) *Store {
	return &Store{Root: root}
}

// SaveRawMessage writes a raw RFC822 message under the owning user path and returns metadata for SQLite.
func (s *Store) SaveRawMessage(userID, accountID int64, mailbox string, uid uint32, raw []byte) (Saved, error) {
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	name := fmt.Sprintf("uid-%d-%s.eml", uid, hash[:16])
	parts := []string{
		"users", strconv.FormatInt(userID, 10), "blobs",
		"accounts", strconv.FormatInt(accountID, 10),
		"mailboxes", safeSegment(mailbox),
		name,
	}
	return s.save(parts, raw, hash)
}

// SaveOutboxMessage durably spools one immutable RFC822 payload before the
// compose request reports that it was queued. The submission key is hashed so
// browser-controlled identifiers never become path components.
func (s *Store) SaveOutboxMessage(userID int64, submissionKey string, raw []byte) (Saved, error) {
	if userID <= 0 || strings.TrimSpace(submissionKey) == "" || len(raw) == 0 {
		return Saved{}, errors.New("invalid outbox message")
	}
	rawSum := sha256.Sum256(raw)
	rawHash := hex.EncodeToString(rawSum[:])
	keySum := sha256.Sum256([]byte(strings.TrimSpace(submissionKey)))
	name := fmt.Sprintf("%s-%s.eml", hex.EncodeToString(keySum[:8]), rawHash[:16])
	parts := []string{
		"users", strconv.FormatInt(userID, 10), "blobs", "outbox", name,
	}
	return s.saveDurable(parts, raw, rawHash)
}

// SaveAttachment writes a standalone attachment body for the owning user when retention rules allow it.
func (s *Store) SaveAttachment(userID, messageID int64, index int, filename string, data []byte) (Saved, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if strings.TrimSpace(filename) == "" {
		filename = "attachment.bin"
	}
	name := fmt.Sprintf("%03d-%s-%s", index, hash[:16], safeSegment(filename))
	parts := []string{
		"users", strconv.FormatInt(userID, 10), "blobs",
		"attachments", strconv.FormatInt(messageID, 10),
		name,
	}
	return s.save(parts, data, hash)
}

// SaveContactIcon stores a contact avatar/icon blob under the owning user path.
func (s *Store) SaveContactIcon(userID, contactID int64, filename string, data []byte) (Saved, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	if strings.TrimSpace(filename) == "" {
		filename = "contact-icon"
	}
	name := fmt.Sprintf("%s-%s", hash[:16], safeSegment(filename))
	parts := []string{
		"users", strconv.FormatInt(userID, 10), "blobs",
		"contacts", strconv.FormatInt(contactID, 10),
		"icons", name,
	}
	return s.save(parts, data, hash)
}

// SaveRemoteImage stores a warmed remote email image in the owning user's blob cache.
func (s *Store) SaveRemoteImage(userID int64, urlHash string, data []byte) (Saved, error) {
	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])
	urlHash = safeSegment(urlHash)
	name := fmt.Sprintf("%s-%s", urlHash, hash[:16])
	parts := []string{
		"users", strconv.FormatInt(userID, 10), "blobs",
		"remote-images", urlHash[:2],
		name,
	}
	return s.save(parts, data, hash)
}

func (s *Store) save(parts []string, data []byte, hash string) (Saved, error) {
	rel := filepath.Join(parts...)
	if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
		return Saved{}, errors.New("unsafe blob path")
	}
	abs := filepath.Join(s.Root, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return Saved{}, err
	}
	if err := os.WriteFile(abs, data, 0o600); err != nil {
		return Saved{}, err
	}
	return Saved{Path: rel, SHA256: hash, Size: int64(len(data))}, nil
}

func (s *Store) saveDurable(parts []string, data []byte, hash string) (Saved, error) {
	rel := filepath.Join(parts...)
	if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
		return Saved{}, errors.New("unsafe blob path")
	}
	abs := filepath.Join(s.Root, rel)
	dir := filepath.Dir(abs)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Saved{}, err
	}
	// Persist the new outbox-directory entry before relying on a file inside it.
	if err := syncDirectory(filepath.Dir(dir)); err != nil {
		return Saved{}, err
	}
	tmp, err := os.CreateTemp(dir, ".outbox-*.tmp")
	if err != nil {
		return Saved{}, err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return Saved{}, err
	}
	if _, err := io.Copy(tmp, bytes.NewReader(data)); err != nil {
		cleanup()
		return Saved{}, err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return Saved{}, err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return Saved{}, err
	}
	if err := os.Rename(tmpName, abs); err != nil {
		_ = os.Remove(tmpName)
		return Saved{}, err
	}
	if err := syncDirectory(dir); err != nil {
		return Saved{}, err
	}
	return Saved{Path: rel, SHA256: hash, Size: int64(len(data))}, nil
}

func syncDirectory(dir string) error {
	dirHandle, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := dirHandle.Sync()
	closeErr := dirHandle.Close()
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	return nil
}

// OpenUserBlob opens a previously recorded blob path only inside the requested user directory.
func (s *Store) OpenUserBlob(userID int64, rel string) (*os.File, error) {
	clean := filepath.Clean(rel)
	if !userBlobPathAllowed(userID, clean) {
		return nil, errors.New("blob path is outside user scope")
	}
	return os.Open(filepath.Join(s.Root, clean))
}

// DeleteUserBlob removes a blob path from the requested user directory and ignores already-missing files.
func (s *Store) DeleteUserBlob(userID int64, rel string) error {
	clean := filepath.Clean(rel)
	if !userBlobPathAllowed(userID, clean) {
		return errors.New("blob path is outside user scope")
	}
	err := os.Remove(filepath.Join(s.Root, clean))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// OwnsUserBlobPath reports whether a relative path is inside the requested
// user's blob directory. Legacy malformed metadata may be discarded without
// ever passing such a path to filesystem deletion.
func (s *Store) OwnsUserBlobPath(userID int64, rel string) bool {
	return userBlobPathAllowed(userID, filepath.Clean(rel))
}

func userBlobPathAllowed(userID int64, clean string) bool {
	if filepath.IsAbs(clean) || clean == "." || strings.Contains(clean, "..") {
		return false
	}
	id := strconv.FormatInt(userID, 10)
	prefix := filepath.Join("users", id, "blobs") + string(filepath.Separator)
	return strings.HasPrefix(clean, prefix)
}

func safeSegment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "_"
	}
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r)
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := b.String()
	if out == "." || out == ".." {
		return "_"
	}
	if len(out) > 120 {
		out = out[:120]
	}
	return out
}
