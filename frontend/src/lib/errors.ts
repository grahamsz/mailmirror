// File overview: Error-message extraction helpers for user-visible toast and panel failures.

import { ApiError } from "../api";

/** messageFromError extracts a useful user-facing message from thrown API or runtime errors. */
export function messageFromError(err: unknown): string {
  if (err instanceof ApiError) return err.message;
  if (err instanceof Error) return err.message;
  return "Something went wrong.";
}

/**
 * isNetworkError reports whether a failure looks like missing connectivity
 * rather than an answer from the server. Fetch rejects with TypeError for
 * network problems, and some browsers surface them as "Load failed".
 */
export function isNetworkError(err: unknown): boolean {
  if (typeof navigator !== "undefined" && navigator.onLine === false) return true;
  if (err instanceof ApiError) return false;
  if (err instanceof TypeError) return true;
  if (err instanceof Error) return /failed to fetch|networkerror|load failed|internet connection/i.test(err.message);
  return false;
}
