// File overview: Conversation detail view. It loads thread bodies, shows IMAP/blob fetch status,
// renders message headers/actions, handles inline replies, image trust, and search highlights.

import { Fragment, useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { MouseEvent, ReactNode } from "react";
import { Star } from "@phosphor-icons/react";
import { api } from "../../api";
import type { DatePrefs, LocationState, SecurityUnlockState, Toast } from "../../appTypes";
import type { Attachment, AuthenticationResult, Bootstrap, ComposeForm, ComposeIdentity, ContactPGPKey, HeaderDetail, Mailbox, MessageOriginalSource, MessageSecurityIndicators, SearchExplanation, ThreadMessage } from "../../types";
import { Icon } from "../../components/Icon";
import { androidNativeAvailable } from "../../lib/androidNative";
import { messageFromError } from "../../lib/errors";
import { displayDateTime, displaySnoozeUntil, displayTime, formatBytes } from "../../lib/format";
import { trashMailboxesByAccount } from "../../lib/folders";
import { shouldIgnoreMailShortcut } from "../../lib/keyboard";
import { HighlightedText, highlightEmailDocument } from "../../lib/searchHighlight";
import { messageBackURL, messageHighlightQuery, messageHighlightTerms, messageSearchHitID } from "../../lib/routes";
import { ComposeBox } from "../compose/ComposeViews";
import { AttachmentPreviewSlot } from "../../plugins/attachmentPreview";
import { brandDomainKeyForThread, loadBrandIconsForDomains } from "../../plugins/bimiBrandIcons";
import { messageSecurityPreviewText, messageSecuritySnippetClassName } from "../../plugins/messageSecurity";
import { OneClickUnsubscribeInlineAction, OneClickUnsubscribeMenuAction } from "../../plugins/oneClickUnsubscribe";
import { RemoteImageNotice } from "../../plugins/remoteImageBlocklist/RemoteImageNotice";
import { createPluginSet } from "../../plugins/registry";
import { senderVisualURL } from "../../plugins/senderVisuals";
import { TrustImageSourceAction } from "../../plugins/trustedImageSources/TrustImageSourceAction";
import type { RuntimeMessageDetailsPlugin, RuntimePlugin } from "../../plugins/runtime";
import { threadSecurityPlugin, type ThreadSecurityDecryptedAttachment, type ThreadSecurityGossipKey, type ThreadSecurityOpenResult, type ThreadSecuritySignatureStatus } from "../../plugins/threadSecurity";
import { SnoozeControl } from "./SnoozeControl";

type MessageLoadStatus = {
  conversation: number;
  imap_fetch_count: number;
  local_blob_count: number;
  indexed_count: number;
  unavailable_count: number;
  source: string;
};

type SearchExplanationState = {
  open: boolean;
  loading: boolean;
  error: string;
  data: SearchExplanation | null;
};

type OriginalSourceState = {
  messageID: number;
  loading: boolean;
  error: string;
  data: MessageOriginalSource | null;
};

type MessagePluginPanelState = {
  messageID: number;
  panelID: string;
};

type MessageActionPlugin = RuntimePlugin & {
  renderMessageMenuActions?: (context: MessageActionContext) => ReactNode;
  renderMessageActionPanels?: (context: MessageActionContext) => ReactNode;
};

type MessageAnnotationPlugin = RuntimePlugin & {
  renderMessageAnnotations?: (context: {
    location: "message-list" | "thread";
    message: ThreadMessage["message"];
    annotations: NonNullable<ThreadMessage["message"]["annotations"]>;
  }) => ReactNode;
};

type MessageActionContext = {
  csrf: string;
  item: ThreadMessage;
  datePrefs: DatePrefs;
  activePanel: string;
  openPanel: (panelID: string) => void;
  closePanel: () => void;
  addToast: (message: string, kind?: Toast["kind"]) => number;
};

type PGPBodyState = {
  loading: boolean;
  error: string;
  doc: string;
  status: string;
  signatureStatus?: ThreadSecuritySignatureStatus;
  signatureDetail?: string;
  securityDetails?: string[];
  quoteText?: string;
  protectedSubject?: string;
  decryptedAttachments?: ThreadSecurityDecryptedAttachment[];
};

type PGPVerificationKeys = {
  armors: string[];
  senderKeyCount: number;
  senderEmail: string;
  loadError?: string;
};

type AttachmentPGPImportState = {
  status: "candidate" | "checking" | "ready" | "same" | "different" | "importing" | "imported" | "ignored" | "error";
  email?: string;
  key?: ContactPGPKey;
  existing?: ContactPGPKey;
  error?: string;
};

const PGP_ATTACHMENT_AUTO_PARSE_BYTES = 1024;
const PGP_ATTACHMENT_IMPORT_BYTES = 16 * 1024;

function revokeDecryptedMIMEAttachments(items: ThreadSecurityDecryptedAttachment[] | undefined) {
  (items || []).forEach((attachment) => URL.revokeObjectURL(attachment.objectURL));
}

function shouldShowLoadStatus(status: MessageLoadStatus | null): status is MessageLoadStatus {
  return Boolean(status && (status.imap_fetch_count > 0 || status.source === "local_blob" || status.source === "local"));
}

function loadStatusTitle(status: MessageLoadStatus): string {
  if (status.imap_fetch_count > 0) {
    const count = status.imap_fetch_count;
    return `Fetching ${count} ${count === 1 ? "message" : "messages"} from IMAP server`;
  }
  if (status.source === "local_blob") {
    const count = status.local_blob_count || status.conversation;
    return `Loading ${count} ${count === 1 ? "message" : "messages"} from local blob store`;
  }
  const count = status.conversation;
  return `Loading ${count} ${count === 1 ? "message" : "messages"} from local storage`;
}

function loadStatusDetail(status: MessageLoadStatus): string {
  if (status.imap_fetch_count > 0) {
    return status.local_blob_count > 0
      ? `${status.local_blob_count} already local; waiting on the IMAP server for the rest.`
      : "Retrieving full message bodies and attachment data.";
  }
  if (status.source === "local_blob") {
    return "Everything needed for this conversation is already in the local blob store.";
  }
  return "Using local message data; no IMAP fetch is needed.";
}

function messageActionContext({
  csrf,
  item,
  datePrefs,
  activePanel,
  openPanel,
  closePanel,
  addToast
}: MessageActionContext): MessageActionContext {
  return { csrf, item, datePrefs, activePanel, openPanel, closePanel, addToast };
}

function messageMenuActionNodes(plugins: readonly RuntimePlugin[], context: MessageActionContext) {
  return (plugins as readonly MessageActionPlugin[])
    .map((plugin, index) => {
      const node = plugin.renderMessageMenuActions?.(context);
      return node ? <Fragment key={`message-menu-plugin-${index}`}>{node}</Fragment> : null;
    })
    .filter(Boolean);
}

function messageActionPanelNodes(plugins: readonly RuntimePlugin[], context: MessageActionContext) {
  return (plugins as readonly MessageActionPlugin[])
    .map((plugin, index) => {
      const node = plugin.renderMessageActionPanels?.(context);
      return node ? <Fragment key={`message-panel-plugin-${index}`}>{node}</Fragment> : null;
    })
    .filter(Boolean);
}

function messageAnnotationNodes(plugins: readonly RuntimePlugin[], item: ThreadMessage) {
  const annotations = item.message.annotations || [];
  if (annotations.length === 0) return [];
  return (plugins as readonly MessageAnnotationPlugin[])
    .map((plugin, index) => {
      const node = plugin.renderMessageAnnotations?.({ location: "thread", message: item.message, annotations });
      return node ? <Fragment key={`message-annotation-${index}`}>{node}</Fragment> : null;
    })
    .filter(Boolean);
}

function messageDetailsNodes(plugins: readonly RuntimePlugin[], item: ThreadMessage, datePrefs: DatePrefs) {
  const annotations = item.message.annotations || [];
  return (plugins as readonly RuntimeMessageDetailsPlugin[])
    .map((plugin, index) => {
      const node = plugin.renderMessageDetails?.({ message: item.message, annotations, datePrefs });
      return node ? <Fragment key={`message-details-plugin-${index}`}>{node}</Fragment> : null;
    })
    .filter(Boolean);
}

// MessageDetailsToggle keeps the Gmail-style compact recipient line but exposes
// full message headers without leaving the conversation view.
function MessageDetailsToggle({
  summary,
  details,
  indicators,
  pluginRows,
  highlightQuery,
  highlightTerms
}: {
  summary: string;
  details: HeaderDetail[];
  indicators?: MessageSecurityIndicators;
  pluginRows: ReactNode[];
  highlightQuery: string;
  highlightTerms: string[];
}) {
  const visibleDetails = details.filter((detail) => detail.value.trim() !== "");
  const authenticationResults = reportedAuthenticationResults(indicators);
  if (visibleDetails.length === 0 && authenticationResults.length === 0 && pluginRows.length === 0) {
    return (
      <div className="thread-recipients">
        <HighlightedText text={summary} query={highlightQuery} terms={highlightTerms} />
      </div>
    );
  }
  return (
    <details className="thread-recipients message-details" onClick={(event) => event.stopPropagation()}>
      <summary>
        <span>
          <HighlightedText text={summary} query={highlightQuery} terms={highlightTerms} />
        </span>
        <Icon name="expand_more" />
      </summary>
      <dl>
        {authenticationResults.length > 0 ? <ReportedAuthenticationDetails results={authenticationResults} /> : null}
        {visibleDetails.map((detail) => (
          <Fragment key={`${detail.label}:${detail.value}`}>
            <dt>{detail.label}</dt>
            <dd>
              <HighlightedText text={detail.value} query={highlightQuery} terms={highlightTerms} />
            </dd>
          </Fragment>
        ))}
        {pluginRows}
      </dl>
    </details>
  );
}

function SenderVisualOrAvatar({
  src,
  initial
}: {
  src: string;
  initial: string;
}) {
  const [failedSrc, setFailedSrc] = useState("");

  useEffect(() => {
    setFailedSrc("");
  }, [src]);

  if (src && failedSrc !== src) {
    return <img className="thread-brand-icon" src={src} alt="" loading="lazy" onError={() => setFailedSrc(src)} />;
  }
  return <div className="avatar">{initial}</div>;
}

const reportedAuthenticationMethods = [
  { key: "spf", label: "SPF" },
  { key: "dkim", label: "DKIM" },
  { key: "dmarc", label: "DMARC" }
] as const;

function authenticationResultClass(result: string): string {
  const normalized = result.toLowerCase();
  if (normalized === "pass") return "pass";
  if (["fail", "softfail", "temperror", "permerror"].includes(normalized)) return "attention";
  return "neutral";
}

function authenticationResultSource(result: AuthenticationResult): string {
  return result.source === "received-spf" ? "Received-SPF" : "Authentication-Results";
}

function authenticationResultTooltip(method: string, result: AuthenticationResult): string {
  const source = authenticationResultSource(result);
  return `Reported ${method} result: ${result.result}. Source: ${source} message header. Rolltop did not independently verify this result.`;
}

function reportedAuthenticationResults(indicators?: MessageSecurityIndicators) {
  const reported = indicators?.reported_authentication;
  return reportedAuthenticationMethods.flatMap(({ key, label }) => {
    const result = reported?.[key];
    return result ? [{ key, label, result }] : [];
  });
}

function ReportedAuthenticationDetails({ results }: { results: ReturnType<typeof reportedAuthenticationResults> }) {
  const sourceSummary = results
    .map(({ label, result }) => `${label}: ${authenticationResultSource(result)}`)
    .join("; ");
  return (
    <>
      <dt className="reported-authentication-details-label">authentication</dt>
      <dd className="reported-authentication-details">
        <span className="reported-authentication-detail-pills" role="list" aria-label="Reported message authentication results">
          {results.map(({ key, label, result }) => {
            const statusClass = authenticationResultClass(result.result);
            const tooltip = authenticationResultTooltip(label, result);
            return (
              <span
                className={`reported-authentication-detail-pill ${statusClass}`}
                key={key}
                role="listitem"
                title={tooltip}
                aria-label={tooltip}
              >
                <Icon name={statusClass === "attention" ? "shield_warning" : "shield"} weight={statusClass === "attention" ? "fill" : "regular"} />
                <span>{label}</span>
                <strong>{result.result}</strong>
              </span>
            );
          })}
        </span>
        <span className="reported-authentication-detail-source">
          Sources: {sourceSummary}. Reported by message headers; Rolltop did not independently verify these results.
        </span>
      </dd>
    </>
  );
}

function ReportedAuthenticationIndicators({ indicators }: { indicators?: MessageSecurityIndicators }) {
  const results = reportedAuthenticationResults(indicators)
    .filter(({ result }) => authenticationResultClass(result.result) === "attention");
  if (results.length === 0) return null;
  const tooltip = [
    "Authentication results reported by message headers. Rolltop did not independently verify them.",
    ...results.map(({ label, result }) => authenticationResultTooltip(label, result))
  ].join("\n");
  return (
    <span className="reported-authentication attention" title={tooltip} aria-label={tooltip} role="note">
      <Icon name="shield_warning" weight="fill" />
      <span className="reported-authentication-label">Authentication warning</span>
      {results.map(({ key, label, result }) => (
        <span className={`reported-authentication-result ${authenticationResultClass(result.result)}`} key={key}>
          {label} {result.result}
        </span>
      ))}
    </span>
  );
}

function MessageSenderSecurityCaution({ indicators }: { indicators?: MessageSecurityIndicators }) {
  const signals = (indicators?.signals || []).filter((signal) => (
    signal.kind === "sender_display_address_mismatch" || signal.kind === "reply_to_domain_mismatch"
  ));
  if (signals.length === 0) return null;
  const details = signals.map((signal) => {
    if (signal.kind === "sender_display_address_mismatch") {
      const domains = signal.display_host && signal.target_host
        ? ` The displayed address uses ${signal.display_host}; the actual sender uses ${signal.target_host}.`
        : "";
      return `The sender name contains an address from a different domain.${domains}`;
    }
    const domains = signal.display_host && signal.target_host
      ? ` The From address uses ${signal.display_host}; replies go to ${signal.target_host}.`
      : "";
    return `The Reply-To address uses a different domain from the sender.${domains}`;
  });
  const tooltip = `${details.join("\n")} This can be legitimate and is not proof of phishing.`;
  return (
    <span className="sender-security-caution" title={tooltip} aria-label={tooltip}>
      <Icon name="shield_warning" />
      Sender caution
    </span>
  );
}

function MessageLinkSecurityNotice({ indicators }: { indicators?: MessageSecurityIndicators }) {
  const signals = indicators?.signals || [];
  const hasDestinationMismatch = signals.some((signal) => signal.kind === "link_destination_mismatch");
  const hasRiskyScheme = signals.some((signal) => signal.kind === "risky_link_scheme");
  if (!hasDestinationMismatch && !hasRiskyScheme) return null;
  const detail = hasDestinationMismatch && hasRiskyScheme
    ? "A visible link address differs from its destination, and another link uses a potentially unsafe destination type."
    : hasDestinationMismatch
      ? "A visible link address differs from where the link goes."
      : "A link uses a potentially unsafe destination type.";
  return (
    <div className="message-link-warning" role="note">
      <Icon name="shield_warning" weight="fill" />
      <span><strong>Use care with links.</strong> {detail}</span>
    </div>
  );
}

function PGPEncryptionPill({
  state,
  hasUnlockedKey,
  onOpen
}: {
  state?: PGPBodyState;
  hasUnlockedKey: boolean;
  onOpen: () => void;
}) {
  const loading = Boolean(state?.loading);
  const decrypted = Boolean(state?.doc && !state?.error);
  const failed = Boolean(state?.error);
  const statusClass = decrypted ? "verified" : failed ? "invalid" : "unverified";
  const label = loading ? "Decrypting" : decrypted ? "Decrypted" : "Encrypted";
  const detail = loading
    ? "Decrypting this message in the browser."
    : decrypted
      ? "Message decrypted in this browser with an unlocked PGP key."
      : failed
        ? state?.error || "rolltop could not decrypt this message with the active key."
        : hasUnlockedKey
          ? "An unlocked PGP key is available. Click to try decrypting this message."
          : "No unlocked PGP key is available. Click to unlock a key in this browser.";
  const detailLines = [detail, ...(state?.securityDetails || [])].filter(Boolean);
  if (decrypted && detailLines.length > 1) {
    return (
      <details className={`pgp-signature-pill pgp-encryption-pill ${statusClass}`} onClick={(event) => event.stopPropagation()}>
        <summary title={detailLines.join("\n")}>
          <Icon name="lock_open" weight="bold" />
          {label}
        </summary>
        <PGPPillDetailPanel lines={detailLines} />
      </details>
    );
  }
  return (
    <button className={`pgp-status-pill encrypted ${statusClass}`} type="button" title={detail} onClick={(event) => { event.stopPropagation(); onOpen(); }}>
      <Icon name={decrypted ? "lock_open" : "lock"} weight={decrypted ? "bold" : "regular"} />
      {label}
    </button>
  );
}

function PGPSignaturePill({ encrypted, state }: { encrypted: boolean; state?: PGPBodyState }) {
  const loading = !encrypted && (!state || state.loading);
  const status = loading ? "checking" : state?.signatureStatus || (encrypted ? "unverified" : "unverified");
  const statusClass = status === "verified" ? "verified" : status === "invalid" ? "invalid" : "unverified";
  const summary = loading
    ? "Checking the PGP signature in this browser."
    : state?.signatureDetail || (encrypted && !state
      ? "Decrypt this message to verify its PGP signature."
      : "No saved public key is available for this sender, so rolltop cannot verify the signature.");
  const detailLines = [summary, ...(state?.securityDetails || [])].filter(Boolean);
  const detail = detailLines.join("\n");
  const missingKey = /^no (?:saved )?public key/i.test(summary);
  const label = loading
    ? "Checking signature"
    : status === "verified"
      ? "Signature verified"
      : status === "invalid"
        ? "Signature mismatch"
        : encrypted && !state
          ? "Signature locked"
          : missingKey ? "No public key" : "Signature unverified";
  return (
    <details className={`pgp-signature-pill ${statusClass}`} onClick={(event) => event.stopPropagation()}>
      <summary title={detail}>
        <Icon name="signature" weight={status === "verified" ? "bold" : "regular"} />
        {label}
      </summary>
      <PGPPillDetailPanel lines={detailLines} />
    </details>
  );
}

function PGPPillDetailPanel({ lines }: { lines: string[] }) {
  const summaryLines: string[] = [];
  const rowLines: Array<{ label: string; value: string }> = [];
  for (const line of lines) {
    const colon = line.indexOf(": ");
    if (colon > 0) {
      rowLines.push({ label: line.slice(0, colon), value: line.slice(colon + 2) });
    } else {
      summaryLines.push(line);
    }
  }
  return (
    <div className="pgp-signature-detail">
      {summaryLines.map((line) => <div className="pgp-pill-detail-summary" key={line}>{line}</div>)}
      {rowLines.length > 0 ? (
        <dl className="pgp-pill-detail-rows">
          {rowLines.map((row) => (
            <Fragment key={`${row.label}:${row.value}`}>
              <dt>{row.label}</dt>
              <dd>{row.value}</dd>
            </Fragment>
          ))}
        </dl>
      ) : null}
    </div>
  );
}

function PGPImportStatusAction({
  state,
  fallbackEmail,
  onImport
}: {
  state?: AttachmentPGPImportState;
  fallbackEmail: string;
  onImport: () => void;
}) {
  if (!state || state.status === "ignored") return null;
  if (state.status === "checking") {
    return <span className="attachment-preview-link">Checking key</span>;
  }
  if (state.status === "candidate" || state.status === "ready") {
    return (
      <button
        className="attachment-preview-link"
        type="button"
        title={`Import PGP public key for ${state.email || fallbackEmail}`}
        onClick={onImport}
      >
        Import key
      </button>
    );
  }
  if (state.status === "same") {
    return <span className="attachment-preview-link" title={`Already saved for ${state.email || fallbackEmail}`}>Already saved</span>;
  }
  if (state.status === "different") {
    return (
      <button
        className="attachment-preview-link"
        type="button"
        title={`A different key is already saved for ${state.email || fallbackEmail}`}
        onClick={onImport}
      >
        Import different key
      </button>
    );
  }
  if (state.status === "importing") {
    return <span className="attachment-preview-link">Importing</span>;
  }
  if (state.status === "imported") {
    return <span className="attachment-preview-link">Key imported</span>;
  }
  if (state.status === "error") {
    return (
      <button
        className="attachment-preview-link"
        type="button"
        title={state.error || "Import failed"}
        onClick={onImport}
      >
        Retry key
      </button>
    );
  }
  return null;
}

function PGPKeyDiscoveryAttachment({
  kind,
  email,
  state,
  onImport
}: {
  kind: string;
  email: string;
  state?: AttachmentPGPImportState;
  onImport: () => void;
}) {
  return (
    <div className="attachment-group pgp-key-attachment-group">
      <span className="attachment pgp-key-attachment">
        <Icon name="key" />
        <span>
          <strong>{email || "Unknown email"}</strong>
          <small> {kind}</small>
        </span>
      </span>
      <PGPImportStatusAction state={state} fallbackEmail={email} onImport={onImport} />
    </div>
  );
}

function pgpOpenStatus(result: ThreadSecurityOpenResult, signatureStatus = result.signatureStatus): string {
  if (result.encrypted && signatureStatus === "verified") return "Decrypted and signature verified.";
  if (result.encrypted && signatureStatus === "invalid") return "Decrypted, but the signature does not match the saved sender key.";
  if (result.encrypted && signatureStatus === "unverified") return "Decrypted, but the signature could not be verified because no saved sender key is available.";
  if (result.encrypted) return "Decrypted.";
  if (signatureStatus === "verified") return "Signature verified.";
  if (signatureStatus === "invalid") return "Signature does not match the saved sender key.";
  if (signatureStatus === "unverified") return "Signature could not be verified because no saved sender key is available.";
  return "PGP content opened.";
}

function pgpKeyDiscoveryID(email: string, key: ContactPGPKey, index = 0): string {
  const stableID = key.fingerprint || key.key_id || String(index);
  return `${email.trim().toLowerCase()}:${stableID.replace(/\s+/g, "").toUpperCase()}`;
}

function pgpSignatureState(item: ThreadMessage, result: ThreadSecurityOpenResult, keys: PGPVerificationKeys): Pick<PGPBodyState, "signatureStatus" | "signatureDetail"> {
  if (!item.message.is_signed && !result.signed) return { signatureStatus: "none", signatureDetail: "" };
  const sender = keys.senderEmail || item.sender_email || "this sender";
  if (result.signatureStatus === "verified") {
    return {
      signatureStatus: "verified",
      signatureDetail: result.signerKeyID
        ? `Signature verified for ${sender} with key ${result.signerKeyID}.`
        : `Signature verified for ${sender}.`
    };
  }
  if (keys.loadError) {
    return {
      signatureStatus: "unverified",
      signatureDetail: `rolltop could not load saved public keys for ${sender}: ${keys.loadError}`
    };
  }
  if (keys.senderKeyCount === 0) {
    return {
      signatureStatus: "unverified",
      signatureDetail: `No public key is saved for ${sender}. Add one to the contact before trusting this signature.`
    };
  }
  return {
    signatureStatus: "invalid",
    signatureDetail: `This signature does not match the public key saved for ${sender}. ${result.signatureDetail || "The message may have been changed or the saved key may be wrong."}`
  };
}

function pgpSecurityDetails(item: ThreadMessage, result: ThreadSecurityOpenResult, signature: Pick<PGPBodyState, "signatureStatus" | "signatureDetail">, keys: PGPVerificationKeys, unlocked: SecurityUnlockState): string[] {
  const details: string[] = [];
  if (result.encrypted) {
    details.push(`OpenPGP mode: ${result.secureMime ? "Encrypted PGP/MIME message" : "Encrypted inline message"}`);
    if (result.symmetricAlgorithm) details.push(`Message cipher: ${formatPGPAlgorithm(result.symmetricAlgorithm)}`);
    if (result.encryptionKeyIDs?.length) details.push(`Recipient key IDs: ${result.encryptionKeyIDs.map(shortPGPDetailValue).join(", ")}`);
    const unlockedKey = unlocked.keys[0];
    if (unlockedKey) {
      const parts = [unlockedKey.label, shortPGPDetailValue(unlockedKey.fingerprint || unlockedKey.key_id || ""), unlockedKey.algorithm].filter(Boolean);
      details.push(`Unlocked key: ${parts.join(" · ")}`);
      if (unlockedKey.encryption_key_id) details.push(`Unlocked encryption key ID: ${shortPGPDetailValue(unlockedKey.encryption_key_id)}`);
    }
  } else if (item.message.is_signed || result.signed) {
    details.push(`OpenPGP mode: ${result.secureMime ? "Detached PGP/MIME signature" : "Clear-signed inline message"}`);
  }
  if (signature.signatureStatus && signature.signatureStatus !== "none") {
    details.push(`Signature status: ${signature.signatureStatus}`);
  }
  if (result.signerKeyID) details.push(`Signer key ID: ${shortPGPDetailValue(result.signerKeyID)}`);
  if (result.signaturePublicKeyAlgorithm || result.signatureHashAlgorithm) {
    const parts = [formatPGPAlgorithm(result.signaturePublicKeyAlgorithm || ""), formatPGPAlgorithm(result.signatureHashAlgorithm || "")].filter(Boolean);
    details.push(`Signature algorithms: ${parts.join(" / ")}`);
  }
  if (item.message.is_signed || result.signed) {
    const sender = keys.senderEmail || item.sender_email || "sender";
    details.push(`Saved sender keys: ${keys.senderKeyCount} for ${sender}`);
  }
  if (result.autocryptGossip?.length) {
    details.push(`Autocrypt-Gossip: ${result.autocryptGossip.length} encrypted key ${result.autocryptGossip.length === 1 ? "header" : "headers"} found`);
  }
  return details;
}

function shortPGPDetailValue(value: string) {
  const clean = value.replace(/\s+/g, "").toUpperCase();
  if (clean.length <= 16) return clean;
  return `${clean.slice(0, 8)}...${clean.slice(-8)}`;
}

function formatPGPAlgorithm(value: string) {
  return value
    .replace(/^aes(\d+)$/i, "AES-$1")
    .replace(/^sha(\d+)$/i, "SHA-$1")
    .replace(/^eddsaLegacy$/i, "EdDSA")
    .replace(/^rsaEncryptSign$/i, "RSA")
    .replace(/^ecdh$/i, "ECDH")
    .replace(/^ecdsa$/i, "ECDSA");
}

function replySubjectForProtectedSubject(value: string) {
  const subject = value.trim();
  if (!subject) return "";
  return /^re\s*:/i.test(subject) ? subject : `Re: ${subject}`;
}

function identityIDForMessageRecipients(item: ThreadMessage, identities: ComposeIdentity[], source = "") {
  const messageEmails = new Set([
    ...emailAddressesFromText(item.message.to_addr),
    ...emailAddressesFromText(item.message.cc_addr),
    ...emailAddressesFromText(item.recipient_line),
    ...emailAddressesFromMessageHeaders(source)
  ]);
  if (messageEmails.size === 0) return 0;
  const identity = identities.find((candidate) => messageEmails.has(candidate.email.trim().toLowerCase()));
  return identity?.pgp_identity_id || 0;
}

function recipientEmailForMessage(item: ThreadMessage, source = "") {
  const fromMessage = [
    ...emailAddressesFromText(item.message.to_addr),
    ...emailAddressesFromText(item.message.cc_addr),
    ...emailAddressesFromText(item.recipient_line),
    ...emailAddressesFromMessageHeaders(source)
  ];
  return fromMessage[0] || "";
}

function emailAddressesFromMessageHeaders(source: string): string[] {
  const splitAt = source.search(/\r?\n\r?\n/);
  if (splitAt < 0) return [];
  const headerBlock = source.slice(0, splitAt);
  const wanted = new Set(["delivered-to", "envelope-to", "x-original-to", "apparently-to", "to", "cc"]);
  const out: string[] = [];
  for (const line of headerBlock.replace(/\r\n/g, "\n").split(/\n(?=[!-9;-~]+:)/)) {
    const colon = line.indexOf(":");
    if (colon <= 0) continue;
    const name = line.slice(0, colon).trim().toLowerCase();
    if (!wanted.has(name)) continue;
    out.push(...emailAddressesFromText(line.slice(colon + 1)));
  }
  return out;
}

function emailAddressesFromText(value: string): string[] {
  const matches = value.match(/[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}/gi) || [];
  return Array.from(new Set(matches.map((email) => email.trim().toLowerCase()).filter(Boolean)));
}

function quotedReplyBodyForThreadMessage(item: ThreadMessage, bodyText: string): string {
  const quoteText = bodyText.trim()
    ? bodyText
    : "[Encrypted message not quoted. Decrypt it in rolltop before replying to include the plaintext quote.]";
  const lines = ["", ""];
  const date = replyQuoteDate(item.message.date);
  if (date || item.message.from_addr.trim()) {
    lines.push(`On ${[date, item.message.from_addr.trim()].filter(Boolean).join(", ")} wrote:`);
  }
  for (const line of quoteText.replace(/\r\n/g, "\n").replace(/\r/g, "\n").split("\n")) {
    lines.push(`> ${line}`);
  }
  return `${lines.join("\n")}\n`;
}

function replyQuoteDate(value: string): string {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return "";
  const parts = new Intl.DateTimeFormat(undefined, {
    month: "short",
    day: "numeric",
    year: "numeric",
    hour: "numeric",
    minute: "2-digit"
  }).formatToParts(date);
  const lookup = new Map(parts.map((part) => [part.type, part.value]));
  const datePart = [lookup.get("month"), lookup.get("day"), lookup.get("year")].filter(Boolean).join(" ");
  const timePart = [lookup.get("hour"), lookup.get("minute")].filter(Boolean).join(":");
  const dayPeriod = lookup.get("dayPeriod") || "";
  return [datePart.replace(/^([A-Za-z]+) (\d+) (.+)$/, "$1 $2, $3"), [timePart, dayPeriod].filter(Boolean).join(" ")].filter(Boolean).join(" at ");
}

function compactPGPPreviewText(value: string): string {
  const compact = value.replace(/\s+/g, " ").trim();
  if (compact.length <= 180) return compact;
  return `${compact.slice(0, 179).trimEnd()}...`;
}

type RangePagerProps = {
  page: number;
  pageSize: number;
  itemCount: number;
  total?: number;
  hasPrev: boolean;
  hasNext: boolean;
  pageURL: (page: number) => string;
  navigate: (url: string) => void;
  ariaLabel: string;
};

function ListHeader({
  title,
  titleClassName = "",
  actions,
  pager
}: {
  title: ReactNode;
  titleClassName?: string;
  actions?: ReactNode;
  pager: RangePagerProps;
}) {
  return (
    <div className="content-head list-head">
      <div className="list-head-main">
        <h1 className={titleClassName}>{title}</h1>
        {actions}
      </div>
      <RangePager {...pager} />
    </div>
  );
}

function RangePager({
  page,
  pageSize,
  itemCount,
  total,
  hasPrev,
  hasNext,
  pageURL,
  navigate,
  ariaLabel
}: RangePagerProps) {
  const start = itemCount > 0 || hasNext ? (page - 1) * pageSize + 1 : 0;
  const end = itemCount > 0 ? (page - 1) * pageSize + itemCount : start > 0 ? page * pageSize : 0;
  const cappedEnd = total && total > 0 ? Math.min(end, total) : end;
  const label = start > 0
    ? `${start.toLocaleString()}-${cappedEnd.toLocaleString()}${total && total > 0 ? ` of ${total.toLocaleString()}` : hasNext ? " of many" : ""}`
    : total && total > 0 ? `0 of ${total.toLocaleString()}` : "0";

  return (
    <div className="range-pager" aria-label={ariaLabel}>
      <span>{label}</span>
      <button className="range-pager-button" type="button" disabled={!hasPrev} onClick={() => navigate(pageURL(page - 1))} title="Previous page">
        <Icon name="chevron_left" />
      </button>
      <button className="range-pager-button" type="button" disabled={!hasNext} onClick={() => navigate(pageURL(page + 1))} title="Next page">
        <Icon name="chevron_right" />
      </button>
    </div>
  );
}

/**
 * ThreadView loads a full conversation, requests status before slow IMAP/blob
 * hydration, renders plugin actions, maintains expanded/collapsed cards, and
 * prepares inline replies with the correct identity and recipient.
 */
export function ThreadView({
  userID,
  csrf,
  datePrefs,
  location,
  navigate,
  mailboxes,
  enabledPlugins,
  refreshChrome,
  openCompose,
  messageSecurityPlugins = [],
  securityUnlock,
  openSecurityUnlock,
  addToast
}: {
  userID: number;
  csrf: string;
  datePrefs: DatePrefs;
  location: LocationState;
  navigate: (url: string) => void;
  mailboxes: Mailbox[];
  enabledPlugins: string[];
  refreshChrome: () => Promise<Bootstrap | null>;
  openCompose: (query?: string) => void;
  messageSecurityPlugins?: RuntimePlugin[];
  securityUnlock: SecurityUnlockState;
  openSecurityUnlock: (identityID?: number, onUnlocked?: (state: SecurityUnlockState) => void, recipientKeyIDs?: string[], fallbackEmail?: string) => void;
  addToast: (message: string, kind?: Toast["kind"]) => number;
}) {
  const id = location.path.split("/").pop() || "";
  const currentMessageID = Number(id) || 0;
  const highlightQuery = messageHighlightQuery(location);
  const highlightTerms = messageHighlightTerms(location);
  const searchHitID = messageSearchHitID(location);
  const [thread, setThread] = useState<ThreadMessage[]>([]);
  const [subject, setSubject] = useState("");
  const [mailboxID, setMailboxID] = useState<number | null>(null);
  const [snoozedUntil, setSnoozedUntil] = useState("");
  const [composeFrom, setComposeFrom] = useState("");
  const [fromIdentities, setFromIdentities] = useState<ComposeIdentity[]>([]);
  const [showImages, setShowImages] = useState(() => new URLSearchParams(location.search).get("images") === "1");
  const [expanded, setExpanded] = useState<Set<number>>(() => new Set());
  const [inlineReply, setInlineReply] = useState<ComposeForm | null>(null);
  const [unsubscribingID, setUnsubscribingID] = useState<number | null>(null);
  const [pendingUnsubscribe, setPendingUnsubscribe] = useState<ThreadMessage | null>(null);
  const [originalSource, setOriginalSource] = useState<OriginalSourceState | null>(null);
  const [messagePluginPanel, setMessagePluginPanel] = useState<MessagePluginPanelState | null>(null);
  const [pgpBodies, setPGPBodies] = useState<Record<number, PGPBodyState>>({});
  const [pgpAttachmentImports, setPGPAttachmentImports] = useState<Record<number, AttachmentPGPImportState>>({});
  const [autocryptImports, setAutocryptImports] = useState<Record<number, AttachmentPGPImportState>>({});
  const [autocryptGossipImports, setAutocryptGossipImports] = useState<Record<number, Record<string, AttachmentPGPImportState>>>({});
  const [searchExplanations, setSearchExplanations] = useState<Record<number, SearchExplanationState>>({});
  const [loadStatus, setLoadStatus] = useState<MessageLoadStatus | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const pluginKey = enabledPlugins.join("|");
  const pluginSet = useMemo(() => createPluginSet(enabledPlugins), [pluginKey]);
  const securityPlugin = useMemo(() => threadSecurityPlugin(messageSecurityPlugins), [messageSecurityPlugins]);
  const securityEnabled = Boolean(securityPlugin);
  const mailbox = mailboxID ? mailboxes.find((item) => item.id === mailboxID) : null;
  const trashByAccount = useMemo(() => trashMailboxesByAccount(mailboxes), [mailboxes]);
  const backURL = messageBackURL(location);
  const composeInitial = (composeFrom.match(/[A-Za-z0-9]/)?.[0] || "M").toUpperCase();
  const canExplainSearch = highlightQuery.trim() !== "";
  const brandDomainKey = useMemo(() => brandDomainKeyForThread(thread, pluginSet), [thread, pluginSet]);
  const [brandIcons, setBrandIcons] = useState<Record<string, string>>({});
  const autoPGPVerificationRef = useRef<Set<string>>(new Set());
  const pgpWasUnlockedRef = useRef(false);
  const pgpBodiesRef = useRef<Record<number, PGPBodyState>>({});

  useEffect(() => {
    pgpBodiesRef.current = pgpBodies;
  }, [pgpBodies]);

  useEffect(() => {
    if (securityEnabled) return;
    Object.values(pgpBodiesRef.current).forEach((body) => revokeDecryptedMIMEAttachments(body.decryptedAttachments));
    setPGPBodies({});
    setPGPAttachmentImports({});
    setAutocryptImports({});
    setAutocryptGossipImports({});
    autoPGPVerificationRef.current = new Set();
  }, [securityEnabled]);

  useEffect(() => () => {
    Object.values(pgpBodiesRef.current).forEach((body) => revokeDecryptedMIMEAttachments(body.decryptedAttachments));
  }, []);

  // Loading is split into a quick status probe and the actual message request.
  // The status dialog is delayed slightly to avoid flashing for local conversations.
  const load = useCallback(
    async (images: boolean) => {
      setLoading(true);
      setError("");
      setLoadStatus(null);
      setOriginalSource(null);
      setMessagePluginPanel(null);
      Object.values(pgpBodiesRef.current).forEach((body) => revokeDecryptedMIMEAttachments(body.decryptedAttachments));
      setPGPBodies({});
      setPGPAttachmentImports({});
      setAutocryptImports({});
      setAutocryptGossipImports({});
      autoPGPVerificationRef.current = new Set();
      setSearchExplanations({});
      let statusTimer = 0;
      try {
        const status = await api.messageLoadStatus(id).catch(() => null);
        if (shouldShowLoadStatus(status)) {
          statusTimer = window.setTimeout(() => setLoadStatus(status), 250);
        }
        const data = await api.message(id, images, highlightQuery);
        setThread(data.thread);
        setSubject(data.message.subject || "(no subject)");
        setMailboxID(data.mailbox_id);
    setSnoozedUntil(data.snoozed_until || "");
        setComposeFrom(data.compose_from);
        setFromIdentities(data.from_identities || []);
        setExpanded(new Set(data.thread.filter((item) => item.expanded).map((item) => item.message.id)));
        void refreshChrome();
      } catch (err) {
        setError(messageFromError(err));
      } finally {
        if (statusTimer) window.clearTimeout(statusTimer);
        setLoadStatus(null);
        setLoading(false);
      }
    },
    [highlightQuery, id, refreshChrome]
  );

  useEffect(() => {
    void load(showImages);
  }, [load, showImages]);

  useEffect(() => {
    if (!brandDomainKey) {
      setBrandIcons({});
      return;
    }
    let cancelled = false;
    loadBrandIconsForDomains(brandDomainKey)
      .then((data) => {
        if (!cancelled) setBrandIcons(data);
      })
      .catch(() => {
        if (!cancelled) setBrandIcons({});
      });
    return () => {
      cancelled = true;
    };
  }, [brandDomainKey]);

  useEffect(() => {
    if (!securityEnabled || loading) return;
    for (const item of thread) {
      for (const attachment of item.attachments) {
        if (!attachment.pgp_public_key_candidate || pgpAttachmentImports[attachment.id]) continue;
        void checkPGPPublicKeyAttachment(item, attachment);
      }
    }
  }, [loading, pgpAttachmentImports, securityEnabled, thread]);

  useEffect(() => {
    if (!securityEnabled || loading) return;
    for (const item of thread) {
      if (autocryptImports[item.message.id]) continue;
      void checkAutocryptPublicKey(item);
    }
  }, [autocryptImports, loading, securityEnabled, thread]);

  useEffect(() => {
    if (!securityEnabled || loading) return;
    const unlockedKey = securityUnlock.keys.map((key) => key.fingerprint || key.id).join(",") || "locked";
    for (const item of thread) {
      if (item.message.is_encrypted) {
        if (securityUnlock.keys.length === 0 || securityUnlock.unlockedUntil <= Date.now()) continue;
        const decryptKey = `${id}:${item.message.id}:decrypt:${securityUnlock.unlockedUntil}:${unlockedKey}`;
        if (autoPGPVerificationRef.current.has(decryptKey)) continue;
        autoPGPVerificationRef.current.add(decryptKey);
        void openPGPMessage(item, { automatic: true });
        continue;
      }
      if (!item.message.is_signed) continue;
      const verifyKey = `${id}:${item.message.id}:verify`;
      if (autoPGPVerificationRef.current.has(verifyKey)) continue;
      autoPGPVerificationRef.current.add(verifyKey);
      void openPGPMessage(item, { automatic: true });
    }
  }, [id, loading, securityEnabled, securityUnlock.keys, securityUnlock.unlockedUntil, thread]);

  useEffect(() => {
    const unlocked = securityUnlock.keys.length > 0 && securityUnlock.unlockedUntil > Date.now();
    if (!unlocked && pgpWasUnlockedRef.current) {
      const encryptedIDs = new Set(thread.filter((item) => item.message.is_encrypted).map((item) => item.message.id));
      if (encryptedIDs.size > 0) {
        setPGPBodies((current) => {
          const next = { ...current };
          encryptedIDs.forEach((messageID) => {
            revokeDecryptedMIMEAttachments(next[messageID]?.decryptedAttachments);
            delete next[messageID];
          });
          return next;
        });
      }
    }
    pgpWasUnlockedRef.current = unlocked;
  }, [securityUnlock.keys.length, securityUnlock.unlockedUntil, thread]);

  async function trustImages(messageID: number) {
    try {
      await api.trustImages(csrf, messageID);
      addToast("Remote images will be shown for this sender.");
      setShowImages(true);
      await load(true);
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }

  async function unsubscribe(item: ThreadMessage) {
    setUnsubscribingID(item.message.id);
    try {
      const data = await api.unsubscribe(csrf, item.message.id);
      const sentAt = data.sent_at || new Date().toISOString();
      setThread((current) => current.map((threadItem) =>
        threadItem.message.id === item.message.id
          ? { ...threadItem, one_click_unsubscribe_sent_at: sentAt }
          : threadItem
      ));
      addToast(data.already_sent ? `Unsubscribed on ${displayDateTime(sentAt, datePrefs)}.` : "One-click unsubscribe request sent.");
    } catch (err) {
      addToast(messageFromError(err), "error");
    } finally {
      setUnsubscribingID(null);
    }
  }

  async function addSender(item: ThreadMessage) {
    try {
      const data = await api.addSenderContact(csrf, item.message.id);
      addToast(data.created ? "Sender added to contacts." : "Sender is already in contacts.");
      await load(showImages);
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }

  async function checkPGPPublicKeyAttachment(item: ThreadMessage, attachment: Attachment) {
    const email = item.sender_email.trim();
    if (!email || attachment.size > PGP_ATTACHMENT_IMPORT_BYTES) {
      setPGPAttachmentImports((current) => ({ ...current, [attachment.id]: { status: "ignored" } }));
      return;
    }
    if (attachment.size > PGP_ATTACHMENT_AUTO_PARSE_BYTES) {
      setPGPAttachmentImports((current) => ({ ...current, [attachment.id]: { status: "candidate", email } }));
      return;
    }
    setPGPAttachmentImports((current) => ({ ...current, [attachment.id]: { status: "checking", email } }));
    try {
      const text = await fetchPGPKeyAttachmentText(attachment);
      if (text.length > PGP_ATTACHMENT_AUTO_PARSE_BYTES || !/-----BEGIN PGP PUBLIC KEY BLOCK-----/i.test(text)) {
        setPGPAttachmentImports((current) => ({ ...current, [attachment.id]: { status: "ignored" } }));
        return;
      }
      if (!securityPlugin) throw new Error("PGP plugin is still loading. Try again in a moment.");
      const key = await securityPlugin.publicKeyRecordFromArmored(text, email, "message-attachment", attachment.filename || `message-${item.message.id}`);
      const resolution = await savedPGPPublicKeyResolution(email, key);
      if (resolution.status === "same") {
        setPGPAttachmentImports((current) => ({ ...current, [attachment.id]: { status: "same", email, key, existing: resolution.existing } }));
        return;
      }
      if (resolution.status === "different") {
        setPGPAttachmentImports((current) => ({ ...current, [attachment.id]: { status: "different", email, key, existing: resolution.existing } }));
        return;
      }
      setPGPAttachmentImports((current) => ({ ...current, [attachment.id]: { status: "ready", email, key } }));
    } catch (err) {
      setPGPAttachmentImports((current) => ({ ...current, [attachment.id]: { status: "error", email, error: messageFromError(err) } }));
    }
  }

  async function importAttachmentPGPPublicKey(attachment: Attachment) {
    let state = pgpAttachmentImports[attachment.id];
    if (!state || state.status === "ignored" || state.status === "imported") return;
    const fallbackEmail = state.email || "";
    if (state.status === "same") {
      addToast(`PGP public key already saved for ${fallbackEmail || "this sender"}.`);
      return;
    }
    if (state.status === "different") {
      const accept = window.confirm(`A different PGP public key is already saved for ${fallbackEmail || "this sender"}. Import this one too?`);
      if (!accept) return;
    }
    setPGPAttachmentImports((current) => ({ ...current, [attachment.id]: { ...state, status: "importing" } }));
    try {
      let key = state.key;
      const email = state.email || "";
      if (!key) {
        const text = await fetchPGPKeyAttachmentText(attachment);
        if (!securityPlugin) throw new Error("PGP plugin is still loading. Try again in a moment.");
        key = await securityPlugin.publicKeyRecordFromArmored(text, email, "message-attachment", attachment.filename || `message-${attachment.id}`);
        state = { ...state, email, key };
      }
      if (!securityPlugin) throw new Error("PGP plugin is still loading. Try again in a moment.");
      const targetEmail = (email || state.email || key.email).toLowerCase();
      const existing = await securityPlugin.publicKeys([targetEmail], true).catch(() => ({ keys: [] as ContactPGPKey[] }));
      const hasPreferred = (existing.keys || []).some((candidate) => candidate.email.toLowerCase() === targetEmail && candidate.is_preferred);
      await securityPlugin.savePublicKey(csrf, { ...key, email: state.email || key.email, is_preferred: !hasPreferred });
      setPGPAttachmentImports((current) => ({ ...current, [attachment.id]: { ...state, status: "imported" } }));
      addToast(`PGP public key imported for ${state.email || key.email}.`);
    } catch (err) {
      setPGPAttachmentImports((current) => ({ ...current, [attachment.id]: { ...state, status: "error", error: messageFromError(err) } }));
      addToast(messageFromError(err), "error");
    }
  }

  async function checkAutocryptPublicKey(item: ThreadMessage) {
    const email = item.sender_email.trim();
    const messageID = item.message.id;
    if (!email) {
      setAutocryptImports((current) => ({ ...current, [messageID]: { status: "ignored" } }));
      return;
    }
    setAutocryptImports((current) => ({ ...current, [messageID]: { status: "checking", email } }));
    try {
      const original = await api.messageOriginal(messageID);
      if (!securityPlugin) throw new Error("PGP plugin is still loading. Try again in a moment.");
      const key = await securityPlugin.autocryptKeyRecordFromMessageSource(original.source, email);
      if (!key) {
        setAutocryptImports((current) => ({ ...current, [messageID]: { status: "ignored" } }));
        return;
      }
      const resolution = await savedPGPPublicKeyResolution(key.email || email, key);
      if (resolution.status === "same") {
        setAutocryptImports((current) => ({ ...current, [messageID]: { status: "same", email: key.email || email, key, existing: resolution.existing } }));
        return;
      }
      if (resolution.status === "different") {
        setAutocryptImports((current) => ({ ...current, [messageID]: { status: "different", email: key.email || email, key, existing: resolution.existing } }));
        return;
      }
      setAutocryptImports((current) => ({ ...current, [messageID]: { status: "ready", email: key.email || email, key } }));
    } catch {
      setAutocryptImports((current) => ({ ...current, [messageID]: { status: "ignored" } }));
    }
  }

  async function importAutocryptPGPPublicKey(item: ThreadMessage) {
    const messageID = item.message.id;
    let state = autocryptImports[messageID];
    if (!state || state.status === "ignored" || state.status === "imported" || state.status === "checking") return;
    if (state.status === "same") {
      addToast(`PGP public key already saved for ${state.email || item.sender_email.trim()}.`);
      return;
    }
    if (state.status === "different") {
      const accept = window.confirm(`A different PGP public key is already saved for ${state.email || item.sender_email.trim()}. Import this one too?`);
      if (!accept) return;
    }
    setAutocryptImports((current) => ({ ...current, [messageID]: { ...state, status: "importing" } }));
    try {
      let key = state.key;
      const email = state.email || item.sender_email.trim();
      if (!key) {
        const original = await api.messageOriginal(messageID);
        if (!securityPlugin) throw new Error("PGP plugin is still loading. Try again in a moment.");
        key = await securityPlugin.autocryptKeyRecordFromMessageSource(original.source, email) || undefined;
        if (!key) throw new Error("This message no longer contains a usable Autocrypt public key.");
        state = { ...state, email: key.email || email, key };
      }
      if (!securityPlugin) throw new Error("PGP plugin is still loading. Try again in a moment.");
      const existing = await securityPlugin.publicKeys([email], true).catch(() => ({ keys: [] as ContactPGPKey[] }));
      const hasPreferred = (existing.keys || []).some((candidate) => candidate.email.toLowerCase() === email.toLowerCase() && candidate.is_preferred);
      await securityPlugin.savePublicKey(csrf, { ...key, email: state.email || key.email, is_preferred: !hasPreferred });
      setAutocryptImports((current) => ({ ...current, [messageID]: { ...state, status: "imported" } }));
      addToast(`PGP public key imported for ${state.email || key.email}.`);
    } catch (err) {
      setAutocryptImports((current) => ({ ...current, [messageID]: { ...state, status: "error", error: messageFromError(err) } }));
      addToast(messageFromError(err), "error");
    }
  }

  async function importAutocryptGossipPGPPublicKey(messageID: number, discoveryID: string) {
    const state = autocryptGossipImports[messageID]?.[discoveryID];
    if (!state || state.status === "ignored" || state.status === "imported" || state.status === "checking" || !state.key) return;
    setAutocryptGossipImports((current) => ({
      ...current,
      [messageID]: {
        ...(current[messageID] || {}),
        [discoveryID]: { ...state, status: "importing" }
      }
    }));
    try {
      if (!securityPlugin) throw new Error("PGP plugin is still loading. Try again in a moment.");
      await securityPlugin.savePublicKey(csrf, { ...state.key, email: state.email || state.key.email, is_preferred: true });
      setAutocryptGossipImports((current) => ({
        ...current,
        [messageID]: {
          ...(current[messageID] || {}),
          [discoveryID]: { ...state, status: "imported" }
        }
      }));
      addToast(`PGP public key imported for ${state.email || state.key.email}.`);
    } catch (err) {
      setAutocryptGossipImports((current) => ({
        ...current,
        [messageID]: {
          ...(current[messageID] || {}),
          [discoveryID]: { ...state, status: "error", error: messageFromError(err) }
        }
      }));
      addToast(messageFromError(err), "error");
    }
  }

  async function fetchPGPKeyAttachmentText(attachment: Attachment): Promise<string> {
    if (attachment.size > PGP_ATTACHMENT_IMPORT_BYTES) {
      throw new Error("This PGP key attachment is too large to import from the message view.");
    }
    const response = await fetch(attachment.download_url, {
      headers: { Accept: "application/pgp-keys,text/plain,*/*;q=0.8" },
      credentials: "same-origin"
    });
    if (!response.ok) throw new Error(`Attachment download failed (${response.status}).`);
    const text = await response.text();
    if (text.length > PGP_ATTACHMENT_IMPORT_BYTES) {
      throw new Error("This PGP key attachment is too large to import from the message view.");
    }
    if (!/-----BEGIN PGP PUBLIC KEY BLOCK-----/i.test(text)) {
      throw new Error("This attachment does not contain an ASCII-armored PGP public key.");
    }
    return text;
  }

  async function savedPGPPublicKeyResolution(email: string, key: ContactPGPKey) {
    if (!securityPlugin) return { status: "new" as const };
    try {
      const existing = await securityPlugin.publicKeys([email], true);
      return securityPlugin.resolveContactPGPKeyImport(existing.keys || [], key);
    } catch {
      return { status: "new" as const };
    }
  }

  async function viewOriginal(event: MouseEvent<HTMLButtonElement>, item: ThreadMessage) {
    event.stopPropagation();
    event.currentTarget.closest("details")?.removeAttribute("open");
    const messageID = item.message.id;
    setOriginalSource({ messageID, loading: true, error: "", data: null });
    try {
      const data = await api.messageOriginal(messageID);
      setOriginalSource({ messageID, loading: false, error: "", data });
    } catch (err) {
      setOriginalSource({ messageID, loading: false, error: messageFromError(err), data: null });
    }
  }

  async function pgpVerificationKeysForSender(item: ThreadMessage): Promise<PGPVerificationKeys> {
    const senderEmail = item.sender_email.trim();
    if (!senderEmail) return { armors: [], senderKeyCount: 0, senderEmail: "" };
    if (!securityPlugin) return { armors: [], senderKeyCount: 0, senderEmail };
    try {
      const data = await securityPlugin.publicKeys([senderEmail], true);
      const armors = (data.keys || []).map((key) => key.public_key_armored).filter(Boolean);
      return {
        armors: Array.from(new Set(armors.map((armor) => armor.trim()).filter(Boolean))),
        senderKeyCount: data.keys?.length || 0,
        senderEmail
      };
    } catch (err) {
      return { armors: [], senderKeyCount: 0, senderEmail, loadError: messageFromError(err) };
    }
  }

  async function stageAutocryptGossip(messageID: number, gossip: ThreadSecurityGossipKey[] | undefined) {
    if (!securityEnabled || !gossip || gossip.length === 0) return;
    const staged: Record<string, AttachmentPGPImportState> = {};
    for (const [index, item] of gossip.entries()) {
      try {
        if (!securityPlugin) throw new Error("PGP plugin is still loading. Try again in a moment.");
        const record = await securityPlugin.publicKeyRecordFromArmored(item.publicKeyArmored, item.email, "autocrypt-gossip", item.email);
        const email = record.email || item.email;
        const resolution = await savedPGPPublicKeyResolution(email, record);
        if (resolution.status === "same") {
          staged[pgpKeyDiscoveryID(email, record, index)] = { status: "same", email, key: { ...record, email, is_preferred: true }, existing: resolution.existing };
          continue;
        }
        if (resolution.status === "different") {
          staged[pgpKeyDiscoveryID(email, record, index)] = { status: "different", email, key: { ...record, email, is_preferred: true }, existing: resolution.existing };
          continue;
        }
        staged[pgpKeyDiscoveryID(email, record, index)] = { status: "ready", email, key: { ...record, email, is_preferred: true } };
      } catch {
        // Gossip is opportunistic key discovery. A malformed or duplicate key
        // should not interrupt reading the encrypted message.
      }
    }
    if (Object.keys(staged).length === 0) return;
    setAutocryptGossipImports((current) => {
      const existing = current[messageID] || {};
      return {
        ...current,
        [messageID]: { ...staged, ...existing }
      };
    });
  }

  async function openPGPMessage(item: ThreadMessage, options: { automatic?: boolean } = {}) {
    const messageID = item.message.id;
    if (item.message.is_encrypted && (securityUnlock.keys.length === 0 || securityUnlock.unlockedUntil <= Date.now())) {
      if (!options.automatic) {
        await openSecurityUnlockForMessage(item);
        addToast("Unlock a PGP key to decrypt this message.", "error");
      }
      return;
    }
    setPGPBodies((current) => ({
      ...current,
      [messageID]: {
        loading: true,
        error: "",
        doc: current[messageID]?.doc || "",
        quoteText: current[messageID]?.quoteText || "",
        protectedSubject: current[messageID]?.protectedSubject || "",
        status: "",
        signatureStatus: current[messageID]?.signatureStatus || (item.message.is_signed ? "unverified" : "none"),
        signatureDetail: current[messageID]?.signatureDetail || "",
        decryptedAttachments: current[messageID]?.decryptedAttachments || []
      }
    }));
    let openedAttachments: ThreadSecurityDecryptedAttachment[] = [];
    try {
      const original = await api.messageOriginal(messageID);
      const verificationKeys = await pgpVerificationKeysForSender(item);
      if (!securityPlugin) throw new Error("PGP plugin is still loading. Try again in a moment.");
      const opened = await securityPlugin.openSecureMessageSource(original.source, item.message.is_encrypted ? securityUnlock.keys : [], verificationKeys.armors);
      const signature = pgpSignatureState(item, opened, verificationKeys);
      const securityDetails = pgpSecurityDetails(item, opened, signature, verificationKeys, securityUnlock);
      await stageAutocryptGossip(messageID, opened.autocryptGossip);
      openedAttachments = securityPlugin.decryptedMIMEAttachments(opened.text);
      const doc = await securityPlugin.decryptedHTMLDoc(opened.text, openedAttachments);
      const quoteText = securityPlugin.decryptedPlainText(opened.text);
      setPGPBodies((current) => {
        revokeDecryptedMIMEAttachments(current[messageID]?.decryptedAttachments);
        return {
          ...current,
          [messageID]: {
            loading: false,
            error: "",
            doc,
            quoteText,
            protectedSubject: opened.protectedSubject || "",
            status: pgpOpenStatus(opened, signature.signatureStatus),
            signatureStatus: signature.signatureStatus,
            signatureDetail: signature.signatureDetail,
            securityDetails,
            decryptedAttachments: openedAttachments
          }
        };
      });
    } catch (err) {
      revokeDecryptedMIMEAttachments(openedAttachments);
      const detail = messageFromError(err);
      setPGPBodies((current) => ({
        ...current,
        [messageID]: {
          loading: false,
          error: item.message.is_encrypted || !options.automatic ? detail : "",
          doc: current[messageID]?.doc || "",
          quoteText: current[messageID]?.quoteText || "",
          protectedSubject: current[messageID]?.protectedSubject || "",
          status: "",
          signatureStatus: item.message.is_signed ? "unverified" : current[messageID]?.signatureStatus || "none",
          signatureDetail: item.message.is_signed ? `rolltop could not verify this signature: ${detail}` : current[messageID]?.signatureDetail || "",
          decryptedAttachments: current[messageID]?.decryptedAttachments || []
        }
      }));
    }
  }

  async function openSecurityUnlockForMessage(item: ThreadMessage) {
    try {
      const original = await api.messageOriginal(item.message.id);
      if (!securityPlugin) throw new Error("PGP plugin is still loading. Try again in a moment.");
      const recipientKeyIDs = await securityPlugin.encryptionRecipientKeyIDsFromSource(original.source);
      const identityID = identityIDForMessageRecipients(item, fromIdentities, original.source);
      openSecurityUnlock(identityID || undefined, undefined, recipientKeyIDs, recipientEmailForMessage(item, original.source));
    } catch {
      const identityID = identityIDForMessageRecipients(item, fromIdentities);
      openSecurityUnlock(identityID || undefined, undefined, undefined, recipientEmailForMessage(item));
    }
  }

  async function moveToTrash(event: MouseEvent<HTMLButtonElement>, item: ThreadMessage) {
    event.stopPropagation();
    event.currentTarget.closest("details")?.removeAttribute("open");
    const trashMailbox = trashByAccount.get(item.message.account_id);
    if (!trashMailbox || item.message.mailbox_id === trashMailbox.id) return;
    try {
      await api.moveMessage(csrf, item.message.id, trashMailbox.id);
      addToast(`Moved message to ${trashMailbox.name}.`);
      await refreshChrome();
      navigate(backURL);
    } catch (err) {
      addToast(`Move to trash failed: ${messageFromError(err)}`, "error");
    }
  }

  function requestUnsubscribe(item: ThreadMessage) {
    if (item.one_click_unsubscribe_sent_at) return;
    setPendingUnsubscribe(item);
  }

  async function toggleSearchExplanation(event: MouseEvent<HTMLButtonElement>, item: ThreadMessage) {
    event.stopPropagation();
    event.currentTarget.closest("details")?.removeAttribute("open");
    const messageID = item.message.id;
    const query = highlightQuery.trim();
    if (!query) return;

    const current = searchExplanations[messageID];
    if (current?.open) {
      setSearchExplanations((items) => ({
        ...items,
        [messageID]: { ...current, open: false }
      }));
      return;
    }
    if (current?.data) {
      setSearchExplanations((items) => ({
        ...items,
        [messageID]: { ...current, open: true, error: "", loading: false }
      }));
      return;
    }

    setSearchExplanations((items) => ({
      ...items,
      [messageID]: { open: true, loading: true, error: "", data: null }
    }));
    try {
      const data = await api.searchExplanation(messageID, query, searchHitID);
      setSearchExplanations((items) => ({
        ...items,
        [messageID]: { open: true, loading: false, error: "", data }
      }));
    } catch (err) {
      setSearchExplanations((items) => ({
        ...items,
        [messageID]: { open: true, loading: false, error: messageFromError(err), data: null }
      }));
    }
  }

  function closeSearchExplanation(messageID: number) {
    setSearchExplanations((items) => {
      const current = items[messageID];
      if (!current) return items;
      return {
        ...items,
        [messageID]: { ...current, open: false }
      };
    });
  }

  function openMessagePluginPanel(messageID: number, panelID: string) {
    setMessagePluginPanel({ messageID, panelID });
  }

  function closeMessagePluginPanel(messageID: number, panelID: string) {
    setMessagePluginPanel((current) => current?.messageID === messageID && current.panelID === panelID ? null : current);
  }

  async function confirmUnsubscribe() {
    if (!pendingUnsubscribe) return;
    const item = pendingUnsubscribe;
    setPendingUnsubscribe(null);
    await unsubscribe(item);
  }

  function unsubscribeSentLabel(item: ThreadMessage) {
    if (!item.one_click_unsubscribe_sent_at) return "";
    return `Unsubscribed on ${displayDateTime(item.one_click_unsubscribe_sent_at, datePrefs)}`;
  }

  // Reply setup asks the backend for recipients, identity, threading headers, and
  // reusable attachment metadata so reply/reply-all behavior stays consistent.
  async function beginReply(item: ThreadMessage, replyAll = false) {
    setExpanded((current) => new Set(current).add(item.message.id));
    try {
      const data = await api.compose(`${replyAll ? "reply_all" : "reply"}=${item.message.id}`);
      const protectedSubject = pgpBodies[item.message.id]?.protectedSubject || "";
      const compose = securityEnabled && item.message.is_encrypted
        ? {
            ...data.compose,
            subject: protectedSubject ? replySubjectForProtectedSubject(protectedSubject) : data.compose.subject,
            body: quotedReplyBodyForThreadMessage(item, pgpBodies[item.message.id]?.quoteText || ""),
            body_html: ""
          }
        : data.compose;
      setComposeFrom(data.compose_from);
      setFromIdentities(data.from_identities || []);
      setInlineReply(compose);
    } catch (err) {
      addToast(messageFromError(err), "error");
    }
  }

  function toggleMessage(messageID: number) {
    setExpanded((current) => {
      const next = new Set(current);
      if (next.has(messageID)) next.delete(messageID);
      else next.add(messageID);
      return next;
    });
  }

  useEffect(() => {
    function handleThreadShortcut(event: globalThis.KeyboardEvent) {
      if (event.shiftKey || event.repeat || shouldIgnoreMailShortcut(event)) return;
      if (inlineReply || originalSource || messagePluginPanel || pendingUnsubscribe) return;
      const key = event.key.toLowerCase();
      if (key === "u" || key === "escape") {
        event.preventDefault();
        navigate(backURL);
        return;
      }
      if (loading || (key !== "r" && key !== "a" && key !== "f")) return;
      const item = thread.find((threadItem) => threadItem.message.id === currentMessageID) || thread[thread.length - 1];
      if (!item || (key === "a" && !item.can_reply_all)) return;
      event.preventDefault();
      if (key === "r") void beginReply(item);
      else if (key === "a") void beginReply(item, true);
      else openCompose(`forward=${item.message.id}`);
    }
    window.addEventListener("keydown", handleThreadShortcut);
    return () => window.removeEventListener("keydown", handleThreadShortcut);
  }, [backURL, currentMessageID, inlineReply, loading, messagePluginPanel, originalSource, pendingUnsubscribe, thread]);

  const displaySubject = pgpBodies[currentMessageID]?.protectedSubject || subject;

  async function toggleThreadStar(event: MouseEvent<HTMLButtonElement>, item: ThreadMessage) {
    event.stopPropagation();
    const next = !item.message.is_starred;
    setThread((current) => current.map((threadItem) => threadItem.message.id === item.message.id
      ? { ...threadItem, message: { ...threadItem.message, is_starred: next } }
      : threadItem));
    try {
      await api.setStarred(csrf, item.message.id, next);
    } catch (err) {
      setThread((current) => current.map((threadItem) => threadItem.message.id === item.message.id
        ? { ...threadItem, message: { ...threadItem.message, is_starred: item.message.is_starred } }
        : threadItem));
      addToast(`Star update failed: ${messageFromError(err)}`, "error");
    }
  }

  async function snoozeThread(until: Date) {
    try {
      await api.snoozeMessage(csrf, currentMessageID, until);
    } catch (err) {
      addToast(`Snooze failed: ${messageFromError(err)}`, "error");
      throw err;
    }
    addToast(`Conversation snoozed until ${displaySnoozeUntil(until, datePrefs)}.`);
    navigate(backURL);
    void refreshChrome().catch(() => undefined);
  }

  async function unsnoozeThread() {
    try {
      await api.unsnoozeMessage(csrf, currentMessageID);
    } catch (err) {
      addToast(`Unsnooze failed: ${messageFromError(err)}`, "error");
      return;
    }
    setSnoozedUntil("");
    addToast("Conversation returned to mail.");
    void refreshChrome().catch(() => undefined);
  }

  return (
    <>
      <div className="content-head">
        <div className="thread-head-main">
          {!androidNativeAvailable() ? (
            <button className="ghost" type="button" onClick={() => navigate(backURL)} title="Back to results">
              <Icon name="arrow_back" />
            </button>
          ) : null}
          <h1 className="thread-title">
            <HighlightedText text={displaySubject} query={highlightQuery} terms={highlightTerms} />
          </h1>
          {mailbox ? <span className="label-pill">{mailbox.name}</span> : null}
        </div>
      </div>
      {error ? <div className="error">{error}</div> : null}
      {loading ? <div className="panel muted">Loading conversation...</div> : null}
      {loadStatus ? (
        <div className="fetch-status-backdrop" role="presentation">
          <section className="fetch-status-dialog" role="status" aria-live="polite" aria-label={loadStatusTitle(loadStatus)}>
            <h2>{loadStatusTitle(loadStatus)}</h2>
            <p>{loadStatusDetail(loadStatus)}</p>
            <div className="fetch-status-progress" aria-hidden="true">
              <span />
            </div>
          </section>
        </div>
      ) : null}
      {originalSource ? (
        <div className="original-source-backdrop" role="presentation" onClick={() => setOriginalSource(null)}>
          <section
            className="original-source-dialog"
            role="dialog"
            aria-modal="true"
            aria-labelledby="original-source-title"
            onClick={(event) => event.stopPropagation()}
          >
            <header>
              <div>
                <h2 id="original-source-title">Original source</h2>
                {originalSource.data?.filename ? <span>{originalSource.data.filename}</span> : null}
              </div>
              <button className="ghost" type="button" title="Close original source" onClick={() => setOriginalSource(null)}>
                <Icon name="close" />
              </button>
            </header>
            {originalSource.loading ? <div className="original-source-status">Loading raw message source...</div> : null}
            {originalSource.error ? <div className="original-source-status error-text">{originalSource.error}</div> : null}
            {originalSource.data ? <pre tabIndex={0}>{originalSource.data.source}</pre> : null}
          </section>
        </div>
      ) : null}
      {pendingUnsubscribe ? (
        <div className="confirm-backdrop" role="presentation" onClick={() => setPendingUnsubscribe(null)}>
          <section
            className="confirm-dialog"
            role="dialog"
            aria-modal="true"
            aria-labelledby="unsubscribe-confirm-title"
            onClick={(event) => event.stopPropagation()}
          >
            <h2 id="unsubscribe-confirm-title">Unsubscribe?</h2>
            <p>
              Send a one-click unsubscribe request for {pendingUnsubscribe.sender_name || pendingUnsubscribe.sender_email || "this sender"}?
            </p>
            <div className="actions">
              <button className="secondary" type="button" onClick={() => setPendingUnsubscribe(null)}>No</button>
              <button type="button" onClick={() => void confirmUnsubscribe()}>Yes, unsubscribe</button>
            </div>
          </section>
        </div>
      ) : null}
      {!loading ? (
        <section className="thread-shell">
          {thread.map((item, index) => {
            const isExpanded = expanded.has(item.message.id);
            const itemTrashMailbox = trashByAccount.get(item.message.account_id);
            const senderVisual = senderVisualURL(item, brandIcons, pluginSet);
            const unsubscribeSent = unsubscribeSentLabel(item);
            const pgpBody = pgpBodies[item.message.id];
            const decryptedAttachments = pgpBody?.decryptedAttachments || [];
            const pgpMessage = securityEnabled && (item.message.is_encrypted || item.message.is_signed);
            const pgpSignatureVisible = pgpMessage && Boolean(item.message.is_signed || (pgpBody?.signatureStatus && pgpBody.signatureStatus !== "none"));
            const encryptedPreviewLocked = pgpMessage && item.message.is_encrypted && !pgpBody?.doc;
            const decryptedPreviewText = pgpMessage && item.message.is_encrypted && pgpBody?.quoteText ? compactPGPPreviewText(pgpBody.quoteText) : "";
            const securitySnippetClass = encryptedPreviewLocked ? messageSecuritySnippetClassName(messageSecurityPlugins, item.message) : "";
            const previewText = decryptedPreviewText || messageSecurityPreviewText(messageSecurityPlugins, item.snippet, item.message);
            const hasUnlockedSecurityKey = securityUnlock.keys.length > 0 && securityUnlock.unlockedUntil > Date.now();
            const autocryptImport = autocryptImports[item.message.id];
            const showAutocryptImport = Boolean(securityEnabled && autocryptImport && !["ignored", "checking"].includes(autocryptImport.status));
            const gossipImportEntries = Object.entries(autocryptGossipImports[item.message.id] || {})
              .filter(([, state]) => !["ignored", "checking"].includes(state.status));
            const autocryptDiscoveryItems = showAutocryptImport && autocryptImport ? [{
              id: `autocrypt-${item.message.id}`,
              kind: "Autocrypt public key",
              email: autocryptImport.email || item.sender_email,
              state: autocryptImport,
              onImport: () => void importAutocryptPGPPublicKey(item)
            }] : [];
            const gossipDiscoveryItems = gossipImportEntries.map(([discoveryID, state]) => ({
              id: `gossip-${item.message.id}-${discoveryID}`,
              kind: "Autocrypt-Gossip public key",
              email: state.email || state.key?.email || "",
              state,
              onImport: () => void importAutocryptGossipPGPPublicKey(item.message.id, discoveryID)
            }));
            const pgpDiscoveryItems = [...autocryptDiscoveryItems, ...gossipDiscoveryItems];
            const showAttachments = item.attachments.length > 0 || decryptedAttachments.length > 0 || pgpDiscoveryItems.length > 0;
            const activeMessagePanel = messagePluginPanel?.messageID === item.message.id ? messagePluginPanel.panelID : "";
            const actionContext = messageActionContext({
              csrf,
              item,
              datePrefs,
              activePanel: activeMessagePanel,
              openPanel: (panelID) => openMessagePluginPanel(item.message.id, panelID),
              closePanel: () => closeMessagePluginPanel(item.message.id, activeMessagePanel),
              addToast
            });
            const annotationNodes = messageAnnotationNodes(messageSecurityPlugins, item);
            return (
              <article className={`thread-card ${isExpanded ? "" : "collapsed"}`} key={item.message.id}>
                <div
                  className="thread-summary"
                  role="button"
                  tabIndex={0}
                  aria-expanded={isExpanded}
                  onClick={() => toggleMessage(item.message.id)}
                  onKeyDown={(event) => {
                    if (event.key === "Enter" || event.key === " ") {
                      event.preventDefault();
                      toggleMessage(item.message.id);
                    }
                  }}
                >
                  <SenderVisualOrAvatar src={senderVisual} initial={item.sender_initial} />
                  <div className="thread-person">
                    <div className="thread-from">
                      <span>
                        <HighlightedText text={item.sender_name || item.sender_email || "Unknown sender"} query={highlightQuery} terms={highlightTerms} />
                      </span>
                      <span className="thread-email">
                        <HighlightedText text={item.sender_email} query={highlightQuery} terms={highlightTerms} />
                      </span>
                      <MessageSenderSecurityCaution indicators={item.security_indicators} />
                      {annotationNodes}
                      <OneClickUnsubscribeInlineAction
                        item={item}
                        plugins={pluginSet}
                        busy={unsubscribingID === item.message.id}
                        sentLabel={unsubscribeSent}
                        onRequest={requestUnsubscribe}
                      />
                      <ReportedAuthenticationIndicators indicators={item.security_indicators} />
                      {securityEnabled && item.message.is_encrypted ? (
                        <PGPEncryptionPill
                          state={pgpBody}
                          hasUnlockedKey={hasUnlockedSecurityKey}
                          onOpen={() => {
                            if (!hasUnlockedSecurityKey) void openSecurityUnlockForMessage(item);
                            else void openPGPMessage(item);
                          }}
                        />
                      ) : null}
                      {pgpSignatureVisible ? <PGPSignaturePill encrypted={item.message.is_encrypted} state={pgpBody} /> : null}
                    </div>
                    <MessageDetailsToggle
                      summary={item.recipient_line}
                      details={item.header_details || []}
                      indicators={item.security_indicators}
                      pluginRows={messageDetailsNodes(messageSecurityPlugins, item, datePrefs)}
                      highlightQuery={highlightQuery}
                      highlightTerms={highlightTerms}
                    />
                    <div className={`thread-collapsed-snippet ${securitySnippetClass}`}>
                      <HighlightedText text={previewText} query={securitySnippetClass ? "" : highlightQuery} terms={securitySnippetClass ? [] : highlightTerms} />
                    </div>
                  </div>
                  <div className="thread-meta">
                    <span>{displayTime(item.message.date, datePrefs)}</span>
                    <button
                      className={`star-action thread-star ${item.message.is_starred ? "starred" : ""}`}
                      type="button"
                      aria-pressed={item.message.is_starred}
                      title={item.message.is_starred ? "Unstar" : "Star"}
                      onClick={(event) => void toggleThreadStar(event, item)}
                    >
                      <Star className="icon" weight={item.message.is_starred ? "fill" : "regular"} />
                    </button>
                    <details className="message-menu" onClick={(event) => event.stopPropagation()}>
                      <summary className="icon-action" title="Message actions" aria-label="Message actions">
                        <Icon name="more_vert" />
                      </summary>
                      <div className="message-menu-panel">
                        <button type="button" onClick={() => void beginReply(item)}>
                          <Icon name="reply" />
                          Reply
                        </button>
                        {item.can_reply_all ? (
                          <button type="button" onClick={() => void beginReply(item, true)}>
                            <Icon name="reply_all" />
                            Reply all
                          </button>
                        ) : null}
                        <button type="button" onClick={() => openCompose(`forward=${item.message.id}`)}>
                          <Icon name="forward" />
                          Forward
                        </button>
                        <button type="button" onClick={() => openCompose(`forward_attachment=${item.message.id}`)}>
                          <Icon name="attach_file" />
                          Forward as attachment
                        </button>
                        {snoozedUntil && Date.parse(snoozedUntil) > Date.now() ? (
                          <button
                            type="button"
                            disabled={loading}
                            onClick={(event) => {
                              event.currentTarget.closest("details")?.removeAttribute("open");
                              void unsnoozeThread();
                            }}
                          >
                            <Icon name="clock" />
                            Unsnooze
                          </button>
                        ) : (
                          <SnoozeControl
                            datePrefs={datePrefs}
                            disabled={loading || currentMessageID <= 0}
                            onSnooze={snoozeThread}
                          />
                        )}
                        <button type="button" onClick={(event) => void viewOriginal(event, item)}>
                          <Icon name="file_text" />
                          View original
                        </button>
                        {itemTrashMailbox && item.message.mailbox_id !== itemTrashMailbox.id ? (
                          <button type="button" onClick={(event) => void moveToTrash(event, item)}>
                            <Icon name="delete" />
                            Move to trash
                          </button>
                        ) : null}
                        {canExplainSearch ? (
                          <button type="button" onClick={(event) => void toggleSearchExplanation(event, item)}>
                            <Icon name="search" />
                            Why this matched
                          </button>
                        ) : null}
                        <button type="button" onClick={() => void addSender(item)}>
                          <Icon name="group" />
                          Add sender to contacts
                        </button>
                        <OneClickUnsubscribeMenuAction
                          item={item}
                          plugins={pluginSet}
                          busy={unsubscribingID === item.message.id}
                          sentLabel={unsubscribeSent}
                          onRequest={requestUnsubscribe}
                        />
                        {messageMenuActionNodes(messageSecurityPlugins, actionContext)}
                      </div>
                    </details>
                  </div>
                </div>
                {searchExplanations[item.message.id]?.open ? (
                  <SearchExplanationPanel state={searchExplanations[item.message.id]} onClose={() => closeSearchExplanation(item.message.id)} />
                ) : null}
                {messageActionPanelNodes(messageSecurityPlugins, actionContext)}
                <RemoteImageNotice
                  item={item}
                  plugins={pluginSet}
                  onShowImages={() => setShowImages(true)}
                >
                  <TrustImageSourceAction
                    item={item}
                    plugins={pluginSet}
                    onTrustImages={() => trustImages(item.message.id)}
                  />
                </RemoteImageNotice>
                <div className="thread-body">
                  <MessageLinkSecurityNotice indicators={item.security_indicators} />
                  {item.body_preview_only ? (
                    <div className="body-notice">
                      <Icon name="report" />
                      <span>Showing the indexed preview only. rolltop could not fetch the full original from IMAP.</span>
                      <button className="secondary" type="button" onClick={() => navigate("/settings/account")}>Account settings</button>
                    </div>
                  ) : null}
                  {pgpMessage && pgpBody?.loading ? (
                    <div className="body-notice pgp-body-notice">
                      <Icon name="lock_open" />
                      <span>{item.message.is_encrypted ? "Decrypting this message in the browser..." : "Checking this PGP signature in the browser..."}</span>
                    </div>
                  ) : null}
                  {pgpMessage && pgpBody?.error ? (
                    <div className="body-notice pgp-body-notice">
                      <Icon name="report" />
                      <span>{pgpBody.error}</span>
                    </div>
                  ) : null}
                  {pgpMessage && pgpBody?.doc && pgpBody.status && (pgpBody.signatureStatus === "invalid" || pgpBody.signatureStatus === "unverified") ? (
                    <div className="body-notice pgp-body-notice">
                      <Icon name={pgpBody.signatureStatus === "invalid" ? "report" : "lock_open"} />
                      <span>{pgpBody.status}</span>
                    </div>
                  ) : null}
                  {pgpMessage && item.message.is_encrypted && !pgpBody?.doc && !pgpBody?.loading && !pgpBody?.error ? (
                    <div className="body-notice pgp-body-notice encrypted-placeholder">
                      <Icon name="lock" />
                      <span>Encrypted content will appear here after a matching PGP key is unlocked.</span>
                    </div>
                  ) : null}
                  {pgpMessage && item.message.is_encrypted && !pgpBody?.doc ? (
                    <div className="pgp-encrypted-fallback encrypted-preview" aria-hidden="true">
                      {securityPlugin?.encryptedPreviewText} {securityPlugin?.encryptedPreviewText} {securityPlugin?.encryptedPreviewText}
                    </div>
                  ) : null}
                  {pgpMessage && item.message.is_encrypted && !pgpBody?.doc ? null : (
                    <EmailFrame
                      key={pgpBody?.doc ? `pgp-${item.message.id}` : `body-${item.message.id}`}
                      srcDoc={pgpBody?.doc || item.body_doc}
                      highlightQuery={highlightQuery}
                      highlightTerms={highlightTerms}
                      full={Boolean(pgpBody?.doc)}
                    />
                  )}
                  {item.has_hidden_quoted && item.full_body_doc && !(securityEnabled && item.message.is_signed) ? (
                    <QuotedDetails srcDoc={item.full_body_doc} highlightQuery={highlightQuery} highlightTerms={highlightTerms} />
                  ) : null}
                </div>
                {showAttachments ? (
                  <div className="attachments">
                    {pgpDiscoveryItems.length > 0 ? (
                      <details className="pgp-key-attachment-group">
                        <summary className="pgp-key-attachment-toggle" title={`Attached Public Keys: ${pgpDiscoveryItems.length} ${pgpDiscoveryItems.length === 1 ? "key" : "keys"}`} aria-label="Attached Public Keys">
                          <Icon name="chevron_right" />
                          <span>{pgpDiscoveryItems.length} Attached Public {pgpDiscoveryItems.length === 1 ? "Key" : "Keys"}</span>
                        </summary>
                        <div className="attachment-group-list">
                          {pgpDiscoveryItems.map((item) => (
                            <PGPKeyDiscoveryAttachment
                              key={item.id}
                              kind={item.kind}
                              email={item.email}
                              state={item.state}
                              onImport={item.onImport}
                            />
                          ))}
                        </div>
                      </details>
                    ) : null}
                    {decryptedAttachments.map((attachment) => (
                      <div className="attachment-group decrypted-attachment" key={`decrypted-${attachment.id}`}>
                        <a
                          className="attachment"
                          href={attachment.objectURL}
                          download={attachment.filename || "attachment"}
                        >
                          <Icon name="lock_open" />
                          <span>
                            <strong>{attachment.filename || "Attachment"}</strong>
                            <small>{item.message.is_encrypted ? "Decrypted PGP attachment" : "Signed PGP/MIME attachment"} - {formatBytes(attachment.size)}</small>
                          </span>
                        </a>
                      </div>
                    ))}
                    {item.attachments.map((attachment) => (
                      <div className={`attachment-group ${attachment.matched ? "matched" : ""}`} key={attachment.id}>
                        <a
                          className="attachment"
                          href={attachment.download_url}
                          download={attachment.filename || "attachment"}
                        >
                          <Icon name="attach_file" />
                          <HighlightedText
                            text={attachment.filename || "Attachment"}
                            query={highlightQuery}
                            terms={highlightTerms}
                          />
                        </a>
                        <AttachmentPreviewSlot attachment={attachment} plugins={pluginSet} />
                        {securityEnabled ? (
                          <PGPImportStatusAction
                            state={pgpAttachmentImports[attachment.id]}
                            fallbackEmail={item.sender_email}
                            onImport={() => void importAttachmentPGPPublicKey(attachment)}
                          />
                        ) : null}
                      </div>
                    ))}
                  </div>
                ) : null}
                {index === thread.length - 1 && inlineReply?.in_reply_to_id !== item.message.id ? (
                  <div className="thread-actions">
                    <button className="thread-action" type="button" onClick={() => void beginReply(item)}>
                      <Icon name="reply" weight="bold" />
                      Reply
                    </button>
                    {item.can_reply_all ? (
                      <button className="thread-action" type="button" onClick={() => void beginReply(item, true)}>
                        <Icon name="reply_all" weight="bold" />
                        Reply all
                      </button>
                    ) : null}
                    <button
                      className="thread-action"
                      type="button"
                      onClick={() => openCompose(`forward=${item.message.id}`)}
                    >
                      <Icon name="forward" weight="bold" />
                      Forward
                    </button>
                  </div>
                ) : null}
                {inlineReply && inlineReply.in_reply_to_id === item.message.id ? (
                  <div className="inline-reply-row">
                    <div className="avatar inline-reply-avatar">{composeInitial}</div>
                    <ComposeBox
                      userID={userID}
                      csrf={csrf}
                      composeFrom={composeFrom}
                      identities={fromIdentities}
                      initial={inlineReply}
                      recoveryContext={`reply:${item.message.id}`}
                      inline
                      securityEnabled={securityEnabled}
                      securityPlugins={messageSecurityPlugins}
                      securityUnlock={securityUnlock}
                      openSecurityUnlock={openSecurityUnlock}
                      addToast={addToast}
                      onSent={() => {
                        setInlineReply(null);
                        void load(showImages);
                      }}
                      onCancel={() => setInlineReply(null)}
                    />
                  </div>
                ) : null}
                {index === thread.length - 1 && !inlineReply ? <div className="thread-tail" /> : null}
              </article>
            );
          })}
        </section>
      ) : null}
    </>
  );
}


function SearchExplanationPanel({ state, onClose }: { state: SearchExplanationState; onClose: () => void }) {
  const data = state.data;
  const explainedDifferentMessage = data?.matched && data.message_id && data.requested_message_id && data.message_id !== data.requested_message_id;
  return (
    <section className="search-explanation" aria-live="polite">
      <div className="search-explanation-head">
        <div>
          <strong>Why this matched</strong>
          {data?.score !== undefined ? <span>Score {formatSearchScore(data.score)}</span> : null}
        </div>
        <button className="ghost search-explanation-close" type="button" title="Close" aria-label="Close search explanation" onClick={onClose}>
          <Icon name="close" />
        </button>
      </div>
      {state.loading ? <p>Loading scoring details...</p> : null}
      {state.error ? <p className="error-text">{state.error}</p> : null}
      {!state.loading && !state.error && data && !data.matched ? (
        <p>{data.reason || "No message in this conversation matched the current search."}</p>
      ) : null}
      {!state.loading && !state.error && data?.matched ? (
        <>
          {explainedDifferentMessage ? <p className="search-explanation-note">Explaining the message in this conversation that actually matched the search result.</p> : null}
          <div className="search-explanation-grid">
            <div>
              <span className="search-explanation-label">Query</span>
              <p>{data.query}</p>
            </div>
            <div>
              <span className="search-explanation-label">Bleve Terms</span>
              <p>{data.query_terms && data.query_terms.length > 0 ? data.query_terms.join(", ") : "No field-qualified text terms reported"}</p>
            </div>
          </div>
          {data.field_matches && data.field_matches.length > 0 ? (
            <div className="search-explanation-section">
              <span className="search-explanation-label">Matched Sections</span>
              <div className="search-explanation-contributions">
                {data.field_matches.map((match) => (
                  <div className="search-explanation-contribution" key={match.field}>
                    <strong>{searchFieldLabel(match.field)}</strong>
                    <span>{match.terms.length > 0 ? match.terms.join(", ") : "Matched"}</span>
                  </div>
                ))}
              </div>
            </div>
          ) : null}
          {data.term_contributions && data.term_contributions.length > 0 ? (
            <div className="search-explanation-section">
              <span className="search-explanation-label">Scoring Terms</span>
              <div className="search-explanation-term-list">
                {data.term_contributions.map((item) => (
                  <div className="search-explanation-term" key={`${item.query_term}:${item.score}`}>
                    <div>
                      <code>{item.query_term}</code>
                      <strong>{formatSearchScore(item.score)}</strong>
                    </div>
                    <span>{item.section}{contributionDetailParts(item).length > 0 ? ` / ${contributionDetailParts(item).join(" / ")}` : ""}</span>
                  </div>
                ))}
              </div>
            </div>
          ) : null}
          {data.score !== undefined ? (
            <div className="search-explanation-section">
              <span className="search-explanation-label">Rank Score</span>
              <div className="search-explanation-score">
                <div>
                  <strong>{formatSearchScore(data.score)}</strong>
                  <span>Final Bleve rank score for this exact query</span>
                </div>
              </div>
              <p className="search-explanation-note">Text-match terms and ranking boosts both contribute to the final rank score. Raw scores from different query shapes are not directly comparable.</p>
            </div>
          ) : null}
          {data.boosts && data.boosts.length > 0 ? (
            <div className="search-explanation-section">
              <span className="search-explanation-label">Ranking Boosts</span>
              <div className="search-explanation-boosts">
                {data.boosts.map((boost) => (
                  <div className="search-explanation-boost" key={`${boost.kind}:${boost.label}`}>
                    <strong>{boost.label}</strong>
                    <span>{boost.description}</span>
                    {boost.value || boost.boost !== undefined ? (
                      <code>{boost.value || "boost"}{boost.boost !== undefined ? ` / boost ${formatSearchScore(boost.boost)}` : ""}</code>
                    ) : null}
                  </div>
                ))}
              </div>
            </div>
          ) : null}
        </>
      ) : null}
    </section>
  );
}

function contributionDetailParts(item: NonNullable<SearchExplanation["term_contributions"]>[number]): string[] {
  const parts: string[] = [];
  if (item.term_frequency) parts.push(`tf ${formatSearchScore(item.term_frequency)}`);
  if (item.idf) parts.push(`idf ${formatSearchScore(item.idf)}`);
  if (item.field_norm) parts.push(`norm ${formatSearchScore(item.field_norm)}`);
  if (item.query_weight) parts.push(`query weight ${formatSearchScore(item.query_weight)}`);
  if (item.boost && item.boost !== 1) parts.push(`boost ${formatSearchScore(item.boost)}`);
  return parts;
}

function searchFieldLabel(field: string): string {
  switch (field) {
    case "subject":
    case "subject_compound":
      return "Subject";
    case "from":
    case "from_compound":
    case "from_domain":
      return "Sender";
    case "to":
      return "To";
    case "cc":
      return "Cc";
    case "body":
      return "Body";
    case "attachment_names":
      return "Attachment name";
    case "attachment_types":
      return "Attachment type";
    case "attachments":
      return "Attachment text";
    case "compound":
      return "Joined words";
    case "message_id":
      return "Message ID";
    default:
      return field.replace(/_/g, " ");
  }
}

function formatSearchScore(value: number): string {
  if (!Number.isFinite(value)) return "0";
  if (Math.abs(value) >= 1000) return value.toLocaleString(undefined, { maximumFractionDigits: 0 });
  if (Math.abs(value) >= 10) return value.toLocaleString(undefined, { maximumFractionDigits: 1 });
  return value.toLocaleString(undefined, { maximumFractionDigits: 3 });
}

// QuotedDetails lazy-renders the full body iframe only after the user asks for
// hidden quoted text, avoiding extra iframe work during initial thread paint.
function QuotedDetails({
  srcDoc,
  highlightQuery,
  highlightTerms
}: {
  srcDoc: string;
  highlightQuery: string;
  highlightTerms: string[];
}) {
  const [open, setOpen] = useState(false);

  return (
    <details className="quoted-details" onToggle={(event) => setOpen(event.currentTarget.open)}>
      <summary>...</summary>
      {open ? <EmailFrame srcDoc={srcDoc} highlightQuery={highlightQuery} highlightTerms={highlightTerms} full /> : null}
    </details>
  );
}

function currentEmailDocumentTheme(): "classic" | "classic_dark" | "matrix" {
  const theme = document.documentElement.dataset.theme;
  return theme === "classic_dark" || theme === "matrix" ? theme : "classic";
}

function themedEmailSrcDoc(srcDoc: string): string {
  const theme = currentEmailDocumentTheme();
  if (theme === "classic") return srcDoc;
  return srcDoc.replace(/<html(\s|>)/i, `<html data-rolltop-theme="${theme}"$1`);
}

function applyEmailDocumentTheme(doc: Document | null | undefined) {
  if (!doc) return;
  const theme = currentEmailDocumentTheme();
  if (theme === "classic") {
    doc.documentElement.removeAttribute("data-rolltop-theme");
    return;
  }
  doc.documentElement.setAttribute("data-rolltop-theme", theme);
}

// EmailFrame isolates message HTML in a sandboxed iframe, applies the active
// Rolltop theme, highlights search terms inside the iframe document, and
// repeatedly measures height because images/fonts can settle after load.
function EmailFrame({
  srcDoc,
  highlightQuery = "",
  highlightTerms = [],
  full = false
}: {
  srcDoc: string;
  highlightQuery?: string;
  highlightTerms?: string[];
  full?: boolean;
}) {
  const ref = useRef<HTMLIFrameElement | null>(null);
  const [height, setHeight] = useState(full ? 220 : 96);
  const highlightKey = `${highlightQuery}:${highlightTerms.join(",")}`;
  const themedSrcDoc = themedEmailSrcDoc(srcDoc);

  useEffect(() => {
    setHeight(full ? 220 : 96);
  }, [srcDoc, highlightKey, full]);

  function resize() {
    const doc = ref.current?.contentDocument;
    const body = doc?.body;
    const html = doc?.documentElement;
    if (!body || !html) return;
    const next = Math.max(body.scrollHeight, body.offsetHeight, html.scrollHeight, html.offsetHeight, full ? 180 : 84) + 12;
    setHeight(next);
  }

  return (
    <iframe
      ref={ref}
      className={`email-frame ${full ? "full" : ""}`}
      srcDoc={themedSrcDoc}
      title="Email body"
      sandbox="allow-same-origin allow-popups allow-popups-to-escape-sandbox"
      scrolling="no"
      style={{ height }}
      onLoad={() => {
        const doc = ref.current?.contentDocument;
        applyEmailDocumentTheme(doc);
        highlightEmailDocument(doc, highlightQuery, highlightTerms);
        resize();
        window.requestAnimationFrame(resize);
        window.setTimeout(resize, 120);
        window.setTimeout(resize, 600);
        window.setTimeout(resize, 1600);
      }}
    />
  );
}
