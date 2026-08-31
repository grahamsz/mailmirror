// Bounded, user-scoped compose persistence. localStorage is origin-scoped by
// the browser; user IDs prevent drafts and templates crossing local accounts.

const recoveryVersion = 1;
const maxRecipientLength = 16_000;
const maxSubjectLength = 2_000;
const maxBodyLength = 200_000;
const maxHTMLLength = 300_000;
const maxRecoveryEntries = 6;
const maxTemplates = 20;
const maxTemplateBytes = 800_000;

export type LocalComposeContent = {
  submissionKey: string;
  to: string;
  cc: string;
  bcc: string;
  subject: string;
  body: string;
  bodyHTML: string;
  fromIdentityID: number;
};

export type LocalComposeRecovery = LocalComposeContent & {
  version: number;
  updatedAt: number;
};

export type LocalComposeTemplate = Pick<LocalComposeContent, "subject" | "body" | "bodyHTML"> & {
  id: string;
  name: string;
  updatedAt: number;
};

export function composeRecoveryStorageKey(userID: number, context: string): string {
  return `rolltop.compose.recovery.v${recoveryVersion}.${userID}.${stableContextHash(context)}`;
}

export function loadComposeRecovery(userID: number, context: string): LocalComposeRecovery | null {
  if (userID <= 0) return null;
  try {
    const parsed = JSON.parse(localStorage.getItem(composeRecoveryStorageKey(userID, context)) || "null") as Partial<LocalComposeRecovery> | null;
    if (!parsed || parsed.version !== recoveryVersion || !Number.isFinite(parsed.updatedAt)) return null;
    return {
      version: recoveryVersion,
      updatedAt: Number(parsed.updatedAt),
      submissionKey: safeString(parsed.submissionKey).slice(0, 160),
      to: safeString(parsed.to).slice(0, maxRecipientLength),
      cc: safeString(parsed.cc).slice(0, maxRecipientLength),
      bcc: safeString(parsed.bcc).slice(0, maxRecipientLength),
      subject: safeString(parsed.subject).slice(0, maxSubjectLength),
      body: safeString(parsed.body).slice(0, maxBodyLength),
      bodyHTML: safeString(parsed.bodyHTML).slice(0, maxHTMLLength),
      fromIdentityID: positiveInteger(parsed.fromIdentityID)
    };
  } catch {
    return null;
  }
}

export function saveComposeRecovery(userID: number, context: string, content: LocalComposeContent): boolean {
  if (userID <= 0) return false;
  const recovery: LocalComposeRecovery = {
    version: recoveryVersion,
    updatedAt: Date.now(),
    submissionKey: content.submissionKey.slice(0, 160),
    to: content.to.slice(0, maxRecipientLength),
    cc: content.cc.slice(0, maxRecipientLength),
    bcc: content.bcc.slice(0, maxRecipientLength),
    subject: content.subject.slice(0, maxSubjectLength),
    body: content.body.slice(0, maxBodyLength),
    bodyHTML: content.bodyHTML.length <= maxHTMLLength ? content.bodyHTML : "",
    fromIdentityID: positiveInteger(content.fromIdentityID)
  };
  const key = composeRecoveryStorageKey(userID, context);
  const serialized = JSON.stringify(recovery);
  const oldEntries = composeRecoveryEntries(userID, key);
  while (oldEntries.length >= maxRecoveryEntries) {
    removeLocalStorageItem(oldEntries.shift()?.key || "");
  }
  if (setLocalStorageItem(key, serialized)) return true;
  // A large older recovery can still exhaust the origin quota. Prefer the
  // active composer, pruning only this user's oldest Rolltop recoveries.
  while (oldEntries.length > 0) {
    removeLocalStorageItem(oldEntries.shift()?.key || "");
    if (setLocalStorageItem(key, serialized)) return true;
  }
  return false;
}

export function clearComposeRecovery(userID: number, context: string) {
  if (userID <= 0) return;
  try {
    localStorage.removeItem(composeRecoveryStorageKey(userID, context));
  } catch {
    // Storage may be unavailable in private or locked-down browser contexts.
  }
}

