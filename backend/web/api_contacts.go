// File overview: Contact CRUD, vCard import/export, and me-contact API handlers.

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/mail"
	"strconv"
	"strings"

	"rolltop/backend/store"
)

const maxContactIconBytes = 2 << 20

func (s *Server) handleContactOrApp(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/contacts/"), "/"), "/")
	if len(parts) == 2 && parts[1] == "icon" {
		s.handleContactIcon(w, r)
		return
	}
	s.handleApp(w, r)
}

func (s *Server) apiContacts(w http.ResponseWriter, r *http.Request) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		contacts, err := s.store.ListContactsForUser(r.Context(), cu.User.ID, r.URL.Query().Get("q"), 500)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		writeJSON(w, map[string]any{"contacts": apiContactsFromStore(contacts)})
	case http.MethodPost:
		if !s.verifyCSRF(w, r) {
			return
		}
		var in apiContact
		if !decodeJSON(w, r, &in) {
			return
		}
		contact, err := s.store.CreateContact(r.Context(), cu.User.ID, contactFromAPI(in))
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "could not save contact")
			return
		}
		s.clearComposeIdentityCache(cu.User.ID)
		writeJSON(w, map[string]any{"contact": apiContactFromStore(contact)})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) apiContactPath(w http.ResponseWriter, r *http.Request, rest string) {
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	rest = strings.Trim(rest, "/")
	switch rest {
	case "autocomplete":
		s.apiContactAutocomplete(w, r, cu)
		return
	case "import":
		s.apiImportContacts(w, r, cu)
		return
	case "export":
		s.apiExportContacts(w, r, cu)
		return
	}
	parts := strings.Split(rest, "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 1 {
		s.apiContact(w, r, cu, id)
		return
	}
	if len(parts) == 2 && parts[1] == "icon" {
		s.apiContactIcon(w, r, cu, id)
		return
	}
	http.NotFound(w, r)
}

func (s *Server) apiContact(w http.ResponseWriter, r *http.Request, cu currentUser, id int64) {
	switch r.Method {
	case http.MethodGet:
		contact, err := s.store.GetContactForUser(r.Context(), cu.User.ID, id)
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		writeJSON(w, map[string]any{"contact": apiContactFromStore(contact)})
	case http.MethodPut:
		if !s.verifyCSRF(w, r) {
			return
		}
		var in apiContact
		if !decodeJSON(w, r, &in) {
			return
		}
		contact, err := s.store.UpdateContact(r.Context(), cu.User.ID, id, contactFromAPI(in))
		if store.IsNotFound(err) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "could not save contact")
			return
		}
		s.clearComposeIdentityCache(cu.User.ID)
		writeJSON(w, map[string]any{"contact": apiContactFromStore(contact)})
	case http.MethodDelete:
		if !s.verifyCSRF(w, r) {
			return
		}
		if err := s.store.DeleteContactForUser(r.Context(), cu.User.ID, id); err != nil {
			if store.IsNotFound(err) {
				http.NotFound(w, r)
				return
			}
			s.serverError(w, r, err)
			return
		}
		s.clearComposeIdentityCache(cu.User.ID)
		writeJSON(w, map[string]any{"ok": true})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) apiContactAutocomplete(w http.ResponseWriter, r *http.Request, cu currentUser) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	items, err := s.store.AutocompleteContactsForUser(r.Context(), cu.User.ID, r.URL.Query().Get("q"), 12)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	out := make([]apiContactAutocomplete, 0, len(items))
	for _, item := range items {
		out = append(out, apiContactAutocomplete{
			ContactID: item.ContactID,
			Name:      item.Name,
			Email:     item.Email,
			Label:     item.Label,
			IconURL:   item.IconURL,
		})
	}
	writeJSON(w, map[string]any{"contacts": out})
}

