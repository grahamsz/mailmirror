// File overview: Tests for quote clipping and forwarded-message display behavior.

package web

import (
	"strings"
	"testing"

	"rolltop/backend/store"
)

func TestClipTextQuoteUsesStandardReplyMarker(t *testing.T) {
	body := "Thanks, that works for me.\n\nOn Tue, Alice <alice@example.test> wrote:\n> The earlier note\n> with quoted details"
	displayHTML, displayText, hidden := clippedEmailBody("", body, nil)
	if displayHTML != "" {
		t.Fatalf("displayHTML = %q", displayHTML)
	}
	if !hidden {
		t.Fatal("expected quoted text to be hidden")
	}
	if displayText != "Thanks, that works for me." {
		t.Fatalf("displayText = %q", displayText)
	}
}

func TestClipTextQuoteUsesPriorMessageOverlap(t *testing.T) {
	previous := strings.Join([]string{
		"The prior message has enough text to be recognized later.",
		"It spans multiple lines so the overlap is not accidental.",
		"The final line closes the repeated copied section cleanly.",
	}, "\n")
	current := "Fresh reply at the top.\n\n" + previous

	_, displayText, hidden := clippedEmailBody("", current, []string{previous})
	if !hidden {
		t.Fatal("expected repeated prior message to be hidden")
	}
	if displayText != "Fresh reply at the top." {
		t.Fatalf("displayText = %q", displayText)
	}
}

func TestClipTextQuoteSkipsLeadingQuotedPrefaceBeforeFreshReply(t *testing.T) {
	body := strings.Join([]string{
		"On 10/4/07, InjuryProneErik <injprone@gmail.com> wrote:",
		"> It reads first like a mail order bride, but turn into nigerian sneakiness with a drop of the pant...err, a hat!",
		"",
		"*cracking knuckles*",
		"You are now mesmerized by the thought of nailing an exotic European woman.",
		"",
		"On 10/4/07, InjuryProneErik <injprone@gmail.com> wrote:",
		"> It reads first like a mail order bride, but turn into nigerian sneakiness with a drop of the pant...err, a hat!",
	}, "\n")

	_, displayText, hidden := clippedEmailBody("", body, nil)
	if !hidden {
		t.Fatal("expected leading quoted preface to be hidden")
	}
	if !strings.HasPrefix(displayText, "*cracking knuckles*") {
		t.Fatalf("displayText starts with wrong fragment: %q", displayText)
	}
	if strings.HasPrefix(displayText, "On 10/4/07") {
		t.Fatalf("displayText kept leading quote attribution: %q", displayText)
	}
}

func TestClipTextQuoteKeepsQuotedOnlyReplyVisible(t *testing.T) {
	body := strings.Join([]string{
		"On Tue, Alice <alice@example.test> wrote:",
		"> The earlier note",
		"> with quoted details",
	}, "\n")

	_, displayText, hidden := clippedEmailBody("", body, nil)
	if hidden {
		t.Fatal("did not expect quoted-only body to be hidden")
	}
	if displayText != body {
		t.Fatalf("displayText = %q", displayText)
	}
}

func TestClipTextQuoteKeepsForwardedBlockWithInlineCommentVisible(t *testing.T) {
	body := strings.Join([]string{
		"Initial note before the forward.",
		"",
		"---------- Forwarded message ---------",
		"From: Jennifer Welsh <jenny@example.test>",
		"Date: Oct 4, 2007 12:54 PM",
		"Subject: Your new friend",
		"To: Erik <erik@example.test>",
		"",
		"Forwarded body that provides context.",
		"",
		"Inline comment added underneath the forwarded note.",
	}, "\n")

	_, displayText, hidden := clippedEmailBody("", body, nil)
	if hidden {
		t.Fatal("did not expect forwarded body with inline comment to be hidden")
	}
	if !strings.Contains(displayText, "Forwarded body that provides context.") || !strings.Contains(displayText, "Inline comment added underneath") {
		t.Fatalf("displayText lost forwarded or inline content: %q", displayText)
	}
}

