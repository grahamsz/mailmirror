// File overview: SMTP send implementation for composed messages.

package smtpclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strings"
	"time"

	"rolltop/backend/buildinfo"
	mmcrypto "rolltop/backend/crypto"
	"rolltop/backend/store"
)

// Attachment is an outgoing MIME attachment or inline part prepared by compose.
type Attachment struct {
	Filename    string
	ContentType string
	ContentID   string
	Inline      bool
	Data        []byte
}

// Header is an extra outbound RFC822 header prepared by compose or a plugin.
type Header struct {
	Name  string
	Value string
}

// MIMEBodyOverride lets compose supply a fully prepared root MIME body.
type MIMEBodyOverride struct {
	ContentType string
	Body        string
}

// Message is the normalized outbound compose payload passed to the SMTP sender.
type Message struct {
	From             string
	To               []string
	Cc               []string
	Bcc              []string
	Subject          string
	BodyText         string
	BodyHTML         string
	MessageID        string
	InReplyTo        string
	References       string
	Date             time.Time
	ExtraHeaders     []Header
	MIMEBodyOverride *MIMEBodyOverride
	Attachments      []Attachment
}

// Sender sends compose messages through an encrypted Rolltop SMTP account.
type Sender struct {
	MasterKey []byte
	Timeout   time.Duration
}

// DeliveryPhase identifies the SMTP boundary that failed without exposing
// envelope addresses or message content.
type DeliveryPhase string

const (
	DeliveryPhaseConnect      DeliveryPhase = "connect"
	DeliveryPhaseTLS          DeliveryPhase = "tls"
	DeliveryPhaseHello        DeliveryPhase = "hello"
	DeliveryPhaseAuthenticate DeliveryPhase = "authenticate"
	DeliveryPhaseEnvelope     DeliveryPhase = "envelope"
	DeliveryPhaseData         DeliveryPhase = "data"
	DeliveryPhaseTransmit     DeliveryPhase = "transmit"
	DeliveryPhaseAccept       DeliveryPhase = "accept"
)

// DeliveryOutcome describes whether retrying SMTP is safe.
type DeliveryOutcome string

const (
	DeliveryNotAccepted DeliveryOutcome = "not_accepted"
	DeliveryAccepted    DeliveryOutcome = "accepted"
	DeliveryUnknown     DeliveryOutcome = "unknown"
)

// DeliveryError preserves retry and ambiguity information that ordinary
// wrapped SMTP errors lose.
type DeliveryError struct {
	Phase     DeliveryPhase
	Temporary bool
	Outcome   DeliveryOutcome
	Err       error
}

func (e *DeliveryError) Error() string {
	return fmt.Sprintf("SMTP %s: %v", e.Phase, e.Err)
}

func (e *DeliveryError) Unwrap() error {
	return e.Err
}

type smtpIdleDeadlineConn struct {
	net.Conn
	timeout time.Duration
}

func (c *smtpIdleDeadlineConn) Read(p []byte) (int, error) {
	if c.timeout > 0 {
		if err := c.Conn.SetReadDeadline(time.Now().Add(c.timeout)); err != nil {
			return 0, err
		}
	}
	return c.Conn.Read(p)
}

func (c *smtpIdleDeadlineConn) Write(p []byte) (int, error) {
	if c.timeout > 0 {
		if err := c.Conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
			return 0, err
		}
	}
	return c.Conn.Write(p)
}