func (s *Server) apiContactIcon(w http.ResponseWriter, r *http.Request, cu currentUser, contactID int64) {
	switch r.Method {
	case http.MethodPost:
		if !s.verifyCSRF(w, r) {
			return
		}
		if s.blobs == nil {
			writeAPIError(w, http.StatusServiceUnavailable, "blob storage is not configured")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxContactIconBytes+1024)
		file, header, err := r.FormFile("icon")
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "upload an image")
			return
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, maxContactIconBytes+1))
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if len(data) == 0 || len(data) > maxContactIconBytes {
			writeAPIError(w, http.StatusBadRequest, "image must be 2 MB or smaller")
			return
		}
		contentType := detectContactIconType(data, header.Header.Get("Content-Type"))
		if contentType == "" {
			writeAPIError(w, http.StatusBadRequest, "unsupported image type")
			return
		}
		saved, err := s.blobs.SaveContactIcon(cu.User.ID, contactID, header.Filename, data)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		blob, err := s.store.CreateBlob(r.Context(), store.BlobRecord{
			UserID: cu.User.ID,
			Kind:   "contact_icon",
			Path:   saved.Path,
			SHA256: saved.SHA256,
			Size:   saved.Size,
		})
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if _, err := s.store.SetContactIcon(r.Context(), cu.User.ID, contactID, blob.ID, contentType, header.Filename, saved.Size); err != nil {
			if store.IsNotFound(err) {
				http.NotFound(w, r)
				return
			}
			s.serverError(w, r, err)
			return
		}
		s.clearSenderContactIconCache(cu.User.ID)
		s.clearComposeIdentityCache(cu.User.ID)
		contact, err := s.store.GetContactForUser(r.Context(), cu.User.ID, contactID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		writeJSON(w, map[string]any{"contact": apiContactFromStore(contact)})
	case http.MethodDelete:
		if !s.verifyCSRF(w, r) {
			return
		}
		if err := s.store.DeleteContactIconForUser(r.Context(), cu.User.ID, contactID); err != nil {
			if store.IsNotFound(err) {
				http.NotFound(w, r)
				return
			}
			s.serverError(w, r, err)
			return
		}
		s.clearSenderContactIconCache(cu.User.ID)
		s.clearComposeIdentityCache(cu.User.ID)
		contact, err := s.store.GetContactForUser(r.Context(), cu.User.ID, contactID)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		writeJSON(w, map[string]any{"contact": apiContactFromStore(contact)})
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) handleContactIcon(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	cu, ok := current(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/contacts/"), "/"), "/")
	if len(parts) != 2 || parts[1] != "icon" {
		http.NotFound(w, r)
		return
	}
	contactID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || contactID <= 0 {
		http.NotFound(w, r)
		return
	}
	icon, err := s.store.GetContactIconForUser(r.Context(), cu.User.ID, contactID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if s.blobs == nil {
		http.NotFound(w, r)
		return
	}
	f, err := s.blobs.OpenUserBlob(cu.User.ID, icon.BlobPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", icon.ContentType)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, icon.Filename, icon.UpdatedAt, f)
}

func (s *Server) apiAddSenderContact(w http.ResponseWriter, r *http.Request, messageID int64) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	cu, ok := s.requireAPIAuth(w, r)
	if !ok {
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	msg, err := s.store.GetMessageForUser(r.Context(), cu.User.ID, messageID)
	if store.IsNotFound(err) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	contact, created, err := s.addSenderContact(r.Context(), cu.User.ID, msg.FromAddr)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "could not add sender")
		return
	}
	s.clearComposeIdentityCache(cu.User.ID)
	writeJSON(w, map[string]any{"contact": apiContactFromStore(contact), "created": created})
}

func (s *Server) addSenderContact(ctx context.Context, userID int64, from string) (store.Contact, bool, error) {
	addr, err := mail.ParseAddress(strings.TrimSpace(from))
	if err != nil || strings.TrimSpace(addr.Address) == "" {
		email := senderEmail(from)
		if email == "" {
			return store.Contact{}, false, fmt.Errorf("sender has no email")
		}
		addr = &mail.Address{Name: senderDisplayName(from), Address: email}
	}
	if existing, err := s.store.GetContactByEmailForUser(ctx, userID, addr.Address); err == nil {
		if strings.TrimSpace(existing.DisplayName) == "" && strings.TrimSpace(addr.Name) != "" {
			existing.DisplayName = strings.TrimSpace(addr.Name)
			updated, err := s.store.UpdateContact(ctx, userID, existing.ID, existing)
			return updated, false, err
		}
		return existing, false, nil
	} else if !store.IsNotFound(err) {
		return store.Contact{}, false, err
	}
	name := strings.TrimSpace(addr.Name)
	if name == "" {
		name = strings.TrimSpace(addr.Address)
	}
	contact, err := s.store.CreateContact(ctx, userID, store.Contact{
		DisplayName: name,
		Emails: []store.ContactEmail{{
			Label:     "Email",
			Email:     addr.Address,
			IsPrimary: true,
		}},
	})
	return contact, true, err
}

func (s *Server) apiImportContacts(w http.ResponseWriter, r *http.Request, cu currentUser) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !s.verifyCSRF(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	data, err := readImportBody(r)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "upload a vCard file")
		return
	}
	contacts, err := parseVCards(data)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "could not parse vCard data")
		return
	}
	imported := 0
	updated := 0
	for _, contact := range contacts {
		if len(contact.Emails) == 0 && strings.TrimSpace(contact.DisplayName+contact.GivenName+contact.FamilyName+contact.Organization) == "" {
			continue
		}
		existing, ok, err := s.findImportMergeTarget(r.Context(), cu.User.ID, contact)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		if ok {
			merged := mergeImportedContact(existing, contact)
			if _, err := s.store.UpdateContact(r.Context(), cu.User.ID, existing.ID, merged); err != nil {
				writeAPIError(w, http.StatusBadRequest, "could not import contacts")
				return
			}
			updated++
			continue
		}
		if _, err := s.store.CreateContact(r.Context(), cu.User.ID, contact); err != nil {
			writeAPIError(w, http.StatusBadRequest, "could not import contacts")
			return
		}
		imported++
	}
	if imported > 0 || updated > 0 {
		s.clearComposeIdentityCache(cu.User.ID)
	}
	writeJSON(w, map[string]any{"ok": true, "imported": imported, "updated": updated})
}