func TestClipTextQuoteKeepsInlineCommentAfterQuotedOverlapVisible(t *testing.T) {
	previous := strings.Join([]string{
		"The prior message has enough text to be recognized later.",
		"It spans multiple lines so the overlap is not accidental.",
		"The final line closes the repeated copied section cleanly.",
	}, "\n")
	body := "Fresh reply at the top.\n\n" + previous + "\n\nInline comment after the quoted prior message."

	_, displayText, hidden := clippedEmailBody("", body, []string{previous})
	if hidden {
		t.Fatal("did not expect inline comment after quoted overlap to be hidden")
	}
	if !strings.Contains(displayText, "Inline comment after the quoted prior message") {
		t.Fatalf("displayText lost inline comment: %q", displayText)
	}
}

func TestClipTextQuoteKeepsInlineCommentAfterReplyQuoteVisible(t *testing.T) {
	body := strings.Join([]string{
		"Fresh reply at the top.",
		"",
		"On Tue, Alice <alice@example.test> wrote:",
		"> The earlier note",
		"> with quoted details",
		"",
		"Inline comment below the quoted note.",
	}, "\n")

	_, displayText, hidden := clippedEmailBody("", body, nil)
	if hidden {
		t.Fatal("did not expect inline comment after reply quote to be hidden")
	}
	if !strings.Contains(displayText, "Inline comment below the quoted note") {
		t.Fatalf("displayText lost inline comment: %q", displayText)
	}
}

func TestClipHTMLQuoteKeepsRichPrefix(t *testing.T) {
	body := `<div><p>Fresh answer with enough text to keep as rich HTML.</p><blockquote type="cite"><p>Older copied text</p></blockquote></div>`
	displayHTML, _, hidden := clippedEmailBody(body, "Fresh answer with enough text to keep as rich HTML.\n\nOlder copied text", nil)
	if !hidden {
		t.Fatal("expected HTML quote to be hidden")
	}
	if strings.Contains(displayHTML, "Older copied text") {
		t.Fatalf("displayHTML still contains quoted content: %s", displayHTML)
	}
	if !strings.Contains(displayHTML, "Fresh answer") {
		t.Fatalf("displayHTML lost fresh content: %s", displayHTML)
	}
}

func TestClipHTMLQuoteFallsBackToTextForAttributionOnlyInlineReply(t *testing.T) {
	bodyHTML := `<div>On 10/4/07, InjuryProneErik &lt;<a href="mailto:injprone@gmail.com">injprone@gmail.com</a>&gt; wrote:</div><blockquote><div>Earlier quoted text.</div></blockquote><div>*cracking knuckles*</div><div>Inline reply text below the quote.</div>`
	bodyText := strings.Join([]string{
		"On 10/4/07, InjuryProneErik <injprone@gmail.com> wrote:",
		"> Earlier quoted text.",
		"",
		"*cracking knuckles*",
		"Inline reply text below the quote.",
	}, "\n")

	displayHTML, displayText, hidden := clippedEmailBody(bodyHTML, bodyText, nil)
	if !hidden {
		t.Fatal("expected quoted text to remain hidden")
	}
	if displayHTML != "" {
		t.Fatalf("expected text fallback, got displayHTML = %q", displayHTML)
	}
	if !strings.HasPrefix(displayText, "*cracking knuckles*") || !strings.Contains(displayText, "Inline reply text") {
		t.Fatalf("displayText = %q", displayText)
	}
}

func TestClipHTMLQuoteDoesNotHideForwardedHTMLMessage(t *testing.T) {
	bodyHTML := `<div>Fresh intro.</div><div>---------- Forwarded message ---------</div><span class="gmail_quote">From: Sender</span><blockquote class="gmail_quote"><div>Forwarded body.</div></blockquote>`
	bodyText := "Fresh intro.\n\n---------- Forwarded message ---------\nFrom: Sender\n\nForwarded body."
	displayHTML, _, hidden := clippedEmailBody(bodyHTML, bodyText, nil)
	if hidden {
		t.Fatal("did not expect forwarded HTML message to be hidden")
	}
	if displayHTML != bodyHTML {
		t.Fatalf("displayHTML = %q", displayHTML)
	}
}

