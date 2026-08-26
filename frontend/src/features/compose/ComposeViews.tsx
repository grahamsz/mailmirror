// File overview: Compose, reply, and forward UI. It owns local recovery, templates, recipient
// chips, identity choice, file uploads, inline media CIDs, and optional photo resizing.

import { useEffect, useMemo, useRef, useState } from "react";
import type { ClipboardEvent, DragEvent, FormEvent, KeyboardEvent as ReactKeyboardEvent } from "react";
import DOMPurify from "dompurify";
import { api } from "../../api";
import type { LocationState, Toast } from "../../appTypes";
import type { ContactAutocomplete, ComposeAttachmentUpload, ComposeExistingAttachment, ComposeForm, ComposeIdentity } from "../../types";
import { Icon, LogoMark } from "../../components/Icon";
import { isNetworkError, messageFromError } from "../../lib/errors";
import { textToHTML } from "../../lib/html";
import { enqueueOfflineSend } from "../../lib/outbox";
import {
  clearComposeRecovery,
  composeContentEqual,
  loadComposeRecovery,
  loadComposeTemplates,
  saveComposeRecovery,
  saveComposeTemplates,
  type LocalComposeContent,
  type LocalComposeRecovery,
  type LocalComposeTemplate
} from "../../lib/composeLocal";
import {
  androidContactSuggestions,
  androidNativeAvailable,
  loadAndroidSharedFiles,
  pickAndroidContactEmail,
  requestAndroidContactAccess
} from "../../lib/androidNative";
import type { AndroidContactAccess, NativeContactEmail } from "../../lib/androidNative";
import type { RuntimePlugin } from "../../plugins/runtime";
import { useComposeSecurity } from "../../plugins/composeSecurity";
import type { ComposeSecurityUnlockState, OpenComposeSecurityUnlock } from "../../plugins/composeSecurity";

const ATTACHMENT_WARNING_BYTES = 20 * 1024 * 1024;
const RESIZE_PHOTO_MAX_EDGE = 1920;
const RESIZE_PHOTO_QUALITY = 0.82;

type ComposeAttachment = ComposeAttachmentUpload & {
  id: string;
  objectURL?: string;
};

/** Floating compose dialog used by the shell for new mail, replies, and forwards. */
export function ComposeOverlay({
  userID,
  csrf,
  query,
  securityEnabled,
  securityPlugins,
  securityUnlock,
  openSecurityUnlock,
  addToast,
  onClose
}: {
  userID: number;
  csrf: string;
  query: string;
  securityEnabled: boolean;
  securityPlugins: readonly RuntimePlugin[];
  securityUnlock: ComposeSecurityUnlockState;
  openSecurityUnlock: OpenComposeSecurityUnlock;
  addToast: (message: string, kind?: Toast["kind"]) => number;
  onClose: () => void;
}) {
  const [form, setForm] = useState<ComposeForm | null>(null);
  const [from, setFrom] = useState("");
  const [identities, setIdentities] = useState<ComposeIdentity[]>([]);
  const [error, setError] = useState("");
  const [minimized, setMinimized] = useState(false);

  useEffect(() => {
    let cancelled = false;
    setForm(null);
    setError("");
    setMinimized(false);
    api
      .compose(query)
      .then((data) => {
        if (cancelled) return;
        setForm(data.compose);
        setFrom(data.compose_from);
        setIdentities(data.from_identities || []);
      })
      .catch((err) => {
        if (!cancelled) setError(messageFromError(err));
      });
    return () => {
      cancelled = true;
    };
  }, [query]);

  return (
    <div className={`compose-popover ${minimized ? "minimized" : ""}`} role="dialog" aria-label="Compose message">
      {error ? <div className="error">{error}</div> : null}
      {form ? (
        <ComposeBox
          userID={userID}
          csrf={csrf}
          composeFrom={from}
          identities={identities}
          initial={form}
          recoveryContext={query || "new"}
          minimized={minimized}
          securityEnabled={securityEnabled}
          securityPlugins={securityPlugins}
          securityUnlock={securityUnlock}
          openSecurityUnlock={openSecurityUnlock}
          addToast={addToast}
          onSent={onClose}
          onCancel={onClose}
          onMinimize={() => setMinimized((current) => !current)}
        />
      ) : (
        <div className="compose-window compose-loading">
          <div className="compose-head">
            <span className="compose-head-title">
              <LogoMark className="compose-head-mark" />
              <span>New Message</span>
            </span>
            <button className="ghost" type="button" title="Close" onClick={onClose}>
              <Icon name="close" />
            </button>
          </div>
          <div className="panel muted">Loading compose...</div>
        </div>
      )}
    </div>
  );
}

/** Full-page compose route, mainly useful for direct links or narrow layouts. */
export function ComposePage({
  userID,
  csrf,
  location,
  navigate,
  securityEnabled,
  securityPlugins,
  securityUnlock,
  openSecurityUnlock,
  addToast
}: {
  userID: number;
  csrf: string;
  location: LocationState;
  navigate: (url: string) => void;
  securityEnabled: boolean;
  securityPlugins: readonly RuntimePlugin[];
  securityUnlock: ComposeSecurityUnlockState;
  openSecurityUnlock: OpenComposeSecurityUnlock;
  addToast: (message: string, kind?: Toast["kind"]) => number;
}) {
  const [form, setForm] = useState<ComposeForm | null>(null);
  const [from, setFrom] = useState("");
  const [identities, setIdentities] = useState<ComposeIdentity[]>([]);
  const [error, setError] = useState("");

  useEffect(() => {
    let cancelled = false;
    api
      .compose(location.search.replace(/^\?/, ""))
      .then((data) => {
        if (cancelled) return;
        setForm(data.compose);
        setFrom(data.compose_from);
        setIdentities(data.from_identities || []);
      })
      .catch((err) => {
        if (!cancelled) setError(messageFromError(err));
      });
    return () => {
      cancelled = true;
    };
  }, [location.search]);

  return (
    <div className="compose-page">
      {error ? <div className="error">{error}</div> : null}
      {form ? (
        <ComposeBox
          userID={userID}
          csrf={csrf}
          composeFrom={from}
          identities={identities}
          initial={form}
          recoveryContext={location.search.replace(/^\?/, "") || "new"}
          securityEnabled={securityEnabled}
          securityPlugins={securityPlugins}
          securityUnlock={securityUnlock}
          openSecurityUnlock={openSecurityUnlock}
          addToast={addToast}
          onSent={() => navigate("/mail")}
          onCancel={() => navigate("/mail")}
        />
      ) : (
        <div className="panel muted">Loading compose...</div>
      )}
    </div>
  );
}

/**
 * ComposeBox owns the mutable compose draft: contenteditable HTML, plain-text
 * fallback, From identity, recipient fields, files, inline media, and send state.
 */
