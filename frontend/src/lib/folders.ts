// File overview: Folder tree helpers. They turn flat mailbox rows into nested sidebar/settings
// nodes without changing the backend mailbox identifiers.

import type { Mailbox } from "../types";

/** FolderNode is one mailbox plus nested child folders for sidebar/settings tree rendering. */
export type FolderNode = {
  mailbox: Mailbox;
  label: string;
  children: FolderNode[];
};

/** folderTree converts flat mailbox names into a nested tree while preserving mailbox IDs. */
export function folderTree(mailboxes: Mailbox[], options: { includeHidden?: boolean } = {}): FolderNode[] {
  const visible = options.includeHidden ? mailboxes : mailboxes.filter((mailbox) => mailbox.show_in_sidebar !== false);
  const byName = new Map(visible.map((mailbox) => [folderAccountKey(mailbox, mailbox.name), mailbox]));
  const nodes = new Map<number, FolderNode>();
  for (const mailbox of visible) {
    nodes.set(mailbox.id, { mailbox, label: folderLabel(mailbox.name), children: [] });
  }
  const roots: FolderNode[] = [];
  for (const mailbox of visible) {
    const node = nodes.get(mailbox.id);
    if (!node) continue;
    const parent = closestVisibleParent(mailbox, byName);
    if (!parent) {
      roots.push(node);
      continue;
    }
    const parentNode = nodes.get(parent.id);
    if (!parentNode) {
      roots.push(node);
      continue;
    }
    node.label = folderLabel(mailbox.name, parent.name);
    parentNode.children.push(node);
  }
  const sortNodes = (items: FolderNode[]) => {
    items.sort((a, b) => folderSortKey(a.mailbox).localeCompare(folderSortKey(b.mailbox), undefined, { numeric: true, sensitivity: "base" }));
    for (const item of items) sortNodes(item.children);
    return items;
  };
  return sortNodes(roots);
}

function closestVisibleParent(mailbox: Mailbox, byName: Map<string, Mailbox>): Mailbox | null {
  for (const parent of folderParentNames(mailbox.name)) {
    const match = byName.get(folderAccountKey(mailbox, parent));
    if (match) return match;
  }
  return null;
}

function folderAccountKey(mailbox: Pick<Mailbox, "account_id">, name: string): string {
  return `${mailbox.account_id}:${name}`;
}

/** folderParentNames returns the implied parent paths for a mailbox name. */
export function folderParentNames(name: string): string[] {
  const out: string[] = [];
  for (let i = name.length - 1; i > 0; i--) {
    if (name[i] === "." || name[i] === "/" || name[i] === "\\") out.push(name.slice(0, i));
  }
  return out;
}

/** folderLabel returns the display label for a mailbox relative to an optional parent path. */
export function folderLabel(name: string, parent = ""): string {
  if (!parent) return name;
  const next = name.slice(parent.length);
  return next.replace(/^[./\\]+/, "") || name;
}

/** folderSortKey gives inbox/system folders predictable order before alphabetical folders. */
export function folderSortKey(mailbox: Mailbox): string {
  if (mailbox.role === "inbox" || mailbox.name.toLowerCase() === "inbox") return "00";
  if (mailbox.role === "trash") return `90:${mailbox.name}`;
  return `10:${mailbox.name}`;
}

/** folderTreeUnreadCount totals unread mail across folder subtrees, skipping junk and trash. */
export function folderTreeUnreadCount(nodes: FolderNode[]): number {
  let total = 0;
  for (const node of nodes) {
    if (node.mailbox.role === "junk" || node.mailbox.role === "trash") continue;
    total += Math.max(0, node.mailbox.unread_count) + folderTreeUnreadCount(node.children);
  }
  return total;
}

/** nodeContainsMailbox checks whether a tree node contains the active mailbox ID. */
export function nodeContainsMailbox(node: FolderNode, id: string | null): boolean {
  if (!id) return false;
  return String(node.mailbox.id) === id || node.children.some((child) => nodeContainsMailbox(child, id));
}