func TestClipHTMLQuoteKeepsForwardedHTMLWhenTextQuoteWasClipped(t *testing.T) {
	bodyHTML := `<div>On 10/4/07, Sender wrote:</div><blockquote class="gmail_quote"><div>Earlier note.</div></blockquote><div>*cracking knuckles*</div><div>---------- Forwarded message ---------</div><blockquote class="gmail_quote"><div>Forwarded body.</div></blockquote><div>Inline comment.</div>`
	bodyText := strings.Join([]string{
		"On 10/4/07, Sender wrote:",
		"> Earlier note.",
		"",
		"*cracking knuckles*",
		"",
		"---------- Forwarded message ---------",
		"> Forwarded body.",
		"",
		"Inline comment.",
	}, "\n")

	displayHTML, displayText, hidden := clippedEmailBody(bodyHTML, bodyText, nil)
	if hidden {
		t.Fatal("did not expect forwarded HTML to be hidden just because text was clipped")
	}
	if displayHTML != bodyHTML {
		t.Fatalf("displayHTML = %q", displayHTML)
	}
	if !strings.HasPrefix(displayText, "*cracking knuckles*") {
		t.Fatalf("displayText = %q", displayText)
	}
}

func TestClipHTMLQuotePreservesForwardedBlockquoteContent(t *testing.T) {
	bodyHTML := `<div>*cracking knuckles*</div><div>---------- Forwarded message ---------</div><blockquote><div>From: Jennifer Welsh &lt;<a href="mailto:jenny@example.test">jenny@example.test</a>&gt;</div><div>Date: Oct 4, 2007 12:54 PM</div><div>Subject: Your new friend</div><br><div>Forwarded body that provides context.</div></blockquote><div>Inline comment added underneath the forwarded note.</div>`
	bodyText := strings.Join([]string{
		"*cracking knuckles*",
		"",
		"---------- Forwarded message ---------",
		"From: Jennifer Welsh <jenny@example.test>",
		"Date: Oct 4, 2007 12:54 PM",
		"Subject: Your new friend",
		"",
		"Forwarded body that provides context.",
		"",
		"Inline comment added underneath the forwarded note.",
	}, "\n")

	displayHTML, displayText, hidden := clippedEmailBody(bodyHTML, bodyText, nil)
	if hidden {
		t.Fatal("did not expect forwarded blockquote content to be hidden")
	}
	if !strings.Contains(displayHTML, "Forwarded body that provides context.") || !strings.Contains(displayHTML, "Inline comment added underneath") {
		t.Fatalf("displayHTML lost forwarded or inline content: %q", displayHTML)
	}
	if displayText != bodyText {
		t.Fatalf("displayText = %q", displayText)
	}
}

func TestClipHTMLQuoteFallsBackToFullTextForInlineCommentAfterForward(t *testing.T) {
	bodyHTML := `<div>Initial note before the forward.</div><blockquote><div>---------- Forwarded message ---------</div><div>Forwarded body that provides context.</div></blockquote><div>Inline comment added underneath the forwarded note.</div>`
	bodyText := strings.Join([]string{
		"Initial note before the forward.",
		"",
		"---------- Forwarded message ---------",
		"Forwarded body that provides context.",
		"",
		"Inline comment added underneath the forwarded note.",
	}, "\n")

	displayHTML, displayText, hidden := clippedEmailBody(bodyHTML, bodyText, nil)
	if hidden {
		t.Fatal("did not expect full text fallback to be hidden")
	}
	if displayHTML != bodyHTML {
		t.Fatalf("displayHTML = %q", displayHTML)
	}
	if !strings.Contains(displayText, "Forwarded body that provides context.") || !strings.Contains(displayText, "Inline comment added underneath") {
		t.Fatalf("displayText lost forwarded or inline content: %q", displayText)
	}
}

func TestClipHTMLQuoteDoesNotRemoveLeadingBlockquoteFormatting(t *testing.T) {
	body := `<blockquote><p>This whole message uses blockquote styling.</p></blockquote>`
	displayHTML, _, hidden := clippedEmailBody(body, "This whole message uses blockquote styling.", nil)
	if hidden {
		t.Fatal("did not expect leading blockquote-only body to be hidden")
	}
	if displayHTML != body {
		t.Fatalf("displayHTML = %q", displayHTML)
	}
}

