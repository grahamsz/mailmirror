// File overview: Email document rendering helpers for safe in-app message display.

package web

import (
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"rolltop/backend/store"
)

var plainTextURLRE = regexp.MustCompile(`https?://[^\s<>"']+`)

func replySubject(subject string) string {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		subject = "(no subject)"
	}
	if strings.HasPrefix(strings.ToLower(subject), "re:") {
		return subject
	}
	return "Re: " + subject
}

func emailDocument(bodyHTML, bodyText string, allowRemoteImages bool) string {
	return emailDocumentWithBlocklist(bodyHTML, bodyText, allowRemoteImages, nil)
}

func emailDocumentWithBlocklist(bodyHTML, bodyText string, allowRemoteImages bool, blockedImagePatterns []string) string {
	return emailDocumentWithInlineAttachments(bodyHTML, bodyText, allowRemoteImages, blockedImagePatterns, nil)
}

func emailDocumentWithInlineAttachments(bodyHTML, bodyText string, allowRemoteImages bool, blockedImagePatterns []string, attachments []store.Attachment) string {
	plainTextDoc := false
	if strings.TrimSpace(bodyHTML) == "" {
		plainTextDoc = true
		bodyHTML = `<div class="plaintext">` + plainTextBodyHTML(bodyText) + `</div>`
	}
	bodyHTML = strings.ReplaceAll(bodyHTML, "\x00", "")
	bodyHTML = replaceInlineCIDRefs(bodyHTML, attachments)
	if allowRemoteImages {
		bodyHTML = normalizeProtocolRelativeRemoteRefs(bodyHTML)
		bodyHTML = removeBlockedRemoteImages(bodyHTML, blockedImagePatterns)
	}
	imgSrc := "'self' data: cid:"
	styleSrc := "'unsafe-inline'"
	fontSrc := "data:"
	if allowRemoteImages {
		imgSrc = "'self' data: cid: http: https:"
		styleSrc = "'unsafe-inline' http: https:"
		fontSrc = "data: http: https:"
	}
	documentClass := ""
	if plainTextDoc {
		documentClass = ` class="plaintext-doc"`
	}
	documentCSS := `html,body{margin:0;padding:0;background:#fff;color:#1f2328;font:14px/1.55 Arial,sans-serif;overflow:hidden}body{padding:18px}.plaintext{white-space:pre-wrap;overflow-wrap:anywhere;font:14px/1.55 Arial,sans-serif}.plaintext a{color:#245f80;text-decoration:none;border-bottom:1px solid #9cc5d8}pre{white-space:pre-wrap;overflow-wrap:anywhere}table{max-width:100%}img{max-width:100%;height:auto}html[data-rolltop-theme="classic_dark"],html[data-rolltop-theme="classic_dark"] body{background:#151f1c!important;color:#e6eee9!important;color-scheme:dark}html[data-rolltop-theme="classic_dark"] body :where(div,p,span,blockquote,pre,td,th,li){background:transparent!important;color:inherit!important;border-color:rgba(174,190,183,.28)!important}html[data-rolltop-theme="classic_dark"] a{color:#8bd4c8!important;border-bottom-color:rgba(139,212,200,.5)!important}html[data-rolltop-theme="matrix"],html[data-rolltop-theme="matrix"] body{background:#06130d!important;color:#dcffe9!important;color-scheme:dark}html[data-rolltop-theme="matrix"] body :where(div,p,span,blockquote,pre,td,th,li){background:transparent!important;color:inherit!important;border-color:rgba(74,222,128,.24)!important}html[data-rolltop-theme="matrix"] a{color:#7dffbf!important;border-bottom-color:rgba(125,255,191,.5)!important}`
	return `<!doctype html><html` + documentClass + `><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><base target="_blank"><meta name="referrer" content="no-referrer"><meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src ` + imgSrc + `; style-src ` + styleSrc + `; font-src ` + fontSrc + `;"><style>` + documentCSS + `</style></head><body>` + bodyHTML + `</body></html>`
}

var cidURLRE = regexp.MustCompile(`(?i)cid:([^\s"'<>\)]+)`)

func replaceInlineCIDRefs(bodyHTML string, attachments []store.Attachment) string {
	if len(attachments) == 0 || !strings.Contains(strings.ToLower(bodyHTML), "cid:") {
		return bodyHTML
	}
	urlsByCID := inlineAttachmentURLsByCID(attachments)
	if len(urlsByCID) == 0 {
		return bodyHTML
	}
	return cidURLRE.ReplaceAllStringFunc(bodyHTML, func(match string) string {
		parts := cidURLRE.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		if replacement, ok := urlsByCID[normalizeContentID(parts[1])]; ok {
			return replacement
		}
		return match
	})
}

func inlineAttachmentURLsByCID(attachments []store.Attachment) map[string]string {
	out := make(map[string]string)
	for _, att := range attachments {
		key := normalizeContentID(att.ContentID)
		if key == "" || att.ID <= 0 {
			continue
		}
		out[key] = "/attachments/" + strconv.FormatInt(att.ID, 10) + "/inline"
	}
	return out
}