// Send builds a MIME message from the compose form and sends it through the configured SMTP account.
func (s *Sender) Send(ctx context.Context, account store.MailAccount, msg Message) ([]byte, error) {
	raw, recipients, err := BuildRaw(msg)
	if err != nil {
		return nil, err
	}
	if err := s.SendRaw(ctx, account, recipients, raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// SendRaw sends an already-built RFC822 payload to all recipients using the configured SMTP account.
func (s *Sender) SendRaw(ctx context.Context, account store.MailAccount, recipients []string, raw []byte) error {
	return s.SendRawReader(ctx, account, recipients, bytes.NewReader(raw))
}

// SendRawReader streams an immutable RFC822 payload without retaining another
// full copy in memory. Callers must open a fresh reader for every attempt.
func (s *Sender) SendRawReader(ctx context.Context, account store.MailAccount, recipients []string, raw io.Reader) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(recipients) == 0 || raw == nil {
		return errors.New("message has no recipients")
	}
	password, err := mmcrypto.DecryptString(s.MasterKey, account.EncryptedSMTPPassword)
	if err != nil {
		return deliveryError(DeliveryPhaseAuthenticate, DeliveryNotAccepted, false,
			fmt.Errorf("decrypt SMTP password: %w", err))
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	addr := net.JoinHostPort(account.SMTPHost, fmt.Sprintf("%d", account.SMTPPort))
	dialer := &net.Dialer{Timeout: timeout}
	rawConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return deliveryError(DeliveryPhaseConnect, DeliveryNotAccepted, true,
			fmt.Errorf("connect to SMTP server %s: %w", addr, err))
	}
	deadlineConn := &smtpIdleDeadlineConn{Conn: rawConn, timeout: timeout}
	var conn net.Conn = deadlineConn
	if account.SMTPUseTLS && account.SMTPPort == 465 {
		// net/smtp recognizes implicit TLS only when it receives the concrete
		// *tls.Conn. Keep the deadline wrapper underneath the TLS connection.
		tlsConn := tls.Client(deadlineConn, &tls.Config{ServerName: account.SMTPHost, MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = tlsConn.Close()
			return deliveryError(DeliveryPhaseTLS, DeliveryNotAccepted, smtpErrorTemporary(err),
				fmt.Errorf("start SMTP TLS: %w", err))
		}
		conn = tlsConn
	}
	return sendRawReaderOnConn(ctx, account, password, recipients, raw, conn)
}

func sendRawOnConn(ctx context.Context, account store.MailAccount, password string, recipients []string, raw []byte, conn net.Conn) error {
	return sendRawReaderOnConn(ctx, account, password, recipients, bytes.NewReader(raw), conn)
}

func sendRawReaderOnConn(ctx context.Context, account store.MailAccount, password string, recipients []string, raw io.Reader, conn net.Conn) error {
	defer conn.Close()
	stopContext := watchSMTPContext(ctx, conn)
	defer stopContext()

	c, err := smtp.NewClient(conn, account.SMTPHost)
	if err != nil {
		return deliveryError(DeliveryPhaseConnect, DeliveryNotAccepted, true,
			fmt.Errorf("initialize SMTP client for %s: %w", account.SMTPHost, err))
	}
	defer c.Close()
	if err := c.Hello("localhost"); err != nil {
		return deliveryError(DeliveryPhaseHello, DeliveryNotAccepted, smtpErrorTemporary(err),
			fmt.Errorf("SMTP hello: %w", err))
	}
	if account.SMTPUseTLS && account.SMTPPort != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: account.SMTPHost, MinVersion: tls.VersionTLS12}); err != nil {
				return deliveryError(DeliveryPhaseTLS, DeliveryNotAccepted, smtpErrorTemporary(err),
					fmt.Errorf("start SMTP TLS: %w", err))
			}
		} else {
			return deliveryError(DeliveryPhaseTLS, DeliveryNotAccepted, false,
				errors.New("SMTP server does not advertise STARTTLS"))
		}
	}
	if strings.TrimSpace(account.SMTPUsername) != "" {
		auth := smtp.PlainAuth("", account.SMTPUsername, password, account.SMTPHost)
		if err := c.Auth(auth); err != nil {
			return deliveryError(DeliveryPhaseAuthenticate, DeliveryNotAccepted, smtpErrorTemporary(err),
				fmt.Errorf("authenticate to SMTP server: %w", err))
		}
	}
	fromAddr, err := firstAddress(account.Email)
	if err != nil {
		return deliveryError(DeliveryPhaseEnvelope, DeliveryNotAccepted, false, err)
	}
	if err := c.Mail(fromAddr); err != nil {
		return deliveryError(DeliveryPhaseEnvelope, DeliveryNotAccepted, smtpErrorTemporary(err),
			fmt.Errorf("SMTP MAIL FROM: %w", err))
	}
	for _, recipient := range recipients {
		if err := c.Rcpt(recipient); err != nil {
			return deliveryError(DeliveryPhaseEnvelope, DeliveryNotAccepted, smtpErrorTemporary(err),
				fmt.Errorf("SMTP RCPT TO: %w", err))
		}
	}
	w, err := c.Data()
	if err != nil {
		return deliveryError(DeliveryPhaseData, DeliveryNotAccepted, smtpErrorTemporary(err),
			fmt.Errorf("SMTP DATA: %w", err))
	}
	if _, err := io.Copy(w, raw); err != nil {
		_ = w.Close()
		// SMTP cannot accept DATA until the terminating dot is written. A body
		// stream failure before Close is therefore safe to retry.
		return deliveryError(DeliveryPhaseTransmit, DeliveryNotAccepted, smtpErrorTemporary(err),
			fmt.Errorf("write SMTP message: %w", err))
	}
	if err := w.Close(); err != nil {
		var protocol *textproto.Error
		if errors.As(err, &protocol) {
			// A final 4xx/5xx response is a definitive rejection, not an
			// ambiguous connection loss.
			return deliveryError(DeliveryPhaseAccept, DeliveryNotAccepted, smtpErrorTemporary(err),
				fmt.Errorf("finish SMTP message: %w", err))
		}
		return deliveryError(DeliveryPhaseAccept, DeliveryUnknown, false,
			fmt.Errorf("finish SMTP message: %w", err))
	}
	// A successful DATA close includes the server's final acceptance response.
	// Closing the session via the deferred client close avoids waiting on QUIT
	// after delivery and cannot misreport an accepted message as failed.
	return nil
}