func TestEmailDocumentRendersPlainTextAsProportionalWrappedText(t *testing.T) {
	doc := emailDocument("", "Hello\n\nThis should wrap like mail.", false)
	if !strings.Contains(doc, `<meta name="viewport" content="width=device-width, initial-scale=1">`) {
		t.Fatalf("missing mobile viewport: %s", doc)
	}
	if strings.Contains(doc, "<pre>") {
		t.Fatalf("plain text rendered as pre: %s", doc)
	}
	if !strings.Contains(doc, `class="plaintext"`) {
		t.Fatalf("missing plaintext wrapper: %s", doc)
	}
	if !strings.Contains(doc, "white-space:pre-wrap") {
		t.Fatalf("missing whitespace preservation: %s", doc)
	}
}

func TestEmailDocumentIncludesDarkThemeStyles(t *testing.T) {
	doc := emailDocument("", "Hello dark mode", false)
	if !strings.Contains(doc, `class="plaintext-doc"`) {
		t.Fatalf("missing plaintext document marker: %s", doc)
	}
	if !strings.Contains(doc, `data-rolltop-theme="classic_dark"`) {
		t.Fatalf("missing classic dark plaintext styles: %s", doc)
	}
	if !strings.Contains(doc, `data-rolltop-theme="matrix"`) {
		t.Fatalf("missing matrix plaintext styles: %s", doc)
	}

	htmlDoc := emailDocument(`<p>Hello HTML dark mode</p>`, "", false)
	if strings.Contains(htmlDoc, `class="plaintext-doc"`) {
		t.Fatalf("html document should not use plaintext marker: %s", htmlDoc)
	}
	if !strings.Contains(htmlDoc, `html[data-rolltop-theme="matrix"],html[data-rolltop-theme="matrix"] body`) {
		t.Fatalf("missing matrix html document styles: %s", htmlDoc)
	}
}

func TestEmailDocumentRewritesInlineCIDImages(t *testing.T) {
	attachments := []store.Attachment{
		{ID: 42, ContentID: "hero@example.test", IsInline: true, ContentType: "image/png"},
		{ID: 43, ContentID: "Logo.JPG", IsInline: true, ContentType: "image/jpeg"},
	}
	body := `<p>Images</p><img src="cid:hero%40example.test"><img src='cid:logo.jpg'><img src="cid:missing">`
	doc := emailDocumentWithInlineAttachments(body, "", false, nil, attachments)
	if !strings.Contains(doc, `src="/attachments/42/inline"`) {
		t.Fatalf("encoded cid was not rewritten: %s", doc)
	}
	if !strings.Contains(doc, `src='/attachments/43/inline'`) {
		t.Fatalf("case-insensitive cid was not rewritten: %s", doc)
	}
	if !strings.Contains(doc, `cid:missing`) {
		t.Fatalf("unknown cid should be left alone: %s", doc)
	}
	if !strings.Contains(doc, `img-src 'self' data: cid:`) {
		t.Fatalf("same-origin inline images not allowed by CSP: %s", doc)
	}
}

func TestEmailDocumentAllowsRemoteStylesAndFontsWithImages(t *testing.T) {
	doc := emailDocumentWithBlocklist(`<link rel="stylesheet" href="//cdn.example.test/mail.css"><style>@font-face{src:url(//cdn.example.test/mail.woff2)}</style>`, "", true, nil)
	if !strings.Contains(doc, `style-src 'unsafe-inline' http: https:`) {
		t.Fatalf("remote styles not allowed by CSP: %s", doc)
	}
	if !strings.Contains(doc, `font-src data: http: https:`) {
		t.Fatalf("remote fonts not allowed by CSP: %s", doc)
	}
	if !strings.Contains(doc, `href="https://cdn.example.test/mail.css"`) {
		t.Fatalf("protocol-relative stylesheet URL not normalized: %s", doc)
	}
	if !strings.Contains(doc, `url(https://cdn.example.test/mail.woff2)`) {
		t.Fatalf("protocol-relative font URL not normalized: %s", doc)
	}
}

func TestEmailDocumentRemovesBlockedRemoteImages(t *testing.T) {
	body := `<p>Brand mail</p><img src="https://track.example.test/open.php?id=1"><img src="https://cdn.example.test/logo.png">`
	doc := emailDocumentWithBlocklist(body, "", true, []string{`(?i)/open\.php`})
	if strings.Contains(doc, "open.php") {
		t.Fatalf("blocked tracker image was retained: %s", doc)
	}
	if !strings.Contains(doc, "logo.png") {
		t.Fatalf("legitimate image was removed: %s", doc)
	}
}