export function ComposeBox({
  userID,
  csrf,
  composeFrom,
  identities = [],
  initial,
  recoveryContext = "",
  inline = false,
  minimized = false,
  securityEnabled = false,
  securityPlugins,
  securityUnlock,
  openSecurityUnlock,
  addToast,
  onSent,
  onCancel,
  onMinimize
}: {
  userID: number;
  csrf: string;
  composeFrom: string;
  identities?: ComposeIdentity[];
  initial: ComposeForm;
  recoveryContext?: string;
  inline?: boolean;
  minimized?: boolean;
  securityEnabled?: boolean;
  securityPlugins: readonly RuntimePlugin[];
  securityUnlock: ComposeSecurityUnlockState;
  openSecurityUnlock: OpenComposeSecurityUnlock;
  addToast: (message: string, kind?: Toast["kind"]) => number;
  onSent: () => void;
  onCancel?: () => void;
  onMinimize?: () => void;
}) {
  const [form, setForm] = useState<ComposeForm>(initial);
  const [showCc, setShowCc] = useState(Boolean(initial.cc));
  const [showBcc, setShowBcc] = useState(Boolean(initial.bcc));
  const [sending, setSending] = useState(false);
  const [savingDraft, setSavingDraft] = useState(false);
  const [resizing, setResizing] = useState(false);
  const [attachments, setAttachments] = useState<ComposeAttachment[]>([]);
  const [pendingRecovery, setPendingRecovery] = useState<LocalComposeRecovery | null>(null);
  const [recoveryReady, setRecoveryReady] = useState(false);
  const [dirty, setDirty] = useState(false);
  const [editorRevision, setEditorRevision] = useState(0);
  const [templates, setTemplates] = useState<LocalComposeTemplate[]>(() => loadComposeTemplates(userID));
  const [templateName, setTemplateName] = useState("");
  const [offlineNow, setOfflineNow] = useState(() => typeof navigator !== "undefined" && navigator.onLine === false);

  useEffect(() => {
    const update = () => setOfflineNow(navigator.onLine === false);
    window.addEventListener("online", update);
    window.addEventListener("offline", update);
    return () => {
      window.removeEventListener("online", update);
      window.removeEventListener("offline", update);
    };
  }, []);
  const editorRef = useRef<HTMLDivElement | null>(null);
  const formRef = useRef<HTMLFormElement | null>(null);
  const templateMenuRef = useRef<HTMLDetailsElement | null>(null);
  const attachmentInputRef = useRef<HTMLInputElement | null>(null);
  const inlineMediaInputRef = useRef<HTMLInputElement | null>(null);
  const attachmentsRef = useRef<ComposeAttachment[]>([]);
  const recoveryTimerRef = useRef<number | null>(null);
  const baselineRef = useRef<LocalComposeContent | null>(null);
  const baselineAttachmentStateRef = useRef("");
  const nativeShareID = !inline ? new URLSearchParams(window.location.search).get("android_share") || "" : "";
  const localComposeContext = useMemo(
    () => composeLocalContext(recoveryContext, initial, inline),
    [recoveryContext, initial.draft_message_id, initial.in_reply_to_id, inline]
  );
  const primaryIdentity = useMemo(() => identities.find((identity) => identity.is_primary) || identities[0] || null, [identities]);
  const availableExistingAttachments = form.available_attachments || [];
  const includedExistingAttachmentIDs = form.include_attachment_ids || [];
  const forwardedMessageAttachment = form.forward_attachment_message_id ? form.forward_attachment || null : null;
  const includedExistingAttachments = useMemo(() => {
    const ids = new Set(includedExistingAttachmentIDs);
    return availableExistingAttachments.filter((attachment) => ids.has(attachment.id));
  }, [availableExistingAttachments, includedExistingAttachmentIDs]);
  const remainingExistingAttachmentCount = Math.max(0, availableExistingAttachments.length - includedExistingAttachments.length);
  const totalAttachmentBytes = useMemo(() => {
    const uploadBytes = attachments.reduce((total, attachment) => total + attachment.size, 0);
    const existingBytes = includedExistingAttachments.reduce((total, attachment) => total + attachment.size, 0);
    const forwardedBytes = forwardedMessageAttachment?.size || 0;
    return uploadBytes + existingBytes + forwardedBytes;
  }, [attachments, includedExistingAttachments, forwardedMessageAttachment]);
  const hasAttachmentWarning = totalAttachmentBytes > ATTACHMENT_WARNING_BYTES;
  const canResizePhotos = attachments.some((attachment) => isResizablePhoto(attachment.file));
  const selectedIdentity = identities.find((identity) => identity.id === (form.from_identity_id || primaryIdentity?.id || 0)) || primaryIdentity;
  const hasAttachedItems = attachments.length > 0 || includedExistingAttachments.length > 0 || Boolean(forwardedMessageAttachment);
  const recipientEmails = useMemo(() => recipientEmailAddresses([form.to, form.cc, form.bcc]), [form.to, form.cc, form.bcc]);
  const composeSecurity = useComposeSecurity({
    enabled: securityEnabled,
    plugins: securityPlugins,
    initial,
    selectedIdentity,
    recipientEmails,
    includedExistingAttachments,
    forwardedMessageAttachment,
    unlockState: securityUnlock,
    openUnlock: openSecurityUnlock,
    addToast
  });

  useEffect(() => {
    const initialForm = {
      ...initial,
      from_identity_id: initial.from_identity_id || primaryIdentity?.id || 0
    };
    const initialHTML = initialEditorHTML(initial);
    const baseline = localComposeContent(initialForm, recoverableEditorHTML(initialHTML));
    baselineRef.current = baseline;
    baselineAttachmentStateRef.current = composeAttachmentStateKey(initialForm, []);
    setRecoveryReady(false);
    setPendingRecovery(null);
    setDirty(false);
    setForm(initialForm);
    setShowCc(Boolean(initial.cc));
    setShowBcc(Boolean(initial.bcc));
    setAttachments((current) => {
      revokeAttachmentObjectURLs(current);
      return [];
    });
    if (editorRef.current) {
      editorRef.current.innerHTML = initialHTML;
      placeInitialCaret(editorRef.current);
    }
    const recovered = loadComposeRecovery(userID, localComposeContext);
    if (recovered && !composeContentEqual(recoveryContent(recovered), baseline)) {
      if (explicitServerComposeContext(recoveryContext, initial)) {
        setPendingRecovery(recovered);
        setDirty(true);
      } else {
        applyRecoveredContent(recovered, initialForm);
      }
    } else if (recovered) {
      clearComposeRecovery(userID, localComposeContext);
    }
    setRecoveryReady(true);
  }, [initial, localComposeContext, primaryIdentity?.id, userID]);

  useEffect(() => {
    setTemplates(loadComposeTemplates(userID));
  }, [userID]);

  useEffect(() => {
    const closeTemplateMenu = (event: PointerEvent) => {
      const menu = templateMenuRef.current;
      if (!menu?.open || (event.target instanceof Node && menu.contains(event.target))) return;
      menu.open = false;
    };
    document.addEventListener("pointerdown", closeTemplateMenu);
    return () => document.removeEventListener("pointerdown", closeTemplateMenu);
  }, []);

  useEffect(() => {
    attachmentsRef.current = attachments;
  }, [attachments]);

  useEffect(() => () => {
    revokeAttachmentObjectURLs(attachmentsRef.current);
    if (recoveryTimerRef.current !== null) window.clearTimeout(recoveryTimerRef.current);
  }, []);

  useEffect(() => {
    if (!recoveryReady || pendingRecovery) return;
    const content = currentLocalComposeContent(form, editorRef.current);
    const contentChanged = baselineRef.current ? !composeContentEqual(content, baselineRef.current) : false;
    const attachmentsChanged = composeAttachmentStateKey(form, attachments) !== baselineAttachmentStateRef.current;
    setDirty(contentChanged || attachmentsChanged);
    if (recoveryTimerRef.current !== null) window.clearTimeout(recoveryTimerRef.current);
    recoveryTimerRef.current = null;
    if (!contentChanged) {
      clearComposeRecovery(userID, localComposeContext);
      return;
    }
    recoveryTimerRef.current = window.setTimeout(() => {
      recoveryTimerRef.current = null;
      saveComposeRecovery(userID, localComposeContext, content);
    }, 650);
  }, [attachments.length, editorRevision, form, localComposeContext, pendingRecovery, recoveryReady, userID]);

  useEffect(() => {
    if (!dirty) return;
    const warnBeforeUnload = (event: BeforeUnloadEvent) => {
      flushComposeRecovery();
      event.preventDefault();
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", warnBeforeUnload);
    return () => window.removeEventListener("beforeunload", warnBeforeUnload);
  }, [dirty, form, editorRevision, localComposeContext, userID]);

  // Android WebView can report the keyboard through VisualViewport before dvh
  // catches up. Size the composer within its container so the persistent mobile
  // app bar remains visible and the action bar stays above the IME.
  useEffect(() => {
    if (inline) return;
    const viewport = window.visualViewport;
    const updateViewport = () => {
      const viewportTop = viewport?.offsetTop || 0;
      const viewportBottom = viewportTop + (viewport?.height || window.innerHeight);
      const containerTop = formRef.current?.parentElement?.getBoundingClientRect().top || 0;
      const visibleTop = Math.max(viewportTop, containerTop);
      const height = Math.max(1, Math.round(viewportBottom - visibleTop));
      const top = Math.max(0, Math.round(visibleTop - containerTop));
      formRef.current?.style.setProperty("--compose-viewport-height", `${height}px`);
      formRef.current?.style.setProperty("--compose-viewport-top", `${top}px`);
    };
    updateViewport();
    viewport?.addEventListener("resize", updateViewport);
    viewport?.addEventListener("scroll", updateViewport);
    window.addEventListener("resize", updateViewport);
    return () => {
      viewport?.removeEventListener("resize", updateViewport);
      viewport?.removeEventListener("scroll", updateViewport);
      window.removeEventListener("resize", updateViewport);
    };
  }, [inline]);

  // An inline reply opens deep inside a long thread, so nothing else brings it
  // into view. Focus its editor immediately and re-reveal it while the
  // on-screen keyboard settles, so the reply starts in view instead of sitting
  // below the fold. The deadline keeps later resizes from fighting the user's
  // own scrolling.
  useEffect(() => {
    if (!inline) return;
    const editor = editorRef.current;
    if (!editor) return;
    const deadline = Date.now() + 2500;
    const reveal = () => {
      editor.scrollIntoView({ block: "center", behavior: "smooth" });
    };
    const raf = window.requestAnimationFrame(() => {
      editor.focus({ preventScroll: true });
      reveal();
    });
    const viewport = window.visualViewport;
    const onViewportChange = () => {
      if (Date.now() <= deadline) reveal();
    };
    viewport?.addEventListener("resize", onViewportChange);
    window.addEventListener("resize", onViewportChange);
    return () => {
      window.cancelAnimationFrame(raf);
      viewport?.removeEventListener("resize", onViewportChange);
      window.removeEventListener("resize", onViewportChange);
    };
  }, [inline]);

  useEffect(() => {
    if (!nativeShareID) return;
    let cancelled = false;
    void loadAndroidSharedFiles(nativeShareID)
      .then((files) => {
        if (!cancelled && files.length > 0) addFiles(files, false);
      })
      .catch((err) => {
        if (!cancelled) addToast(err instanceof Error ? err.message : "Could not load Android shared files.", "error");
      });
    return () => {
      cancelled = true;
    };
  }, [nativeShareID]);

  function setField<K extends keyof ComposeForm>(field: K, value: ComposeForm[K]) {
    setForm((current) => ({ ...current, [field]: value }));
  }

  function applyRecoveredContent(recovery: LocalComposeRecovery, baseForm = form) {
    setForm({
      ...baseForm,
      to: recovery.to,
      cc: recovery.cc,
      bcc: recovery.bcc,
      subject: recovery.subject,
      body: recovery.body,
      body_html: recovery.bodyHTML,
      from_identity_id: recovery.fromIdentityID || baseForm.from_identity_id
    });
    setShowCc(Boolean(recovery.cc));
    setShowBcc(Boolean(recovery.bcc));
    if (editorRef.current) {
      editorRef.current.innerHTML = safeRecoveredEditorHTML(recovery);
      placeInitialCaret(editorRef.current);
    }
    setPendingRecovery(null);
    setEditorRevision((current) => current + 1);
  }

  function discardPendingRecovery() {
    clearComposeRecovery(userID, localComposeContext);
    setPendingRecovery(null);
  }

  function flushComposeRecovery() {
    if (!recoveryReady || pendingRecovery) return;
    const content = currentLocalComposeContent(form, editorRef.current);
    if (baselineRef.current && !composeContentEqual(content, baselineRef.current)) {
      saveComposeRecovery(userID, localComposeContext, content);
    }
  }

  function clearLocalComposeRecovery() {
    if (recoveryTimerRef.current !== null) window.clearTimeout(recoveryTimerRef.current);
    recoveryTimerRef.current = null;
    clearComposeRecovery(userID, localComposeContext);
    setDirty(false);
  }

  function discardCompose() {
    const changed = Boolean(pendingRecovery) || composeIsDirty(form, editorRef.current, baselineRef.current) ||
      composeAttachmentStateKey(form, attachments) !== baselineAttachmentStateRef.current;
    if (changed && !window.confirm("Discard this unsent message?")) return;
    clearLocalComposeRecovery();
    onCancel?.();
  }

  function minimizeCompose() {
    flushComposeRecovery();
    onMinimize?.();
  }

  function handleEditorInput() {
    setEditorRevision((current) => current + 1);
  }

  function applyFormat(command: string, value?: string) {
    editorRef.current?.focus();
    document.execCommand(command, false, value);
    handleEditorInput();
  }

  // Attachments are kept as File objects until submit. Inline media also gets an
  // object URL immediately so the editor can render it before it becomes a CID.
  function addFiles(fileList: FileList | File[], inline = false) {
    const files = Array.from(fileList).filter((file) => file.size > 0 || file.name);
    if (files.length === 0) return;
    const nextAttachments = files.map((file) => createComposeAttachment(file, inline));
    setAttachments((current) => [...current, ...nextAttachments]);
    if (inline) {
      nextAttachments.forEach((attachment) => insertInlineAttachment(attachment));
    }
  }

  function insertInlineAttachment(attachment: ComposeAttachment) {
    const editor = editorRef.current;
    if (!editor || !attachment.objectURL) return;
    const media = document.createElement(attachment.file.type.startsWith("video/") ? "video" : "img");
    media.setAttribute("src", attachment.objectURL);
    media.setAttribute("data-compose-attachment-id", attachment.id);
    media.setAttribute("class", "compose-inline-media");
    media.setAttribute("alt", attachment.filename);
    if (media instanceof HTMLVideoElement) {
      media.controls = true;
      media.preload = "metadata";
    }
    const spacer = document.createElement("br");
    const fragment = document.createDocumentFragment();
    fragment.appendChild(media);
    fragment.appendChild(spacer);
    editor.focus();
    const selection = window.getSelection();
    const range = selection && selection.rangeCount > 0 ? selection.getRangeAt(0) : null;
    if (range && editor.contains(range.commonAncestorContainer)) {
      range.deleteContents();
      range.insertNode(fragment);
      range.setStartAfter(spacer);
      range.collapse(true);
      selection?.removeAllRanges();
      selection?.addRange(range);
    } else {
      editor.appendChild(fragment);
    }
  }

  function removeAttachment(id: string) {
    setAttachments((current) => {
      current.filter((attachment) => attachment.id === id).forEach((attachment) => {
        if (attachment.objectURL) URL.revokeObjectURL(attachment.objectURL);
      });
      return current.filter((attachment) => attachment.id !== id);
    });
    removeInlineAttachmentElement(editorRef.current, id);
  }

  function includeExistingAttachments() {
    setForm((current) => {
      const ids = new Set(current.include_attachment_ids || []);
      (current.available_attachments || []).forEach((attachment) => ids.add(attachment.id));
      return { ...current, include_attachment_ids: Array.from(ids) };
    });
  }

  function removeExistingAttachment(id: number) {
    setForm((current) => ({
      ...current,
      include_attachment_ids: (current.include_attachment_ids || []).filter((attachmentID) => attachmentID !== id)
    }));
  }

  function removeForwardAttachment() {
    setForm((current) => ({
      ...current,
      forward_attachment_message_id: 0,
      forward_attachment: undefined
    }));
  }

  async function resizePhotos() {
    if (resizing || !canResizePhotos) return;
    setResizing(true);
    try {
      const resized = await Promise.all(attachmentsRef.current.map(async (attachment) => {
        const file = await resizePhotoFile(attachment.file);
        if (!file) return attachment;
        if (attachment.objectURL) URL.revokeObjectURL(attachment.objectURL);
        return {
          ...attachment,
          file,
          filename: file.name,
          content_type: file.type || attachment.content_type,
          size: file.size,
          objectURL: attachment.inline ? URL.createObjectURL(file) : undefined
        };
      }));
      setAttachments(resized);
      syncInlineAttachmentObjectURLs(editorRef.current, resized);
    } catch {
      addToast("Could not resize photos.", "error");
    } finally {
      setResizing(false);
    }
  }

  function handleEditorPaste(event: ClipboardEvent<HTMLDivElement>) {
    const files = Array.from(event.clipboardData.files).filter((file) => isInlineMedia(file));
    if (files.length === 0) return;
    event.preventDefault();
    addFiles(files, true);
  }

  function handleComposeDrop(event: DragEvent<HTMLDivElement>) {
    const files = Array.from(event.dataTransfer.files);
    if (files.length === 0) return;
    event.preventDefault();
    addFiles(files, false);
  }

  function saveCurrentTemplate() {
    const name = templateName.trim().slice(0, 80);
    if (!name) return;
    const content = currentLocalComposeContent(form, editorRef.current);
    const existing = templates.find((template) => template.name.toLowerCase() === name.toLowerCase());
    if (existing && !window.confirm(`Replace the template "${existing.name}"?`)) return;
    const template: LocalComposeTemplate = {
      id: existing?.id || randomTemplateID(),
      name,
      subject: content.subject,
      body: content.body,
      bodyHTML: content.bodyHTML,
      updatedAt: Date.now()
    };
    const next = [template, ...templates.filter((item) => item.id !== template.id)];
    if (!saveComposeTemplates(userID, next)) {
      addToast("Template is too large to save in this browser.", "error");
      return;
    }
    setTemplates(next.slice(0, 20));
    setTemplateName("");
    if (templateMenuRef.current) templateMenuRef.current.open = false;
    addToast(existing ? "Template updated." : "Template saved.");
  }

  function insertTemplate(template: LocalComposeTemplate) {
    const editor = editorRef.current;
    if (editor) insertComposeHTML(editor, safeTemplateHTML(template));
    if (!form.subject.trim() && template.subject.trim()) setField("subject", template.subject);
    handleEditorInput();
    if (templateMenuRef.current) templateMenuRef.current.open = false;
  }

  function deleteTemplate(template: LocalComposeTemplate) {
    if (!window.confirm(`Delete the template "${template.name}"?`)) return;
    const next = templates.filter((item) => item.id !== template.id);
    if (!saveComposeTemplates(userID, next)) {
      addToast("Could not update templates in this browser.", "error");
      return;
    }
    setTemplates(next);
  }

  // Before sending, replace inline object URLs with cid: URLs and only upload
  // inline files that are still referenced in the edited body.
  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!composeSecurity.beginSubmit(formRef)) return;
    const editor = editorRef.current;
    const preparedHTML = prepareComposeHTML(editor?.innerHTML || "", attachments);
    const uploadAttachments = attachments.filter((attachment) => !attachment.inline || preparedHTML.inlineIDs.has(attachment.id));
    const nextForm: ComposeForm = {
      ...form,
      from_identity_id: form.from_identity_id || primaryIdentity?.id || 0,
      body: editor?.innerText || "",
      body_html: preparedHTML.html,
      attach_public_key: composeSecurity.attachPublicKey
    };
    let sent = false;
    let prepared: { form: ComposeForm; attachments: ComposeAttachmentUpload[] } | null = null;
    setSending(true);
    if (composeSecurity.active) {
      composeSecurity.setTransform({ active: true, phase: "plaintext", ciphertext: "" });
      await waitForFrame();
    }
    try {
      prepared = await composeSecurity.prepareSubmitForm(nextForm, uploadAttachments, (ciphertext) => {
        composeSecurity.setTransform({ active: true, phase: "ciphertext", ciphertext });
      });
      if (composeSecurity.active) await delay(520);
      await api.send(csrf, prepared.form, prepared.attachments);
      sent = true;
      clearLocalComposeRecovery();
      addToast("Message sent.");
      onSent();
    } catch (err) {
      // No connectivity: queue the exact prepared payload and deliver it on
      // reconnect instead of losing the compose.
      if (isNetworkError(err) && prepared) {
        const queued = await enqueueOfflineSend(userID, prepared.form, prepared.attachments);
        if (queued) {
          sent = true;
          clearLocalComposeRecovery();
          addToast("No connection — message saved to Outbox. It will send automatically once you're back online.");
          onSent();
          return;
        }
      }
      addToast(messageFromError(err), "error");
    } finally {
      if (!sent) composeSecurity.setTransform({ active: false, phase: "plaintext", ciphertext: "" });
      setSending(false);
    }
  }

  async function saveDraft() {
    const editor = editorRef.current;
    const preparedHTML = prepareComposeHTML(editor?.innerHTML || "", attachments);
    const uploadAttachments = attachments.filter((attachment) => !attachment.inline || preparedHTML.inlineIDs.has(attachment.id));
    const nextForm: ComposeForm = {
      ...form,
      from_identity_id: form.from_identity_id || primaryIdentity?.id || 0,
      body: editor?.innerText || "",
      body_html: preparedHTML.html,
      attach_public_key: composeSecurity.attachPublicKey
    };
    const savedContent = currentLocalComposeContent(nextForm, editorRef.current);
    const savedAttachmentState = composeAttachmentStateKey(nextForm, attachments);
    setSavingDraft(true);
    try {
      const prepared = await composeSecurity.prepareSubmitForm(nextForm, uploadAttachments);
      const data = await api.saveDraft(csrf, prepared.form, prepared.attachments);
      baselineRef.current = savedContent;
      baselineAttachmentStateRef.current = savedAttachmentState;
      clearLocalComposeRecovery();
      setForm((current) => ({ ...current, draft_message_id: data.message_id }));
      addToast("Draft saved.");
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setSavingDraft(false);
    }
  }

  return (
    <form ref={formRef} className={`${inline ? "inline-reply" : "compose-window"}${minimized ? " minimized" : ""}`} onSubmit={submit}>
      {!inline ? (
        <div className="compose-head">
          <span className="compose-head-title">
            <LogoMark className="compose-head-mark" />
            <span>New Message</span>
          </span>
          <div className="compose-head-actions">
            {onMinimize ? (
              <button className="ghost" type="button" title={minimized ? "Restore" : "Minimize"} onClick={minimizeCompose}>
                <Icon name={minimized ? "edit" : "minimize"} />
              </button>
            ) : null}
            <button className="ghost" type="button" title="Discard and close" onClick={discardCompose}>
              <Icon name="close" />
            </button>
          </div>
        </div>
      ) : null}
      <div className="compose-scroll-region">
        {pendingRecovery ? (
          <div className="compose-recovery-banner" role="status">
            <Icon name="draft" />
            <span>
              <strong>Unsent changes found</strong>
              <small>{formatRecoveryTime(pendingRecovery.updatedAt)}</small>
            </span>
            <button className="secondary" type="button" onClick={() => applyRecoveredContent(pendingRecovery)}>Restore</button>
            <button className="ghost" type="button" title="Discard recovered changes" aria-label="Discard recovered changes" onClick={discardPendingRecovery}>
              <Icon name="delete" />
            </button>
          </div>
        ) : null}
        {offlineNow ? (
          <div className="mail-cache-warning" role="status">
            <Icon name="send" />
            <span>You're offline — sending will queue this message in the Outbox and deliver it once you reconnect.</span>
          </div>
        ) : null}
        {!inline ? (
          <div className="compose-fields">
            <div className="compose-line">
              <span>From</span>
              {identities.length > 1 ? (
                <select
                  value={form.from_identity_id || primaryIdentity?.id || 0}
                  onChange={(event) => setField("from_identity_id", Number(event.target.value))}
                >
                  {identities.map((identity) => (
                    <option key={identity.id} value={identity.id}>
                      {identity.header || identity.email}
                    </option>
                  ))}
                </select>
              ) : (
                <strong>{identities[0]?.header || composeFrom}</strong>
              )}
            </div>
            <div className="compose-line">
              <span>To</span>
              <RecipientInput value={form.to} onChange={(value) => setField("to", value)} required />
              <button className="ghost text-link" type="button" onClick={() => setShowCc((value) => !value)}>Cc</button>
              <button className="ghost text-link" type="button" onClick={() => setShowBcc((value) => !value)}>Bcc</button>
            </div>
            {showCc ? (
              <div className="compose-line">
                <span>Cc</span>
                <RecipientInput value={form.cc} onChange={(value) => setField("cc", value)} />
              </div>
            ) : null}
            {showBcc ? (
              <div className="compose-line">
                <span>Bcc</span>
                <RecipientInput value={form.bcc} onChange={(value) => setField("bcc", value)} />
              </div>
            ) : null}
            <div className="compose-line">
              <span>Subject</span>
              <input
                aria-label="Subject"
                value={form.subject}
                onChange={(event) => setField("subject", event.target.value)}
                spellCheck
                required
              />
            </div>
          </div>
        ) : (
          <div className="inline-reply-meta">
            {identities.length > 1 ? (
              <div className="inline-reply-from">
                <span>From</span>
                <select
                  value={form.from_identity_id || primaryIdentity?.id || 0}
                  onChange={(event) => setField("from_identity_id", Number(event.target.value))}
                >
                  {identities.map((identity) => (
                    <option key={identity.id} value={identity.id}>
                      {identity.header || identity.email}
                    </option>
                  ))}
                </select>
              </div>
            ) : null}
            <div className="inline-reply-to">
              <span>To</span>
              <strong>{form.to}</strong>
            </div>
            <div className="inline-reply-actions">
              <button className="ghost text-link" type="button" onClick={() => setShowCc((value) => !value)}>Cc</button>
              <button className="ghost text-link" type="button" onClick={() => setShowBcc((value) => !value)}>Bcc</button>
              <button className="ghost inline-close" type="button" title="Discard reply" onClick={discardCompose}>
                <Icon name="close" />
              </button>
            </div>
          </div>
        )}
        {inline && showCc ? (
          <div className="inline-reply-meta">
            <span>Cc</span>
            <RecipientInput value={form.cc} onChange={(value) => setField("cc", value)} />
          </div>
        ) : null}
        {inline && showBcc ? (
          <div className="inline-reply-meta">
            <span>Bcc</span>
            <RecipientInput value={form.bcc} onChange={(value) => setField("bcc", value)} />
          </div>
        ) : null}
        <div
          className={`compose-body ${composeSecurity.transform.active ? `pgp-transforming pgp-transform-${composeSecurity.transform.phase}` : ""}`}
          onDragOver={(event) => event.preventDefault()}
          onDrop={handleComposeDrop}
        >
          <div
            ref={editorRef}
            className="compose-editor"
            contentEditable={!sending}
            data-placeholder="Write a message"
            onInput={handleEditorInput}
            onPaste={handleEditorPaste}
            spellCheck
            suppressContentEditableWarning
          />
          {composeSecurity.transform.active ? (
            <pre className="compose-pgp-ciphertext" aria-hidden="true">
              {composeSecurity.transform.ciphertext || "Preparing PGP message..."}
            </pre>
          ) : null}
        </div>
        {attachments.length > 0 || includedExistingAttachments.length > 0 || forwardedMessageAttachment || remainingExistingAttachmentCount > 0 || hasAttachmentWarning ? (
          <div className="compose-attachments" aria-live="polite">
            {remainingExistingAttachmentCount > 0 ? (
              <button className="compose-existing-attachment-link ghost text-link" type="button" onClick={includeExistingAttachments}>
                <Icon name="attach_file" />
                Include previous {remainingExistingAttachmentCount === 1 ? "attachment" : "attachments"}
              </button>
            ) : null}
            {includedExistingAttachments.length > 0 || attachments.length > 0 ? (
              <div className="compose-attachment-list">
                {includedExistingAttachments.map((attachment) => (
                  <div className="compose-attachment compose-attachment-existing" key={`existing-${attachment.id}`}>
                    <Icon name="attach_file" />
                    <span>
                      <strong>{composeExistingAttachmentName(attachment)}</strong>
                      <small>Previous attachment - {formatBytes(attachment.size)}</small>
                    </span>
                    <button className="ghost" type="button" title="Remove attachment" onClick={() => removeExistingAttachment(attachment.id)}>
                      <Icon name="close" />
                    </button>
                  </div>
                ))}
                {forwardedMessageAttachment ? (
                  <div className="compose-attachment compose-attachment-existing" key="forwarded-message">
                    <Icon name="file_text" />
                    <span>
                      <strong>{composeExistingAttachmentName(forwardedMessageAttachment)}</strong>
                      <small>Original message - {formatBytes(forwardedMessageAttachment.size)}</small>
                    </span>
                    <button className="ghost" type="button" title="Remove attachment" onClick={removeForwardAttachment}>
                      <Icon name="close" />
                    </button>
                  </div>
                ) : null}
                {attachments.map((attachment) => (
                  <div className="compose-attachment" key={attachment.id}>
                    <Icon name={attachment.inline ? "image" : "attach_file"} />
                    <span>
                      <strong>{attachment.filename}</strong>
                      <small>{attachment.inline ? "Inline media" : "Attachment"} - {formatBytes(attachment.size)}</small>
                    </span>
                    <button className="ghost" type="button" title="Remove attachment" onClick={() => removeAttachment(attachment.id)}>
                      <Icon name="close" />
                    </button>
                  </div>
                ))}
              </div>
            ) : null}
            {hasAttachmentWarning ? (
              <div className="compose-attachment-warning">
                <Icon name="report" />
                <span>Attachments total {formatBytes(totalAttachmentBytes)}. Many providers reject messages over 20 MB.</span>
                {canResizePhotos ? (
                  <button className="ghost text-link" type="button" disabled={resizing} onClick={resizePhotos}>
                    {resizing ? "Resizing..." : "Resize photos"}
                  </button>
                ) : null}
              </div>
            ) : null}
          </div>
        ) : null}
      </div>
      <div className="compose-format" aria-label="Formatting">
        <button type="button" title="Bold" onClick={() => applyFormat("bold")}>B</button>
        <button type="button" title="Italic" onClick={() => applyFormat("italic")}><em>I</em></button>
        <button type="button" title="Underline" onClick={() => applyFormat("underline")}><u>U</u></button>
        <button type="button" title="Text color" onClick={() => applyFormat("foreColor", "#c46b44")}>
          <Icon name="format_color_text" />
        </button>
        <button type="button" title="Bulleted list" onClick={() => applyFormat("insertUnorderedList")}>
          <Icon name="format_list_bulleted" />
        </button>
        <button type="button" title="Numbered list" onClick={() => applyFormat("insertOrderedList")}>
          <Icon name="format_list_numbered" />
        </button>
        <button type="button" title="Quote" onClick={() => applyFormat("formatBlock", "blockquote")}>
          <Icon name="format_quote" />
        </button>
        <details className="compose-template-menu" ref={templateMenuRef}>
          <summary title="Templates" aria-label="Templates">
            <Icon name="bookmark" />
          </summary>
          <div className="compose-template-panel" aria-label="Templates">
            {templates.map((template) => (
              <div className="compose-template-row" key={template.id}>
                <button type="button" title={`Insert ${template.name}`} onClick={() => insertTemplate(template)}>
                  <span>{template.name}</span>
                </button>
                <button type="button" title={`Delete ${template.name}`} aria-label={`Delete ${template.name}`} onClick={() => deleteTemplate(template)}>
                  <Icon name="delete" />
                </button>
              </div>
            ))}
            <div className="compose-template-save-row">
              <input
                value={templateName}
                maxLength={80}
                placeholder="Template name"
                aria-label="Template name"
                onChange={(event) => setTemplateName(event.target.value)}
                onKeyDown={(event) => {
                  if (event.key !== "Enter") return;
                  event.preventDefault();
                  saveCurrentTemplate();
                }}
              />
              <button type="button" title="Save template" aria-label="Save template" disabled={!templateName.trim()} onClick={saveCurrentTemplate}>
                <Icon name="add" />
              </button>
            </div>
          </div>
        </details>
      </div>
      <input
        ref={attachmentInputRef}
        className="compose-file-input"
        type="file"
        multiple
        onChange={(event) => {
          if (event.currentTarget.files) addFiles(event.currentTarget.files, false);
          event.currentTarget.value = "";
        }}
      />
      <input
        ref={inlineMediaInputRef}
        className="compose-file-input"
        type="file"
        accept="image/*,video/*"
        multiple
        onChange={(event) => {
          if (event.currentTarget.files) addFiles(event.currentTarget.files, true);
          event.currentTarget.value = "";
        }}
      />
      {composeSecurity.renderSendChoice(formRef)}
      <div className="compose-sendbar">
        <div className="compose-send-actions">
          <button className="send-button" disabled={sending || savingDraft || resizing}>
            <Icon name="send" />
            {composeSecurity.sendButtonLabel(sending)}
          </button>
          <button className="secondary save-draft-button" type="button" title="Save draft" aria-label={savingDraft ? "Saving draft" : "Save draft"} disabled={sending || savingDraft || resizing} onClick={() => void saveDraft()}>
            <Icon name="draft" />
            <span>{savingDraft ? "Saving..." : "Save draft"}</span>
          </button>
          <div className="compose-lower-tools" aria-label="Message tools">
            {composeSecurity.renderControls({ attachmentInputRef, inlineMediaInputRef })}
          </div>
        </div>
        <button className="ghost" type="button" title="Discard" onClick={discardCompose}>
          <Icon name="delete" />
        </button>
      </div>
    </form>
  );
}