func deliveryError(phase DeliveryPhase, outcome DeliveryOutcome, temporary bool, err error) error {
	if err == nil {
		return nil
	}
	return &DeliveryError{Phase: phase, Temporary: temporary, Outcome: outcome, Err: err}
}

func smtpErrorTemporary(err error) bool {
	var protocol *textproto.Error
	if errors.As(err, &protocol) {
		return protocol.Code >= 400 && protocol.Code < 500
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return netErr.Timeout() || netErr.Temporary()
	}
	return !errors.Is(err, context.Canceled)
}

func watchSMTPContext(ctx context.Context, conn net.Conn) func() {
	if ctx == nil || conn == nil {
		return func() {}
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	return func() { stop() }
}

// BuildRaw constructs the RFC822 message, including text/html alternatives,
// headers, and attachments. It is used for real sends, so at least one
// recipient is required before SMTP is attempted.
func BuildRaw(msg Message) ([]byte, []string, error) {
	return buildRaw(msg, true)
}

// BuildDraftRaw constructs an unsent RFC822 draft. Drafts are allowed to be
// incomplete, so To/Cc/Bcc may all be empty while the MIME body and attachments
// are still preserved for IMAP APPEND.
func BuildDraftRaw(msg Message) ([]byte, error) {
	raw, _, err := buildRaw(msg, false)
	return raw, err
}

func buildRaw(msg Message, requireRecipients bool) ([]byte, []string, error) {
	if msg.Date.IsZero() {
		msg.Date = time.Now()
	}
	from, err := mail.ParseAddress(msg.From)
	if err != nil {
		return nil, nil, fmt.Errorf("from address: %w", err)
	}
	to, err := parseAddresses(msg.To)
	if err != nil {
		return nil, nil, fmt.Errorf("to address: %w", err)
	}
	cc, err := parseAddresses(msg.Cc)
	if err != nil {
		return nil, nil, fmt.Errorf("cc address: %w", err)
	}
	bcc, err := parseAddresses(msg.Bcc)
	if err != nil {
		return nil, nil, fmt.Errorf("bcc address: %w", err)
	}
	recipients := addressStrings(append(append(to, cc...), bcc...))
	if requireRecipients && len(recipients) == 0 {
		return nil, nil, errors.New("message has no recipients")
	}
	if strings.TrimSpace(msg.MessageID) == "" {
		msg.MessageID = NewMessageID(from.Address)
	}

	var b bytes.Buffer
	w := bufio.NewWriter(&b)
	writeHeader(w, "From", from.String())
	if len(to) > 0 {
		writeHeader(w, "To", addressListString(to))
	}
	if len(cc) > 0 {
		writeHeader(w, "Cc", addressListString(cc))
	}
	if !requireRecipients && len(bcc) > 0 {
		writeHeader(w, "Bcc", addressListString(bcc))
	}
	writeHeader(w, "Subject", mime.QEncoding.Encode("utf-8", strings.TrimSpace(msg.Subject)))
	writeHeader(w, "Date", msg.Date.Format(time.RFC1123Z))
	writeHeader(w, "Message-ID", msg.MessageID)
	writeHeader(w, "X-Mailer", xMailerHeaderValue())
	for _, header := range msg.ExtraHeaders {
		writeExtraHeader(w, header.Name, header.Value)
	}
	if strings.TrimSpace(msg.InReplyTo) != "" {
		writeHeader(w, "In-Reply-To", sanitizeHeaderValue(msg.InReplyTo))
	}
	if strings.TrimSpace(msg.References) != "" {
		writeHeader(w, "References", sanitizeHeaderValue(msg.References))
	}
	writeHeader(w, "MIME-Version", "1.0")
	writeRootBody(w, msg)
	if err := w.Flush(); err != nil {
		return nil, nil, err
	}
	return b.Bytes(), recipients, nil
}

func writeRootBody(w *bufio.Writer, msg Message) {
	if msg.MIMEBodyOverride != nil && strings.TrimSpace(msg.MIMEBodyOverride.ContentType) != "" {
		writeHeader(w, "Content-Type", msg.MIMEBodyOverride.ContentType)
		_, _ = w.WriteString("\r\n")
		body := normalizeCRLF(msg.MIMEBodyOverride.Body)
		_, _ = w.WriteString(body)
		if !strings.HasSuffix(body, "\r\n") {
			_, _ = w.WriteString("\r\n")
		}
		return
	}
	inlineAttachments, regularAttachments := splitAttachments(msg.Attachments)
	hasInlineHTML := len(inlineAttachments) > 0 && strings.TrimSpace(msg.BodyHTML) != ""
	if len(msg.Attachments) == 0 {
		writeBodyEntity(w, msg)
		return
	}
	if len(regularAttachments) > 0 {
		boundary := boundaryFor(msg, "mixed")
		writeHeader(w, "Content-Type", mime.FormatMediaType("multipart/mixed", map[string]string{"boundary": boundary}))
		_, _ = w.WriteString("\r\n")
		if hasInlineHTML {
			relatedBoundary := boundaryFor(msg, "related")
			_, _ = fmt.Fprintf(w, "--%s\r\n", boundary)
			writeHeader(w, "Content-Type", mime.FormatMediaType("multipart/related", map[string]string{"boundary": relatedBoundary}))
			_, _ = w.WriteString("\r\n")
			writeBodyEntityPart(w, relatedBoundary, msg)
			for _, attachment := range inlineAttachments {
				writeAttachmentPart(w, relatedBoundary, attachment)
			}
			_, _ = fmt.Fprintf(w, "--%s--\r\n", relatedBoundary)
		} else {
			writeBodyEntityPart(w, boundary, msg)
			regularAttachments = append(inlineAttachments, regularAttachments...)
		}
		for _, attachment := range regularAttachments {
			writeAttachmentPart(w, boundary, attachment)
		}
		_, _ = fmt.Fprintf(w, "--%s--\r\n", boundary)
		return
	}
	if hasInlineHTML {
		boundary := boundaryFor(msg, "related")
		writeHeader(w, "Content-Type", mime.FormatMediaType("multipart/related", map[string]string{"boundary": boundary}))
		_, _ = w.WriteString("\r\n")
		writeBodyEntityPart(w, boundary, msg)
		for _, attachment := range inlineAttachments {
			writeAttachmentPart(w, boundary, attachment)
		}
		_, _ = fmt.Fprintf(w, "--%s--\r\n", boundary)
		return
	}
	boundary := boundaryFor(msg, "mixed")
	writeHeader(w, "Content-Type", mime.FormatMediaType("multipart/mixed", map[string]string{"boundary": boundary}))
	_, _ = w.WriteString("\r\n")
	writeBodyEntityPart(w, boundary, msg)
	for _, attachment := range inlineAttachments {
		writeAttachmentPart(w, boundary, attachment)
	}
	_, _ = fmt.Fprintf(w, "--%s--\r\n", boundary)
}

func splitAttachments(attachments []Attachment) ([]Attachment, []Attachment) {
	var inlineAttachments []Attachment
	var regularAttachments []Attachment
	for _, attachment := range attachments {
		if attachment.Inline {
			inlineAttachments = append(inlineAttachments, attachment)
		} else {
			regularAttachments = append(regularAttachments, attachment)
		}
	}
	return inlineAttachments, regularAttachments
}

func writeBodyEntityPart(w *bufio.Writer, boundary string, msg Message) {
	_, _ = fmt.Fprintf(w, "--%s\r\n", boundary)
	writeBodyEntity(w, msg)
}

func writeBodyEntity(w *bufio.Writer, msg Message) {
	if strings.TrimSpace(msg.BodyHTML) != "" {
		boundary := boundaryFor(msg, "alt")
		writeHeader(w, "Content-Type", mime.FormatMediaType("multipart/alternative", map[string]string{"boundary": boundary}))
		_, _ = w.WriteString("\r\n")
		writePart(w, boundary, `text/plain; charset="utf-8"`, msg.BodyText)
		writePart(w, boundary, `text/html; charset="utf-8"`, msg.BodyHTML)
		_, _ = fmt.Fprintf(w, "--%s--\r\n", boundary)
		return
	}
	writeHeader(w, "Content-Type", `text/plain; charset="utf-8"`)
	writeHeader(w, "Content-Transfer-Encoding", "8bit")
	_, _ = w.WriteString("\r\n")
	body := normalizeCRLF(msg.BodyText)
	_, _ = w.WriteString(body)
	if !strings.HasSuffix(body, "\r\n") {
		_, _ = w.WriteString("\r\n")
	}
}

func writeAttachmentPart(w *bufio.Writer, boundary string, attachment Attachment) {
	_, _ = fmt.Fprintf(w, "--%s\r\n", boundary)
	filename := sanitizeAttachmentFilename(attachment.Filename)
	contentType := strings.TrimSpace(attachment.ContentType)
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || strings.TrimSpace(mediaType) == "" {
		mediaType = "application/octet-stream"
		params = map[string]string{}
	}
	if filename != "" {
		params["name"] = filename
	}
	writeHeader(w, "Content-Type", mime.FormatMediaType(mediaType, params))
	writeHeader(w, "Content-Transfer-Encoding", "base64")
	if attachment.Inline && strings.TrimSpace(attachment.ContentID) != "" {
		writeHeader(w, "Content-ID", contentIDHeader(attachment.ContentID))
	}
	disposition := "attachment"
	if attachment.Inline {
		disposition = "inline"
	}
	dispositionParams := map[string]string{}
	if filename != "" {
		dispositionParams["filename"] = filename
	}
	writeHeader(w, "Content-Disposition", mime.FormatMediaType(disposition, dispositionParams))
	_, _ = w.WriteString("\r\n")
	writeBase64Body(w, attachment.Data)
}

func writeBase64Body(w *bufio.Writer, data []byte) {
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(data)))
	base64.StdEncoding.Encode(encoded, data)
	for len(encoded) > 0 {
		lineLength := 76
		if len(encoded) < lineLength {
			lineLength = len(encoded)
		}
		_, _ = w.Write(encoded[:lineLength])
		_, _ = w.WriteString("\r\n")
		encoded = encoded[lineLength:]
	}
}