export function composeContentEqual(left: LocalComposeContent, right: LocalComposeContent): boolean {
  return left.submissionKey === right.submissionKey &&
    left.to === right.to && left.cc === right.cc && left.bcc === right.bcc &&
    left.subject === right.subject && left.body === right.body && left.bodyHTML === right.bodyHTML &&
    left.fromIdentityID === right.fromIdentityID;
}

export function loadComposeTemplates(userID: number): LocalComposeTemplate[] {
  if (userID <= 0) return [];
  try {
    const parsed = JSON.parse(localStorage.getItem(templateStorageKey(userID)) || "[]") as Partial<LocalComposeTemplate>[];
    if (!Array.isArray(parsed)) return [];
    return parsed.slice(0, maxTemplates).flatMap((item) => {
      const id = safeString(item.id).slice(0, 100);
      const name = safeString(item.name).trim().slice(0, 80);
      if (!id || !name) return [];
      return [{
        id,
        name,
        subject: safeString(item.subject).slice(0, maxSubjectLength),
        body: safeString(item.body).slice(0, maxBodyLength),
        bodyHTML: safeString(item.bodyHTML).slice(0, maxHTMLLength),
        updatedAt: Number.isFinite(item.updatedAt) ? Number(item.updatedAt) : 0
      }];
    });
  } catch {
    return [];
  }
}

export function saveComposeTemplates(userID: number, templates: LocalComposeTemplate[]): boolean {
  if (userID <= 0) return false;
  const bounded: LocalComposeTemplate[] = [];
  let bytes = 2;
  for (const template of templates.slice(0, maxTemplates)) {
    if (!template.id || !template.name.trim() || template.bodyHTML.length > maxHTMLLength || template.body.length > maxBodyLength) return false;
    const normalized = {
      ...template,
      id: template.id.slice(0, 100),
      name: template.name.trim().slice(0, 80),
      subject: template.subject.slice(0, maxSubjectLength)
    };
    const size = JSON.stringify(normalized).length + 1;
    if (bytes + size > maxTemplateBytes) return false;
    bytes += size;
    bounded.push(normalized);
  }
  try {
    localStorage.setItem(templateStorageKey(userID), JSON.stringify(bounded));
    return true;
  } catch {
    return false;
  }
}

function templateStorageKey(userID: number): string {
  return `rolltop.compose.templates.v${recoveryVersion}.${userID}`;
}

function composeRecoveryEntries(userID: number, keepKey: string): Array<{ key: string; updatedAt: number }> {
  const prefix = `rolltop.compose.recovery.v${recoveryVersion}.${userID}.`;
  const entries: Array<{ key: string; updatedAt: number }> = [];
  try {
    for (let index = 0; index < localStorage.length; index += 1) {
      const key = localStorage.key(index) || "";
      if (!key.startsWith(prefix) || key === keepKey) continue;
      let updatedAt = 0;
      try {
        const value = JSON.parse(localStorage.getItem(key) || "null") as { updatedAt?: unknown } | null;
        if (value && typeof value.updatedAt === "number" && Number.isFinite(value.updatedAt)) updatedAt = value.updatedAt;
      } catch {
        // Malformed entries are treated as oldest and removed first.
      }
      entries.push({ key, updatedAt });
    }
  } catch {
    return [];
  }
  return entries.sort((left, right) => left.updatedAt - right.updatedAt || left.key.localeCompare(right.key));
}

function setLocalStorageItem(key: string, value: string): boolean {
  try {
    localStorage.setItem(key, value);
    return true;
  } catch {
    return false;
  }
}

function removeLocalStorageItem(key: string) {
  if (!key) return;
  try {
    localStorage.removeItem(key);
  } catch {
    // Storage may be unavailable in private or locked-down browser contexts.
  }
}

function stableContextHash(value: string): string {
  let hash = 2166136261;
  for (let index = 0; index < value.length; index += 1) {
    hash ^= value.charCodeAt(index);
    hash = Math.imul(hash, 16777619);
  }
  return (hash >>> 0).toString(36);
}

function safeString(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function positiveInteger(value: unknown): number {
  const number = Number(value);
  return Number.isFinite(number) && number > 0 ? Math.floor(number) : 0;
}