function composeExistingAttachmentName(attachment: ComposeExistingAttachment): string {
  return attachment.filename || attachment.content_type || "Attachment";
}

function createComposeAttachment(file: File, inline: boolean): ComposeAttachment {
  const id = randomAttachmentID();
  const safeID = id.replace(/[^a-zA-Z0-9]/g, "_");
  return {
    id,
    field: `attachment_${safeID}`,
    filename: file.name || "attachment",
    content_type: file.type || "application/octet-stream",
    content_id: `rolltop-${safeID}@compose.local`,
    inline,
    size: file.size,
    file,
    objectURL: inline ? URL.createObjectURL(file) : undefined
  };
}

function randomAttachmentID(): string {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) {
    return crypto.randomUUID();
  }
  return `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

// Convert editor-only inline media markers into MIME Content-ID references that
// the backend can package into the outgoing message.
function prepareComposeHTML(html: string, attachments: ComposeAttachment[]): { html: string; inlineIDs: Set<string> } {
  const template = document.createElement("template");
  template.innerHTML = html;
  template.content.querySelectorAll<HTMLElement>("[data-compose-caret-start]").forEach((node) => {
    node.removeAttribute("data-compose-caret-start");
  });
  const byID = new Map(attachments.map((attachment) => [attachment.id, attachment]));
  const inlineIDs = new Set<string>();
  template.content.querySelectorAll<HTMLElement>("[data-compose-attachment-id]").forEach((node) => {
    const id = node.dataset.composeAttachmentId || "";
    const attachment = byID.get(id);
    if (!attachment || !attachment.inline) return;
    inlineIDs.add(id);
    node.removeAttribute("data-compose-attachment-id");
    node.classList.remove("compose-inline-media");
    if (node instanceof HTMLImageElement || node instanceof HTMLVideoElement) {
      node.setAttribute("src", `cid:${attachment.content_id}`);
    }
  });
  return { html: template.innerHTML, inlineIDs };
}

function initialEditorHTML(initial: ComposeForm): string {
  const html = initial.body_html || textToHTML(initial.body);
  if (!isForwardDraft(initial)) return html;
  return `<div data-compose-caret-start="true"><br></div>${stripLeadingBreaks(html)}`;
}

function composeLocalContext(context: string, initial: ComposeForm, inline: boolean): string {
  const query = context.replace(/^\?/, "").trim();
  const params = new URLSearchParams(query);
  for (const key of ["draft", "reply", "reply_all", "forward", "forward_attachment", "android_share"]) {
    const value = params.get(key);
    if (value) return `${key}:${value}`;
  }
  if (initial.draft_message_id > 0) return `draft:${initial.draft_message_id}`;
  if (initial.in_reply_to_id > 0) return `${inline ? "inline-reply" : "reply"}:${initial.in_reply_to_id}`;
  return query && query !== "new" ? `prefill:${query}` : "new";
}

function explicitServerComposeContext(context: string, initial: ComposeForm): boolean {
  if (initial.draft_message_id > 0 || initial.in_reply_to_id > 0) return true;
  const params = new URLSearchParams(context.replace(/^\?/, ""));
  return ["draft", "reply", "reply_all", "forward", "forward_attachment"].some((key) => params.has(key));
}

function localComposeContent(form: ComposeForm, html: string): LocalComposeContent {
  const safeHTML = recoverableEditorHTML(html);
  return {
    to: form.to.trim(),
    cc: form.cc.trim(),
    bcc: form.bcc.trim(),
    subject: form.subject,
    body: composeTextFromHTML(safeHTML),
    bodyHTML: safeHTML,
    fromIdentityID: form.from_identity_id || 0
  };
}

function currentLocalComposeContent(form: ComposeForm, editor: HTMLDivElement | null): LocalComposeContent {
  return localComposeContent(form, editor?.innerHTML || form.body_html || textToHTML(form.body));
}

function recoveryContent(recovery: LocalComposeRecovery): LocalComposeContent {
  return {
    to: recovery.to,
    cc: recovery.cc,
    bcc: recovery.bcc,
    subject: recovery.subject,
    body: composeTextFromHTML(recovery.bodyHTML || textToHTML(recovery.body)),
    bodyHTML: recoverableEditorHTML(recovery.bodyHTML || textToHTML(recovery.body)),
    fromIdentityID: recovery.fromIdentityID
  };
}

function recoverableEditorHTML(html: string): string {
  const clean = DOMPurify.sanitize(html, {
    USE_PROFILES: { html: true },
    FORBID_TAGS: ["script", "style", "iframe", "object", "embed", "form"],
    FORBID_ATTR: ["contenteditable"]
  });
  const template = document.createElement("template");
  template.innerHTML = clean;
  template.content.querySelectorAll<HTMLElement>("[data-compose-attachment-id]").forEach((node) => {
    const next = node.nextSibling;
    node.remove();
    if (next instanceof HTMLBRElement) next.remove();
  });
  template.content.querySelectorAll<HTMLElement>("[data-compose-caret-start]").forEach((node) => node.removeAttribute("data-compose-caret-start"));
  template.content.querySelectorAll<HTMLElement>("[src^='blob:']").forEach((node) => node.remove());
  return template.innerHTML;
}

function safeRecoveredEditorHTML(recovery: LocalComposeRecovery): string {
  return recoverableEditorHTML(recovery.bodyHTML || textToHTML(recovery.body));
}

function safeTemplateHTML(template: LocalComposeTemplate): string {
  return recoverableEditorHTML(template.bodyHTML || textToHTML(template.body));
}

function composeTextFromHTML(html: string): string {
  const template = document.createElement("template");
  template.innerHTML = html;
  template.content.querySelectorAll("br").forEach((node) => node.replaceWith("\n"));
  template.content.querySelectorAll("p, div, blockquote, li").forEach((node) => node.append("\n"));
  return (template.content.textContent || "").replace(/\n{3,}/g, "\n\n").replace(/\n+$/, "");
}

function composeIsDirty(form: ComposeForm, editor: HTMLDivElement | null, baseline: LocalComposeContent | null): boolean {
  return baseline ? !composeContentEqual(currentLocalComposeContent(form, editor), baseline) : false;
}

function composeAttachmentStateKey(form: ComposeForm, attachments: ComposeAttachment[]): string {
  const existing = [...(form.include_attachment_ids || [])].filter((id) => id > 0).sort((left, right) => left - right);
  const uploads = attachments.map((attachment) => [
    attachment.id,
    attachment.filename,
    attachment.size,
    attachment.file.lastModified,
    attachment.inline ? 1 : 0
  ]);
  return JSON.stringify({ existing, forward: form.forward_attachment_message_id || 0, uploads });
}

function insertComposeHTML(editor: HTMLDivElement, html: string) {
  const selection = window.getSelection();
  const range = selection && selection.rangeCount > 0 ? selection.getRangeAt(0) : null;
  const fragment = document.createRange().createContextualFragment(html);
  if (range && editor.contains(range.commonAncestorContainer)) {
    range.deleteContents();
    const last = fragment.lastChild;
    range.insertNode(fragment);
    if (last) {
      range.setStartAfter(last);
      range.collapse(true);
      selection?.removeAllRanges();
      selection?.addRange(range);
    }
  } else {
    editor.appendChild(fragment);
  }
  editor.focus();
}

function randomTemplateID(): string {
  if (typeof crypto !== "undefined" && "randomUUID" in crypto) return crypto.randomUUID();
  return `template-${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

function formatRecoveryTime(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return "Saved locally";
  return new Intl.DateTimeFormat(undefined, { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" }).format(new Date(value));
}

function isForwardDraft(initial: ComposeForm): boolean {
  const subject = initial.subject.trim().toLowerCase();
  if (subject.startsWith("fwd:") || subject.startsWith("fw:")) return true;
  const body = `${initial.body_html}\n${initial.body}`.toLowerCase();
  return body.includes("rolltop-forwarded-body") || body.includes("forwarded message");
}

function stripLeadingBreaks(html: string): string {
  return html.replace(/^(?:\s|<br\s*\/?>|<div><br><\/div>|<p><br><\/p>)+/i, "");
}

function placeInitialCaret(editor: HTMLDivElement) {
  const marker = editor.querySelector<HTMLElement>("[data-compose-caret-start]");
  if (!marker) return;
  editor.focus({ preventScroll: true });
  const range = document.createRange();
  range.selectNodeContents(marker);
  range.collapse(true);
  const selection = window.getSelection();
  selection?.removeAllRanges();
  selection?.addRange(range);
}

function removeInlineAttachmentElement(editor: HTMLDivElement | null, id: string) {
  if (!editor) return;
  editor.querySelectorAll<HTMLElement>("[data-compose-attachment-id]").forEach((node) => {
    if (node.dataset.composeAttachmentId !== id) return;
    const next = node.nextSibling;
    node.remove();
    if (next instanceof HTMLBRElement) next.remove();
  });
}

function syncInlineAttachmentObjectURLs(editor: HTMLDivElement | null, attachments: ComposeAttachment[]) {
  if (!editor) return;
  const byID = new Map(attachments.map((attachment) => [attachment.id, attachment]));
  editor.querySelectorAll<HTMLElement>("[data-compose-attachment-id]").forEach((node) => {
    const attachment = byID.get(node.dataset.composeAttachmentId || "");
    if (!attachment?.objectURL) return;
    if (node instanceof HTMLImageElement || node instanceof HTMLVideoElement) {
      node.setAttribute("src", attachment.objectURL);
    }
  });
}

function revokeAttachmentObjectURLs(attachments: ComposeAttachment[]) {
  attachments.forEach((attachment) => {
    if (attachment.objectURL) URL.revokeObjectURL(attachment.objectURL);
  });
}

function isInlineMedia(file: File): boolean {
  return file.type.startsWith("image/") || file.type.startsWith("video/");
}

function isResizablePhoto(file: File): boolean {
  return file.type === "image/jpeg" || file.type === "image/png" || file.type === "image/webp";
}

// Client-side photo resizing is opportunistic: if the resized JPEG is not
// smaller, keep the original file so image quality is not degraded for no gain.
async function resizePhotoFile(file: File): Promise<File | null> {
  if (!isResizablePhoto(file)) return null;
  const image = await loadImage(file);
  const largestEdge = Math.max(image.naturalWidth, image.naturalHeight);
  if (largestEdge <= 0) return null;
  const scale = Math.min(1, RESIZE_PHOTO_MAX_EDGE / largestEdge);
  const width = Math.max(1, Math.round(image.naturalWidth * scale));
  const height = Math.max(1, Math.round(image.naturalHeight * scale));
  const canvas = document.createElement("canvas");
  canvas.width = width;
  canvas.height = height;
  const context = canvas.getContext("2d");
  if (!context) return null;
  context.fillStyle = "#fff";
  context.fillRect(0, 0, width, height);
  context.drawImage(image, 0, 0, width, height);
  const blob = await new Promise<Blob | null>((resolve) => canvas.toBlob(resolve, "image/jpeg", RESIZE_PHOTO_QUALITY));
  if (!blob || blob.size >= file.size) return null;
  return new File([blob], photoFilename(file.name), { type: "image/jpeg", lastModified: Date.now() });
}

function loadImage(file: File): Promise<HTMLImageElement> {
  return new Promise((resolve, reject) => {
    const url = URL.createObjectURL(file);
    const image = new Image();
    image.onload = () => {
      URL.revokeObjectURL(url);
      resolve(image);
    };
    image.onerror = () => {
      URL.revokeObjectURL(url);
      reject(new Error("image load failed"));
    };
    image.src = url;
  });
}

function photoFilename(filename: string): string {
  const trimmed = filename.trim() || "photo";
  return trimmed.replace(/\.[^.]*$/, "") + ".jpg";
}

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / 1024 / 1024).toFixed(1)} MB`;
}


function recipientEmailAddresses(values: string[]): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  values.flatMap(parseRecipientList).forEach((recipient) => {
    if (!recipient.valid || seen.has(recipient.emailKey)) return;
    seen.add(recipient.emailKey);
    out.push(recipient.email);
  });
  return out;
}

type RecipientToken = {
  raw: string;
  name: string;
  email: string;
  emailKey: string;
  valid: boolean;
};

type RecipientSuggestion = {
  key: string;
  name: string;
  email: string;
  iconURL: string;
  source: "rolltop" | "android";
};

function RecipientInput({
  value,
  required = false,
  onChange
}: {
  value: string;
  required?: boolean;
  onChange: (value: string) => void;
}) {
  const [inputValue, setInputValue] = useState("");
  const [rolltopSuggestions, setRolltopSuggestions] = useState<ContactAutocomplete[]>([]);
  const [nativeSuggestions, setNativeSuggestions] = useState<NativeContactEmail[]>([]);
  const [nativeAccess, setNativeAccess] = useState<AndroidContactAccess | "unknown">("unknown");
  const [focused, setFocused] = useState(false);
  const [activeSuggestion, setActiveSuggestion] = useState(0);
  const [pickingAndroidContact, setPickingAndroidContact] = useState(false);
  const [requestingNativeAccess, setRequestingNativeAccess] = useState(false);
  const inputRef = useRef<HTMLInputElement | null>(null);
  const recipients = useMemo(() => dedupeRecipients(parseRecipientList(value)), [value]);
  const query = inputValue.trim();
  const nativeAvailable = androidNativeAvailable();
  const suggestions = useMemo(
    () => {
      const selected = new Set(recipients.map((recipient) => recipient.emailKey).filter(Boolean));
      return mergeRecipientSuggestions(rolltopSuggestions, nativeSuggestions).filter((suggestion) => !selected.has(suggestion.email.trim().toLowerCase()));
    },
    [recipients, rolltopSuggestions, nativeSuggestions]
  );

  useEffect(() => {
    let cancelled = false;
    if (!focused) {
      setRolltopSuggestions([]);
      setNativeSuggestions([]);
      return;
    }
    const timer = window.setTimeout(() => {
      api.contactAutocomplete(query).then((data) => {
        if (!cancelled) setRolltopSuggestions(data.contacts || []);
      }).catch(() => {
        if (!cancelled) setRolltopSuggestions([]);
      });
      if (query.length >= 2 && nativeAvailable && nativeAccess !== "denied") {
        androidContactSuggestions(query).then((data) => {
          if (cancelled) return;
          setNativeAccess(data.access);
          setNativeSuggestions(data.contacts);
        }).catch(() => {
          if (!cancelled) setNativeSuggestions([]);
        });
      } else {
        setNativeSuggestions([]);
      }
    }, 120);
    return () => {
      window.clearTimeout(timer);
      cancelled = true;
    };
  }, [focused, nativeAccess, nativeAvailable, query]);

  useEffect(() => {
    setActiveSuggestion(0);
  }, [query, suggestions.length]);

  useEffect(() => {
    const invalid = recipients.some((recipient) => !recipient.valid);
    const pending = inputValue.trim();
    const pendingRecipients = pending ? parseRecipientList(pending) : [];
    const pendingInvalid = pending !== "" && (pendingRecipients.length === 0 || pendingRecipients.some((recipient) => !recipient.valid));
    inputRef.current?.setCustomValidity(invalid || pendingInvalid ? "Enter a valid email address." : "");
  }, [inputValue, recipients]);

  useEffect(() => {
    const normalized = serializeRecipients(recipients);
    if (normalized !== value) onChange(normalized);
  }, [recipients, value]);

  function commitRecipients(raw: string) {
    const additions = parseRecipientList(raw);
    if (additions.length === 0) return;
    const next = dedupeRecipients([...recipients, ...additions]);
    onChange(serializeRecipients(next));
    setInputValue("");
    setRolltopSuggestions([]);
    setNativeSuggestions([]);
  }

  function removeRecipient(index: number) {
    onChange(serializeRecipients(recipients.filter((_, recipientIndex) => recipientIndex !== index)));
    window.requestAnimationFrame(() => inputRef.current?.focus());
  }

  function choose(contact: RecipientSuggestion) {
    commitRecipients(formatNativeRecipient(contact.name, contact.email));
    setFocused(true);
    window.requestAnimationFrame(() => inputRef.current?.focus());
  }

  function handleRecipientKeyDown(event: ReactKeyboardEvent<HTMLInputElement>) {
    if (event.key === "ArrowDown" && suggestions.length > 0) {
      event.preventDefault();
      setActiveSuggestion((current) => (current + 1) % suggestions.length);
      return;
    }
    if (event.key === "ArrowUp" && suggestions.length > 0) {
      event.preventDefault();
      setActiveSuggestion((current) => (current - 1 + suggestions.length) % suggestions.length);
      return;
    }
    if (event.key === "Escape") {
      setFocused(false);
      setNativeSuggestions([]);
      setRolltopSuggestions([]);
      return;
    }
    if (event.key === "Backspace" && !inputValue && recipients.length > 0) {
      event.preventDefault();
      removeRecipient(recipients.length - 1);
      return;
    }
    if (event.key === "Enter" && suggestions[activeSuggestion]) {
      event.preventDefault();
      choose(suggestions[activeSuggestion]);
      return;
    }
    if (event.key === "Enter" || event.key === "," || event.key === ";") {
      if (!inputValue.trim()) return;
      event.preventDefault();
      commitRecipients(inputValue);
      return;
    }
    if (event.key === "Tab" && inputValue.trim()) commitRecipients(inputValue);
  }

  async function enableAndroidContactSuggestions() {
    if (requestingNativeAccess) return;
    setRequestingNativeAccess(true);
    try {
      const access = await requestAndroidContactAccess();
      setNativeAccess(access);
      if (access !== "granted") {
        setNativeSuggestions([]);
        return;
      }
      const data = await androidContactSuggestions(query);
      setNativeAccess(data.access);
      setNativeSuggestions(data.contacts);
    } catch {
      setNativeAccess("permission_required");
      setNativeSuggestions([]);
    } finally {
      setRequestingNativeAccess(false);
    }
  }

  async function chooseAndroidContact() {
    if (pickingAndroidContact) return;
    setPickingAndroidContact(true);
    try {
      const contact = await pickAndroidContactEmail();
      if (!contact) return;
      commitRecipients(formatNativeRecipient(contact.name, contact.email));
    } catch {
      // The system picker may be unavailable or dismissed while the WebView is changing pages.
    } finally {
      setPickingAndroidContact(false);
    }
  }

  return (
    <div className="recipient-input">
      <div className="recipient-input-control">
        <div className="recipient-chip-list">
          {recipients.map((recipient, index) => (
            <span className={`recipient-chip ${recipient.valid ? "" : "invalid"}`} key={`${recipient.emailKey || recipient.raw.toLowerCase()}:${index}`} title={recipient.valid ? recipient.email : "Invalid email address"}>
              <span>{recipient.name || recipient.email || recipient.raw}</span>
              <button type="button" title={`Remove ${recipient.name || recipient.email || "recipient"}`} aria-label={`Remove ${recipient.name || recipient.email || "recipient"}`} onClick={() => removeRecipient(index)}>
                <Icon name="close" />
              </button>
            </span>
          ))}
          <input
            ref={inputRef}
            value={inputValue}
            required={required && recipients.length === 0}
            aria-label="Add recipient"
            onFocus={() => setFocused(true)}
            onBlur={() => {
              if (inputValue.trim()) commitRecipients(inputValue);
              window.setTimeout(() => setFocused(false), 120);
            }}
            onChange={(event) => {
              const next = event.target.value;
              if (/[;,]$/.test(next)) commitRecipients(next);
              else setInputValue(next);
            }}
            onKeyDown={handleRecipientKeyDown}
          />
        </div>
        {nativeAvailable ? (
          <button
            className="ghost native-contact-picker"
            type="button"
            title="Choose Android contact"
            disabled={pickingAndroidContact}
            onClick={() => void chooseAndroidContact()}
          >
            <Icon name="group" />
          </button>
        ) : null}
      </div>
      {focused && (suggestions.length > 0 || (nativeAvailable && nativeAccess === "permission_required")) ? (
        <div className="recipient-suggest">
          {suggestions.map((contact, index) => (
            <button
              type="button"
              key={contact.key}
              title={contact.source === "android" ? "Android contact" : undefined}
              className={activeSuggestion === index ? "active" : undefined}
              onMouseDown={(event) => {
                event.preventDefault();
                choose(contact);
              }}
            >
              {contact.iconURL ? <img src={contact.iconURL} alt="" /> : <span>{(contact.name || contact.email).slice(0, 1).toUpperCase()}</span>}
              <strong>{contact.name || contact.email}</strong>
              <small>{contact.email}</small>
            </button>
          ))}
          {nativeAvailable && nativeAccess === "permission_required" ? (
            <button
              type="button"
              className="native-contact-enable"
              disabled={requestingNativeAccess}
              onMouseDown={(event) => {
                event.preventDefault();
                void enableAndroidContactSuggestions();
              }}
            >
              <span><Icon name="group" /></span>
              <strong>{requestingNativeAccess ? "Requesting access..." : "Enable Android contacts"}</strong>
              <small>Device suggestions</small>
            </button>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

function parseRecipientList(value: string): RecipientToken[] {
  return splitRecipientValues(value).map(parseRecipient).filter((recipient) => recipient.raw !== "");
}

function splitRecipientValues(value: string): string[] {
  const values: string[] = [];
  let start = 0;
  let quoted = false;
  let angleDepth = 0;
  for (let index = 0; index < value.length; index += 1) {
    const char = value[index];
    if (char === '"' && value[index - 1] !== "\\") quoted = !quoted;
    else if (!quoted && char === "<") angleDepth += 1;
    else if (!quoted && char === ">") angleDepth = Math.max(0, angleDepth - 1);
    else if (!quoted && angleDepth === 0 && (char === "," || char === ";")) {
      values.push(value.slice(start, index).trim());
      start = index + 1;
    }
  }
  values.push(value.slice(start).trim());
  return values.filter(Boolean);
}

function parseRecipient(rawValue: string): RecipientToken {
  const raw = rawValue.trim();
  const angle = raw.match(/^(.*)<([^<>]+)>\s*$/);
  const email = (angle ? angle[2] : raw).trim();
  const name = angle ? angle[1].trim().replace(/^"|"$/g, "") : "";
  const valid = /^[a-z0-9.!#$%&'*+/=?^_`{|}~-]+@[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$/i.test(email);
  return { raw, name, email, emailKey: valid ? email.toLowerCase() : "", valid };
}