func boundaryFor(msg Message, kind string) string {
	return MIMEBoundary(msg.MessageID, kind)
}

// MIMEBoundary returns Rolltop's stable boundary format for a message and body
// kind so callers that prepare a MIMEBodyOverride can match normal compose.
func MIMEBoundary(messageID, kind string) string {
	boundary := "rolltop-" + kind + "-" + strings.Trim(messageID, "<>")
	return strings.NewReplacer("@", "-", ".", "-", "_", "-", "/", "-", "+", "-").Replace(boundary)
}

func contentIDHeader(contentID string) string {
	contentID = strings.Trim(strings.TrimSpace(contentID), "<>")
	return "<" + sanitizeHeaderValue(contentID) + ">"
}

func sanitizeAttachmentFilename(filename string) string {
	filename = strings.TrimSpace(filename)
	filename = strings.ReplaceAll(filename, "\x00", "")
	filename = strings.ReplaceAll(filename, "/", "_")
	filename = strings.ReplaceAll(filename, "\\", "_")
	if filename == "" {
		return "attachment"
	}
	return filename
}

// NewMessageID creates a local Message-ID suitable for outbound composed mail.
func NewMessageID(fromAddress string) string {
	domain := "rolltop.local"
	if _, host, ok := strings.Cut(fromAddress, "@"); ok && strings.TrimSpace(host) != "" {
		domain = strings.ToLower(strings.TrimSpace(host))
	}
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return fmt.Sprintf("<%d@rolltop.%s>", time.Now().UnixNano(), domain)
	}
	return fmt.Sprintf("<%d.%s@%s>", time.Now().UnixNano(), hex.EncodeToString(random), domain)
}