func (s *Server) apiExportContacts(w http.ResponseWriter, r *http.Request, cu currentUser) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	contacts, err := s.store.ListContactsForUser(r.Context(), cu.User.ID, "", 10000)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data := writeVCards(contacts)
	w.Header().Set("Content-Type", "text/vcard; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="rolltop-contacts.vcf"`)
	_, _ = w.Write(data)
}

func (s *Server) findImportMergeTarget(ctx context.Context, userID int64, contact store.Contact) (store.Contact, bool, error) {
	for _, email := range contact.Emails {
		existing, err := s.store.GetContactByEmailForUser(ctx, userID, email.Email)
		if err == nil {
			return existing, true, nil
		}
		if !store.IsNotFound(err) {
			return store.Contact{}, false, err
		}
	}
	return store.Contact{}, false, nil
}

func contactFromAPI(in apiContact) store.Contact {
	c := store.Contact{
		NamePrefix:     in.NamePrefix,
		GivenName:      in.GivenName,
		AdditionalName: in.AdditionalName,
		FamilyName:     in.FamilyName,
		NameSuffix:     in.NameSuffix,
		DisplayName:    in.DisplayName,
		Nickname:       in.Nickname,
		Organization:   in.Organization,
		Department:     in.Department,
		JobTitle:       in.JobTitle,
		Birthday:       in.Birthday,
		Notes:          in.Notes,
		Categories:     in.Categories,
		IsMe:           in.IsMe,
		IsPrimary:      in.IsPrimary,
	}
	for _, item := range in.Emails {
		c.Emails = append(c.Emails, store.ContactEmail{Label: item.Label, Email: item.Email, IsPrimary: item.IsPrimary})
	}
	for _, item := range in.Phones {
		c.Phones = append(c.Phones, store.ContactPhone{Label: item.Label, Number: item.Number, IsPrimary: item.IsPrimary})
	}
	for _, item := range in.Addresses {
		c.Addresses = append(c.Addresses, store.ContactAddress{Label: item.Label, Street: item.Street, Locality: item.Locality, Region: item.Region, PostalCode: item.PostalCode, Country: item.Country, IsPrimary: item.IsPrimary})
	}
	for _, item := range in.URLs {
		c.URLs = append(c.URLs, store.ContactURL{Label: item.Label, URL: item.URL, IsPrimary: item.IsPrimary})
	}
	return c
}

func readImportBody(r *http.Request) ([]byte, error) {
	contentType := r.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if strings.HasPrefix(mediaType, "multipart/") {
		file, _, err := r.FormFile("file")
		if err != nil {
			return nil, err
		}
		defer file.Close()
		return io.ReadAll(file)
	}
	return io.ReadAll(r.Body)
}

func detectContactIconType(data []byte, declared string) string {
	detected := http.DetectContentType(data)
	if strings.HasPrefix(detected, "image/jpeg") {
		return "image/jpeg"
	}
	if strings.HasPrefix(detected, "image/png") {
		return "image/png"
	}
	if strings.HasPrefix(detected, "image/gif") {
		return "image/gif"
	}
	if bytes.HasPrefix(data, []byte("RIFF")) && len(data) > 12 && string(data[8:12]) == "WEBP" {
		return "image/webp"
	}
	declared = strings.ToLower(strings.TrimSpace(strings.Split(declared, ";")[0]))
	if declared == "image/webp" {
		return "image/webp"
	}
	return ""
}