function dedupeRecipients(recipients: RecipientToken[]): RecipientToken[] {
  const seen = new Set<string>();
  return recipients.filter((recipient) => {
    const key = recipient.emailKey || `invalid:${recipient.raw.toLowerCase()}`;
    if (seen.has(key)) return false;
    seen.add(key);
    return true;
  });
}

function serializeRecipients(recipients: RecipientToken[]): string {
  return recipients.map((recipient) => recipient.raw).join(", ");
}

function mergeRecipientSuggestions(
  rolltop: ContactAutocomplete[],
  android: NativeContactEmail[]
): RecipientSuggestion[] {
  const serverRows = rolltop.map((contact) => ({
    key: `rolltop:${contact.contact_id}:${contact.email.toLowerCase()}`,
    name: contact.name,
    email: contact.email,
    iconURL: contact.icon_url,
    source: "rolltop" as const
  }));
  const nativeRows = android.map((contact) => ({
    key: `android:${contact.email.toLowerCase()}`,
    name: contact.name,
    email: contact.email,
    iconURL: "",
    source: "android" as const
  }));
  const seen = new Set<string>();
  const merged: RecipientSuggestion[] = [];
  for (let index = 0; index < Math.max(serverRows.length, nativeRows.length); index++) {
    for (const row of [serverRows[index], nativeRows[index]]) {
      if (!row) continue;
      const key = row.email.trim().toLowerCase();
      if (!key || seen.has(key)) continue;
      seen.add(key);
      merged.push(row);
      if (merged.length === 8) return merged;
    }
  }
  return merged;
}

function formatNativeRecipient(name: string, email: string): string {
  const trimmedName = name.trim();
  if (!trimmedName || trimmedName.toLowerCase() === email.toLowerCase()) return email;
  return `"${trimmedName.replaceAll('"', "'")}" <${email}>`;
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => window.setTimeout(resolve, ms));
}

function waitForFrame(): Promise<void> {
  return new Promise((resolve) => window.requestAnimationFrame(() => resolve()));
}