// NewSubmissionID creates an opaque compose idempotency key. It is separate
// from Message-ID because SMTP servers do not promise Message-ID deduplication.
func NewSubmissionID() string {
	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		return fmt.Sprintf("submission-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(random)
}

func writePart(w *bufio.Writer, boundary, contentType, body string) {
	_, _ = fmt.Fprintf(w, "--%s\r\n", boundary)
	writeHeader(w, "Content-Type", contentType)
	writeHeader(w, "Content-Transfer-Encoding", "8bit")
	_, _ = w.WriteString("\r\n")
	body = normalizeCRLF(body)
	_, _ = w.WriteString(body)
	if !strings.HasSuffix(body, "\r\n") {
		_, _ = w.WriteString("\r\n")
	}
}

func normalizeCRLF(body string) string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	return strings.ReplaceAll(body, "\n", "\r\n")
}

func parseAddresses(values []string) ([]*mail.Address, error) {
	var out []*mail.Address
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		addrs, err := mail.ParseAddressList(value)
		if err != nil {
			return nil, err
		}
		out = append(out, addrs...)
	}
	return out, nil
}

func addressStrings(addrs []*mail.Address) []string {
	out := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		out = append(out, addr.Address)
	}
	return out
}

func addressListString(addrs []*mail.Address) string {
	parts := make([]string, 0, len(addrs))
	for _, addr := range addrs {
		parts = append(parts, addr.String())
	}
	return strings.Join(parts, ", ")
}