func normalizeContentID(value string) string {
	value = strings.TrimSpace(html.UnescapeString(value))
	value = strings.Trim(value, "<>'\" ")
	if strings.HasPrefix(strings.ToLower(value), "cid:") {
		value = value[4:]
	}
	if decoded, err := url.PathUnescape(value); err == nil {
		value = decoded
	}
	value = strings.TrimSpace(html.UnescapeString(value))
	value = strings.Trim(value, "<>'\" ")
	return strings.ToLower(value)
}

func normalizeProtocolRelativeRemoteRefs(value string) string {
	replacements := []struct {
		old string
		new string
	}{
		{`src="//`, `src="https://`},
		{`src='//`, `src='https://`},
		{`srcset="//`, `srcset="https://`},
		{`srcset='//`, `srcset='https://`},
		{`href="//`, `href="https://`},
		{`href='//`, `href='https://`},
		{`url(//`, `url(https://`},
	}
	for _, repl := range replacements {
		value = strings.ReplaceAll(value, repl.old, repl.new)
	}
	return value
}

var (
	emailImageTagRE = regexp.MustCompile(`(?is)<img\b[^>]*>`)
	imageURLAttrRE  = regexp.MustCompile(`(?is)\b(?:src|srcset)\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
)

func removeBlockedRemoteImages(bodyHTML string, patterns []string) string {
	blockers := compileRemoteImageBlockPatterns(patterns)
	if len(blockers) == 0 {
		return bodyHTML
	}
	return emailImageTagRE.ReplaceAllStringFunc(bodyHTML, func(tag string) string {
		for _, candidate := range imageURLCandidatesFromTag(tag) {
			for _, blocker := range blockers {
				if blocker.MatchString(candidate) {
					return ""
				}
			}
		}
		return tag
	})
}

func compileRemoteImageBlockPatterns(patterns []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		re, err := regexp.Compile(pattern)
		if err == nil {
			out = append(out, re)
		}
	}
	return out
}

func imageURLCandidatesFromTag(tag string) []string {
	var out []string
	for _, match := range imageURLAttrRE.FindAllStringSubmatch(tag, -1) {
		value := ""
		for _, candidate := range match[2:] {
			if candidate != "" {
				value = candidate
				break
			}
		}
		for _, candidate := range srcsetURLCandidates(value) {
			if candidate != "" {
				out = append(out, candidate)
			}
		}
	}
	return out
}

func srcsetURLCandidates(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		out = append(out, strings.TrimSpace(fields[0]))
	}
	return out
}

func plainTextBodyHTML(bodyText string) string {
	bodyText = strings.ReplaceAll(bodyText, "\x00", "")
	matches := plainTextURLRE.FindAllStringIndex(bodyText, -1)
	if len(matches) == 0 {
		return html.EscapeString(bodyText)
	}
	var b strings.Builder
	last := 0
	for _, match := range matches {
		if match[0] < last {
			continue
		}
		b.WriteString(html.EscapeString(bodyText[last:match[0]]))
		rawMatch := bodyText[match[0]:match[1]]
		rawURL, trailing := splitTrailingURLPunctuation(rawMatch)
		if rawURL == "" {
			b.WriteString(html.EscapeString(rawMatch))
		} else {
			escapedURL := html.EscapeString(rawURL)
			b.WriteString(`<a href="` + escapedURL + `" target="_blank" rel="noreferrer noopener">` + html.EscapeString(shortURLLabel(rawURL)) + `</a>`)
			b.WriteString(html.EscapeString(trailing))
		}
		last = match[1]
	}
	b.WriteString(html.EscapeString(bodyText[last:]))
	return b.String()
}

func splitTrailingURLPunctuation(value string) (string, string) {
	cut := len(value)
	for cut > 0 {
		r := rune(value[cut-1])
		size := 1
		if r >= 0x80 {
			r, size = lastRune(value[:cut])
		}
		if !strings.ContainsRune(".,;:!?)\"]}", r) {
			break
		}
		cut -= size
	}
	return value[:cut], value[cut:]
}

func lastRune(value string) (rune, int) {
	runes := []rune(value)
	if len(runes) == 0 {
		return 0, 0
	}
	r := runes[len(runes)-1]
	return r, len(string(r))
}

func shortURLLabel(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return truncateMiddle(rawURL, 76)
	}
	label := u.Host
	if u.EscapedPath() != "" && u.EscapedPath() != "/" {
		label += u.EscapedPath()
	}
	if u.RawQuery != "" || u.Fragment != "" {
		label += "?..."
	}
	return truncateMiddle(label, 76)
}

func truncateMiddle(value string, maxRunes int) string {
	runes := []rune(value)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return value
	}
	if maxRunes <= 8 {
		return string(runes[:maxRunes])
	}
	head := (maxRunes - 3) / 2
	tail := maxRunes - 3 - head
	return string(runes[:head]) + "..." + string(runes[len(runes)-tail:])
}