func mergeImportedContact(existing, incoming store.Contact) store.Contact {
	merged := existing
	if strings.TrimSpace(merged.NamePrefix) == "" {
		merged.NamePrefix = incoming.NamePrefix
	}
	if strings.TrimSpace(merged.GivenName) == "" {
		merged.GivenName = incoming.GivenName
	}
	if strings.TrimSpace(merged.AdditionalName) == "" {
		merged.AdditionalName = incoming.AdditionalName
	}
	if strings.TrimSpace(merged.FamilyName) == "" {
		merged.FamilyName = incoming.FamilyName
	}
	if strings.TrimSpace(merged.NameSuffix) == "" {
		merged.NameSuffix = incoming.NameSuffix
	}
	if strings.TrimSpace(merged.DisplayName) == "" {
		merged.DisplayName = incoming.DisplayName
	}
	if strings.TrimSpace(merged.Nickname) == "" {
		merged.Nickname = incoming.Nickname
	}
	if strings.TrimSpace(merged.Organization) == "" {
		merged.Organization = incoming.Organization
	}
	if strings.TrimSpace(merged.Department) == "" {
		merged.Department = incoming.Department
	}
	if strings.TrimSpace(merged.JobTitle) == "" {
		merged.JobTitle = incoming.JobTitle
	}
	if strings.TrimSpace(merged.Birthday) == "" {
		merged.Birthday = incoming.Birthday
	}
	if strings.TrimSpace(merged.Notes) == "" {
		merged.Notes = incoming.Notes
	}
	if strings.TrimSpace(merged.Categories) == "" {
		merged.Categories = incoming.Categories
	}
	merged.Emails = mergeContactEmails(merged.Emails, incoming.Emails)
	merged.Phones = mergeContactPhones(merged.Phones, incoming.Phones)
	merged.Addresses = mergeContactAddresses(merged.Addresses, incoming.Addresses)
	merged.URLs = mergeContactURLs(merged.URLs, incoming.URLs)
	return merged
}

func mergeContactEmails(existing, incoming []store.ContactEmail) []store.ContactEmail {
	seen := map[string]bool{}
	out := append([]store.ContactEmail{}, existing...)
	for _, email := range existing {
		seen[store.NormalizeContactEmail(email.Email)] = true
	}
	for _, email := range incoming {
		key := store.NormalizeContactEmail(email.Email)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, email)
	}
	return out
}

func mergeContactPhones(existing, incoming []store.ContactPhone) []store.ContactPhone {
	seen := map[string]bool{}
	out := append([]store.ContactPhone{}, existing...)
	for _, phone := range existing {
		seen[strings.ToLower(strings.TrimSpace(phone.Number))] = true
	}
	for _, phone := range incoming {
		key := strings.ToLower(strings.TrimSpace(phone.Number))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, phone)
	}
	return out
}

func mergeContactAddresses(existing, incoming []store.ContactAddress) []store.ContactAddress {
	seen := map[string]bool{}
	out := append([]store.ContactAddress{}, existing...)
	for _, addr := range existing {
		seen[addressKey(addr)] = true
	}
	for _, addr := range incoming {
		key := addressKey(addr)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, addr)
	}
	return out
}

func mergeContactURLs(existing, incoming []store.ContactURL) []store.ContactURL {
	seen := map[string]bool{}
	out := append([]store.ContactURL{}, existing...)
	for _, u := range existing {
		seen[strings.ToLower(strings.TrimSpace(u.URL))] = true
	}
	for _, u := range incoming {
		key := strings.ToLower(strings.TrimSpace(u.URL))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, u)
	}
	return out
}

func addressKey(addr store.ContactAddress) string {
	return strings.ToLower(strings.Join([]string{
		strings.TrimSpace(addr.Street),
		strings.TrimSpace(addr.Locality),
		strings.TrimSpace(addr.Region),
		strings.TrimSpace(addr.PostalCode),
		strings.TrimSpace(addr.Country),
	}, "|"))
}

func decodeContactJSON(data []byte) (apiContact, error) {
	var c apiContact
	err := json.Unmarshal(data, &c)
	return c, err
}