func writeHeader(w *bufio.Writer, name, value string) {
	_, _ = fmt.Fprintf(w, "%s: %s\r\n", name, sanitizeHeaderValue(value))
}

func writeExtraHeader(w *bufio.Writer, name, value string) {
	name = sanitizeHeaderName(name)
	value = sanitizeHeaderValue(value)
	if name == "" || value == "" {
		return
	}
	prefix := name + ": "
	_, _ = w.WriteString(prefix)
	firstLineRemaining := 76 - len(prefix)
	if firstLineRemaining < 16 {
		firstLineRemaining = 16
	}
	writeFoldedToken(w, value, firstLineRemaining, 76)
	_, _ = w.WriteString("\r\n")
}

func xMailerHeaderValue() string {
	info := buildinfo.Current()
	version := strings.TrimSpace(info.Version)
	if version == "" {
		version = "dev"
	}
	return "rolltop/" + version
}

func writeFoldedToken(w *bufio.Writer, value string, firstLine, nextLine int) {
	lineLimit := firstLine
	for len(value) > 0 {
		n := lineLimit
		if len(value) < n {
			n = len(value)
		}
		_, _ = w.WriteString(value[:n])
		value = value[n:]
		if value != "" {
			_, _ = w.WriteString("\r\n ")
			lineLimit = nextLine
		}
	}
}

func sanitizeHeaderName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	for _, r := range name {
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			continue
		default:
			return ""
		}
	}
	return name
}

func sanitizeHeaderValue(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return strings.Join(strings.Fields(value), " ")
}

func firstAddress(value string) (string, error) {
	addr, err := mail.ParseAddress(value)
	if err != nil {
		return "", fmt.Errorf("from address: %w", err)
	}
	return addr.Address, nil
}
