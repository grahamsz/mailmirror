// File overview: Contact management view. It edits Me identities and address-book data used by
// compose, sender display, reply targeting, and avatar/icon lookup.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { FormEvent, ReactNode } from "react";
import { api } from "../../api";
import type { Contact, ContactAddress, ContactEmail, ContactInteraction, ContactPhone, ContactURL } from "../../types";
import type { Toast } from "../../appTypes";
import { Icon } from "../../components/Icon";
import { messageFromError } from "../../lib/errors";
import { messageURL, searchURL } from "../../lib/routes";
import type { RuntimePlugin } from "../../plugins/runtime";
import { contactKeyEditors } from "../../plugins/contactDetails";

/** ContactsView manages the user address book and Me contacts used by compose/reply identity logic. */
export function ContactsView({
  csrf,
  contactPlugins,
  navigate,
  openCompose,
  addToast
}: {
  csrf: string;
  contactPlugins: readonly RuntimePlugin[];
  navigate: (url: string) => void;
  openCompose: (query?: string) => void;
  addToast: (message: string, kind?: Toast["kind"]) => number;
}) {
  const [contacts, setContacts] = useState<Contact[]>([]);
  const [query, setQuery] = useState("");
  const [selectedID, setSelectedID] = useState<number | "new" | null>(null);
  const [draft, setDraft] = useState<Contact>(() => blankContact());
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [editing, setEditing] = useState(false);
  const [interactions, setInteractions] = useState<ContactInteraction[]>([]);
  const [interactionsLoading, setInteractionsLoading] = useState(false);
  const importRef = useRef<HTMLInputElement | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    try {
      const data = await api.contacts(query);
      const nextContacts = data.contacts || [];
      setContacts(nextContacts);
      if (selectedID === null) {
        const first = nextContacts[0];
        if (first) {
          setSelectedID(first.id);
          setDraft(cloneContact(first));
        } else {
          setSelectedID("new");
          setDraft(blankContact());
          setEditing(true);
        }
      } else if (selectedID !== "new") {
        const selected = nextContacts.find((contact) => contact.id === selectedID);
        if (selected) setDraft(cloneContact(selected));
        else {
          setSelectedID("new");
          setDraft(blankContact());
          setEditing(true);
        }
      }
    } finally {
      setLoading(false);
    }
  }, [query, selectedID]);

  useEffect(() => {
    void load().catch((err) => addToast(messageFromError(err), "error"));
  }, [addToast, load]);

  const selected = useMemo(() => contacts.find((contact) => contact.id === selectedID) || null, [contacts, selectedID]);

  useEffect(() => {
    if (!selected?.id || editing) {
      setInteractions([]);
      setInteractionsLoading(false);
      return;
    }
    let cancelled = false;
    setInteractionsLoading(true);
    api.contactInteractions(selected.id)
      .then((data) => {
        if (!cancelled) setInteractions(data.interactions || []);
      })
      .catch((err) => {
        if (!cancelled) {
          setInteractions([]);
          addToast(messageFromError(err), "error");
        }
      })
      .finally(() => {
        if (!cancelled) setInteractionsLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [addToast, editing, selected?.id]);

  function choose(contact: Contact) {
    setSelectedID(contact.id);
    setDraft(cloneContact(contact));
    setEditing(false);
  }

  function newContact() {
    setSelectedID("new");
    setDraft(blankContact());
    setEditing(true);
  }

  function setField<K extends keyof Contact>(field: K, value: Contact[K]) {
    setDraft((current) => ({ ...current, [field]: value }));
  }

  async function save(event: FormEvent) {
    event.preventDefault();
    setSaving(true);
    try {
      const data = draft.id ? await api.updateContact(csrf, draft) : await api.createContact(csrf, draft);
      addToast("Contact saved.");
      setSelectedID(data.contact.id);
      setDraft(cloneContact(data.contact));
      setContacts((current) => {
        const next = current.some((contact) => contact.id === data.contact.id)
          ? current.map((contact) => contact.id === data.contact.id ? data.contact : contact)
          : [...current, data.contact];
        return sortContacts(next);
      });
      setEditing(false);
      if (query) setQuery("");
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setSaving(false);
    }
  }

  async function deleteContact() {
    const contactID = selected?.id || draft.id;
    if (!contactID || !window.confirm("Delete this contact?")) return;
    try {
      await api.deleteContact(csrf, contactID);
      addToast("Contact deleted.");
      const remaining = contacts.filter((contact) => contact.id !== contactID);
      setContacts(remaining);
      const next = remaining[0] || null;
      if (next) {
        setSelectedID(next.id);
        setDraft(cloneContact(next));
        setEditing(false);
      } else {
        setSelectedID("new");
        setDraft(blankContact());
        setEditing(true);
      }
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }

  async function uploadIcon(file: File | null) {
    if (!file || !draft.id) return;
    try {
      const data = await api.uploadContactIcon(csrf, draft.id, file);
      setDraft(cloneContact(data.contact));
      setContacts((current) => current.map((contact) => contact.id === data.contact.id ? data.contact : contact));
      addToast("Contact icon updated.");
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }

  async function removeIcon() {
    if (!draft.id) return;
    try {
      const data = await api.deleteContactIcon(csrf, draft.id);
      setDraft(cloneContact(data.contact));
      setContacts((current) => current.map((contact) => contact.id === data.contact.id ? data.contact : contact));
      addToast("Contact icon removed.");
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }

  async function importContacts(file: File | null) {
    if (!file) return;
    try {
      const data = await api.importContacts(csrf, file);
      addToast(`Imported ${data.imported}, updated ${data.updated}.`);
      await load();
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      if (importRef.current) importRef.current.value = "";
    }
  }

  function cancelEditing() {
    if (selected) {
      setDraft(cloneContact(selected));
      setEditing(false);
      return;
    }
    const first = contacts[0];
    if (first) {
      choose(first);
    } else {
      setDraft(blankContact());
    }
  }

  return (
    <>
      <div className="content-head">
        <div>
          <h1>Contacts</h1>
          <span className="label-pill">{contacts.length.toLocaleString()}</span>
        </div>
        <div className="contact-actions">
          <button className="secondary" type="button" onClick={newContact}>
            <Icon name="add" />
            New
          </button>
          <button className="secondary" type="button" onClick={() => importRef.current?.click()}>
            <Icon name="upload" />
            Import VCF
          </button>
          <a className="button secondary" href="/api/contacts/export">
            <Icon name="download" />
            Export VCF
          </a>
          <input ref={importRef} type="file" accept=".vcf,text/vcard,text/x-vcard" hidden onChange={(event) => void importContacts(event.target.files?.[0] || null)} />
        </div>
      </div>
      <section className="contacts-shell">
        <aside className="contacts-list">
          <div className="contacts-search">
            <Icon name="search" />
            <input value={query} placeholder="Search contacts" onChange={(event) => setQuery(event.target.value)} />
          </div>
          {loading ? <div className="muted">Loading contacts...</div> : null}
          <div className="contacts-list-items">
            {contacts.map((contact) => (
              <button
                type="button"
                className={`contact-row ${contact.id === selectedID ? "active" : ""}`}
                key={contact.id}
                onClick={() => choose(contact)}
              >
                <ContactAvatar contact={contact} />
                <span>
                  <strong>{contact.display_name || primaryEmail(contact) || "Unnamed contact"}</strong>
                  <small>{primaryEmail(contact) || contact.organization}</small>
                </span>
                {contact.is_me ? <em>Me</em> : null}
              </button>
            ))}
          </div>
        </aside>
        {editing || !selected ? <form className="contact-editor" onSubmit={save}>
          <div className="contact-editor-toolbar">
            <div>
              <span className="eyebrow">{draft.id ? "Edit contact" : "New contact"}</span>
              <h2>{draft.display_name || primaryEmail(draft) || "Contact details"}</h2>
            </div>
            <button className="ghost" type="button" onClick={cancelEditing}>
              <Icon name="close" />
              Cancel
            </button>
          </div>
          <div className="contact-editor-head">
            <ContactAvatar contact={draft} large />
            <div>
              <label className="icon-upload">
                <input type="file" accept="image/png,image/jpeg,image/gif,image/webp" disabled={!draft.id} onChange={(event) => void uploadIcon(event.target.files?.[0] || null)} />
                <span>{draft.id ? "Upload icon" : "Save before icon upload"}</span>
              </label>
              {draft.icon_url ? <button className="ghost text-link" type="button" onClick={() => void removeIcon()}>Remove icon</button> : null}
            </div>
          </div>
          <div className="contact-editor-sections">
            <section className="contact-section">
              <div className="contact-section-head">
                <h2>Name</h2>
              </div>
              <div className="contact-grid contact-grid-name">
                <Field className="contact-field contact-field-wide" label="Display name" value={draft.display_name} required onChange={(value) => setField("display_name", value)} />
                <Field className="contact-field" label="Nickname" value={draft.nickname} onChange={(value) => setField("nickname", value)} />
              </div>
              <div className="contact-name-row">
                <Field className="contact-field" label="Prefix" value={draft.name_prefix} onChange={(value) => setField("name_prefix", value)} />
                <Field className="contact-field" label="Given" value={draft.given_name} onChange={(value) => setField("given_name", value)} />
                <Field className="contact-field" label="Middle" value={draft.additional_name} onChange={(value) => setField("additional_name", value)} />
                <Field className="contact-field" label="Family" value={draft.family_name} onChange={(value) => setField("family_name", value)} />
                <Field className="contact-field" label="Suffix" value={draft.name_suffix} onChange={(value) => setField("name_suffix", value)} />
              </div>
            </section>
            <section className="contact-section">
              <div className="contact-section-head">
                <h2>Details</h2>
              </div>
              <div className="contact-grid contact-grid-details">
                <Field className="contact-field contact-field-wide" label="Organization" value={draft.organization} onChange={(value) => setField("organization", value)} />
                <Field className="contact-field" label="Department" value={draft.department} onChange={(value) => setField("department", value)} />
                <Field className="contact-field" label="Job title" value={draft.job_title} onChange={(value) => setField("job_title", value)} />
                <Field className="contact-field" label="Birthday" value={draft.birthday} type="text" placeholder="YYYY-MM-DD" onChange={(value) => setField("birthday", value)} />
                <Field className="contact-field contact-field-wide" label="Categories" value={draft.categories} onChange={(value) => setField("categories", value)} />
              </div>
            </section>
          </div>
          <div className="contact-flags">
            <label><input type="checkbox" checked={draft.is_me} onChange={(event) => setField("is_me", event.target.checked)} /> Me identity</label>
            <label><input type="checkbox" checked={draft.is_primary} disabled={!draft.is_me} onChange={(event) => setField("is_primary", event.target.checked)} /> Primary From identity</label>
          </div>
          <ContactEmailEditor value={draft.emails} onChange={(emails) => setField("emails", emails)} />
          {contactKeyEditors(contactPlugins, {
            csrf,
            contactID: draft.id || 0,
            emails: draft.emails,
            addToast
          })}
          <ContactPhoneEditor value={draft.phones} onChange={(phones) => setField("phones", phones)} />
          <ContactAddressEditor value={draft.addresses} onChange={(addresses) => setField("addresses", addresses)} />
          <ContactURLEditor value={draft.urls} onChange={(urls) => setField("urls", urls)} />
          <label className="contact-notes">
            Notes
            <textarea value={draft.notes} onChange={(event) => setField("notes", event.target.value)} />
          </label>
          <div className="contact-savebar">
            <button disabled={saving}>{saving ? "Saving..." : "Save contact"}</button>
            <button className="secondary" type="button" onClick={cancelEditing}>Cancel</button>
            {selected ? <button className="ghost danger" type="button" onClick={() => void deleteContact()}>Delete</button> : null}
          </div>
        </form> : (
          <ContactProfile
            contact={selected}
            interactions={interactions}
            interactionsLoading={interactionsLoading}
            onEdit={() => {
              setDraft(cloneContact(selected));
              setEditing(true);
            }}
            onDelete={() => void deleteContact()}
            onCompose={(email) => openCompose(`to=${encodeURIComponent(email)}`)}
            onFindMail={(email) => navigate(searchURL(email))}
            onOpenMessage={(messageID) => navigate(messageURL(messageID, "", [], "/contacts"))}
          />
        )}
      </section>
    </>
  );
}

function ContactProfile({
  contact,
  interactions,
  interactionsLoading,
  onEdit,
  onDelete,
  onCompose,
  onFindMail,
  onOpenMessage
}: {
  contact: Contact;
  interactions: ContactInteraction[];
  interactionsLoading: boolean;
  onEdit: () => void;
  onDelete: () => void;
  onCompose: (email: string) => void;
  onFindMail: (email: string) => void;
  onOpenMessage: (messageID: number) => void;
}) {
  const email = primaryEmail(contact);
  const name = contact.display_name || email || "Unnamed contact";
  const role = [contact.job_title, contact.organization].filter(Boolean).join(" at ");
  const phones = contact.phones.filter((item) => item.number.trim());
  const addresses = contact.addresses.filter((item) => formatAddress(item));
  const urls = contact.urls.filter((item) => externalURL(item.url));
  const keys = contact.pgp_keys || [];
  const hasAbout = Boolean(role || contact.department || contact.birthday || contact.categories || contact.notes);

  return (
    <article className="contact-profile">
      <header className="contact-profile-hero">
        <div className="contact-profile-tools">
          <button className="secondary" type="button" onClick={onEdit}>
            <Icon name="edit" />
            Edit
          </button>
          <button className="ghost icon-only danger" type="button" title="Delete contact" aria-label="Delete contact" onClick={onDelete}>
            <Icon name="delete" />
          </button>
        </div>
        <div className="contact-profile-identity">
          <div className="contact-profile-avatar">
            <ContactAvatar contact={contact} large />
          </div>
          <div>
            <h2>{name}</h2>
            {role ? <p>{role}</p> : email ? <p>{email}</p> : <p className="muted">Add an email or organization</p>}
            <div className="contact-profile-badges">
              {contact.nickname ? <span>{contact.nickname}</span> : null}
              {contact.is_me ? <span>Me identity</span> : null}
              {contact.is_primary ? <span>Primary sender</span> : null}
            </div>
          </div>
        </div>
        {email ? (
          <div className="contact-profile-actions" aria-label="Contact actions">
            <button type="button" onClick={() => onCompose(email)}>
              <span><Icon name="mail" /></span>
              Email
            </button>
            <button type="button" onClick={() => onFindMail(email)}>
              <span><Icon name="search" /></span>
              Find mail
            </button>
          </div>
        ) : null}
      </header>

      <div className="contact-profile-grid">
        <section className="contact-profile-card">
          <h3>Contact details</h3>
          <div className="contact-detail-list">
            {contact.emails.filter((item) => item.email.trim()).map((item, index) => (
              <div className="contact-detail-row" key={`email-${item.id || index}`}>
                <Icon name="mail" />
                <button className="contact-detail-value" type="button" onClick={() => onCompose(item.email)}>
                  {item.email}
                </button>
                <DetailLabel label={item.label} primary={item.is_primary} />
              </div>
            ))}
            {phones.map((item, index) => (
              <div className="contact-detail-row" key={`phone-${item.id || index}`}>
                <Icon name="phone" />
                <a className="contact-detail-value" href={`tel:${item.number}`}>{item.number}</a>
                <DetailLabel label={item.label} primary={item.is_primary} />
              </div>
            ))}
            {addresses.map((item, index) => (
              <div className="contact-detail-row contact-detail-row-address" key={`address-${item.id || index}`}>
                <Icon name="location" />
                <address className="contact-detail-value">{formatAddress(item)}</address>
                <DetailLabel label={item.label} primary={item.is_primary} />
              </div>
            ))}
            {urls.map((item, index) => (
              <div className="contact-detail-row" key={`url-${item.id || index}`}>
                <Icon name="link" />
                <a className="contact-detail-value" href={externalURL(item.url)} target="_blank" rel="noreferrer">
                  {displayURL(item.url)}
                </a>
                <DetailLabel label={item.label} primary={item.is_primary} />
              </div>
            ))}
            {!email && phones.length === 0 && addresses.length === 0 && urls.length === 0 ? (
              <p className="contact-card-empty">No contact details yet.</p>
            ) : null}
          </div>
        </section>

        <section className="contact-profile-card contact-interactions-card">
          <div className="contact-profile-card-head">
            <h3>Recent interactions</h3>
            {email ? <button className="ghost text-link" type="button" onClick={() => onFindMail(email)}>All mail</button> : null}
          </div>
          {interactionsLoading ? <p className="contact-card-empty">Looking for recent mail…</p> : null}
          {!interactionsLoading && interactions.length === 0 ? (
            <p className="contact-card-empty">No recent mail found for this contact.</p>
          ) : null}
          <div className="contact-interaction-list">
            {interactions.map((item) => (
              <button type="button" key={item.message_id} onClick={() => onOpenMessage(item.message_id)}>
                <span className={`contact-interaction-icon ${item.direction}`}>
                  <Icon name={item.direction === "sent" ? "send" : "mail"} />
                </span>
                <span>
                  <strong>{item.subject || "(No subject)"}</strong>
                  <small>
                    {item.direction === "sent" ? "Sent" : "Received"} · {formatInteractionDate(item.date)}
                    {item.has_attachments ? " · Attachment" : ""}
                  </small>
                </span>
                <Icon name="chevron_right" />
              </button>
            ))}
          </div>
        </section>

        {hasAbout ? (
          <section className="contact-profile-card">
            <h3>About</h3>
            <div className="contact-about-list">
              {role || contact.department ? (
                <div>
                  <Icon name="building" />
                  <span>
                    {role || contact.organization}
                    {contact.department ? <small>{contact.department}</small> : null}
                  </span>
                </div>
              ) : null}
              {contact.birthday ? (
                <div>
                  <Icon name="calendar" />
                  <span>{formatBirthday(contact.birthday)}</span>
                </div>
              ) : null}
              {contact.categories ? (
                <div>
                  <Icon name="label" />
                  <span className="contact-category-list">
                    {contact.categories.split(/[,;]/).map((category) => category.trim()).filter(Boolean).map((category) => <em key={category}>{category}</em>)}
                  </span>
                </div>
              ) : null}
              {contact.notes ? (
                <div className="contact-about-notes">
                  <Icon name="file_text" />
                  <p>{contact.notes}</p>
                </div>
              ) : null}
            </div>
          </section>
        ) : null}

        {keys.length > 0 ? (
          <section className="contact-profile-card">
            <h3>PGP public keys</h3>
            <div className="contact-key-list">
              {keys.map((key, index) => (
                <div key={key.id || key.fingerprint || index}>
                  <Icon name="key" />
                  <span>
                    <strong>{key.label || key.email || "Public key"}</strong>
                    <small>{formatFingerprint(key.fingerprint || key.key_id)}</small>
                  </span>
                  {key.is_preferred ? <em>Preferred</em> : null}
                </div>
              ))}
            </div>
          </section>
        ) : null}
      </div>
    </article>
  );
}

function DetailLabel({ label, primary }: { label: string; primary: boolean }) {
  const text = [label.trim(), primary ? "Primary" : ""].filter(Boolean).join(" · ");
  return text ? <small className="contact-detail-label">{text}</small> : null;
}

function Field({
  label,
  value,
  type = "text",
  placeholder = "",
  required = false,
  className = "",
  onChange
}: {
  label: string;
  value: string;
  type?: string;
  placeholder?: string;
  required?: boolean;
  className?: string;
  onChange: (value: string) => void;
}) {
  return (
    <label className={className}>
      {label}
      <input type={type} value={value} placeholder={placeholder} required={required} onChange={(event) => onChange(event.target.value)} />
    </label>
  );
}

function ContactEmailEditor({ value, onChange }: { value: ContactEmail[]; onChange: (value: ContactEmail[]) => void }) {
  const rows = value.length > 0 ? value : [{ label: "", email: "", is_primary: true }];
  return (
    <ContactSection title="Emails" onAdd={() => onChange([...rows, { label: "", email: "", is_primary: false }])}>
      {rows.map((row, index) => (
        <div className="contact-repeat-row" key={index}>
          <input value={row.label} placeholder="Label" onChange={(event) => onChange(updateAt(rows, index, { ...row, label: event.target.value }))} />
          <input value={row.email} type="email" placeholder="email@example.com" onChange={(event) => onChange(updateAt(rows, index, { ...row, email: event.target.value }))} />
          <PrimaryToggle checked={row.is_primary} onChange={() => onChange(markPrimary(rows, index))} />
          <RemoveButton onClick={() => onChange(removeAt(rows, index))} />
        </div>
      ))}
    </ContactSection>
  );
}


function ContactPhoneEditor({ value, onChange }: { value: ContactPhone[]; onChange: (value: ContactPhone[]) => void }) {
  return (
    <ContactSection title="Phones" onAdd={() => onChange([...value, { label: "Phone", number: "", is_primary: value.length === 0 }])}>
      {value.map((row, index) => (
        <div className="contact-repeat-row" key={index}>
          <input value={row.label} placeholder="Label" onChange={(event) => onChange(updateAt(value, index, { ...row, label: event.target.value }))} />
          <input value={row.number} placeholder="Number" onChange={(event) => onChange(updateAt(value, index, { ...row, number: event.target.value }))} />
          <PrimaryToggle checked={row.is_primary} onChange={() => onChange(markPrimary(value, index))} />
          <RemoveButton onClick={() => onChange(removeAt(value, index))} />
        </div>
      ))}
    </ContactSection>
  );
}

function ContactAddressEditor({ value, onChange }: { value: ContactAddress[]; onChange: (value: ContactAddress[]) => void }) {
  return (
    <ContactSection title="Addresses" onAdd={() => onChange([...value, blankAddress(value.length === 0)])}>
      {value.map((row, index) => (
        <div className="contact-address-row" key={index}>
          <input value={row.label} placeholder="Label" onChange={(event) => onChange(updateAt(value, index, { ...row, label: event.target.value }))} />
          <input value={row.street} placeholder="Street" onChange={(event) => onChange(updateAt(value, index, { ...row, street: event.target.value }))} />
          <input value={row.locality} placeholder="City" onChange={(event) => onChange(updateAt(value, index, { ...row, locality: event.target.value }))} />
          <input value={row.region} placeholder="State/region" onChange={(event) => onChange(updateAt(value, index, { ...row, region: event.target.value }))} />
          <input value={row.postal_code} placeholder="Postal code" onChange={(event) => onChange(updateAt(value, index, { ...row, postal_code: event.target.value }))} />
          <input value={row.country} placeholder="Country" onChange={(event) => onChange(updateAt(value, index, { ...row, country: event.target.value }))} />
          <PrimaryToggle checked={row.is_primary} onChange={() => onChange(markPrimary(value, index))} />
          <RemoveButton onClick={() => onChange(removeAt(value, index))} />
        </div>
      ))}
    </ContactSection>
  );
}

function ContactURLEditor({ value, onChange }: { value: ContactURL[]; onChange: (value: ContactURL[]) => void }) {
  return (
    <ContactSection title="URLs" onAdd={() => onChange([...value, { label: "Website", url: "", is_primary: value.length === 0 }])}>
      {value.map((row, index) => (
        <div className="contact-repeat-row" key={index}>
          <input value={row.label} placeholder="Label" onChange={(event) => onChange(updateAt(value, index, { ...row, label: event.target.value }))} />
          <input value={row.url} placeholder="https://example.com" onChange={(event) => onChange(updateAt(value, index, { ...row, url: event.target.value }))} />
          <PrimaryToggle checked={row.is_primary} onChange={() => onChange(markPrimary(value, index))} />
          <RemoveButton onClick={() => onChange(removeAt(value, index))} />
        </div>
      ))}
    </ContactSection>
  );
}

function ContactSection({ title, onAdd, children }: { title: string; onAdd: () => void; children: ReactNode }) {
  return (
    <section className="contact-section">
      <div>
        <h2>{title}</h2>
        <button className="secondary" type="button" onClick={onAdd}>Add</button>
      </div>
      {children}
    </section>
  );
}

function PrimaryToggle({ checked, onChange }: { checked: boolean; onChange: () => void }) {
  return <label className="primary-toggle"><input type="radio" checked={checked} onChange={onChange} /> Primary</label>;
}

function RemoveButton({ onClick }: { onClick: () => void }) {
  return <button className="ghost icon-only" type="button" title="Remove" onClick={onClick}><Icon name="close" /></button>;
}

function ContactAvatar({ contact, large = false }: { contact: Contact; large?: boolean }) {
  const label = contact.display_name || primaryEmail(contact) || "?";
  if (contact.icon_url) {
    return <img className={`contact-avatar ${large ? "large" : ""}`} src={contact.icon_url} alt="" />;
  }
  return <span className={`contact-avatar ${large ? "large" : ""}`}>{label.slice(0, 1).toUpperCase()}</span>;
}

function blankContact(): Contact {
  return {
    id: 0,
    name_prefix: "",
    given_name: "",
    additional_name: "",
    family_name: "",
    name_suffix: "",
    display_name: "",
    nickname: "",
    organization: "",
    department: "",
    job_title: "",
    birthday: "",
    notes: "",
    categories: "",
    is_me: false,
    is_primary: false,
    emails: [{ label: "Email", email: "", is_primary: true }],
    phones: [],
    addresses: [],
    urls: [],
    pgp_keys: [],
    icon_url: ""
  };
}

function cloneContact(contact: Contact): Contact {
  return JSON.parse(JSON.stringify(contact)) as Contact;
}

function primaryEmail(contact: Contact): string {
  return contact.emails.find((email) => email.is_primary && email.email.trim())?.email || contact.emails.find((email) => email.email.trim())?.email || "";
}

function sortContacts(contacts: Contact[]): Contact[] {
  return [...contacts].sort((left, right) => {
    const leftName = left.display_name || primaryEmail(left);
    const rightName = right.display_name || primaryEmail(right);
    return leftName.localeCompare(rightName, undefined, { sensitivity: "base" });
  });
}

function formatAddress(address: ContactAddress): string {
  return [
    address.street.trim(),
    [address.locality.trim(), address.region.trim(), address.postal_code.trim()].filter(Boolean).join(" "),
    address.country.trim()
  ].filter(Boolean).join(", ");
}

function externalURL(value: string): string {
  const trimmed = value.trim();
  if (!trimmed) return "";
  try {
    const parsed = new URL(/^https?:\/\//i.test(trimmed) ? trimmed : `https://${trimmed}`);
    return parsed.protocol === "http:" || parsed.protocol === "https:" ? parsed.toString() : "";
  } catch {
    return "";
  }
}

function displayURL(value: string): string {
  return value.trim().replace(/^https?:\/\//i, "").replace(/\/$/, "");
}

function formatInteractionDate(value: string): string {
  const date = new Date(value);
  if (!value || Number.isNaN(date.getTime())) return "Unknown date";
  const now = new Date();
  if (date.getFullYear() === now.getFullYear()) {
    return new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric" }).format(date);
  }
  return new Intl.DateTimeFormat(undefined, { month: "short", year: "numeric" }).format(date);
}

function formatBirthday(value: string): string {
  const trimmed = value.trim();
  const match = /^(\d{4})-(\d{2})-(\d{2})$/.exec(trimmed);
  if (!match) return trimmed;
  const date = new Date(Number(match[1]), Number(match[2]) - 1, Number(match[3]));
  if (Number.isNaN(date.getTime())) return trimmed;
  return new Intl.DateTimeFormat(undefined, { month: "long", day: "numeric", year: "numeric" }).format(date);
}

function formatFingerprint(value: string): string {
  const compact = value.replace(/\s/g, "").toUpperCase();
  if (!compact) return "Fingerprint unavailable";
  return compact.match(/.{1,4}/g)?.join(" ") || compact;
}

function updateAt<T>(items: T[], index: number, value: T): T[] {
  return items.map((item, itemIndex) => itemIndex === index ? value : item);
}

function removeAt<T>(items: T[], index: number): T[] {
  return items.filter((_item, itemIndex) => itemIndex !== index);
}

function markPrimary<T extends { is_primary: boolean }>(items: T[], index: number): T[] {
  return items.map((item, itemIndex) => ({ ...item, is_primary: itemIndex === index }));
}

function blankAddress(isPrimary: boolean): ContactAddress {
  return {
    label: "Address",
    street: "",
    locality: "",
    region: "",
    postal_code: "",
    country: "",
    is_primary: isPrimary
  };
}
