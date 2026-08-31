// File overview: Tests for SMTP send behavior.

package smtpclient

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"rolltop/backend/buildinfo"
	"rolltop/backend/store"
)

func TestSendRawClassifiesTemporaryEnvelopeFailureAsSafeToRetry(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	serverDone := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		_ = serverConn.SetDeadline(time.Now().Add(2 * time.Second))
		serverDone <- serveSMTPUntilRCPT(serverConn, "451 4.3.0 try again later\r\n")
	}()
	err := sendRawOnConn(context.Background(), store.MailAccount{
		Email: "sender@example.test", SMTPHost: "smtp.example.test",
	}, "", []string{"recipient@example.test"}, []byte("Subject: retry\r\n\r\nbody\r\n"), clientConn)
	var deliveryErr *DeliveryError
	if !errors.As(err, &deliveryErr) {
		t.Fatalf("send error=%v, want DeliveryError", err)
	}
	if deliveryErr.Phase != DeliveryPhaseEnvelope || deliveryErr.Outcome != DeliveryNotAccepted || !deliveryErr.Temporary {
		t.Fatalf("delivery error=%+v, want temporary envelope/not-accepted", deliveryErr)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func TestSendRawClassifiesLostFinalDataResponseAsUncertain(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	serverDone := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		_ = serverConn.SetDeadline(time.Now().Add(2 * time.Second))
		serverDone <- serveSMTPAndDropFinalAcceptance(serverConn)
	}()
	err := sendRawOnConn(context.Background(), store.MailAccount{
		Email: "sender@example.test", SMTPHost: "smtp.example.test",
	}, "", []string{"recipient@example.test"}, []byte("Subject: uncertain\r\n\r\nbody\r\n"), clientConn)
	var deliveryErr *DeliveryError
	if !errors.As(err, &deliveryErr) {
		t.Fatalf("send error=%v, want DeliveryError", err)
	}
	if deliveryErr.Phase != DeliveryPhaseAccept || deliveryErr.Outcome != DeliveryUnknown {
		t.Fatalf("delivery error=%+v, want accept/unknown", deliveryErr)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func serveSMTPUntilRCPT(conn net.Conn, rcptResponse string) error {
	reader := bufio.NewReader(conn)
	if _, err := fmt.Fprint(conn, "220 smtp.example.test ready\r\n"); err != nil {
		return err
	}
	for _, exchange := range []struct {
		prefix   string
		response string
	}{
		{"EHLO ", "250 smtp.example.test\r\n"},
		{"MAIL FROM:", "250 sender ok\r\n"},
		{"RCPT TO:", rcptResponse},
	} {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if !strings.HasPrefix(strings.ToUpper(line), exchange.prefix) {
			return fmt.Errorf("SMTP command=%q, want prefix %q", line, exchange.prefix)
		}
		if _, err := fmt.Fprint(conn, exchange.response); err != nil {
			return err
		}
	}
	return nil
}

func serveSMTPAndDropFinalAcceptance(conn net.Conn) error {
	reader := bufio.NewReader(conn)
	if _, err := fmt.Fprint(conn, "220 smtp.example.test ready\r\n"); err != nil {
		return err
	}
	for _, exchange := range []struct {
		prefix   string
		response string
	}{
		{"EHLO ", "250 smtp.example.test\r\n"},
		{"MAIL FROM:", "250 sender ok\r\n"},
		{"RCPT TO:", "250 recipient ok\r\n"},
		{"DATA", "354 send message\r\n"},
	} {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if !strings.HasPrefix(strings.ToUpper(line), exchange.prefix) {
			return fmt.Errorf("SMTP command=%q, want prefix %q", line, exchange.prefix)
		}
		if _, err := fmt.Fprint(conn, exchange.response); err != nil {
			return err
		}
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if line == ".\r\n" {
			return nil
		}
	}
}

func TestSMTPIdleDeadlineConnBoundsBlockedIO(t *testing.T) {
	for _, test := range []struct {
		name string
		run  func(net.Conn) error
	}{
		{name: "read", run: func(conn net.Conn) error {
			_, err := conn.Read(make([]byte, 1))
			return err
		}},
		{name: "write", run: func(conn net.Conn) error {
			_, err := conn.Write([]byte("x"))
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()
			defer serverConn.Close()
			conn := &smtpIdleDeadlineConn{Conn: clientConn, timeout: 20 * time.Millisecond}
			started := time.Now()
			err := test.run(conn)
			netErr, ok := err.(net.Error)
			if !ok || !netErr.Timeout() {
				t.Fatalf("blocked %s error = %v, want network timeout", test.name, err)
			}
			if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
				t.Fatalf("blocked %s took %s, want bounded wait", test.name, elapsed)
			}
		})
	}
}

func TestWatchSMTPContextClosesBlockedConnection(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	stop := watchSMTPContext(ctx, clientConn)
	defer stop()
	readDone := make(chan error, 1)
	go func() {
		_, err := clientConn.Read(make([]byte, 1))
		readDone <- err
	}()
	cancel()
	select {
	case err := <-readDone:
		if err == nil {
			t.Fatal("blocked SMTP read succeeded after context cancellation")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("context cancellation did not close blocked SMTP connection")
	}
}

func TestSendRawDoesNotWaitForQuitAfterDataAccepted(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	serverDone := make(chan error, 1)
	go func() {
		defer serverConn.Close()
		_ = serverConn.SetDeadline(time.Now().Add(2 * time.Second))
		serverDone <- serveAcceptedSMTPMessageWithoutQuitResponse(serverConn)
	}()
	conn := &smtpIdleDeadlineConn{Conn: clientConn, timeout: time.Second}
	err := sendRawOnConn(context.Background(), store.MailAccount{
		Email:    "sender@example.test",
		SMTPHost: "smtp.example.test",
	}, "", []string{"recipient@example.test"},
		[]byte("From: sender@example.test\r\nTo: recipient@example.test\r\n\r\nbody\r\n"), conn)
	if err != nil {
		t.Fatalf("SendRaw after accepted DATA = %v, want success without waiting for QUIT", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}

func serveAcceptedSMTPMessageWithoutQuitResponse(conn net.Conn) error {
	reader := bufio.NewReader(conn)
	write := func(response string) error {
		_, err := fmt.Fprint(conn, response)
		return err
	}
	readPrefix := func(prefix string) error {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if !strings.HasPrefix(strings.ToUpper(line), prefix) {
			return fmt.Errorf("SMTP command = %q, want prefix %q", line, prefix)
		}
		return nil
	}
	if err := write("220 smtp.example.test ready\r\n"); err != nil {
		return err
	}
	for _, exchange := range []struct {
		command  string
		response string
	}{
		{command: "EHLO ", response: "250 smtp.example.test\r\n"},
		{command: "MAIL FROM:", response: "250 sender ok\r\n"},
		{command: "RCPT TO:", response: "250 recipient ok\r\n"},
		{command: "DATA", response: "354 send message\r\n"},
	} {
		if err := readPrefix(exchange.command); err != nil {
			return err
		}
		if err := write(exchange.response); err != nil {
			return err
		}
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if line == ".\r\n" {
			break
		}
	}
	if err := write("250 message accepted\r\n"); err != nil {
		return err
	}
	line, err := reader.ReadString('\n')
	if err == nil {
		return fmt.Errorf("unexpected SMTP command after accepted DATA: %q", line)
	}
	return nil
}

func TestBuildRawOmitsBccHeaderAndIncludesRecipients(t *testing.T) {
	oldVersion := buildinfo.Version
	buildinfo.Version = "2026.05-test"
	t.Cleanup(func() { buildinfo.Version = oldVersion })
	raw, recipients, err := BuildRaw(Message{
		From:      "Sender <sender@example.test>",
		To:        []string{"one@example.test, Two <two@example.test>"},
		Cc:        []string{"cc@example.test"},
		Bcc:       []string{"hidden@example.test"},
		Subject:   "Hello",
		BodyText:  "Line one\nLine two",
		MessageID: "<id@example.test>",
		Date:      time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	gotRecipients := strings.Join(recipients, ",")
	for _, want := range []string{"one@example.test", "two@example.test", "cc@example.test", "hidden@example.test"} {
		if !strings.Contains(gotRecipients, want) {
			t.Fatalf("recipients %q missing %q", gotRecipients, want)
		}
	}
	if bytes.Contains(bytes.ToLower(raw), []byte("\r\nbcc:")) {
		t.Fatalf("raw message leaked Bcc header:\n%s", raw)
	}
	if !bytes.Contains(raw, []byte("Message-ID: <id@example.test>\r\n")) {
		t.Fatalf("raw message missing Message-ID:\n%s", raw)
	}
	if !bytes.Contains(raw, []byte("X-Mailer: rolltop/2026.05-test\r\n")) {
		t.Fatalf("raw message missing X-Mailer:\n%s", raw)
	}
	if !bytes.Contains(raw, []byte("Line one\r\nLine two\r\n")) {
		t.Fatalf("raw message body not CRLF normalized:\n%s", raw)
	}
}

func TestBuildRawRejectsHeaderInjection(t *testing.T) {
	raw, _, err := BuildRaw(Message{
		From:      "sender@example.test",
		To:        []string{"to@example.test"},
		Subject:   "Hello\r\nBcc: attacker@example.test",
		BodyText:  "Body",
		MessageID: "<id@example.test>",
		Date:      time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("\r\nBcc: attacker@example.test")) {
		t.Fatalf("raw message allowed injected header:\n%s", raw)
	}
}

func TestBuildRawWritesFoldedAutocryptHeader(t *testing.T) {
	keyData := strings.Repeat("AQIDBAUGBwg=", 12)
	raw, _, err := BuildRaw(Message{
		From:      "sender@example.test",
		To:        []string{"to@example.test"},
		Subject:   "Autocrypt",
		BodyText:  "Body",
		MessageID: "<autocrypt@example.test>",
		Date:      time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
		ExtraHeaders: []Header{{
			Name:  "Autocrypt",
			Value: "addr=sender@example.test; prefer-encrypt=mutual; keydata=" + keyData,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "Autocrypt: addr=sender@example.test; prefer-encrypt=mutual; keydata=") {
		t.Fatalf("raw missing Autocrypt header:\n%s", text)
	}
	if !strings.Contains(text, "\r\n ") {
		t.Fatalf("Autocrypt header was not folded:\n%s", text)
	}
	if strings.Contains(text, "\nBcc:") {
		t.Fatalf("raw message leaked an unexpected Bcc header:\n%s", text)
	}
}

func TestBuildRawWithPGPMIMEEncryptedBody(t *testing.T) {
	boundary := MIMEBoundary("<pgp-mime@example.test>", "pgp-encrypted")
	raw, _, err := BuildRaw(Message{
		From:      "sender@example.test",
		To:        []string{"to@example.test"},
		Subject:   "PGP MIME",
		BodyText:  "-----BEGIN PGP MESSAGE-----\n\nciphertext\n-----END PGP MESSAGE-----",
		MessageID: "<pgp-mime@example.test>",
		Date:      time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
		MIMEBodyOverride: &MIMEBodyOverride{
			ContentType: `multipart/encrypted; boundary=` + boundary + `; protocol="application/pgp-encrypted"`,
			Body: "--" + boundary + "\r\n" +
				"Content-Type: application/pgp-encrypted\r\n\r\n" +
				"Version: 1\r\n" +
				"--" + boundary + "\r\n" +
				"Content-Type: application/octet-stream; name=encrypted.asc\r\n" +
				"Content-Disposition: inline; filename=encrypted.asc\r\n\r\n" +
				"-----BEGIN PGP MESSAGE-----\r\n\r\nciphertext\r\n-----END PGP MESSAGE-----\r\n" +
				"--" + boundary + "--\r\n",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		`Content-Type: multipart/encrypted; boundary=rolltop-pgp-encrypted-pgp-mime-example-test; protocol="application/pgp-encrypted"`,
		"Content-Type: application/pgp-encrypted\r\n",
		"Version: 1\r\n",
		`Content-Type: application/octet-stream; name=encrypted.asc`,
		`Content-Disposition: inline; filename=encrypted.asc`,
		"-----BEGIN PGP MESSAGE-----\r\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("PGP/MIME raw missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "Content-Type: text/plain") {
		t.Fatalf("PGP/MIME encrypted body leaked a plain text body part:\n%s", text)
	}
}

func TestBuildRawWithPGPMIMESignedBody(t *testing.T) {
	boundary := MIMEBoundary("<pgp-mime-signed@example.test>", "pgp-signed")
	raw, _, err := BuildRaw(Message{
		From:      "sender@example.test",
		To:        []string{"to@example.test"},
		Subject:   "PGP MIME signed",
		BodyText:  "Content-Type: text/plain; charset=\"utf-8\"\r\nContent-Transfer-Encoding: 8bit\r\n\r\nSigned text\r\n",
		MessageID: "<pgp-mime-signed@example.test>",
		Date:      time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
		MIMEBodyOverride: &MIMEBodyOverride{
			ContentType: `multipart/signed; boundary=` + boundary + `; micalg=pgp-sha256; protocol="application/pgp-signature"`,
			Body: "--" + boundary + "\r\n" +
				"Content-Type: text/plain; charset=\"utf-8\"\r\nContent-Transfer-Encoding: 8bit\r\n\r\nSigned text\r\n" +
				"--" + boundary + "\r\n" +
				"Content-Type: application/pgp-signature; name=signature.asc\r\n" +
				"Content-Disposition: attachment; filename=signature.asc\r\n\r\n" +
				"-----BEGIN PGP SIGNATURE-----\r\n\r\nsignature\r\n-----END PGP SIGNATURE-----\r\n" +
				"--" + boundary + "--\r\n",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{
		`Content-Type: multipart/signed; boundary=rolltop-pgp-signed-pgp-mime-signed-example-test; micalg=pgp-sha256; protocol="application/pgp-signature"`,
		"Content-Type: text/plain; charset=\"utf-8\"\r\n",
		"Signed text\r\n",
		`Content-Type: application/pgp-signature; name=signature.asc`,
		`Content-Disposition: attachment; filename=signature.asc`,
		"-----BEGIN PGP SIGNATURE-----\r\n",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("PGP/MIME signed raw missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "-----BEGIN PGP SIGNED MESSAGE-----") {
		t.Fatalf("PGP/MIME signed body used inline clear signing:\n%s", text)
	}
}

func TestBuildDraftRawAllowsNoRecipientsAndKeepsBcc(t *testing.T) {
	raw, err := BuildDraftRaw(Message{
		From:      "Sender <sender@example.test>",
		Bcc:       []string{"hidden@example.test"},
		Subject:   "Draft",
		BodyText:  "unfinished",
		MessageID: "<draft@example.test>",
		Date:      time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("\r\nBcc: <hidden@example.test>\r\n")) {
		t.Fatalf("draft raw missing Bcc header:\n%s", raw)
	}
	if !bytes.Contains(raw, []byte("unfinished")) {
		t.Fatalf("draft raw missing body:\n%s", raw)
	}
}

func TestBuildRawWithHTMLBodyUsesMultipartAlternative(t *testing.T) {
	raw, _, err := BuildRaw(Message{
		From:      "sender@example.test",
		To:        []string{"to@example.test"},
		Subject:   "Rich",
		BodyText:  "Hello rich mail",
		BodyHTML:  "<p><strong>Hello</strong> rich mail</p>",
		MessageID: "<rich@example.test>",
		Date:      time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	lower := bytes.ToLower(raw)
	if !bytes.Contains(lower, []byte("content-type: multipart/alternative")) {
		t.Fatalf("raw message is not multipart alternative:\n%s", raw)
	}
	if !bytes.Contains(raw, []byte("Hello rich mail")) || !bytes.Contains(raw, []byte("<strong>Hello</strong>")) {
		t.Fatalf("raw message missing text/html parts:\n%s", raw)
	}
}

func TestBuildRawWithAttachmentUsesMultipartMixed(t *testing.T) {
	raw, _, err := BuildRaw(Message{
		From:      "sender@example.test",
		To:        []string{"to@example.test"},
		Subject:   "Attachment",
		BodyText:  "See attached",
		MessageID: "<attach@example.test>",
		Date:      time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
		Attachments: []Attachment{{
			Filename:    "report.txt",
			ContentType: "text/plain",
			Data:        []byte("hello attachment"),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	lower := bytes.ToLower(raw)
	for _, want := range [][]byte{
		[]byte("content-type: multipart/mixed"),
		[]byte("content-disposition: attachment"),
		[]byte("filename=report.txt"),
		[]byte("content-transfer-encoding: base64"),
		[]byte("aGVsbG8gYXR0YWNobWVudA=="),
	} {
		if !bytes.Contains(lower, bytes.ToLower(want)) {
			t.Fatalf("raw message missing %q:\n%s", want, raw)
		}
	}
}

func TestBuildRawWithInlineAttachmentUsesRelatedContentID(t *testing.T) {
	raw, _, err := BuildRaw(Message{
		From:      "sender@example.test",
		To:        []string{"to@example.test"},
		Subject:   "Inline",
		BodyText:  "Inline image",
		BodyHTML:  `<p>Inline <img src="cid:image-1@compose.local"></p>`,
		MessageID: "<inline@example.test>",
		Date:      time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
		Attachments: []Attachment{{
			Filename:    "image.png",
			ContentType: "image/png",
			ContentID:   "image-1@compose.local",
			Inline:      true,
			Data:        []byte{0x89, 0x50, 0x4e, 0x47},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	lower := bytes.ToLower(raw)
	for _, want := range [][]byte{
		[]byte("content-type: multipart/related"),
		[]byte("content-id: <image-1@compose.local>"),
		[]byte("content-disposition: inline"),
		[]byte("content-type: image/png"),
	} {
		if !bytes.Contains(lower, bytes.ToLower(want)) {
			t.Fatalf("raw message missing %q:\n%s", want, raw)
		}
	}
}
