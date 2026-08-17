// File overview: Date, time, count, and byte formatting helpers. They apply per-user localization
// preferences while keeping list and thread rendering consistent.

import type { DatePrefs } from "../appTypes";

/** formatBytes renders unknown byte counts as compact human-readable storage text. */
export function formatBytes(value: unknown): string {
  const bytes = typeof value === "number" ? value : Number(value || 0);
  if (!Number.isFinite(bytes) || bytes <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let amount = bytes;
  let index = 0;
  while (amount >= 1024 && index < units.length - 1) {
    amount /= 1024;
    index++;
  }
  return `${amount >= 10 || index === 0 ? amount.toFixed(0) : amount.toFixed(1)} ${units[index]}`;
}

function dateLocale(prefs?: DatePrefs): string | undefined {
  const locale = prefs?.date_locale?.trim();
  if (!locale) return undefined;
  try {
    Intl.DateTimeFormat(locale);
    return locale;
  } catch {
    return undefined;
  }
}

function isSameLocalDay(a: Date, b: Date): boolean {
  return a.getFullYear() === b.getFullYear() && a.getMonth() === b.getMonth() && a.getDate() === b.getDate();
}

function isOlderThanLastYear(date: Date, now = new Date()): boolean {
  const cutoff = new Date(now);
  cutoff.setFullYear(cutoff.getFullYear() - 1);
  cutoff.setHours(0, 0, 0, 0);
  return date < cutoff;
}

function numericDate(date: Date, prefs?: DatePrefs): string {
  const pad = (value: number) => String(value).padStart(2, "0");
  const mm = pad(date.getMonth() + 1);
  const dd = pad(date.getDate());
  const yy = pad(date.getFullYear() % 100);
  switch (prefs?.date_format) {
    case "dmy":
      return `${dd}/${mm}/${yy}`;
    case "ymd":
      return `${yy}/${mm}/${dd}`;
    case "locale":
      return date.toLocaleDateString(dateLocale(prefs), { year: "2-digit", month: "numeric", day: "numeric" });
    case "mdy":
    default:
      return `${mm}/${dd}/${yy}`;
  }
}

/** displayTime formats list/thread dates according to user preferences and message age. */
export function displayTime(value: string, prefs?: DatePrefs): string {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  const today = new Date();
  const locale = dateLocale(prefs);
  if (isSameLocalDay(date, today)) {
    return date.toLocaleTimeString(locale, { hour: "numeric", minute: "2-digit" });
  }
  if (isOlderThanLastYear(date, today)) {
    return numericDate(date, prefs);
  }
  return date.toLocaleDateString(locale, { month: "short", day: "numeric" });
}

/** displayDateTime renders a full localized date/time for details and settings history. */
export function displayDateTime(value: string, prefs?: DatePrefs): string {
  if (!value) return "";
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  const locale = dateLocale(prefs);
  if (isOlderThanLastYear(date)) {
    return `${numericDate(date, prefs)} ${date.toLocaleTimeString(locale, { hour: "numeric", minute: "2-digit" })}`;
  }
  return date.toLocaleString(locale, { month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
}

/** displaySnoozeUntil keeps same-day confirmations compact and adds a localized
 * calendar day when the reminder is later than today. */
export function displaySnoozeUntil(value: string | Date, prefs?: DatePrefs, now = new Date()): string {
  const date = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(date.getTime())) return typeof value === "string" ? value : "";
  const locale = dateLocale(prefs);
  if (isSameLocalDay(date, now)) {
    return new Intl.DateTimeFormat(locale, { hour: "numeric", minute: "2-digit" }).format(date);
  }
  return new Intl.DateTimeFormat(locale, {
    weekday: "short",
    month: "short",
    day: "numeric",
    ...(date.getFullYear() !== now.getFullYear() ? { year: "numeric" as const } : {}),
    hour: "numeric",
    minute: "2-digit"
  }).format(date);
}

/** messageCountLabel renders "1 message" / "N messages" for move/copy/delete toasts. */
export function messageCountLabel(count: number): string {
  return `${count.toLocaleString()} ${count === 1 ? "message" : "messages"}`;
}
